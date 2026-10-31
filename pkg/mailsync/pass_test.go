package mailsync_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/mailsync"
	"github.com/mrothroc/mailpipe/pkg/store"
)

func passFixture(t *testing.T) (*store.AccountStore, mailsync.PassSpec, mailbox.SyncPage) {
	t.Helper()
	a := mailbox.Account{ID: "invented-account", ProviderID: "fixture"}
	s, err := store.OpenAccount(context.Background(), filepath.Join(t.TempDir(), "mail.db"), a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := mailbox.Collection{Ref: mailbox.CollectionRef{AccountID: a.ID, CollectionID: "inbox"}}
	p := mailbox.SyncPage{Mode: mailbox.Snapshot, Complete: true, Messages: []mailbox.Message{{Ref: mailbox.MessageRef{AccountID: a.ID, MessageID: "one", Locator: "one"}, Metadata: mailbox.Metadata{Sender: mailbox.Address{Address: "sender@example.org"}}}}, Checkpoint: mailbox.Cursor{AccountID: a.ID, CollectionID: c.Ref.CollectionID, Token: "terminal"}}
	return s, mailsync.PassSpec{Account: a, Collection: c}, p
}

func TestRunPass(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range []string{"success", "nil", "incomplete", "late failure", "ignored stage error", "cancel", "cleanup failure", "account"} {
		t.Run(scenario, func(t *testing.T) {
			s, spec, page := passFixture(t)
			failure := errors.New("invented scanner failure")
			callctx, cancel := context.WithCancel(ctx)
			defer cancel()
			scan := mailsync.Scan(func(ctx context.Context, emit mailsync.EmitPage) error {
				switch scenario {
				case "incomplete":
					return nil
				case "ignored stage error":
					bad := page
					bad.Mode = "bad"
					_ = emit(spec.Input, bad, nil)
					_ = emit(spec.Input, page, nil)
					return nil
				case "cleanup failure":
					_ = s.Close()
					return failure
				}
				if err := emit(spec.Input, page, nil); err != nil {
					return err
				}
				if scenario == "late failure" {
					return failure
				}
				if scenario == "cancel" {
					cancel()
				}
				return nil
			})
			if scenario == "nil" {
				scan = nil
			}
			if scenario == "account" {
				spec.Account.ID = "other"
				scan = func(context.Context, mailsync.EmitPage) error {
					t.Fatal("scanner called for mismatched account")
					return nil
				}
			}
			err := mailsync.RunPass(callctx, s, spec, scan)
			if scenario == "success" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unsafe pass accepted")
			}
			if scenario == "late failure" && !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if scenario == "cancel" && (!errors.Is(err, context.Canceled) || errors.Is(err, mailsync.ErrCleanup)) {
				t.Fatalf("abort used canceled parent: %v", err)
			}
			if scenario == "cleanup failure" {
				if !errors.Is(err, failure) || !errors.Is(err, mailsync.ErrCleanup) {
					t.Fatal(err)
				}
				return
			}
			n, countErr := s.Count(ctx, store.AccountFilter{})
			if countErr != nil {
				t.Fatal(countErr)
			}
			if scenario == "success" && n != 1 || scenario != "success" && n != 0 {
				t.Fatalf("published %d rows after %s: %v", n, scenario, err)
			}
		})
	}
}

func TestRunPassConcurrentPublicationAndOwnership(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mail.db")
	account := mailbox.Account{ID: "invented-account", ProviderID: "fixture"}
	a, err := store.OpenAccount(ctx, path, account)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := store.OpenAccount(ctx, path, account)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	c := mailbox.Collection{Ref: mailbox.CollectionRef{AccountID: account.ID, CollectionID: "inbox"}}
	if err = a.PutCollection(ctx, c); err != nil {
		t.Fatal(err)
	}
	token := func(value string) mailbox.Cursor {
		return mailbox.Cursor{AccountID: account.ID, CollectionID: c.Ref.CollectionID, Token: value}
	}
	initial := mailbox.SyncPage{Mode: mailbox.Snapshot, Complete: true, Checkpoint: token("initial")}
	if err = mailsync.RunPass(ctx, a, mailsync.PassSpec{Account: account, Collection: c}, func(ctx context.Context, emit mailsync.EmitPage) error { return emit(mailbox.Cursor{}, initial, nil) }); err != nil {
		t.Fatal(err)
	}
	spec := mailsync.PassSpec{Account: account, Collection: c, Input: token("initial")}
	makePage := func(id, checkpoint string) mailbox.SyncPage {
		return mailbox.SyncPage{Mode: mailbox.Delta, Complete: true, Checkpoint: token(checkpoint), Messages: []mailbox.Message{{Ref: mailbox.MessageRef{AccountID: account.ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator(id)}, Metadata: mailbox.Metadata{Sender: mailbox.Address{Address: "sender@example.org"}}}}}
	}
	calls := 0
	err = mailsync.RunPass(ctx, a, spec, func(ctx context.Context, emit mailsync.EmitPage) error {
		calls++
		if e := emit(spec.Input, makePage("loser", "loser"), nil); e != nil {
			return e
		}
		return mailsync.RunPass(ctx, b, spec, func(ctx context.Context, peer mailsync.EmitPage) error {
			return peer(spec.Input, makePage("winner", "winner"), nil)
		})
	})
	if !errors.Is(err, mailbox.ErrStaleIdentity) || errors.Is(err, mailsync.ErrCleanup) || calls != 1 {
		t.Fatalf("uncertain publication retried/hidden: %v calls %d", err, calls)
	}
	checkpoint, e := a.Checkpoint(ctx, c.Ref)
	if e != nil || checkpoint != token("winner") {
		t.Fatalf("checkpoint %+v %v", checkpoint, e)
	}
	messages, e := a.Messages(ctx, store.AccountFilter{})
	if e != nil || len(messages) != 1 || messages[0].State.Ref.MessageID != "winner" {
		t.Fatalf("loser published %+v %v", messages, e)
	}
	// A scanner failure may abort only its own capability, not this peer staging.
	spec.Input = checkpoint
	peerPass, e := b.BeginSync(ctx, c, checkpoint, nil)
	if e != nil {
		t.Fatal(e)
	}
	failure := errors.New("invented late scanner failure")
	err = mailsync.RunPass(ctx, a, spec, func(ctx context.Context, emit mailsync.EmitPage) error {
		if e := emit(spec.Input, makePage("discarded", "discarded"), nil); e != nil {
			return e
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if e = b.StagePage(ctx, peerPass, checkpoint, makePage("surviving-peer", "peer"), nil); e != nil {
		t.Fatal("other capability aborted", e)
	}
	if e = b.CommitSync(ctx, peerPass); e != nil {
		t.Fatal(e)
	}
}

func TestRunPassSessionChildDoesNotCancelPublication(t *testing.T) {
	s, spec, page := passFixture(t)
	parent := context.Background()
	var child context.Context
	err := mailsync.RunPass(parent, s, spec, func(ctx context.Context, emit mailsync.EmitPage) error {
		var closeSession context.CancelFunc
		child, closeSession = context.WithCancel(ctx)
		defer closeSession()
		return emit(spec.Input, page, nil)
	})
	if err != nil || child.Err() != context.Canceled || parent.Err() != nil {
		t.Fatalf("session/context ownership: %v %v %v", err, child.Err(), parent.Err())
	}
	checkpoint, err := s.Checkpoint(parent, spec.Collection.Ref)
	if err != nil || checkpoint != page.Checkpoint {
		t.Fatalf("publication lost %+v %v", checkpoint, err)
	}
}

func TestRunPassActionWatermarkRefusesStaleAbsence(t *testing.T) {
	s, spec, _ := passFixture(t)
	ctx := context.Background()
	message := mailbox.Message{Ref: mailbox.MessageRef{AccountID: spec.Account.ID, MessageID: "one", Locator: "one"}, Metadata: mailbox.Metadata{InternetMessageID: "<one@example.org>", Sender: mailbox.Address{Address: "sender@example.org"}}}
	local, err := s.UpsertMessage(ctx, spec.Collection, store.MessageRecord{Message: message})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateBoundPlan(ctx, "fixture", []store.PlanItem{{N: local.N, Action: "delete"}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := s.LoadReviewedAction(ctx, id, local, spec.Account)
	if err != nil {
		t.Fatal(err)
	}
	err = mailsync.RunPass(ctx, s, spec, func(ctx context.Context, emit mailsync.EmitPage) error {
		if e := emit(spec.Input, mailbox.SyncPage{Mode: mailbox.Snapshot, Complete: true}, nil); e != nil {
			return e
		}
		result := request.Expected
		result.Memberships = []mailbox.Membership{{Collection: mailbox.CollectionRef{AccountID: spec.Account.ID, CollectionID: "trash"}}}
		return s.RecordBoundActions(ctx, []store.BoundAction{{PlanID: id, Local: local, Expected: request.Expected, Result: &result, Action: "delete", Destination: "trash", Status: store.ActionMoved}})
	})
	if !errors.Is(err, mailbox.ErrStaleIdentity) {
		t.Fatal("stale absence accepted", err)
	}
	current, err := s.GetMessage(ctx, local)
	if err != nil || len(current.State.Memberships) != 1 || current.State.Memberships[0].Collection.CollectionID != "trash" {
		t.Fatalf("action erased %+v %v", current, err)
	}
}

func TestRunPassCleanupFailurePreservesDriverAndContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "mail.db")
	account := mailbox.Account{ID: "invented-account", ProviderID: "fixture"}
	s, err := store.OpenAccount(ctx, path, account)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Test-only independent SQLite writer causes real abort contention. No raw
	// descriptor or transaction access is exposed by production code.
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	conn, err := writer.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	spec := mailsync.PassSpec{Account: account, Collection: mailbox.Collection{Ref: mailbox.CollectionRef{AccountID: account.ID, CollectionID: "inbox"}}}
	failure := errors.New("invented protocol failure")
	calls := 0
	started := time.Now()
	err = mailsync.RunPass(ctx, s, spec, func(ctx context.Context, emit mailsync.EmitPage) error {
		calls++
		if e := emit(spec.Input, mailbox.SyncPage{Mode: mailbox.Snapshot, Complete: true}, nil); e != nil {
			return e
		}
		if _, e := conn.ExecContext(context.Background(), "begin immediate"); e != nil {
			return e
		}
		cancel()
		return failure
	})
	if _, e := conn.ExecContext(context.Background(), "rollback"); e != nil {
		t.Fatal(e)
	}
	var driverErr interface {
		error
		Code() int
	}
	if !errors.Is(err, failure) || !errors.Is(err, context.Canceled) || !errors.Is(err, mailsync.ErrCleanup) || !errors.As(err, &driverErr) {
		t.Fatalf("lost cleanup causes: %v", err)
	}
	if time.Since(started) > 6*time.Second || calls != 1 {
		t.Fatalf("unbounded cleanup/retry %s calls %d", time.Since(started), calls)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := store.CleanupSyncPasses(context.Background(), path, account); err != nil || n != 1 {
		t.Fatalf("owned abandoned staging %d %v", n, err)
	}
}
