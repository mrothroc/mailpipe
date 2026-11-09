package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/imapauth"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
	"github.com/spf13/cobra"
)

type configTestSession struct {
	mu             sync.Mutex
	enter, release chan struct{}
	once           sync.Once
	attempted      chan struct{}
}

func (s *configTestSession) Session(ctx context.Context, fn func(imapauth.Items) error) error {
	if s.attempted != nil {
		select {
		case s.attempted <- struct{}{}:
		default:
		}
	}
	if s.enter != nil {
		s.once.Do(func() {
			close(s.enter)
			select {
			case <-s.release:
			case <-ctx.Done():
			}
		})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(nil) // configuration needs no secret items
}

func TestConcurrentProcessingUpdatesRetainBothDenies(t *testing.T) {
	ctx, c, p, deps, path := graphConfigFixture(t)
	db, e := store.OpenAccount(ctx, path, c.Account())
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	p.unpause()
	sessions := &configTestSession{attempted: make(chan struct{}, 4)}
	deps.Credentials = func() (imapauth.Store, error) { return sessions, nil }
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unpause := func() { once.Do(func() { close(release) }) }
	defer unpause()
	var saveOnce sync.Once
	deps.SaveConfig = func(path string, c config.Config) error {
		saveOnce.Do(func() {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
		return config.Save(path, c)
	}
	first := configurationCommand(deps, ctx, "config", "processing", "--account", c.AccountID, "--models=false")
	second := configurationCommand(deps, ctx, "config", "processing", "--account", c.AccountID, "--external-stages=false")
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() { firstDone <- first.Execute() }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first update did not reach save")
	}
	// Drain the first session attempt. The second writer announces admission
	// before waiting on the same session; no timing sleep is needed.
	select {
	case <-sessions.attempted:
	default:
	}
	go func() { secondDone <- second.Execute() }()
	secondFinished := false
	select {
	case <-sessions.attempted:
	case e := <-secondDone:
		if e != nil {
			t.Fatal(e)
		}
		secondFinished = true
	case <-ctx.Done():
		t.Fatal("second update did not attempt admission")
	}
	unpause()
	if e = <-firstDone; e != nil {
		t.Fatal(e)
	}
	if !secondFinished {
		if e = <-secondDone; e != nil {
			t.Fatal(e)
		}
	}
	after, e := routing()
	if e != nil {
		t.Fatal(e)
	}
	if after.Processing == nil || after.Processing.Models || after.Processing.ExternalStages {
		t.Fatal("concurrent partial updates restored a denied scope")
	}
}

type configTestGraph struct {
	releaseOnce    sync.Once
	account        mailbox.Account
	enter, release chan struct{}
	once           sync.Once
}

func (p *configTestGraph) unpause()                 { p.releaseOnce.Do(func() { close(p.release) }) }
func (p *configTestGraph) Account() mailbox.Account { return p.account }
func (p *configTestGraph) collection(name string) mailbox.Collection {
	return mailbox.Collection{Ref: mailbox.CollectionRef{AccountID: p.account.ID, CollectionID: mailbox.CollectionID("invented-" + name)}, Name: name}
}
func (p *configTestGraph) Resolve(ctx context.Context, name string) (mailbox.Collection, error) {
	if p.enter != nil {
		p.once.Do(func() {
			close(p.enter)
			select {
			case <-p.release:
			case <-ctx.Done():
			}
		})
	}
	return p.collection(name), ctx.Err()
}
func (p *configTestGraph) Collections(context.Context) ([]mailbox.Collection, error) {
	return []mailbox.Collection{p.collection("inbox")}, nil
}
func (*configTestGraph) Total(context.Context, mailbox.CollectionRef) (int, error) { return 0, nil }
func (p *configTestGraph) Sync(_ context.Context, r mailbox.SyncRequest) (mailbox.SyncPage, error) {
	return mailbox.SyncPage{Mode: mailbox.Snapshot, Complete: true, Checkpoint: mailbox.Cursor{AccountID: p.account.ID, CollectionID: r.Collection.CollectionID, Token: "complete"}}, nil
}
func (p *configTestGraph) SyncStates(ctx context.Context, r mailbox.SyncRequest) (mailbox.SyncPage, []mailbox.State, error) {
	v, e := p.Sync(ctx, r)
	return v, nil, e
}
func (*configTestGraph) Metadata(context.Context, mailbox.MessageRef) (mailbox.Metadata, error) {
	return mailbox.Metadata{}, errors.New("unexpected content")
}
func (*configTestGraph) Text(context.Context, mailbox.MessageRef) (string, error) {
	return "", errors.New("unexpected content")
}
func (*configTestGraph) WriteMIME(context.Context, mailbox.MessageRef, io.Writer) error {
	return errors.New("unexpected content")
}
func (*configTestGraph) Inspect(context.Context, mailbox.MessageRef) (mailbox.State, error) {
	return mailbox.State{}, errors.New("unexpected inspection")
}

func configurationCommand(deps Deps, ctx context.Context, args ...string) *cobra.Command {
	cmd := NewRootCommand(deps)
	cmd.SetContext(ctx)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	return cmd
}
func graphConfigFixture(t *testing.T) (context.Context, config.Config, *configTestGraph, Deps, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("MAILPIPE_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("MAILPIPE_DB", filepath.Join(dir, "mail.db"))
	t.Setenv("MAILPIPE_OUT_DIR", filepath.Join(dir, "out"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	c := config.Config{ClientID: "11111111-1111-4111-8111-111111111111", TenantID: "22222222-2222-4222-8222-222222222222", AccountID: "invented-graph-account"}
	if e := config.Save(config.DefaultPath(), c); e != nil {
		t.Fatal(e)
	}
	p := &configTestGraph{account: c.Account(), enter: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(p.unpause)
	sessions := &configTestSession{}
	deps := Deps{Store: store.Open, AccountStore: store.OpenAccount, Provider: func(context.Context) (mailbox.Provider, error) { return p, nil }, Credentials: func() (imapauth.Store, error) { return sessions, nil }, Secret: func(context.Context, string, string) (string, error) {
		t.Fatal("configuration read model secret")
		return "", nil
	}}
	return ctx, c, p, deps, filepath.Join(dir, "mail.db")
}
func protectConfig(c *config.Config) {
	c.Processing = &config.Processing{Version: 1, AccountID: c.AccountID, Extra: map[string]json.RawMessage{"private_processing": json.RawMessage(`7`)}}
	c.Credential = &config.CredentialState{Version: 1, Generation: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Extra: map[string]json.RawMessage{"private_credential": json.RawMessage(`8`)}}
	c.Extra = map[string]json.RawMessage{"private_current": json.RawMessage(`{"opaque":9}`)}
}
func assertProtectedConfig(t *testing.T, c config.Config) {
	t.Helper()
	if c.Processing == nil || c.Processing.Models || c.Processing.ExternalStages || c.Processing.Extra["private_processing"] == nil {
		t.Fatal("routing erased explicit processing deny")
	}
	if c.Credential == nil || c.Credential.Generation != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || c.Credential.Extra["private_credential"] == nil || c.Extra["private_current"] == nil {
		t.Fatal("routing erased current credential/private fields")
	}
}

func TestGraphConfigWritersPreserveConcurrentOptOut(t *testing.T) {
	for _, writer := range []string{"sync", "bind-recovery", "bind-history", "reconcile-history"} {
		t.Run(writer, func(t *testing.T) {
			ctx, c, p, deps, path := graphConfigFixture(t)
			args := []string{"sync", "--folder", "inbox"}
			if writer == "bind-history" || writer == "reconcile-history" {
				legacy, e := store.Open(path)
				if e != nil {
					t.Fatal(e)
				}
				legacy.Close()
				prep, e := store.PrepareLegacyBinding(ctx, path)
				if e != nil {
					t.Fatal(e)
				}
				args = []string{"account", "bind", "--execute", "--confirm-history", prep.Digest()}
				if writer == "reconcile-history" {
					args = reconciliationConfigArgs(t, c.Account(), prep)
				}
			} else {
				db, e := store.OpenAccount(ctx, path, c.Account())
				if e != nil {
					t.Fatal(e)
				}
				db.Close()
				if writer == "bind-recovery" {
					args = []string{"account", "bind", "--execute"}
				}
			}
			// The real processing command supplies the deny; its injected writer adds
			// invented concurrent private fields, without altering its authorization.
			deps.SaveConfig = func(path string, fresh config.Config) error {
				if fresh.Processing != nil && !fresh.Processing.Models && !fresh.Processing.ExternalStages {
					protectConfig(&fresh)
				}
				return config.Save(path, fresh)
			}
			command := configurationCommand(deps, ctx, args...)
			deny := configurationCommand(deps, ctx, "config", "processing", "--account", c.AccountID, "--models=false", "--external-stages=false")
			done := make(chan error, 1)
			go func() { done <- command.Execute() }()
			select {
			case <-p.enter:
			case e := <-done:
				t.Fatalf("writer missed remote barrier: %v", e)
			case <-ctx.Done():
				t.Fatal("writer barrier timeout")
			}
			if e := deny.Execute(); e != nil {
				t.Fatal(e)
			}
			fresh, e := routing()
			if e != nil {
				t.Fatal(e)
			}
			assertProtectedConfig(t, fresh)
			p.unpause()
			if e := <-done; e != nil {
				t.Fatal(e)
			}
			fresh, e = routing()
			if e != nil {
				t.Fatal(e)
			}
			assertProtectedConfig(t, fresh)
			db, e := store.OpenAccount(ctx, path, c.Account())
			if e != nil {
				t.Fatal("actual database binding changed")
			}
			db.Close()
		})
	}
}

func TestGraphRoutingRefusesChangedSelection(t *testing.T) {
	for _, writer := range []string{"sync", "bind-recovery", "bind-history", "reconcile-history"} {
		for _, change := range []string{"registration", "account", "provider"} {
			t.Run(writer+"/"+change, func(t *testing.T) {
				ctx, c, p, deps, path := graphConfigFixture(t)
				args := []string{"sync", "--folder", "inbox"}
				var db *store.AccountStore
				var e error
				if writer == "bind-history" || writer == "reconcile-history" {
					legacy, err := store.Open(path)
					if err != nil {
						t.Fatal(err)
					}
					legacy.Close()
					prep, err := store.PrepareLegacyBinding(ctx, path)
					if err != nil {
						t.Fatal(err)
					}
					args = []string{"account", "bind", "--execute", "--confirm-history", prep.Digest()}
					if writer == "reconcile-history" {
						args = reconciliationConfigArgs(t, c.Account(), prep)
					}
				} else {
					db, e = store.OpenAccount(ctx, path, c.Account())
					if e != nil {
						t.Fatal(e)
					}
					db.Close()
					if writer == "bind-recovery" {
						args = []string{"account", "bind", "--execute"}
					}
				}
				cmd := configurationCommand(deps, ctx, args...)
				done := make(chan error, 1)
				go func() { done <- cmd.Execute() }()
				select {
				case <-p.enter:
				case <-ctx.Done():
					t.Fatal("barrier timeout")
				}
				current := c
				switch change {
				case "registration":
					current.ClientID = "33333333-3333-4333-8333-333333333333"
				case "account":
					current.AccountID = "different-account"
				case "provider":
					principal, _ := imapauth.NewPrincipal("yahoo", "mail.example.org", "owner@example.org")
					current = config.Config{Provider: "yahoo", AccountID: string(principal.Account().ID), IMAP: &config.IMAPConfig{IdentityVersion: 1, Endpoint: principal.Endpoint, Login: principal.Login}}
				}
				protectConfig(&current)
				if e = config.Save(config.DefaultPath(), current); e != nil {
					t.Fatal(e)
				}
				p.unpause()
				if e = <-done; e == nil {
					t.Fatal("stale routing accepted a changed selection")
				}
				after, e := routing()
				if e != nil {
					t.Fatal(e)
				}
				assertProtectedConfig(t, after)
				if after.Mode() != current.Mode() || after.AccountID != current.AccountID || after.ClientID != current.ClientID {
					t.Fatal("current selection overwritten")
				}
				db, e = store.OpenAccount(ctx, path, c.Account())
				if e != nil {
					t.Fatal("original database was rebound")
				}
				db.Close()
			})
		}
	}
}

func TestConfigInitRechecksExistenceInsideSession(t *testing.T) {
	for _, provider := range []string{"graph", "yahoo"} {
		for _, changed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/changed=%t", provider, changed), func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("HOME", dir)
				t.Setenv("MAILPIPE_CONFIG", filepath.Join(dir, "config.json"))
				t.Setenv("MAILPIPE_OUT_DIR", filepath.Join(dir, "out"))
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				sessions := &configTestSession{enter: make(chan struct{}), release: make(chan struct{})}
				var release sync.Once
				defer release.Do(func() { close(sessions.release) })
				deps := Deps{Credentials: func() (imapauth.Store, error) { return sessions, nil }}
				c := config.Config{ClientID: "11111111-1111-4111-8111-111111111111", TenantID: "22222222-2222-4222-8222-222222222222", AccountID: "invented-account"}
				args := []string{"config", "init", "--provider", "graph", "--client-id", c.ClientID, "--tenant-id", c.TenantID}
				if provider == "yahoo" {
					p, _ := imapauth.NewPrincipal("yahoo", "mail.example.org", "owner@example.org")
					c = config.Config{Provider: "yahoo", AccountID: string(p.Account().ID), IMAP: &config.IMAPConfig{IdentityVersion: 1, Endpoint: p.Endpoint, Login: p.Login}}
					args = []string{"config", "init", "--provider", "yahoo", "--endpoint", p.Endpoint, "--login", p.Login}
				}
				cmd := configurationCommand(deps, ctx, args...)
				done := make(chan error, 1)
				go func() { done <- cmd.Execute() }()
				select {
				case <-sessions.enter:
				case e := <-done:
					t.Fatalf("init bypassed configuration session: %v", e)
				case <-ctx.Done():
					t.Fatal("init session timeout")
				}
				if changed {
					if provider == "graph" {
						c.ClientID = "33333333-3333-4333-8333-333333333333"
					} else {
						p, _ := imapauth.NewPrincipal("yahoo", c.IMAP.Endpoint, "Different@example.org")
						c.AccountID = string(p.Account().ID)
						c.IMAP.Login = p.Login
					}
				}
				protectConfig(&c)
				if e := config.Save(config.DefaultPath(), c); e != nil {
					t.Fatal(e)
				}
				release.Do(func() { close(sessions.release) })
				if e := <-done; (e != nil) != changed {
					t.Fatalf("changed=%t, expected refusal=%t, got error=%v", changed, changed, e)
				}
				after, e := routing()
				if e != nil {
					t.Fatal(e)
				}
				assertProtectedConfig(t, after)
				if after.Account() != c.Account() || after.ClientID != c.ClientID {
					t.Fatal("init replaced the current principal")
				}
			})
		}
	}

}

func (p *configTestGraph) ReconcileOriginalReferences(ctx context.Context, refs []graphmail.LegacyReference) (graphmail.ReferenceEvidence, error) {
	// These fixtures have no live references. Keep the existing barrier at
	// post-binding routing resolution, where processing can legitimately reopen
	// the account, rather than inside exclusive historical verification.
	started := time.Now().UTC()
	if e := ctx.Err(); e != nil {
		return graphmail.ReferenceEvidence{}, e
	}
	if len(refs) != 0 {
		return graphmail.ReferenceEvidence{}, errors.New("configuration fixture requires an empty original inventory")
	}
	return graphmail.ReferenceEvidence{Account: p.account, Method: graphmail.ReferenceMethod, Coverage: graphmail.ReferenceCoverage, Started: started, Finished: time.Now().UTC(), Context: graphmail.ReferenceContext{RequestedReferences: 0, CompletedReferences: 0, InspectionBatches: 0, StateBytes: 0}, Outcomes: map[int64]graphmail.ReferenceOutcome{}}, nil
}
func reconciliationConfigArgs(t *testing.T, a mailbox.Account, p store.LegacyPreparation) []string {
	t.Helper()
	now := time.Now().UTC()
	if len(p.Live) != 0 || len(p.Removed) != 0 {
		t.Fatal("configuration fixture requires empty history")
	}
	r, e := store.ReviewLegacyReconciliation(a, p, store.LegacyReconciliation{Method: store.LegacyReferenceMethod, Coverage: store.LegacyReferenceCoverage, Started: now, Finished: now, ReferenceContext: store.LegacyReferenceContext{RequestedReferences: 0, CompletedReferences: 0, InspectionBatches: 0, StateBytes: 0}, ReferenceOutcomes: map[int64]store.LegacyReferenceOutcome{}, Verification: store.LegacyVerification{Account: a, Nonce: p.Challenge(), States: map[int64]mailbox.State{}, Folders: map[string]mailbox.CollectionRef{}}})
	if e != nil {
		t.Fatal(e)
	}
	if r.Version != 9 || r.Method != store.LegacyReferenceMethod || r.ReferenceContext == nil || *r.ReferenceContext != (store.LegacyReferenceContext{}) || len(r.Items) != 0 {
		t.Fatal("configuration fixture fabricated reference acquisition")
	}
	return []string{"account", "reconcile", "--execute", "--confirm-account", string(a.ID), "--confirm-history", p.Digest(), "--confirm-reconciliation", r.Digest}
}
