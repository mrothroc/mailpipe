package imapmail

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type testLoader struct {
	retained ReadAccess
	err      error
	ignored  bool
}

func (l *testLoader) Load(ctx context.Context, o Occurrence, w WireMetadata, a ReadAccess) (mailbox.Metadata, error) {
	l.retained = a
	var b bytes.Buffer
	e := a.ReadSource(ctx, &b)
	if e != nil && !l.ignored {
		return mailbox.Metadata{}, e
	}
	if l.err != nil {
		return mailbox.Metadata{}, l.err
	}
	return mailbox.Metadata{InternetMessageID: "<duplicate@example.org>", Sender: mailbox.Address{Address: "sender@example.org"}, Subject: "invented", IsRead: hasFlag(w.Flags, `\Seen`)}, nil
}
func openClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, e := New(cfg, inventedCredentials(cfg.Account))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func openMirror(t *testing.T, cfg Config) *store.AccountStore {
	t.Helper()
	s, e := store.OpenAccount(context.Background(), filepath.Join(t.TempDir(), "mail.db"), cfg.Account)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func inbox(t *testing.T, c *Client) Folder {
	t.Helper()
	d, e := c.Discover(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range d.Folders {
		if f.Name == "INBOX" {
			return f
		}
	}
	t.Fatal("missing inbox")
	return Folder{}
}
func TestIMAPCapabilities(t *testing.T) {
	for _, profile := range []Profile{Generic, Yahoo} {
		t.Run(string(profile), func(t *testing.T) {
			s, cfg := localScript(t)
			cfg.Profile = profile
			c := openClient(t, cfg)
			d, e := c.Discover(context.Background())
			if e != nil || !d.Capabilities.Has("MOVE") || len(d.Folders) != 4 {
				t.Fatalf("%+v %v", d, e)
			}
			if d.Capabilities.Has("X-INVENTED-PREAUTH") {
				t.Fatal("pre-auth capability survived authenticated discovery")
			}
			if d.Capabilities.Has("WRITABLE") {
				t.Fatal("invented capability")
			}
			s.audit(t)
		})
	}
	t.Run("verified TLS defaults reject private CA", func(t *testing.T) {
		_, cfg := localScript(t)
		cfg.Roots = nil
		c := openClient(t, cfg)
		if _, e := c.Discover(context.Background()); e == nil {
			t.Fatal("untrusted fixture CA accepted")
		}
	})
}
func TestIMAPMissingSpecialUseRequiresExplicitMapping(t *testing.T) {
	for _, scenario := range []string{"missing", "one missing", "valid", "unknown", "ambiguous"} {
		t.Run(scenario, func(t *testing.T) {
			s, cfg := localScript(t)
			s.mu.Lock()
			s.special = false
			s.caps = "IMAP4rev1"
			s.mu.Unlock()
			if scenario != "missing" {
				cfg.RoleMappings = map[Role]string{Sent: "Folder A", Trash: "Folder B", Junk: "Folder C"}
			}
			switch scenario {
			case "one missing":
				delete(cfg.RoleMappings, Junk)
			case "unknown":
				cfg.RoleMappings[Junk] = "Unknown"
			case "ambiguous":
				cfg.RoleMappings[Junk] = "Folder B"
			}
			c := openClient(t, cfg)
			_, e := c.Discover(context.Background())
			if (scenario == "valid") != (e == nil) {
				t.Fatalf("%s %v", scenario, e)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.selected) != 0 {
				t.Fatal("discovery guessed a localized role")
			}
		})
	}
}
func TestIMAPSync(t *testing.T) {
	ctx := context.Background()
	s, cfg := localScript(t)
	cfg.Profile = Yahoo
	c := openClient(t, cfg)
	f := inbox(t, c)
	db := openMirror(t, cfg)
	loader := &testLoader{}
	result, e := c.SyncCollection(ctx, db, f, loader, false)
	if e != nil || result.Local != 2 || result.Remote != 2 {
		t.Fatalf("%+v %v", result, e)
	}
	if e = loader.retained.ReadSource(ctx, io.Discard); e == nil {
		t.Fatal("retained access accepted")
	}
	rows, e := db.Messages(ctx, store.AccountFilter{})
	if e != nil || len(rows) != 2 || rows[0].Local == rows[1].Local {
		t.Fatal("duplicate IDs/content deduplicated", e)
	}
	old := rows[0].Local
	checkpoint, e := db.Checkpoint(ctx, f.Collection.Ref)
	if e != nil {
		t.Fatal(e)
	}
	s.mu.Lock()
	s.messages = s.messages[1:]
	s.mu.Unlock()
	result, e = c.SyncCollection(ctx, db, f, loader, false)
	if e != nil || result.Removed != 1 || result.Local != 1 {
		t.Fatalf("expunge %+v %v", result, e)
	}
	history, e := db.GetMessage(ctx, old)
	if e != nil || len(history.State.Memberships) != 0 {
		t.Fatal("expunge history lost", e)
	}
	s.mu.Lock()
	s.validity = 11
	s.mu.Unlock()
	before, e := db.Checkpoint(ctx, f.Collection.Ref)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.SyncCollection(ctx, db, f, loader, false); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("reset accepted %v", e)
	}
	after, _ := db.Checkpoint(ctx, f.Collection.Ref)
	if before != after || checkpoint.Token == "" {
		t.Fatal("reset advanced checkpoint")
	}
	// Explicit conditional reconciliation permits UID reuse only in a new local
	// epoch, including a remote UIDVALIDITY ABA, without reviving any old N.
	for _, validity := range []uint32{11, 10} {
		s.mu.Lock()
		s.validity = validity
		s.mu.Unlock()
		expected, e := db.ReadIMAPEpochState(ctx, f.Collection.Ref)
		if e != nil {
			t.Fatal(e)
		}
		current, e := c.ReconcileEpoch(ctx, db, f, expected)
		if e != nil || current.EpochGeneration != expected.EpochGeneration+1 {
			t.Fatalf("conditional reset %+v %v", current, e)
		}
		result, e = c.SyncCollection(ctx, db, f, loader, false)
		if e != nil || result.Local != 1 {
			t.Fatalf("reset scan %+v %v", result, e)
		}
		live, e := db.Messages(ctx, store.AccountFilter{})
		if e != nil || len(live) != 1 {
			t.Fatal(e)
		}
		for _, retired := range rows {
			if live[0].Local == retired.Local {
				t.Fatal("UID reset resurrected prior N")
			}
			old, e := db.GetMessage(ctx, retired.Local)
			if e != nil || old.Valid {
				t.Fatal("old epoch not retired", e)
			}
		}
	}
}
func TestIMAPTransientRetryBackoff(t *testing.T) {
	ctx := context.Background()
	s, cfg := localScript(t)
	c := openClient(t, cfg)
	f := inbox(t, c)
	db := openMirror(t, cfg)
	var waits []time.Duration
	c.clock = &fakeClock{waits: &waits}
	c.random = func() float64 { return 1 }
	s.mu.Lock()
	s.byeOnce = true
	s.mu.Unlock()
	result, e := c.SyncCollection(ctx, db, f, &testLoader{}, false)
	if e != nil || result.Local != 2 || len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("retry %+v %v %v", result, e, waits)
	}
	for k, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second} {
		if got := backoff(k, 0, 1); got != want {
			t.Fatalf("backoff %v %v", got, want)
		}
		if got := backoff(k, 0, 0); got != want/2 {
			t.Fatal("jitter bound", got)
		}
	}
	if backoff(0, 10*time.Second, 0) != 10*time.Second {
		t.Fatal("hint ignored")
	}
	cp, _ := db.Checkpoint(ctx, f.Collection.Ref)
	bad := &testLoader{err: io.EOF}
	waits = nil
	if _, e = c.SyncCollection(ctx, db, f, bad, false); e == nil || len(waits) != 0 {
		t.Fatal("opaque loader error retried", e)
	}
	after, _ := db.Checkpoint(ctx, f.Collection.Ref)
	if cp != after {
		t.Fatal("loader failed checkpoint changed")
	}
}

func TestIMAPInventoryResourceBounds(t *testing.T) {
	ctx := context.Background()
	_, cfg := localScript(t)
	c := openClient(t, cfg)
	f := inbox(t, c)
	db := openMirror(t, cfg)
	loader := &testLoader{}
	if _, e := c.SyncCollection(ctx, db, f, loader, false); e != nil {
		t.Fatal(e)
	}
	rows, e := db.Messages(ctx, store.AccountFilter{})
	if e != nil {
		t.Fatal(e)
	}
	for _, m := range rows {
		if e = db.SetBody(ctx, m.Local, strings.Repeat("invented-body", 1<<18)); e != nil {
			t.Fatal(e)
		}
	}
	cp, _ := db.Checkpoint(ctx, f.Collection.Ref)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = c.SyncCollection(canceled, db, f, loader, false); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	after, _ := db.Checkpoint(ctx, f.Collection.Ref)
	if cp != after {
		t.Fatal("inventory cancellation published")
	}
	if _, e = c.SyncCollection(ctx, db, f, loader, false); e != nil {
		t.Fatal(e)
	}
	cp, _ = db.Checkpoint(ctx, f.Collection.Ref)
	original := rows[0]
	bad := store.MessageRecord{Message: mailbox.Message{Ref: original.State.Ref, Metadata: original.State.Metadata}, IMAP: original.IMAP}
	bad.Message.Ref.Locator = mailbox.Locator(strings.Repeat("x", store.IMAPInventoryMaxFieldBytes+1))
	if _, e = db.UpsertMessage(ctx, f.Collection, bad); e != nil {
		t.Fatal(e)
	}
	var waits []time.Duration
	c.clock = &fakeClock{&waits}
	if _, e = c.SyncCollection(ctx, db, f, loader, false); e == nil || len(waits) != 0 {
		t.Fatal("inventory limit succeeded or retried", e)
	}
	after, _ = db.Checkpoint(ctx, f.Collection.Ref)
	if after != cp {
		t.Fatal("inventory limit advanced checkpoint")
	}
	preserved, e := db.GetMessage(ctx, original.Local)
	if e != nil || preserved.Body == nil || len(*preserved.Body) == 0 {
		t.Fatal("inventory failure lost cached history", e)
	}
}
