// Package imapmail provides verified TLS, read-only IMAP discovery and bounded
// occurrence synchronization, using a required scoped metadata collaborator.
package imapmail

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/mailsync"
	"github.com/mrothroc/mailpipe/pkg/store"
)

// Profile selects protocol requirements, never an account inferred from a host.
type Profile string

const (
	Yahoo   Profile = "yahoo"
	Generic Profile = "generic"
)

type Role string

const (
	Inbox Role = "inbox"
	Sent  Role = "sent"
	Trash Role = "trash"
	Junk  Role = "junk"
)

// Config is offline technical configuration. Roots adds explicit trusted roots;
// certificate/hostname verification cannot be disabled. Zero limits use defaults.
type Config struct {
	Account                                         mailbox.Account
	Address, ServerName                             string
	Roots                                           *x509.CertPool
	Profile                                         Profile
	RoleMappings                                    map[Role]string
	UIDWindow, MaxMessages, MaxWindows, MaxAttempts int
	SourceBytes                                     int64
}
type Credentials struct {
	Account         mailbox.Account
	Login, Password string
}
type CredentialFunc func(context.Context) (Credentials, error)

// TemporaryCredentialError is an explicit trusted collaborator signal, not
// server prose. Permanent/unknown credential errors are never automatically retried.
type TemporaryCredentialError struct{ RetryAfter time.Duration }

func (*TemporaryCredentialError) Error() string { return "IMAP credentials temporarily unavailable" }

type Capabilities map[string]bool

func (c Capabilities) Has(name string) bool { return c[strings.ToUpper(name)] }

type Folder struct {
	Name       string
	Delimiter  string
	Role       Role
	Collection mailbox.Collection
	MailboxID  string
}
type Discovery struct {
	Folders      []Folder
	Capabilities Capabilities
}
type Occurrence struct {
	Ref              mailbox.MessageRef
	Collection       mailbox.CollectionRef
	MailboxID        string
	UIDValidity, UID uint32
	EpochGeneration  int64
}

// Envelope is copied protocol evidence, not complete parsed MIME metadata.
type Envelope struct {
	Subject, MessageID, InReplyTo string
	Date                          time.Time
	From, To, Cc, Bcc, ReplyTo    []mailbox.Address
}
type WireMetadata struct {
	Flags        []string
	InternalDate time.Time
	Size         uint32
	Envelope     Envelope
}
type MetadataLoader interface {
	Load(context.Context, Occurrence, WireMetadata, ReadAccess) (mailbox.Metadata, error)
}

// ReadAccess is valid only during its synchronous Load call. The loader must
// propagate cancellation/cleanup errors and close/remove any private spool.
// It must not retain access, spawn reads, or reenter public Client operations.
type ReadAccess interface {
	ReadSource(context.Context, io.Writer) error
}
type SyncResult struct {
	Collection                             mailbox.CollectionRef
	Added, Updated, Removed, Local, Remote int
}

// Error contains only fixed safe operation codes. No server text/credentials,
// raw codec/SQL error or caller writer/loader message is retained.
type Error struct {
	Code   string
	detail string
	cause  error
}

func (e *Error) Error() string {
	if e.detail != "" {
		return "IMAP: " + e.detail
	}
	return "IMAP: " + e.Code
}
func (e *Error) Unwrap() error  { return e.cause }
func failure(code string) error { return &Error{Code: code, detail: code} }
func safe(code string, e error) error {
	var cause error
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded, mailbox.ErrAccountMismatch, mailbox.ErrStaleIdentity, mailsync.ErrCleanup, io.ErrShortWrite} {
		if errors.Is(e, sentinel) {
			cause = errors.Join(cause, sentinel)
		}
	}
	detail := code
	var internal *Error
	var protocol *protocolFault
	var attempt *protocolAttemptFailure
	switch {
	case errors.As(e, &internal) && internal.detail != "":
		detail += ": " + internal.detail
	case errors.As(e, &protocol):
		detail += ": " + protocol.code
	case errors.As(e, &attempt):
		detail += ": " + attempt.reason.code
	}
	return &Error{Code: code, detail: detail, cause: cause}
}

type waitClock interface {
	Wait(context.Context, time.Duration) error
}
type realClock struct{}

func (realClock) Wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Client has no server mutation surface. Its operation lease is context-aware;
// Close cancels active transport without acquiring that lease.
type Client struct {
	runPass     func(context.Context, *store.AccountStore, mailsync.PassSpec, mailsync.Scan) error
	cfg         Config
	credentials CredentialFunc
	gate        chan struct{}
	mu          sync.Mutex
	closed      bool
	cancel      context.CancelFunc
	login       string
	clock       waitClock
	random      func() float64
}

func New(cfg Config, credentials CredentialFunc) (*Client, error) {
	if e := cfg.Account.Validate(); e != nil {
		return nil, safe("invalid account", e)
	}
	if cfg.Account.ProviderID == "graph" || credentials == nil {
		return nil, failure("IMAP account and credentials required")
	}
	if cfg.Address == "" {
		cfg.Address = "imap.mail.yahoo.com:993"
	}
	if cfg.Profile == "" {
		cfg.Profile = Yahoo
	}
	if cfg.Profile != Yahoo && cfg.Profile != Generic {
		return nil, failure("invalid service profile")
	}
	if cfg.UIDWindow == 0 {
		cfg.UIDWindow = 250
	}
	if cfg.MaxMessages == 0 {
		cfg.MaxMessages = 100000
	}
	if cfg.MaxWindows == 0 {
		cfg.MaxWindows = 100000
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 4
	}
	if cfg.SourceBytes == 0 {
		cfg.SourceBytes = 64 << 20
	}
	if cfg.UIDWindow < 1 || cfg.UIDWindow > 1000 || cfg.MaxMessages < 1 || cfg.MaxMessages > 100000 || cfg.MaxWindows < 1 || cfg.MaxWindows > 100000 || cfg.MaxAttempts < 1 || cfg.MaxAttempts > 16 || cfg.SourceBytes < 1 || cfg.SourceBytes > 64<<20 {
		return nil, failure("invalid finite resource limit")
	}
	mappings := make(map[Role]string, len(cfg.RoleMappings))
	for role, name := range cfg.RoleMappings {
		if role != Sent && role != Trash && role != Junk {
			return nil, failure("unknown role mapping")
		}
		if !validText(name, 8192) {
			return nil, failure("invalid role mapping")
		}
		mappings[role] = name
	}
	cfg.RoleMappings = mappings
	if cfg.Roots != nil {
		cfg.Roots = cfg.Roots.Clone()
	}
	return &Client{runPass: mailsync.RunPass, cfg: cfg, credentials: credentials, gate: make(chan struct{}, 1), clock: realClock{}, random: rand.Float64}, nil
}
func (c *Client) Account() mailbox.Account { return c.cfg.Account }
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}
func (c *Client) operation(ctx context.Context) (context.Context, func(), error) {
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.gate
		return nil, nil, failure("client closed")
	}
	root, cancel := context.WithTimeout(ctx, 10*time.Minute)
	c.cancel = cancel
	c.mu.Unlock()
	return root, func() { cancel(); c.mu.Lock(); c.cancel = nil; c.mu.Unlock(); <-c.gate }, nil
}
func validText(v string, max int) bool {
	return v != "" && len(v) <= max && !strings.ContainsAny(v, "\x00\r\n")
}
func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}
