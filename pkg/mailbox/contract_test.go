package mailbox_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

const account mailbox.AccountID = "account-a"

func state() mailbox.State {
	return mailbox.State{
		Ref:         mailbox.MessageRef{AccountID: account, MessageID: "occurrence-a", Locator: "route-a"},
		Metadata:    mailbox.Metadata{InternetMessageID: "<message@example.net>", Sender: mailbox.Address{Address: "sender@example.org", Name: "Sender"}, Recipients: []mailbox.Address{{Address: "reader@example.com"}}, Received: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), Subject: "Invented message", Preview: "Complete content", HasAttachments: true},
		Memberships: []mailbox.Membership{{Collection: mailbox.CollectionRef{AccountID: account, CollectionID: "collection-a"}}, {Collection: mailbox.CollectionRef{AccountID: account, CollectionID: "collection-b"}}}, Revision: "revision-a",
	}
}
func clone(s mailbox.State) mailbox.State {
	s.Memberships = append([]mailbox.Membership(nil), s.Memberships...)
	return s
}
func request() mailbox.ReviewedAction {
	return mailbox.ReviewedAction{PlanID: 7, Local: mailbox.LocalRef{AccountID: account, N: 11}, Expected: state()}
}

// These fixtures implement domain contracts only; no transport or production adapter.
type reader struct {
	current     mailbox.State
	inspections int
	inspectErr  error
	syncPages   []mailbox.SyncPage
	syncErr     error
}

func newReader() *reader { return &reader{current: state()} }
func (r *reader) Account() mailbox.Account {
	return mailbox.Account{ID: account, ProviderID: "synthetic"}
}
func (r *reader) Collections(ctx context.Context) ([]mailbox.Collection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []mailbox.Collection{{Ref: r.current.Memberships[0].Collection, Name: "Collection"}}, nil
}
func (r *reader) Sync(ctx context.Context, q mailbox.SyncRequest) (mailbox.SyncPage, error) {
	if err := q.Validate(account); err != nil {
		return mailbox.SyncPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return mailbox.SyncPage{}, err
	}
	if len(r.syncPages) == 0 {
		if r.syncErr != nil {
			return mailbox.SyncPage{}, r.syncErr
		}
		return mailbox.SyncPage{}, errors.New("unexpected sync call")
	}
	p := r.syncPages[0]
	r.syncPages = r.syncPages[1:]
	return p, p.Validate(q.Collection)
}
func (r *reader) Inspect(ctx context.Context, ref mailbox.MessageRef) (mailbox.State, error) {
	r.inspections++
	if err := ref.Validate(account); err != nil {
		return mailbox.State{}, err
	}
	if err := ctx.Err(); err != nil {
		return mailbox.State{}, err
	}
	if r.inspectErr != nil {
		return mailbox.State{}, r.inspectErr
	}
	return clone(r.current), nil
}
func (r *reader) Metadata(ctx context.Context, ref mailbox.MessageRef) (mailbox.Metadata, error) {
	s, err := r.Inspect(ctx, ref)
	return s.Metadata, err
}
func (r *reader) Text(ctx context.Context, ref mailbox.MessageRef) (string, error) {
	if _, err := r.Inspect(ctx, ref); err != nil {
		return "", err
	}
	return "Invented body\n" + strings.Repeat("content\n", 10000) + "tail", nil
}
func (r *reader) WriteMIME(ctx context.Context, ref mailbox.MessageRef, w io.Writer) error {
	if _, err := r.Inspect(ctx, ref); err != nil {
		return err
	}
	source := "From: sender@example.org\r\nTo: reader@example.com\r\n\r\n" + strings.Repeat("invented content\r\n", 5000) + "tail"
	for len(source) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(len(source), 4096)
		written, err := io.WriteString(w, source[:n])
		if err != nil {
			return err
		}
		if written != n {
			return io.ErrShortWrite
		}
		source = source[n:]
	}
	return nil
}

type trashOnly struct {
	*reader
	trashCalls  int
	mutationErr error
	result      *mailbox.State
	received    mailbox.ReviewedAction
}

func (p *trashOnly) Trash(ctx context.Context, r mailbox.ReviewedAction) (mailbox.State, error) {
	p.trashCalls++
	p.received = r
	if err := ctx.Err(); err != nil {
		return mailbox.State{}, err
	}
	if p.mutationErr != nil {
		return mailbox.State{}, p.mutationErr
	}
	if err := mailbox.CheckPrecondition(account, r.Expected, p.current); err != nil {
		return mailbox.State{}, err
	}
	p.current.Memberships = []mailbox.Membership{{Collection: mailbox.CollectionRef{AccountID: account, CollectionID: "trash"}}}
	if p.result != nil {
		return *p.result, nil
	}
	return clone(p.current), nil
}

type archiveOnly struct {
	*reader
	archiveCalls int
}

func (p *archiveOnly) Archive(ctx context.Context, r mailbox.ReviewedAction) (mailbox.State, error) {
	if err := ctx.Err(); err != nil {
		return mailbox.State{}, err
	}
	if err := mailbox.CheckPrecondition(account, r.Expected, p.current); err != nil {
		return mailbox.State{}, err
	}
	p.archiveCalls++
	p.current.Memberships = nil
	return clone(p.current), nil
}

type capable struct {
	*trashOnly
	archiveCalls int
	rules        []mailbox.NativeRule
}

func (p *capable) Archive(ctx context.Context, r mailbox.ReviewedAction) (mailbox.State, error) {
	a := archiveOnly{reader: p.reader}
	s, err := a.Archive(ctx, r)
	p.archiveCalls += a.archiveCalls
	return s, err
}
func (p *capable) ListRules(ctx context.Context) ([]mailbox.NativeRule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]mailbox.NativeRule(nil), p.rules...), nil
}
func (p *capable) CreateRule(ctx context.Context, r mailbox.ReviewedRuleCreation) (mailbox.NativeRule, error) {
	if err := ctx.Err(); err != nil {
		return mailbox.NativeRule{}, err
	}
	if err := r.Validate(account); err != nil {
		return mailbox.NativeRule{}, err
	}
	rule := mailbox.NativeRule{Ref: mailbox.RuleRef{AccountID: account, ID: "rule-a"}, SenderAddresses: append([]string(nil), r.SenderAddresses...), Revision: "rule-version", Representable: true}
	p.rules = append(p.rules, rule)
	return rule, nil
}
func (p *capable) DeleteRule(ctx context.Context, r mailbox.ReviewedRuleDeletion) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.Validate(account); err != nil {
		return err
	}
	for i, rule := range p.rules {
		if reflect.DeepEqual(rule, r.Expected) {
			p.rules = append(p.rules[:i], p.rules[i+1:]...)
			return nil
		}
	}
	return &mailbox.StaleIdentityError{Operation: "delete rule", Reason: "rule changed"}
}

var _ mailbox.Provider = (*reader)(nil)
var _ mailbox.TrashCapability = (*trashOnly)(nil)
var _ mailbox.ArchiveCapability = (*archiveOnly)(nil)
var _ mailbox.NativeRuleCapability = (*capable)(nil)

func unsupported(t *testing.T, err error) {
	t.Helper()
	var typed *mailbox.UnsupportedError
	if !errors.Is(err, mailbox.ErrUnsupported) || !errors.As(err, &typed) || typed.Operation == "" || !strings.Contains(err.Error(), "supported") {
		t.Fatalf("want actionable typed unsupported, got %v", err)
	}
}
func stale(t *testing.T, err error) {
	t.Helper()
	var typed *mailbox.StaleIdentityError
	if !errors.Is(err, mailbox.ErrStaleIdentity) || !errors.As(err, &typed) {
		t.Fatalf("want typed stale identity, got %v", err)
	}
}
func mismatch(t *testing.T, err error) {
	t.Helper()
	var typed *mailbox.AccountMismatchError
	if !errors.Is(err, mailbox.ErrAccountMismatch) || !errors.As(err, &typed) {
		t.Fatalf("want typed account mismatch, got %v", err)
	}
}

func TestProviderContract(t *testing.T) {
	ctx := context.Background()
	t.Run("memberships are an unordered set", func(t *testing.T) {
		original := state()
		if err := original.Validate(account); err != nil {
			t.Fatal(err)
		}
		reordered := clone(original)
		reordered.Memberships[0], reordered.Memberships[1] = reordered.Memberships[1], reordered.Memberships[0]
		if err := mailbox.CheckPrecondition(account, original, reordered); err != nil {
			t.Fatal(err)
		}
		changed := clone(original)
		changed.Memberships = changed.Memberships[:1]
		stale(t, mailbox.CheckPrecondition(account, original, changed))
		changed = clone(original)
		changed.Memberships[1] = changed.Memberships[0]
		if changed.Validate(account) == nil {
			t.Fatal("duplicate membership accepted")
		}
		changed = clone(original)
		changed.Memberships = nil
		if err := changed.Validate(account); err != nil {
			t.Fatal(err)
		}
		local := mailbox.LocalRef{AccountID: account, N: 11}
		a := mailbox.Binding{Local: local, Remote: original.Ref}
		b := mailbox.Binding{Local: local, Remote: changed.Ref}
		if a != b {
			t.Fatal("membership change changed the local binding")
		}
		if err := a.Validate(account); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("duplicate occurrences and account bound numbers", func(t *testing.T) {
		a, b := state(), state()
		b.Ref.MessageID = "occurrence-b"
		b.Ref.Locator = "route-b"
		bindings := map[mailbox.MessageRef]mailbox.LocalRef{a.Ref: {AccountID: account, N: 11}, b.Ref: {AccountID: account, N: 12}}
		if len(bindings) != 2 || bindings[a.Ref] == bindings[b.Ref] {
			t.Fatal("duplicate RFC IDs merged occurrences")
		}
		stale(t, mailbox.CheckPrecondition(account, a, b))
		b.Ref.MessageID = "occurrence-new-epoch"
		b.Ref.Locator = a.Ref.Locator
		stale(t, mailbox.CheckPrecondition(account, a, b))
		other := mailbox.LocalRef{AccountID: "account-b", N: 11}
		if other == bindings[a.Ref] {
			t.Fatal("local numbers unbound")
		}
		mismatch(t, other.Validate(account))
		bad := mailbox.Binding{Local: bindings[a.Ref], Remote: mailbox.MessageRef{AccountID: "account-b", MessageID: a.Ref.MessageID, Locator: a.Ref.Locator}}
		mismatch(t, bad.Validate(account))
	})
	t.Run("account mismatch refuses before mutation", func(t *testing.T) {
		for _, field := range []string{"local", "expected", "membership", "current"} {
			t.Run(field, func(t *testing.T) {
				p := &trashOnly{reader: newReader()}
				r := request()
				switch field {
				case "local":
					r.Local.AccountID = "account-b"
				case "expected":
					r.Expected.Ref.AccountID = "account-b"
				case "membership":
					r.Expected.Memberships[0].Collection.AccountID = "account-b"
				case "current":
					p.current.Ref.AccountID = "account-b"
				}
				_, err := mailbox.ApplyReviewed(ctx, p, mailbox.Trash, r)
				mismatch(t, err)
				if p.trashCalls != 0 {
					t.Fatal("cross-account mutation")
				}
				if field != "current" && p.inspections != 0 {
					t.Fatal("cross-account inspection")
				}
			})
		}
	})
	t.Run("unsupported and capability truthfulness", func(t *testing.T) {
		ro := newReader()
		before := clone(ro.current)
		for _, action := range []mailbox.Action{mailbox.Trash, mailbox.Archive, "unknown"} {
			_, err := mailbox.ApplyReviewed(ctx, ro, action, request())
			unsupported(t, err)
		}
		rules, err := mailbox.NativeRulesFor(ro)
		unsupported(t, err)
		if rules != nil || ro.inspections != 0 || !reflect.DeepEqual(before, ro.current) {
			t.Fatal("unsupported call had effects")
		}
		cases := []struct {
			p    mailbox.Provider
			want mailbox.Capabilities
		}{
			{newReader(), mailbox.Capabilities{}},
			{&trashOnly{reader: newReader()}, mailbox.Capabilities{Trash: true}},
			{&archiveOnly{reader: newReader()}, mailbox.Capabilities{Archive: true}},
			{&capable{trashOnly: &trashOnly{reader: newReader()}}, mailbox.Capabilities{Trash: true, Archive: true, NativeRules: true}},
		}
		for _, c := range cases {
			if got := mailbox.CapabilitiesOf(c.p); got != c.want {
				t.Fatalf("%T: got %+v want %+v", c.p, got, c.want)
			}
		}
		p := &trashOnly{reader: newReader()}
		_, err = mailbox.ApplyReviewed(ctx, p, mailbox.Archive, request())
		unsupported(t, err)
		got, err := mailbox.ApplyReviewed(ctx, p, mailbox.Trash, request())
		if err != nil {
			t.Fatal(err)
		}
		if p.trashCalls != 1 || p.inspections != 1 || got.Memberships[0].Collection.CollectionID != "trash" || !reflect.DeepEqual(p.received, request()) {
			t.Fatal("trash dispatch lost reviewed context")
		}
		a := &archiveOnly{reader: newReader()}
		got, err = mailbox.ApplyReviewed(ctx, a, mailbox.Archive, request())
		if err != nil || a.archiveCalls != 1 || len(got.Memberships) != 0 {
			t.Fatalf("archive: %+v %v", got, err)
		}
		full := &capable{trashOnly: &trashOnly{reader: newReader()}}
		if _, err = mailbox.ApplyReviewed(ctx, full, mailbox.Archive, request()); err != nil || full.archiveCalls != 1 || full.trashCalls != 0 {
			t.Fatalf("capable archive: %v", err)
		}
	})
	t.Run("stale preconditions block mutation", func(t *testing.T) {
		changes := map[string]func(*mailbox.State){"id": func(s *mailbox.State) { s.Ref.MessageID = "other" }, "locator": func(s *mailbox.State) { s.Ref.Locator = "new-epoch" }, "sender": func(s *mailbox.State) { s.Metadata.Sender.Address = "other@example.org" }, "message-id": func(s *mailbox.State) { s.Metadata.InternetMessageID = "<other@example.net>" }, "membership": func(s *mailbox.State) { s.Memberships = s.Memberships[:1] }, "revision": func(s *mailbox.State) { s.Revision = "new" }}
		for name, change := range changes {
			t.Run(name, func(t *testing.T) {
				p := &trashOnly{reader: newReader()}
				change(&p.current)
				_, err := mailbox.ApplyReviewed(ctx, p, mailbox.Trash, request())
				stale(t, err)
				if p.trashCalls != 0 {
					t.Fatal("stale mutation")
				}
			})
		}
		missing := &trashOnly{reader: newReader()}
		missing.inspectErr = &mailbox.StaleIdentityError{Operation: "inspect", Reason: "missing"}
		_, err := mailbox.ApplyReviewed(ctx, missing, mailbox.Trash, request())
		stale(t, err)
		if missing.trashCalls != 0 {
			t.Fatal("missing target mutated")
		}
		a, b := state(), state()
		a.Metadata.InternetMessageID = ""
		b.Metadata.InternetMessageID = ""
		b.Ref.MessageID = "other"
		stale(t, mailbox.CheckPrecondition(account, a, b))
	})
	t.Run("sender spelling comparison", func(t *testing.T) {
		for _, c := range []struct {
			a, b string
			ok   bool
		}{{" Sender@Example.org ", "sender@example.org", true}, {"sënder@example.org", "SËNDER@example.org", true}, {"sender@bücher.example.org", "sender@xn--bcher-kva.example.org", false}, {"üser@example.org", "u\u0308ser@example.org", false}} {
			a, b := state(), state()
			a.Metadata.Sender.Address = c.a
			b.Metadata.Sender.Address = c.b
			b.Metadata.Sender.Name = "Invented Ñame"
			err := mailbox.CheckPrecondition(account, a, b)
			if c.ok && err != nil {
				t.Fatal(err)
			}
			if !c.ok {
				stale(t, err)
			}
		}
	})
	t.Run("invalid requests have no effects", func(t *testing.T) {
		for name, change := range map[string]func(*mailbox.ReviewedAction){"plan": func(r *mailbox.ReviewedAction) { r.PlanID = 0 }, "number": func(r *mailbox.ReviewedAction) { r.Local.N = 0 }, "account": func(r *mailbox.ReviewedAction) { r.Local.AccountID = "" }, "id": func(r *mailbox.ReviewedAction) { r.Expected.Ref.MessageID = "" }, "locator": func(r *mailbox.ReviewedAction) { r.Expected.Ref.Locator = "" }, "collection": func(r *mailbox.ReviewedAction) { r.Expected.Memberships[0].Collection.CollectionID = "" }} {
			t.Run(name, func(t *testing.T) {
				r := request()
				change(&r)
				p := &trashOnly{reader: newReader()}
				if _, err := mailbox.ApplyReviewed(ctx, p, mailbox.Trash, r); err == nil {
					t.Fatal("invalid request accepted")
				}
				if p.inspections+p.trashCalls != 0 {
					t.Fatal("invalid request reached provider")
				}
			})
		}
	})
	t.Run("errors do not retry or fall back", func(t *testing.T) {
		failure := errors.New("synthetic failure")
		for _, inspect := range []bool{true, false} {
			p := &trashOnly{reader: newReader()}
			if inspect {
				p.inspectErr = failure
			} else {
				p.mutationErr = failure
			}
			_, err := mailbox.ApplyReviewed(ctx, p, mailbox.Trash, request())
			if !errors.Is(err, failure) {
				t.Fatal(err)
			}
			want := 1
			if inspect {
				want = 0
			}
			if p.trashCalls != want {
				t.Fatal("retry or fallback")
			}
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		p := &trashOnly{reader: newReader()}
		_, err := mailbox.ApplyReviewed(canceled, p, mailbox.Trash, request())
		if !errors.Is(err, context.Canceled) || p.trashCalls != 0 {
			t.Fatal("cancellation ignored")
		}
		p = &trashOnly{reader: newReader()}
		bad := state()
		bad.Ref.AccountID = "account-b"
		p.result = &bad
		_, err = mailbox.ApplyReviewed(ctx, p, mailbox.Trash, request())
		mismatch(t, err)
		p = &trashOnly{reader: newReader()}
		bad = state()
		bad.Ref.Locator = ""
		p.result = &bad
		if _, err = mailbox.ApplyReviewed(ctx, p, mailbox.Trash, request()); err == nil {
			t.Fatal("malformed result accepted")
		}
	})
	t.Run("sync pages and interrupted checkpoints", func(t *testing.T) {
		scope := state().Memberships[0].Collection
		cursor := mailbox.Cursor{AccountID: account, CollectionID: scope.CollectionID, Token: "opaque-cursor"}
		first := mailbox.SyncPage{Mode: mailbox.Snapshot, Messages: []mailbox.Message{{Ref: state().Ref, Metadata: state().Metadata}}, Next: cursor}
		final := mailbox.SyncPage{Mode: mailbox.Snapshot, Complete: true, Checkpoint: cursor}
		for _, page := range []mailbox.SyncPage{first, final, {Mode: mailbox.Snapshot, Complete: true}, {Mode: mailbox.Delta, Removed: []mailbox.MessageRef{state().Ref}, Complete: true, Checkpoint: cursor}} {
			if err := page.Validate(scope); err != nil {
				t.Fatal(err)
			}
		}
		bad := cursor
		bad.AccountID = "account-b"
		mismatch(t, bad.Validate(scope))
		bad = cursor
		bad.CollectionID = "other"
		if bad.Validate(scope) == nil {
			t.Fatal("wrong collection cursor")
		}
		if (mailbox.Cursor{Token: "incomplete"}).Validate(scope) == nil {
			t.Fatal("unbound cursor")
		}
		invalid := []mailbox.SyncPage{{Mode: "unknown", Complete: true}, {Mode: mailbox.Snapshot}, {Mode: mailbox.Snapshot, Next: cursor, Checkpoint: cursor}, {Mode: mailbox.Snapshot, Complete: true, Next: cursor}, {Mode: mailbox.Snapshot, Complete: true, Removed: []mailbox.MessageRef{state().Ref}}}
		for i, p := range invalid {
			if p.Validate(scope) == nil {
				t.Fatalf("bad page %d accepted", i)
			}
		}
		p := newReader()
		p.syncPages = []mailbox.SyncPage{first}
		p.syncErr = errors.New("interrupted scan")
		got, err := p.Sync(ctx, mailbox.SyncRequest{Collection: scope})
		if err != nil || got.Complete || got.Checkpoint != (mailbox.Cursor{}) {
			t.Fatalf("intermediate checkpoint: %v", err)
		}
		got, err = p.Sync(ctx, mailbox.SyncRequest{Collection: scope, Cursor: got.Next})
		if err == nil || got.Complete || got.Checkpoint != (mailbox.Cursor{}) {
			t.Fatal("interruption committed checkpoint")
		}
		p.syncErr = &mailbox.StaleIdentityError{Operation: "sync", Reason: "expired cursor"}
		_, err = p.Sync(ctx, mailbox.SyncRequest{Collection: scope, Cursor: cursor})
		stale(t, err)
		if len(p.current.Memberships) != 2 {
			t.Fatal("interruption removed memberships")
		}
	})
	t.Run("native rule subset and ownership preconditions", func(t *testing.T) {
		p := &capable{trashOnly: &trashOnly{reader: newReader()}}
		rules, err := mailbox.NativeRulesFor(p)
		if err != nil {
			t.Fatal(err)
		}
		creation := mailbox.ReviewedRuleCreation{AccountID: account, ReviewID: 4, SenderAddresses: []string{"sender@example.org"}}
		rule, err := rules.CreateRule(ctx, creation)
		if err != nil {
			t.Fatal(err)
		}
		listed, err := rules.ListRules(ctx)
		if err != nil || len(listed) != 1 || !reflect.DeepEqual(listed[0], rule) {
			t.Fatal("native rule not created")
		}
		deletion := mailbox.ReviewedRuleDeletion{AccountID: account, ReviewID: 4, Expected: rule}
		deletion.Expected.Revision = "old"
		stale(t, rules.DeleteRule(ctx, deletion))
		if len(p.rules) != 1 {
			t.Fatal("stale deletion")
		}
		deletion.Expected = rule
		deletion.Expected.Representable = false
		unsupported(t, deletion.Validate(account))
		deletion.Expected = rule
		if err = rules.DeleteRule(ctx, deletion); err != nil || len(p.rules) != 0 {
			t.Fatalf("native delete %v", err)
		}
		creation.AccountID = "account-b"
		mismatch(t, creation.Validate(account))
		creation.AccountID = account
		creation.ReviewID = 0
		if creation.Validate(account) == nil {
			t.Fatal("unreviewed context accepted")
		}
	})
	t.Run("complete content and writer failures", func(t *testing.T) {
		p := newReader()
		metadata, err := p.Metadata(ctx, state().Ref)
		if err != nil || metadata.Subject != state().Metadata.Subject {
			t.Fatal(err)
		}
		text, err := p.Text(ctx, state().Ref)
		if err != nil || !strings.HasSuffix(text, "tail") || len(text) < 32768 {
			t.Fatal("text truncated")
		}
		var b bytes.Buffer
		if err = p.WriteMIME(ctx, state().Ref, &b); err != nil || !strings.HasSuffix(b.String(), "tail") || b.Len() < 32768 {
			t.Fatal("MIME truncated")
		}
		failure := errors.New("writer failed")
		w := &failingWriter{err: failure}
		if err = p.WriteMIME(ctx, state().Ref, w); !errors.Is(err, failure) {
			t.Fatal(err)
		}
		if err = p.WriteMIME(ctx, state().Ref, shortWriter{}); !errors.Is(err, io.ErrShortWrite) {
			t.Fatal(err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cw := &cancelWriter{cancel: cancel}
		if err = p.WriteMIME(canceled, state().Ref, cw); !errors.Is(err, context.Canceled) || cw.calls != 1 {
			t.Fatalf("stream cancellation %v calls %d", err, cw.calls)
		}
	})
	t.Run("safe actionable error strings", func(t *testing.T) {
		r := request()
		secret := "opaque-sensitive-locator"
		r.Expected.Ref.Locator = mailbox.Locator(secret)
		p := &trashOnly{reader: newReader()}
		_, err := mailbox.ApplyReviewed(ctx, p, mailbox.Trash, r)
		stale(t, err)
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), r.Expected.Metadata.Subject) {
			t.Fatal("error leaked remote data")
		}
		wrapped := fmt.Errorf("operation failed: %w", err)
		stale(t, wrapped)
	})
}

type failingWriter struct{ err error }

func (w *failingWriter) Write([]byte) (int, error) { return 0, w.err }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

type cancelWriter struct {
	cancel context.CancelFunc
	calls  int
}

func (w *cancelWriter) Write(p []byte) (int, error) { w.calls++; w.cancel(); return len(p), nil }

// Inspection may race with a server change. The adapter must still revalidate.
type changingInspector struct{ *trashOnly }

func (p *changingInspector) Inspect(ctx context.Context, ref mailbox.MessageRef) (mailbox.State, error) {
	s, err := p.reader.Inspect(ctx, ref)
	p.current.Revision = "changed-after-inspection"
	return s, err
}

type cancelInspector struct {
	*trashOnly
	cancel context.CancelFunc
}

func (p *cancelInspector) Inspect(ctx context.Context, ref mailbox.MessageRef) (mailbox.State, error) {
	s, err := p.reader.Inspect(ctx, ref)
	p.cancel()
	return s, err
}

func TestReviewedExecutionBoundary(t *testing.T) {
	ctx := context.Background()
	t.Run("adapter rechecks race", func(t *testing.T) {
		p := &changingInspector{trashOnly: &trashOnly{reader: newReader()}}
		_, err := mailbox.ApplyReviewed(ctx, p, mailbox.Trash, request())
		stale(t, err)
		if p.trashCalls != 1 || len(p.current.Memberships) != 2 {
			t.Fatal("raced target mutated")
		}
	})
	t.Run("cancel after inspection", func(t *testing.T) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		p := &cancelInspector{trashOnly: &trashOnly{reader: newReader()}, cancel: cancel}
		_, err := mailbox.ApplyReviewed(ctx, p, mailbox.Trash, request())
		if !errors.Is(err, context.Canceled) || p.trashCalls != 0 {
			t.Fatal("cancellation reached mutation")
		}
	})
	t.Run("adapter returns proven continuity", func(t *testing.T) {
		p := &trashOnly{reader: newReader()}
		changed := state()
		changed.Ref.MessageID = "proven-successor"
		changed.Ref.Locator = "successor-route"
		p.result = &changed
		result, err := mailbox.ApplyReviewed(ctx, p, mailbox.Trash, request())
		if err != nil || result.Ref != changed.Ref || p.received.Local != request().Local {
			t.Fatalf("continuity result lost: %v", err)
		}
	})
}

func TestNativeRuleValidation(t *testing.T) {
	for _, address := range []string{"", "@example.org", "sender@", "sender@@example.org", "sender@example.org, other@example.net", "Sender <sender@example.org>", "sender@example.org\r\nother@example.net"} {
		t.Run(address, func(t *testing.T) {
			creation := mailbox.ReviewedRuleCreation{AccountID: account, ReviewID: 4, SenderAddresses: []string{address}}
			if err := creation.Validate(account); err == nil {
				t.Fatal("malformed or non-address sender accepted")
			}
		})
	}
	good := mailbox.ReviewedRuleCreation{AccountID: account, ReviewID: 4, SenderAddresses: []string{"sender@example.org"}}
	if err := good.Validate(account); err != nil {
		t.Fatal(err)
	}
}
