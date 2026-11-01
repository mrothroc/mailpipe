package cli

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/imapmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/msauth"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type subjectAuth interface {
	Subject(context.Context) (msauth.Subject, error)
	TokenFor(context.Context, msauth.Subject) (string, error)
}
type pinnedAuth struct {
	Authenticator
	identity subjectAuth
	subject  msauth.Subject
}

func (a pinnedAuth) Token(ctx context.Context) (string, error) {
	return a.identity.TokenFor(ctx, a.subject)
}
func lazyGraphProvider(ctx context.Context, deps Deps) func() (mailbox.Provider, error) {
	var once sync.Once
	var p mailbox.Provider
	var result error
	return func() (mailbox.Provider, error) {
		once.Do(func() {
			if deps.Provider != nil {
				p, result = deps.Provider(ctx)
				if result == nil && p != nil {
					result = p.Account().Validate()
				}
				if result == nil && p == nil {
					result = errors.New("mailbox provider is required")
				}
				return
			}
			a, err := deps.Auth()
			if err != nil {
				result = err
				return
			}
			identity, ok := a.(subjectAuth)
			if !ok {
				result = errors.New("authenticated immutable mailbox subject is required")
				return
			}
			subject, err := identity.Subject(ctx)
			if err != nil {
				result = err
				return
			}
			p, result = graphmail.New(deps.Graph(pinnedAuth{a, identity, subject}), subject.Account())
		})
		return p, result
	}
}
func routing() (config.Config, error) { return config.Load(config.DefaultPath()) }
func openBound(ctx context.Context, deps Deps, path string, a mailbox.Account) (*store.AccountStore, error) {
	if deps.AccountStore != nil {
		return deps.AccountStore(ctx, path, a)
	}
	return store.OpenAccount(ctx, path, a)
}

// graphSyncSource maps domain evidence to persistence supplements, never wire
// fields. It does not perform a second fetch or retain a mutable page cache.
type syncProvider interface {
	mailbox.Provider
	Resolve(context.Context, string) (mailbox.Collection, error)
	Total(context.Context, mailbox.CollectionRef) (int, error)
	SyncStates(context.Context, mailbox.SyncRequest) (mailbox.SyncPage, []mailbox.State, error)
}
type graphSyncSource struct{ syncProvider }

func (s graphSyncSource) ReadPage(ctx context.Context, r mailbox.SyncRequest) (mailbox.SyncPage, []store.MessageRecord, error) {
	page, states, err := s.SyncStates(ctx, r)
	if err != nil {
		return page, nil, err
	}
	records := make([]store.MessageRecord, len(states))
	for i, state := range states {
		records[i] = store.MessageRecord{Message: mailbox.Message{Ref: state.Ref, Metadata: state.Metadata}, GraphID: string(state.Ref.MessageID), Revision: state.Revision}
	}
	return page, records, nil
}

func resolveCollection(ctx context.Context, p readMailbox, name string) (mailbox.Collection, error) {
	resolver, ok := p.(interface {
		Resolve(context.Context, string) (mailbox.Collection, error)
	})
	if !ok {
		return mailbox.Collection{}, &mailbox.UnsupportedError{Operation: "collection alias resolution"}
	}
	return resolver.Resolve(ctx, name)
}

// readMailbox matches real content/read methods without inventing page sync.
type readMailbox interface {
	Account() mailbox.Account
	Collections(context.Context) ([]mailbox.Collection, error)
	Metadata(context.Context, mailbox.MessageRef) (mailbox.Metadata, error)
	Text(context.Context, mailbox.MessageRef) (string, error)
	WriteMIME(context.Context, mailbox.MessageRef, io.Writer) error
	Inspect(context.Context, mailbox.MessageRef) (mailbox.State, error)
}

// lazyProvider owns only the IMAP client it constructs. Injected Graph providers
// remain borrowed. A command shares this getter across its body/hook callbacks.
func lazyProvider(ctx context.Context, deps Deps) (func() (readMailbox, error), func() error) {
	var once sync.Once
	var mu sync.Mutex
	var closed bool
	var p readMailbox
	var owned *imapmail.Client
	var result error
	get := func() (readMailbox, error) {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return nil, errors.New("mailbox client closed")
		}
		once.Do(func() {
			c, e := routing()
			if e != nil {
				result = e
				return
			}
			if c.Mode() != "yahoo" {
				p, result = lazyGraphProvider(ctx, deps)()
				return
			}
			owned, result = newYahooClient(deps, c, func(ctx context.Context) (imapmail.Credentials, error) { return credentials(ctx, deps, c) })
			if result == nil {
				p = owned
			}
		})
		return p, result
	}
	var closeOnce sync.Once
	var closeErr error
	close := func() error {
		closeOnce.Do(func() {
			mu.Lock()
			closed = true
			client := owned
			mu.Unlock()
			if client != nil {
				closeErr = client.Close()
			}
		})
		return closeErr
	}
	return get, close
}
