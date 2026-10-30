package store

import (
	"context"
	"errors"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func fixtureCollection(id string) mailbox.Collection {
	return mailbox.Collection{Ref: mailbox.CollectionRef{AccountID: fixtureAccount.ID, CollectionID: mailbox.CollectionID(id)}, Name: id}
}
func fixtureRecord(id string) MessageRecord {
	return MessageRecord{Message: mailbox.Message{Ref: mailbox.MessageRef{AccountID: fixtureAccount.ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator("route-" + id)}, Metadata: mailbox.Metadata{InternetMessageID: "<duplicate@example.net>", Sender: mailbox.Address{Address: "sender@example.org"}}}, EmailID: "shared-correlation"}
}
func TestProviderSyncTransactions(t *testing.T) {
	ctx := context.Background()
	path := inventedPath(t)
	a, e := OpenAccount(ctx, path, fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	scope := fixtureCollection("inbox")
	pass, e := a.BeginSync(ctx, scope, mailbox.Cursor{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	r := fixtureRecord("first")
	next := mailbox.Cursor{AccountID: fixtureAccount.ID, CollectionID: scope.Ref.CollectionID, Token: "next"}
	page := mailbox.SyncPage{Mode: mailbox.Snapshot, Messages: []mailbox.Message{r.Message}, Next: next}
	if e = a.StagePage(ctx, pass, mailbox.Cursor{}, page, []MessageRecord{r}); e != nil {
		t.Fatal(e)
	}
	if n, e := a.Count(ctx, AccountFilter{}); e != nil || n != 0 {
		t.Fatalf("staging visible %d %v", n, e)
	}
	if e = a.CommitSync(ctx, pass); e == nil {
		t.Fatal("incomplete pass committed")
	}
	b, e := OpenAccount(ctx, path, fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	b.Close()
	terminal := mailbox.Cursor{AccountID: fixtureAccount.ID, CollectionID: scope.Ref.CollectionID, Token: "terminal"}
	if e = a.StagePage(ctx, pass, next, mailbox.SyncPage{Mode: mailbox.Snapshot, Complete: true, Checkpoint: terminal}, nil); e != nil {
		t.Fatal(e)
	}
	if e = a.CommitSync(ctx, pass); e != nil {
		t.Fatal(e)
	}
	if n, e := a.Count(ctx, AccountFilter{}); e != nil || n != 1 {
		t.Fatalf("commit %d %v", n, e)
	}
	if cursor, e := a.Checkpoint(ctx, scope.Ref); e != nil || cursor != terminal {
		t.Fatalf("checkpoint %+v %v", cursor, e)
	}
	p1, e := a.BeginSync(ctx, scope, terminal, nil)
	if e != nil {
		t.Fatal(e)
	}
	p2, e := a.BeginSync(ctx, scope, terminal, nil)
	if e != nil {
		t.Fatal(e)
	}
	for _, p := range []SyncPass{p1, p2} {
		if e = a.StagePage(ctx, p, terminal, mailbox.SyncPage{Mode: mailbox.Delta, Complete: true}, nil); e != nil {
			t.Fatal(e)
		}
	}
	if e = a.CommitSync(ctx, p1); e != nil {
		t.Fatal(e)
	}
	if e = a.CommitSync(ctx, p2); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("concurrent checkpoint CAS %v", e)
	}
	if cursor, e := a.Checkpoint(ctx, scope.Ref); e != nil || cursor != (mailbox.Cursor{}) {
		t.Fatalf("zero checkpoint not cleared %+v %v", cursor, e)
	}
}

func TestSyncRollbackAndScopeIsolation(t *testing.T) {
	ctx := context.Background()
	a, e := OpenAccount(ctx, inventedPath(t), fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	inbox := fixtureCollection("inbox")
	label := fixtureCollection("label")
	old := fixtureRecord("old")
	ref, e := a.UpsertMessage(ctx, inbox, old)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.AddMembership(ctx, ref, label); e != nil {
		t.Fatal(e)
	}
	pass, e := a.BeginSync(ctx, inbox, mailbox.Cursor{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	first := mailbox.Cursor{AccountID: fixtureAccount.ID, CollectionID: inbox.Ref.CollectionID, Token: "first"}
	if e = a.StagePage(ctx, pass, mailbox.Cursor{}, mailbox.SyncPage{Mode: mailbox.Snapshot, Messages: []mailbox.Message{old.Message}, Complete: true, Checkpoint: first}, []MessageRecord{old}); e != nil {
		t.Fatal(e)
	}
	if e = a.CommitSync(ctx, pass); e != nil {
		t.Fatal(e)
	}
	fresh := fixtureRecord("fresh")
	pass, e = a.BeginSync(ctx, inbox, mailbox.Cursor{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	second := first
	second.Token = "second"
	if e = a.StagePage(ctx, pass, mailbox.Cursor{}, mailbox.SyncPage{Mode: mailbox.Snapshot, Messages: []mailbox.Message{fresh.Message}, Complete: true, Checkpoint: second}, []MessageRecord{fresh}); e != nil {
		t.Fatal(e)
	}
	if _, e = a.store.db.Exec(`create trigger invented_removal_failure before delete on memberships when old.collection_id='inbox' begin select raise(abort,'invented failure'); end`); e != nil {
		t.Fatal(e)
	}
	if e = a.CommitSync(ctx, pass); e == nil {
		t.Fatal("failed absence removal committed")
	}
	if cursor, e := a.Checkpoint(ctx, inbox.Ref); e != nil || cursor != first {
		t.Fatalf("failed pass checkpoint %+v %v", cursor, e)
	}
	if n, e := a.Count(ctx, AccountFilter{}); e != nil || n != 1 {
		t.Fatalf("partial metadata published %d %v", n, e)
	}
	if _, e = a.store.db.Exec("drop trigger invented_removal_failure"); e != nil {
		t.Fatal(e)
	}
	if e = a.CommitSync(ctx, pass); e != nil {
		t.Fatal(e)
	}
	retained, e := a.GetMessage(ctx, ref)
	if e != nil || len(retained.State.Memberships) != 1 || retained.State.Memberships[0].Collection != label.Ref {
		t.Fatalf("snapshot removed unrelated label %+v %v", retained, e)
	}
	pass, e = a.BeginSync(ctx, inbox, second, nil)
	if e != nil {
		t.Fatal(e)
	}
	third := first
	third.Token = "third"
	old.Message.Metadata.Subject = "updated complete metadata"
	if e = a.StagePage(ctx, pass, second, mailbox.SyncPage{Mode: mailbox.Delta, Messages: []mailbox.Message{old.Message}, Removed: []mailbox.MessageRef{fresh.Message.Ref}, Complete: true, Checkpoint: third}, []MessageRecord{old}); e != nil {
		t.Fatal(e)
	}
	if _, e = a.store.db.Exec(`create trigger invented_checkpoint_failure before update on provider_sync_state begin select raise(abort,'invented failure'); end`); e != nil {
		t.Fatal(e)
	}
	if e = a.CommitSync(ctx, pass); e == nil {
		t.Fatal("failed checkpoint update committed")
	}
	if cursor, e := a.Checkpoint(ctx, inbox.Ref); e != nil || cursor != second {
		t.Fatalf("checkpoint falsely advanced %+v %v", cursor, e)
	}
	retained, e = a.GetMessage(ctx, ref)
	if e != nil || retained.State.Metadata.Subject != "" || len(retained.State.Memberships) != 1 {
		t.Fatalf("SQL failure published delta %+v %v", retained, e)
	}
	if _, e = a.store.db.Exec("drop trigger invented_checkpoint_failure"); e != nil {
		t.Fatal(e)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if e = a.CommitSync(canceled, pass); !errors.Is(e, context.Canceled) {
		t.Fatalf("commit cancellation %v", e)
	}
	if e = a.CommitSync(ctx, pass); e != nil {
		t.Fatal(e)
	}
	labelCursor, e := a.Checkpoint(ctx, label.Ref)
	if e != nil || labelCursor != (mailbox.Cursor{}) {
		t.Fatalf("collection checkpoint leaked %+v %v", labelCursor, e)
	}
}
func TestSyncValidationAndAbandonedCleanup(t *testing.T) {
	ctx := context.Background()
	path := inventedPath(t)
	a, e := OpenAccount(ctx, path, fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	scope := fixtureCollection("inbox")
	pass, e := a.BeginSync(ctx, scope, mailbox.Cursor{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	next := mailbox.Cursor{AccountID: fixtureAccount.ID, CollectionID: scope.Ref.CollectionID, Token: "next"}
	r := fixtureRecord("staged")
	if e = a.StagePage(ctx, pass, mailbox.Cursor{}, mailbox.SyncPage{Mode: mailbox.Snapshot, Messages: []mailbox.Message{r.Message}, Next: next}, nil); e != nil {
		t.Fatal(e)
	}
	b, e := OpenAccount(ctx, path, fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	if e = b.AbortSync(ctx, pass); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("other session aborted pass %v", e)
	}
	if _, e = CleanupSyncPasses(ctx, path, fixtureAccount); !errors.Is(e, ErrLifecycleBusy) {
		t.Fatalf("cleanup admitted active sessions %v", e)
	}
	if e = a.StagePage(ctx, pass, next, mailbox.SyncPage{Mode: mailbox.Delta, Complete: true}, nil); e == nil {
		t.Fatal("mode transition accepted")
	}
	wrong := next
	wrong.AccountID = "foreign"
	if e = a.StagePage(ctx, pass, wrong, mailbox.SyncPage{Mode: mailbox.Snapshot, Complete: true}, nil); !errors.Is(e, mailbox.ErrAccountMismatch) {
		t.Fatalf("foreign cursor %v", e)
	}
	wrong = next
	wrong.Token = "wrong"
	if e = a.StagePage(ctx, pass, wrong, mailbox.SyncPage{Mode: mailbox.Snapshot, Complete: true}, nil); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("continuation mismatch %v", e)
	}
	if e = a.StagePage(ctx, pass, next, mailbox.SyncPage{Mode: mailbox.Snapshot, Complete: true, Next: next}, nil); e == nil {
		t.Fatal("terminal continuation accepted")
	}
	if e = a.StagePage(ctx, pass, next, mailbox.SyncPage{Mode: mailbox.Snapshot, Complete: true}, nil); e != nil {
		t.Fatal(e)
	}
	if e = a.CommitSync(ctx, pass); e != nil {
		t.Fatalf("ordinary reopen erased staged pass %v", e)
	}
	abandoned, e := a.BeginSync(ctx, scope, mailbox.Cursor{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.StagePage(ctx, abandoned, mailbox.Cursor{}, mailbox.SyncPage{Mode: mailbox.Snapshot, Next: next}, nil); e != nil {
		t.Fatal(e)
	}
	b.Close()
	a.Close()
	count, e := CleanupSyncPasses(ctx, path, fixtureAccount)
	if e != nil || count != 1 {
		t.Fatalf("abandoned staging cleanup %d %v", count, e)
	}
	a, e = OpenAccount(ctx, path, fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	if n, e := a.Count(ctx, AccountFilter{}); e != nil || n != 1 {
		t.Fatalf("cleanup implied absence %d %v", n, e)
	}
}
