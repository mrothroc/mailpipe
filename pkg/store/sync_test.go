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

func TestSyncRejectsInterveningSuccessfulAction(t *testing.T) {
	for _, timing := range []string{"before stage", "after stage"} {
		for _, effect := range []string{"upsert", "absence", "removal"} {
			t.Run(timing+"/"+effect, func(t *testing.T) {
				ctx := context.Background()
				path := inventedPath(t)
				a, e := OpenAccount(ctx, path, fixtureAccount)
				if e != nil {
					t.Fatal(e)
				}
				defer a.Close()
				b, e := OpenAccount(ctx, path, fixtureAccount)
				if e != nil {
					t.Fatal(e)
				}
				defer b.Close()
				inbox, label, trash := fixtureCollection("inbox"), fixtureCollection("label"), fixtureCollection("trash")
				r := fixtureRecord("action-sync")
				r.Revision = "opaque-z"
				ref, e := a.UpsertMessage(ctx, inbox, r)
				if e != nil {
					t.Fatal(e)
				}
				if e = a.AddMembership(ctx, ref, label); e != nil {
					t.Fatal(e)
				}
				scope := inbox
				if effect == "absence" {
					scope = trash
				}
				if effect == "removal" {
					scope = label
				}
				before := mailbox.Cursor{AccountID: fixtureAccount.ID, CollectionID: scope.Ref.CollectionID, Token: "before-action"}
				seed, e := a.BeginSync(ctx, scope, mailbox.Cursor{}, nil)
				if e != nil {
					t.Fatal(e)
				}
				seedPage := mailbox.SyncPage{Mode: mailbox.Snapshot, Complete: true, Checkpoint: before}
				var seedRecords []MessageRecord
				if scope.Ref != trash.Ref {
					seedPage.Messages = []mailbox.Message{r.Message}
					seedRecords = []MessageRecord{r}
				}
				if e = a.StagePage(ctx, seed, mailbox.Cursor{}, seedPage, seedRecords); e != nil {
					t.Fatal(e)
				}
				if e = a.CommitSync(ctx, seed); e != nil {
					t.Fatal(e)
				}
				input := before
				if effect == "absence" {
					input = mailbox.Cursor{}
				}
				pass, e := a.BeginSync(ctx, scope, input, nil)
				if e != nil {
					t.Fatal(e)
				}
				terminal := before
				terminal.Token = "stale-terminal"
				page := mailbox.SyncPage{Mode: mailbox.Delta, Complete: true, Checkpoint: terminal}
				var supplements []MessageRecord
				if effect == "upsert" {
					fresh := fixtureRecord("only-staged")
					page.Messages = []mailbox.Message{fresh.Message, r.Message}
					supplements = []MessageRecord{fresh, r}
				}
				if effect == "absence" {
					page.Mode = mailbox.Snapshot
				}
				if effect == "removal" {
					page.Removed = []mailbox.MessageRef{r.Message.Ref}
				}
				id, e := b.CreateBoundPlan(ctx, "move", []PlanItem{{N: ref.N, Action: "delete"}})
				if e != nil {
					t.Fatal(e)
				}
				reviewed, e := b.LoadReviewedAction(ctx, id, ref, fixtureAccount)
				if e != nil {
					t.Fatal(e)
				}
				result := reviewed.Expected
				result.Revision = "opaque-a"
				result.Metadata.Subject = "returned after action"
				result.Metadata.IsRead = true
				result.Memberships = []mailbox.Membership{{Collection: trash.Ref}, {Collection: label.Ref}}
				act := func() {
					t.Helper()
					if e = b.RecordBoundActions(ctx, []BoundAction{{PlanID: id, Local: ref, Expected: reviewed.Expected, Result: &result, Action: "delete", Status: ActionMoved}}); e != nil {
						t.Fatal(e)
					}
				}
				if timing == "before stage" {
					act()
				}
				if e = a.StagePage(ctx, pass, input, page, supplements); e != nil {
					t.Fatal(e)
				}
				if timing == "after stage" {
					act()
				}
				if e = a.CommitSync(ctx, pass); !errors.Is(e, mailbox.ErrStaleIdentity) {
					t.Fatalf("stale %s pass overwrote action; want typed reconciliation error, got %v", effect, e)
				}
				assertState := func() {
					t.Helper()
					m, e := a.GetMessage(ctx, ref)
					want, _ := jsonText(canonicalState(result))
					got, _ := jsonText(canonicalState(m.State))
					if e != nil || got != want {
						t.Fatalf("action state changed: %+v %v", m, e)
					}
				}
				assertState()
				if cursor, e := a.Checkpoint(ctx, scope.Ref); e != nil || cursor != before {
					t.Fatalf("rejected pass published checkpoint: %+v %v", cursor, e)
				}
				var count int
				if e = a.store.db.QueryRowContext(ctx, "select count(*) from messages where provider_message_id='only-staged'").Scan(&count); e != nil || count != 0 {
					t.Fatalf("partial page publication: %d %v", count, e)
				}
				if audits, e := a.Actions(ctx, id); e != nil || len(audits) != 1 || audits[0].Result == nil {
					t.Fatalf("audit damaged: %+v %v", audits, e)
				}
				if e = a.AbortSync(ctx, pass); e != nil {
					t.Fatal(e)
				}
				current := mailbox.Message{Ref: result.Ref, Metadata: result.Metadata}
				recovery := mailbox.SyncPage{Mode: mailbox.Snapshot, Complete: true, Checkpoint: terminal}
				recovery.Checkpoint.Token = "fresh-terminal"
				var extra []MessageRecord
				if effect != "upsert" {
					recovery.Messages = []mailbox.Message{current}
					extra = []MessageRecord{{Message: current, Revision: result.Revision}}
				}
				fresh, e := a.BeginSync(ctx, scope, mailbox.Cursor{}, nil)
				if e != nil {
					t.Fatal(e)
				}
				if e = a.StagePage(ctx, fresh, mailbox.Cursor{}, recovery, extra); e != nil {
					t.Fatal(e)
				}
				if e = a.CommitSync(ctx, fresh); e != nil {
					t.Fatalf("fresh evidence cannot recover: %v", e)
				}
				assertState()
				if cursor, e := a.Checkpoint(ctx, scope.Ref); e != nil || cursor != recovery.Checkpoint {
					t.Fatalf("fresh checkpoint: %+v %v", cursor, e)
				}
			})
		}
	}
}

func TestSyncActionAuditControls(t *testing.T) {
	for _, outcome := range []string{ActionFailed, ActionSkipped, "rolled-back success"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			a, e := OpenAccount(ctx, inventedPath(t), fixtureAccount)
			if e != nil {
				t.Fatal(e)
			}
			defer a.Close()
			scope := fixtureCollection("inbox")
			r := fixtureRecord("audit-control")
			ref, e := a.UpsertMessage(ctx, scope, r)
			if e != nil {
				t.Fatal(e)
			}
			pass, e := a.BeginSync(ctx, scope, mailbox.Cursor{}, nil)
			if e != nil {
				t.Fatal(e)
			}
			terminal := mailbox.Cursor{AccountID: fixtureAccount.ID, CollectionID: scope.Ref.CollectionID, Token: "control-terminal"}
			if e = a.StagePage(ctx, pass, mailbox.Cursor{}, mailbox.SyncPage{Mode: mailbox.Snapshot, Messages: []mailbox.Message{r.Message}, Complete: true, Checkpoint: terminal}, nil); e != nil {
				t.Fatal(e)
			}
			id, e := a.CreateBoundPlan(ctx, "audit control", []PlanItem{{N: ref.N, Action: "delete"}})
			if e != nil {
				t.Fatal(e)
			}
			reviewed, e := a.LoadReviewedAction(ctx, id, ref, fixtureAccount)
			if e != nil {
				t.Fatal(e)
			}
			attempt := BoundAction{PlanID: id, Local: ref, Expected: reviewed.Expected, Action: "delete", Status: outcome}
			wantAudits := 1
			if outcome == "rolled-back success" {
				result := reviewed.Expected
				result.Memberships = nil
				attempt.Status = ActionMoved
				attempt.Result = &result
				invalid := attempt
				invalid.Status = "invalid"
				if e = a.RecordBoundActions(ctx, []BoundAction{attempt, invalid}); e == nil {
					t.Fatal("invalid batch succeeded")
				}
				wantAudits = 0
			} else if e = a.RecordBoundActions(ctx, []BoundAction{attempt}); e != nil {
				t.Fatal(e)
			}
			if audits, e := a.Actions(ctx, id); e != nil || len(audits) != wantAudits {
				t.Fatalf("audit transaction: %+v %v", audits, e)
			}
			if e = a.CommitSync(ctx, pass); e != nil {
				t.Fatalf("noncommitted/nonmutating action invalidated pass: %v", e)
			}
			if cursor, e := a.Checkpoint(ctx, scope.Ref); e != nil || cursor != terminal {
				t.Fatalf("control checkpoint: %+v %v", cursor, e)
			}
			m, e := a.GetMessage(ctx, ref)
			if e != nil || len(m.State.Memberships) != 1 || m.State.Memberships[0].Collection != scope.Ref {
				t.Fatalf("action control changed mirror: %+v %v", m, e)
			}
		})
	}
}
