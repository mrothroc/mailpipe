package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"modernc.org/sqlite"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Frozen historical DDL, independent of the provider-aware opener.
const historicalSchema = `
create table if not exists messages (
    n integer primary key,
    graph_id text unique not null,
    internet_message_id text,
    folder text not null,
    received text,
    sender text,
    sender_name text,
    recipients text,
    subject text,
    preview text,
    has_attachments integer not null default 0,
    is_read integer not null default 0,
    removed integer not null default 0,
    body text
);
create index if not exists messages_imid on messages(internet_message_id);
create index if not exists messages_sender on messages(sender);
create index if not exists messages_folder on messages(folder, removed, received);
create table if not exists sync_state (folder text primary key, delta_link text, updated text);
create table if not exists verdicts (
    id integer primary key,
    n integer not null references messages(n),
    stage text not null,
    labels text not null default '[]',
    scores text not null default '{}',
    action text not null default '',
    reason text not null default '',
    cost_usd real not null default 0,
    final integer not null default 0,
    provenance text not null default '',
    error text not null default '',
    at text not null
);
create index if not exists verdicts_stage on verdicts(stage, n);
create index if not exists verdicts_n on verdicts(n, id);
create table if not exists plans (
    id integer primary key,
    scope text not null,
    created_at text not null
);
create table if not exists plan_items (
    plan_id integer not null references plans(id),
    n integer not null references messages(n),
    graph_id text not null,
    internet_message_id text not null,
    sender text not null,
    action text not null,
    reasons text not null default '[]',
    planned_folder text not null default '',
    no_block integer not null default 0,
    primary key (plan_id, n)
);
create index if not exists messages_live on messages(removed, sender);
create table if not exists actions (
    id integer primary key,
    plan_id integer not null references plans(id),
    n integer not null references messages(n),
    graph_id text not null,
    action text not null,
    destination text not null,
    status text not null check (status in ('moved', 'skipped', 'failed')),
    detail text not null default '',
    at text not null
);
create index if not exists actions_plan on actions(plan_id, status, n);
`

func frozenLegacy(t *testing.T) string {
	t.Helper()
	path := inventedPath(t)
	db, e := sql.Open("sqlite", "file:"+path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(historicalSchema); e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec(`insert into messages(n,graph_id,internet_message_id,folder,sender,recipients,body) values(7,'opaque-old','<copy@example.org>','inbox','sender@example.org','[]','invented body'),(77,'opaque-removed','','inbox','sender@example.net','[]',NULL);
 update messages set removed=1 where n=77;
 insert into verdicts(id,n,stage,labels,scores,action,reason,cost_usd,final,provenance,error,at) values(23,7,'category','["record"]','{}','keep','invented',0.01,1,'fixture','','2026-01-01T00:00:00Z');
 insert into plans(id,scope,created_at) values(11,'inbox','2026-01-01T00:00:00Z'),(12,'empty','2026-01-01T00:00:00Z');
 insert into plan_items(plan_id,n,graph_id,internet_message_id,sender,action,reasons,planned_folder,no_block) values(11,7,'opaque-old','<copy@example.org>','sender@example.org','delete','["invented"]','inbox',1);
 insert into actions(id,plan_id,n,graph_id,action,destination,status,detail,at) values(19,11,7,'opaque-old','delete','deleteditems','failed','invented failure','2026-01-01T00:00:00Z');
 insert into sync_state values('inbox','opaque-checkpoint','2026-01-01T00:00:00Z');`)
	if e != nil {
		t.Fatal(e)
	}
	if e = db.Close(); e != nil {
		t.Fatal(e)
	}
	return path
}

var fixtureAccount = mailbox.Account{ID: "account-a", ProviderID: "imap"}

func TestLegacyProviderMigration(t *testing.T) {
	t.Run("testMigrationRollback", testMigrationRollback)
	t.Run("testMigrationRefusesUnknownSchema", testMigrationRefusesUnknownSchema)
	t.Run("testMigrationPreservesAllHistoricalColumns", testMigrationPreservesAllHistoricalColumns)

	path := frozenLegacy(t)
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	m, ok, e := s.Get(context.Background(), 7)
	if e != nil || !ok || m.GraphID != "opaque-old" || m.Body == nil || *m.Body != "invented body" {
		t.Fatalf("preservation: %+v %v", m, e)
	}
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	if _, e = OpenAccount(context.Background(), path, fixtureAccount); e == nil {
		t.Fatal("implicitly bound historical data")
	}
	backups, e := filepath.Glob(path + ".backup-*")
	if e != nil || len(backups) != 1 {
		t.Fatalf("backup/reopen: %v %v", backups, e)
	}
}
func TestProviderBinding(t *testing.T) {
	t.Run("testVerifiedLegacyBinding", testVerifiedLegacyBinding)
	t.Run("testBoundActionAtomicity", testBoundActionAtomicity)
	t.Run("attribution and empty-live history", testBindingAttributionAndEmptyHistory)

	ctx := context.Background()
	path := inventedPath(t)
	a, e := OpenAccount(ctx, path, fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	if _, e = Open(path); e == nil {
		t.Fatal("legacy opener accepted bound DB")
	}
	b := fixtureAccount
	b.ID = "account-b"
	if _, e = OpenAccount(ctx, path, b); !errors.Is(e, mailbox.ErrAccountMismatch) {
		t.Fatalf("account switch: %v", e)
	}
	b = fixtureAccount
	b.ProviderID = "other"
	if _, e = OpenAccount(ctx, path, b); !errors.Is(e, mailbox.ErrAccountMismatch) {
		t.Fatalf("provider switch: %v", e)
	}
	id, e := a.CreateBoundPlan(ctx, "empty", nil)
	if e != nil || id < 1 {
		t.Fatalf("bound empty plan: %d %v", id, e)
	}
}
func TestMessageMembership(t *testing.T) {
	t.Run("testIMAPIdentitiesAndStalePlans", testIMAPIdentitiesAndStalePlans)
	t.Run("IMAP scope type safety", testIMAPScopeTypeSafety)
	t.Run("testAccountVerdictQueries", testAccountVerdictQueries)

	ctx := context.Background()
	a, e := OpenAccount(ctx, inventedPath(t), fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	c := mailbox.Collection{Ref: mailbox.CollectionRef{AccountID: fixtureAccount.ID, CollectionID: "inbox"}, Name: "Inbox"}
	d := c
	d.Ref.CollectionID = "label-a"
	r := MessageRecord{Message: mailbox.Message{Ref: mailbox.MessageRef{AccountID: fixtureAccount.ID, MessageID: "occurrence-a", Locator: "locator-a"}, Metadata: mailbox.Metadata{InternetMessageID: "<copy@example.org>", Sender: mailbox.Address{Address: "sender@example.org"}}}, EmailID: "correlation"}
	ref, e := a.UpsertMessage(ctx, c, r)
	if e != nil {
		t.Fatal(e)
	}
	same, e := a.UpsertMessage(ctx, d, r)
	if e != nil || same != ref {
		t.Fatalf("label duplicated identity %v %v", same, e)
	}
	r.Message.Ref.MessageID = "occurrence-b"
	r.Message.Ref.Locator = "locator-b"
	copyRef, e := a.UpsertMessage(ctx, c, r)
	if e != nil || copyRef == ref {
		t.Fatalf("copy merged %v %v", copyRef, e)
	}
	if e = a.RemoveMembership(ctx, ref, c.Ref); e != nil {
		t.Fatal(e)
	}
	m, e := a.GetMessage(ctx, ref)
	if e != nil || len(m.State.Memberships) != 1 || m.State.Memberships[0].Collection != d.Ref {
		t.Fatalf("unrelated membership erased: %+v %v", m, e)
	}
}

type verifyLegacyFunc func(context.Context, mailbox.Account, LegacyPreparation) (LegacyVerification, error)

func (f verifyLegacyFunc) VerifyLegacy(ctx context.Context, a mailbox.Account, p LegacyPreparation) (LegacyVerification, error) {
	return f(ctx, a, p)
}
func goodLegacyProof(a mailbox.Account, p LegacyPreparation) LegacyVerification {
	proof := LegacyVerification{Account: a, Nonce: p.Challenge(), States: map[int64]mailbox.State{}, Folders: map[string]mailbox.CollectionRef{}, HistoricalAttributionConfirmed: true}
	for _, m := range p.Live {
		scope := mailbox.CollectionRef{AccountID: a.ID, CollectionID: mailbox.CollectionID("collection-" + m.Folder)}
		proof.Folders[m.Folder] = scope
		proof.States[m.N] = mailbox.State{Ref: mailbox.MessageRef{AccountID: a.ID, MessageID: mailbox.MessageID(m.GraphID), Locator: mailbox.Locator(m.GraphID)}, Metadata: legacyMetadata(m), Memberships: []mailbox.Membership{{Collection: scope}}}
	}
	return proof
}
func testVerifiedLegacyBinding(t *testing.T) {
	ctx := context.Background()
	path := frozenLegacy(t)
	db := rawFixtureDB(t, path)
	specs, e := tableSpecs(ctx, db)
	if e != nil {
		t.Fatal(e)
	}
	original, e := dataDigest(ctx, db, specs)
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = PrepareLegacyBinding(ctx, path); !errors.Is(e, ErrLifecycleBusy) {
		t.Fatalf("maintenance admitted active legacy session: %v", e)
	}
	s.Close()
	p, e := PrepareLegacyBinding(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	account := mailbox.Account{ID: "graph-account", ProviderID: GraphProvider}
	for _, kind := range []string{"account", "nonce", "missing", "sender", "locator", "folder", "attribution"} {
		t.Run(kind, func(t *testing.T) {
			verifier := verifyLegacyFunc(func(_ context.Context, a mailbox.Account, p LegacyPreparation) (LegacyVerification, error) {
				proof := goodLegacyProof(a, p)
				switch kind {
				case "account":
					proof.Account.ID = "foreign"
				case "nonce":
					proof.Nonce = "foreign"
				case "missing":
					delete(proof.States, 7)
				case "sender":
					state := proof.States[7]
					state.Metadata.Sender.Address = "different@example.net"
					proof.States[7] = state
				case "locator":
					state := proof.States[7]
					state.Ref.Locator = "different-route"
					proof.States[7] = state
				case "folder":
					delete(proof.Folders, "inbox")
				case "attribution":
					proof.HistoricalAttributionConfirmed = false
				}
				return proof, nil
			})
			if bound, e := BindLegacy(ctx, path, account, p, verifier); e == nil {
				bound.Close()
				t.Fatal("incomplete proof accepted")
			}
		})
	}
	db = rawFixtureDB(t, path)
	if _, e = db.Exec("insert into plans(scope,created_at) values('changed','2026-01-02T00:00:00Z')"); e != nil {
		t.Fatal(e)
	}
	db.Close()
	good := verifyLegacyFunc(func(_ context.Context, a mailbox.Account, p LegacyPreparation) (LegacyVerification, error) {
		return goodLegacyProof(a, p), nil
	})
	if bound, e := BindLegacy(ctx, path, account, p, good); !errors.Is(e, mailbox.ErrStaleIdentity) {
		if bound != nil {
			bound.Close()
		}
		t.Fatalf("stale inventory accepted %v", e)
	}
	db = rawFixtureDB(t, path)
	if _, e = db.Exec("delete from plans where scope='changed'"); e != nil {
		t.Fatal(e)
	}
	db.Close()
	p, e = PrepareLegacyBinding(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	bound, e := BindLegacy(ctx, path, account, p, good)
	if e != nil {
		t.Fatal(e)
	}
	defer bound.Close()
	after, e := dataDigest(ctx, bound.store.db, specs)
	if e != nil || after != original {
		t.Fatalf("binding changed original columns %v", e)
	}
	if _, e = bound.LoadReviewedAction(ctx, 11, mailbox.LocalRef{AccountID: account.ID, N: 7}, account); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("legacy plan executable: %v", e)
	}
	history, e := bound.PlanHistory(ctx, 11)
	if e != nil || len(history.Items) != 1 || history.Items[0].Expected != nil || !history.Items[0].Item.NoBlock {
		t.Fatalf("legacy plan history %+v %v", history, e)
	}
	actions, e := bound.Actions(ctx, 11)
	if e != nil || len(actions) != 1 || actions[0].Audit.ID != 19 || actions[0].Audit.Detail != "invented failure" {
		t.Fatalf("legacy audit %+v %v", actions, e)
	}
	scope := goodLegacyProof(account, p).Folders["inbox"]
	cursor, e := bound.Checkpoint(ctx, scope)
	if e != nil || cursor != (mailbox.Cursor{}) {
		t.Fatalf("legacy cursor promoted: %+v %v", cursor, e)
	}
	if _, e = Open(path); e == nil {
		t.Fatal("bound storage entered legacy path")
	}
	reopened, e := OpenAccount(ctx, path, account)
	if e != nil {
		t.Fatal(e)
	}
	reopened.Close()
	if r := sqlProbe(t, path); !r.Busy {
		t.Fatalf("bound second close released SQLite lock %+v", r)
	}
}
func testIMAPIdentitiesAndStalePlans(t *testing.T) {
	ctx := context.Background()
	a, e := OpenAccount(ctx, inventedPath(t), fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	inbox := fixtureCollection("inbox")
	label := fixtureCollection("label")
	r := fixtureRecord("epoch-one-a")
	r.IMAP = &IMAPLocator{CollectionID: inbox.Ref.CollectionID, MailboxID: "mailbox-a", UIDValidity: 10, UID: 4, EpochGeneration: 1}
	ref, e := a.UpsertMessage(ctx, inbox, r)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.SetBody(ctx, ref, "invented complete body"); e != nil {
		t.Fatal(e)
	}
	if e = a.AddVerdict(ctx, ref, Verdict{Stage: "category", Labels: []string{"record"}}); e != nil {
		t.Fatal(e)
	}
	if e = a.AddMembership(ctx, ref, label); e != nil {
		t.Fatal(e)
	}
	plan, e := a.CreateBoundPlan(ctx, "review", []PlanItem{{N: ref.N, GraphID: "forged", Sender: "forged@example.net", Action: "delete"}})
	if e != nil {
		t.Fatal(e)
	}
	reviewed, e := a.LoadReviewedAction(ctx, plan, ref, fixtureAccount)
	if e != nil || reviewed.Expected.Ref != r.Message.Ref || reviewed.Expected.Metadata.Sender.Address != "sender@example.org" {
		t.Fatalf("caller replaced DB snapshot %+v %v", reviewed, e)
	}
	copyRecord := r
	copyRecord.Message.Ref.MessageID = "epoch-one-b"
	copyRecord.Message.Ref.Locator = "route-copy"
	copyLocator := *r.IMAP
	copyLocator.UID = 5
	copyRecord.IMAP = &copyLocator
	copyRef, e := a.UpsertMessage(ctx, inbox, copyRecord)
	if e != nil || copyRef == ref {
		t.Fatalf("copies collapsed: %v %v", copyRef, e)
	}
	if copies, e := a.ByEmailID(ctx, r.EmailID); e != nil || len(copies) != 2 {
		t.Fatalf("correlation is unique: %d %v", len(copies), e)
	}
	conflict := copyRecord
	conflict.Message.Ref.MessageID = "duplicate-tuple"
	conflict.IMAP = r.IMAP
	if _, e = a.UpsertMessage(ctx, inbox, conflict); e == nil {
		t.Fatal("duplicate occurrence tuple accepted")
	}
	bad := r
	bad.IMAP = &IMAPLocator{CollectionID: inbox.Ref.CollectionID, MailboxID: "mailbox-a", UIDValidity: 10, UID: 0, EpochGeneration: 1}
	if _, e = a.UpsertMessage(ctx, inbox, bad); e == nil {
		t.Fatal("partial locator accepted")
	}
	if e = a.InvalidateIMAPEpoch(ctx, inbox.Ref, "mailbox-a", 11); e != nil {
		t.Fatal(e)
	}
	old, e := a.GetMessage(ctx, ref)
	if e != nil || old.Valid || len(old.State.Memberships) != 1 || old.State.Memberships[0].Collection != label.Ref || old.Body == nil {
		t.Fatalf("epoch erased history/unrelated edge %+v %v", old, e)
	}
	if vs, e := a.Verdicts(ctx, ref); e != nil || len(vs) != 1 {
		t.Fatalf("verdict lost %v", e)
	}
	if _, e = a.LoadReviewedAction(ctx, plan, ref, fixtureAccount); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("old plan survived epoch %v", e)
	}
	if _, e = a.UpsertMessage(ctx, inbox, r); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("retired identity reused %v", e)
	}
	next := fixtureRecord("epoch-two-a")
	next.IMAP = &IMAPLocator{CollectionID: inbox.Ref.CollectionID, MailboxID: "mailbox-a", UIDValidity: 11, UID: 4, EpochGeneration: 2}
	newRef, e := a.UpsertMessage(ctx, inbox, next)
	if e != nil || newRef == ref {
		t.Fatalf("UID reused old N %v %v", newRef, e)
	}
	if e = a.InvalidateIMAPEpoch(ctx, inbox.Ref, "mailbox-a", 10); e != nil {
		t.Fatal(e)
	}
	next.Message.Ref.MessageID = "epoch-three-a"
	next.IMAP.UIDValidity = 10
	next.IMAP.EpochGeneration = 3
	third, e := a.UpsertMessage(ctx, inbox, next)
	if e != nil || third == ref || third == newRef {
		t.Fatalf("numeric epoch reuse resurrected N %v %v", third, e)
	}
	foreign := third
	foreign.AccountID = "foreign"
	if _, e = a.GetMessage(ctx, foreign); !errors.Is(e, mailbox.ErrAccountMismatch) {
		t.Fatalf("cross account local ref %v", e)
	}
	wrong := *a
	wrong.account.ProviderID = "other"
	if _, e = wrong.GetMessage(ctx, third); !errors.Is(e, mailbox.ErrAccountMismatch) {
		t.Fatalf("provider bypass %v", e)
	}
}
func testBoundActionAtomicity(t *testing.T) {
	ctx := context.Background()
	a, e := OpenAccount(ctx, inventedPath(t), fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	inbox := fixtureCollection("inbox")
	label := fixtureCollection("label")
	r := fixtureRecord("action-a")
	r.Revision = "v1"
	ref, e := a.UpsertMessage(ctx, inbox, r)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.AddMembership(ctx, ref, label); e != nil {
		t.Fatal(e)
	}
	id, e := a.CreateBoundPlan(ctx, "review", []PlanItem{{N: ref.N, Action: "delete", NoBlock: true}})
	if e != nil {
		t.Fatal(e)
	}
	reviewed, e := a.LoadReviewedAction(ctx, id, ref, fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	result := reviewed.Expected
	result.Memberships = []mailbox.Membership{{Collection: fixtureCollection("trash").Ref}}
	result.Revision = "v2"
	result.Metadata.IsRead = true
	result.Metadata.Subject = "after action"
	attempt := BoundAction{PlanID: id, Local: ref, Expected: reviewed.Expected, Result: &result, Action: "delete", Destination: "trash", Status: ActionMoved}
	bad := attempt
	bad.Status = "invalid"
	if e = a.RecordBoundActions(ctx, []BoundAction{attempt, bad}); e == nil {
		t.Fatal("partial action batch accepted")
	}
	if actions, e := a.Actions(ctx, id); e != nil || len(actions) != 0 {
		t.Fatalf("partial audit %v", e)
	}
	m, e := a.GetMessage(ctx, ref)
	if e != nil || len(m.State.Memberships) != 2 || m.State.Revision != "v1" {
		t.Fatalf("partial returned state %+v %v", m, e)
	}
	failed := attempt
	failed.Status = ActionFailed
	failed.Result = nil
	failed.Detail = "invented failure"
	if e = a.RecordBoundActions(ctx, []BoundAction{failed}); e != nil {
		t.Fatal(e)
	}
	reordered := attempt.Expected
	reordered.Memberships = append([]mailbox.Membership(nil), reordered.Memberships...)
	reordered.Memberships[0], reordered.Memberships[1] = reordered.Memberships[1], reordered.Memberships[0]
	attempt.Expected = reordered
	if e = a.RecordBoundActions(ctx, []BoundAction{attempt}); e != nil {
		t.Fatal(e)
	}
	unread, e := a.Count(ctx, AccountFilter{Filter: Filter{Unread: true}})
	if e != nil || unread != 0 {
		t.Fatalf("returned read state not projected %d %v", unread, e)
	}
	expired, e := a.ExpiredSenders(ctx)
	if e != nil || expired["sender@example.org"] != 1 {
		t.Fatalf("no_block history %+v %v", expired, e)
	}
	blocked, e := a.MovedSenders(ctx)
	if e != nil || len(blocked) != 0 {
		t.Fatalf("no_block became blocking evidence %+v %v", blocked, e)
	}
	actions, e := a.Actions(ctx, id)
	if e != nil || len(actions) != 2 || actions[0].Audit.Status != ActionFailed || actions[1].Result == nil {
		t.Fatalf("audit %+v %v", actions, e)
	}
	if _, e = a.LoadReviewedAction(ctx, id, ref, fixtureAccount); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("changed state still executable %v", e)
	}
	id, e = a.CreateBoundPlan(ctx, "fresh", []PlanItem{{N: ref.N, Action: "delete"}})
	if e != nil {
		t.Fatal(e)
	}
	if e = a.AddMembership(ctx, ref, label); e != nil {
		t.Fatal(e)
	}
	if _, e = a.LoadReviewedAction(ctx, id, ref, fixtureAccount); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("membership change not stale %v", e)
	}
}

func testAccountVerdictQueries(t *testing.T) {
	ctx := context.Background()
	a, e := OpenAccount(ctx, inventedPath(t), fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	scope := fixtureCollection("inbox")
	r := fixtureRecord("query-a")
	ref, e := a.UpsertMessage(ctx, scope, r)
	if e != nil {
		t.Fatal(e)
	}
	copyRecord := fixtureRecord("query-b")
	other, e := a.UpsertMessage(ctx, scope, copyRecord)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.AddVerdict(ctx, ref, Verdict{Stage: "category", Labels: []string{"old"}, Provenance: "fixture", At: "2026-01-01T00:00:00Z"}); e != nil {
		t.Fatal(e)
	}
	if e = a.AddVerdict(ctx, ref, Verdict{Stage: "category", Labels: []string{"record"}}); e != nil {
		t.Fatal(e)
	}
	if e = a.AddVerdict(ctx, ref, Verdict{Stage: "category", Error: "invented failure"}); e != nil {
		t.Fatal(e)
	}
	latest, e := a.LatestVerdicts(ctx, AccountFilter{})
	if e != nil || len(latest[ref]) != 1 || latest[ref][0].Labels[0] != "record" {
		t.Fatalf("latest %+v %v", latest, e)
	}
	pending, e := a.Pending(ctx, AccountFilter{}, "category")
	if e != nil || len(pending) != 1 || pending[0].Local != other {
		t.Fatalf("pending %+v %v", pending, e)
	}
	grouped, e := a.LatestBySender(ctx, "category")
	if e != nil || len(grouped["sender@example.org"]) != 1 {
		t.Fatalf("sender query %+v %v", grouped, e)
	}
	exists, e := a.HasVerdict(ctx, ref, "category", "fixture", "2026-01-01T00:00:00Z")
	if e != nil || !exists {
		t.Fatalf("verdict identity %v %v", exists, e)
	}
	if e = a.AddVerdicts(ctx, ref, []Verdict{{Stage: "batch"}, {N: other.N, Stage: "batch"}}); e == nil {
		t.Fatal("cross-N batch accepted")
	}
	vs, e := a.Verdicts(ctx, ref)
	if e != nil || len(vs) != 3 {
		t.Fatalf("partial verdict batch %+v %v", vs, e)
	}
	if e = a.SetBody(ctx, ref, "invented body"); e != nil {
		t.Fatal(e)
	}
	if e = a.RemoveMembership(ctx, ref, scope.Ref); e != nil {
		t.Fatal(e)
	}
	same, e := a.UpsertMessage(ctx, scope, r)
	if e != nil || same != ref {
		t.Fatalf("membership re-add changed N %v %v", same, e)
	}
	m, e := a.GetMessage(ctx, ref)
	if e != nil || m.Body == nil || *m.Body != "invented body" {
		t.Fatal("body not retained")
	}
}

func testBindingAttributionAndEmptyHistory(t *testing.T) {
	ctx := context.Background()
	path := inventedPath(t)
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Upsert(ctx, "inbox", Record{GraphID: "historical", Sender: "sender@example.org"}); e != nil {
		t.Fatal(e)
	}
	s.Close()
	db := rawFixtureDB(t, path)
	if _, e = db.Exec("update messages set account_id='foreign'"); e != nil {
		t.Fatal(e)
	}
	db.Close()
	if reopened, e := Open(path); e == nil {
		reopened.Close()
		t.Fatal("partial attribution entered legacy mode")
	}
	if _, e = PrepareLegacyBinding(ctx, path); e == nil {
		t.Fatal("partial attribution was prepared for reassignment")
	}
	path = frozenLegacy(t)
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	db = rawFixtureDB(t, path)
	if _, e = db.Exec("update messages set removed=1"); e != nil {
		t.Fatal(e)
	}
	db.Close()
	account := mailbox.Account{ID: "history-account", ProviderID: GraphProvider}
	if a, e := OpenAccount(ctx, path, account); e == nil {
		a.Close()
		t.Fatal("zero live rows bypassed historical verification")
	}
	preparation, e := PrepareLegacyBinding(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	if len(preparation.Live) != 0 {
		t.Fatal("fixture still live")
	}
	verifier := verifyLegacyFunc(func(_ context.Context, a mailbox.Account, p LegacyPreparation) (LegacyVerification, error) {
		proof := goodLegacyProof(a, p)
		proof.HistoricalAttributionConfirmed = false
		return proof, nil
	})
	if a, e := BindLegacy(ctx, path, account, preparation, verifier); e == nil {
		a.Close()
		t.Fatal("empty-live historical attribution not required")
	}
	verifier = verifyLegacyFunc(func(_ context.Context, a mailbox.Account, p LegacyPreparation) (LegacyVerification, error) {
		return goodLegacyProof(a, p), nil
	})
	a, e := BindLegacy(ctx, path, account, preparation, verifier)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	if history, e := a.PlanHistory(ctx, 12); e != nil || history.Generation != 0 || len(history.Items) != 0 {
		t.Fatalf("empty historical plan changed %+v %v", history, e)
	}
}

func testIMAPScopeTypeSafety(t *testing.T) {
	ctx := context.Background()
	a, e := OpenAccount(ctx, inventedPath(t), fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	scope := fixtureCollection("used")
	r := fixtureRecord("generic-before-epoch")
	ref, e := a.UpsertMessage(ctx, scope, r)
	if e != nil {
		t.Fatal(e)
	}
	id, e := a.CreateBoundPlan(ctx, "before epoch", []PlanItem{{N: ref.N, Action: "delete"}})
	if e != nil {
		t.Fatal(e)
	}
	typed := r
	typed.IMAP = &IMAPLocator{CollectionID: scope.Ref.CollectionID, MailboxID: "mailbox-a", UIDValidity: 20, UID: 1, EpochGeneration: 1}
	if _, e = a.UpsertMessage(ctx, scope, typed); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("used generic identity silently acquired IMAP epoch: %v", e)
	}
	if _, e = a.BeginSync(ctx, scope, mailbox.Cursor{}, &IMAPEpoch{MailboxID: "mailbox-a", UIDValidity: 20, Generation: 1}); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("used generic scope silently acquired IMAP epoch: %v", e)
	}
	for _, kind := range []string{"memberships", "checkpoint", "staging"} {
		t.Run(kind, func(t *testing.T) {
			c := fixtureCollection(kind)
			pass, err := a.BeginSync(ctx, c, mailbox.Cursor{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "checkpoint" {
				if err = a.StagePage(ctx, pass, mailbox.Cursor{}, mailbox.SyncPage{Mode: mailbox.Snapshot, Complete: true}, nil); err != nil {
					t.Fatal(err)
				}
				if err = a.CommitSync(ctx, pass); err != nil {
					t.Fatal(err)
				}
			} else if kind == "memberships" {
				if err = a.AbortSync(ctx, pass); err != nil {
					t.Fatal(err)
				}
				if _, err = a.UpsertMessage(ctx, c, fixtureRecord("generic-membership")); err != nil {
					t.Fatal(err)
				}
			}
			epoch := &IMAPEpoch{MailboxID: "mailbox-a", UIDValidity: 20, Generation: 1}
			if _, err = a.BeginSync(ctx, c, mailbox.Cursor{}, epoch); !errors.Is(err, mailbox.ErrStaleIdentity) {
				t.Fatalf("used scope silently acquired epoch: %v", err)
			}
			fresh := fixtureRecord("fresh-" + kind)
			fresh.IMAP = &IMAPLocator{CollectionID: c.Ref.CollectionID, MailboxID: epoch.MailboxID, UIDValidity: epoch.UIDValidity, UID: 3, EpochGeneration: 1}
			if _, err = a.UpsertMessage(ctx, c, fresh); !errors.Is(err, mailbox.ErrStaleIdentity) {
				t.Fatalf("new occurrence retyped used scope: %v", err)
			}
		})
	}
	reviewed, e := a.LoadReviewedAction(ctx, id, ref, fixtureAccount)
	if e != nil {
		t.Fatalf("refused mutation damaged plan: %v", e)
	}
	other := fixtureCollection("typed")
	record := fixtureRecord("typed-occurrence")
	record.IMAP = &IMAPLocator{CollectionID: other.Ref.CollectionID, MailboxID: "mailbox-b", UIDValidity: 30, UID: 2, EpochGeneration: 1}
	if _, e = a.UpsertMessage(ctx, other, record); e != nil {
		t.Fatal(e)
	}
	result := reviewed.Expected
	result.Memberships = []mailbox.Membership{{Collection: other.Ref}}
	if e = a.RecordBoundActions(ctx, []BoundAction{{PlanID: id, Local: ref, Expected: reviewed.Expected, Result: &result, Action: "delete", Status: ActionMoved}}); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("generic action result acquired a scoped IMAP identity: %v", e)
	}
	if e = a.AddMembership(ctx, ref, other); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("generic membership acquired scoped identity %v", e)
	}
}

func TestIMAPEpochResetProjections(t *testing.T) {
	for _, remaining := range []int{0, 1, 2} {
		t.Run(string(rune('0'+remaining))+" remaining", func(t *testing.T) {
			ctx := context.Background()
			a, e := OpenAccount(ctx, inventedPath(t), fixtureAccount)
			if e != nil {
				t.Fatal(e)
			}
			defer a.Close()
			inbox := fixtureCollection("inbox")
			r := fixtureRecord("old-occurrence")
			r.IMAP = &IMAPLocator{CollectionID: inbox.Ref.CollectionID, MailboxID: "mailbox-a", UIDValidity: 10, UID: 4, EpochGeneration: 1}
			ref, e := a.UpsertMessage(ctx, inbox, r)
			if e != nil {
				t.Fatal(e)
			}
			for i := 0; i < remaining; i++ {
				if e = a.AddMembership(ctx, ref, fixtureCollection("label-"+string(rune('a'+i)))); e != nil {
					t.Fatal(e)
				}
			}
			if e = a.SetBody(ctx, ref, "invented preserved body"); e != nil {
				t.Fatal(e)
			}
			if e = a.AddVerdict(ctx, ref, Verdict{Stage: "category", Labels: []string{"record"}}); e != nil {
				t.Fatal(e)
			}
			id, e := a.CreateBoundPlan(ctx, "before reset", []PlanItem{{N: ref.N, Action: "delete"}})
			if e != nil {
				t.Fatal(e)
			}
			reviewed, e := a.LoadReviewedAction(ctx, id, ref, fixtureAccount)
			if e != nil {
				t.Fatal(e)
			}
			if e = a.RecordBoundActions(ctx, []BoundAction{{PlanID: id, Local: ref, Expected: reviewed.Expected, Action: "delete", Status: ActionFailed}}); e != nil {
				t.Fatal(e)
			}
			if e = a.InvalidateIMAPEpoch(ctx, inbox.Ref, "mailbox-a", 11); e != nil {
				t.Fatal(e)
			}
			var removed bool
			var folder string
			if e = a.store.db.QueryRowContext(ctx, "select removed,folder from messages where n=?", ref.N).Scan(&removed, &folder); e != nil {
				t.Fatal(e)
			}
			wantFolder := ""
			if remaining == 1 {
				wantFolder = "label-a"
			}
			if removed != (remaining == 0) || folder != wantFolder {
				t.Errorf("stale projection: removed=%v folder=%q, remaining=%d", removed, folder, remaining)
			}
			m, e := a.GetMessage(ctx, ref)
			if e != nil || m.Valid || len(m.State.Memberships) != remaining || m.Body == nil || *m.Body != "invented preserved body" {
				t.Fatalf("reset lost identity/history: %+v %v", m, e)
			}
			if vs, e := a.Verdicts(ctx, ref); e != nil || len(vs) != 1 {
				t.Fatalf("verdict history: %+v %v", vs, e)
			}
			if history, e := a.PlanHistory(ctx, id); e != nil || len(history.Items) != 1 {
				t.Fatalf("plan history: %+v %v", history, e)
			}
			if actions, e := a.Actions(ctx, id); e != nil || len(actions) != 1 || actions[0].Audit.Status != ActionFailed {
				t.Fatalf("action history: %+v %v", actions, e)
			}
			if _, e = a.LoadReviewedAction(ctx, id, ref, fixtureAccount); !errors.Is(e, mailbox.ErrStaleIdentity) {
				t.Fatalf("reset plan still executable: %v", e)
			}
		})
	}
}

// The driver delegates to the pinned SQLite engine and captures statements from
// actual exported operations, so planner assertions cannot pass on copied SQL.
type capturedQuery struct {
	sql  string
	args []driver.NamedValue
}
type queryCapture struct {
	sync.Mutex
	queries []capturedQuery
}
type captureDriver struct{ capture *queryCapture }

func (d captureDriver) Open(name string) (driver.Conn, error) {
	c, e := (&sqlite.Driver{}).Open(name)
	if e != nil {
		return nil, e
	}
	return &captureConn{Conn: c, capture: d.capture}, nil
}

type captureConn struct {
	driver.Conn
	capture *queryCapture
}

func (c *captureConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.capture.Lock()
	c.capture.queries = append(c.capture.queries, capturedQuery{q, append([]driver.NamedValue(nil), args...)})
	c.capture.Unlock()
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}
func TestAccountMembershipQueryPlans(t *testing.T) {
	ctx := context.Background()
	path := inventedPath(t)
	a, e := OpenAccount(ctx, path, fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	scope := fixtureCollection("inbox")
	ref, e := a.UpsertMessage(ctx, scope, fixtureRecord("plan-query"))
	if e != nil {
		t.Fatal(e)
	}
	if e = a.AddVerdict(ctx, ref, Verdict{Stage: "category"}); e != nil {
		t.Fatal(e)
	}
	if e = a.store.db.Close(); e != nil {
		t.Fatal(e)
	}
	capture := &queryCapture{}
	name := "invented-query-capture-" + randomID()
	sql.Register(name, captureDriver{capture})
	db, e := sql.Open(name, sqliteURI(path, "rw", false))
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	a.store.db = db
	if _, e = a.Messages(ctx, AccountFilter{Collection: &scope.Ref}); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Count(ctx, AccountFilter{}); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Pending(ctx, AccountFilter{}, "category"); e != nil {
		t.Fatal(e)
	}
	if _, e = a.LatestVerdicts(ctx, AccountFilter{}); e != nil {
		t.Fatal(e)
	}
	if _, e = a.LatestBySender(ctx, "category"); e != nil {
		t.Fatal(e)
	}
	capture.Lock()
	queries := append([]capturedQuery(nil), capture.queries...)
	capture.Unlock()
	checked := 0
	for _, q := range queries {
		if !strings.Contains(q.sql, "exists(select 1 from memberships") {
			continue
		}
		checked++
		args := make([]any, len(q.args))
		for i, v := range q.args {
			args[i] = v.Value
		}
		rows, e := db.QueryContext(ctx, "explain query plan "+q.sql, args...)
		if e != nil {
			t.Fatal(e)
		}
		membershipPlans := 0
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if e = rows.Scan(&id, &parent, &unused, &detail); e != nil {
				t.Fatal(e)
			}
			if strings.Contains(detail, "memberships") || strings.Contains(detail, " ms ") {
				membershipPlans++
				t.Log(detail)
				if !strings.Contains(detail, "SEARCH") || !strings.Contains(detail, "account_id=? AND provider_id=? AND n=?") {
					t.Errorf("membership predicate scans without the account/provider/N key: %s", detail)
				}
			}
		}
		if e = rows.Err(); e != nil {
			t.Fatal(e)
		}
		rows.Close()
		if membershipPlans == 0 {
			t.Fatalf("membership plan was not exercised: %s", q.sql)
		}
	}
	if checked != 5 {
		t.Fatalf("expected five exported query paths, captured %d", checked)
	}
}

func TestIMAPEpochProjectionFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	a, e := OpenAccount(ctx, inventedPath(t), fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	scope := fixtureCollection("inbox")
	r := fixtureRecord("projection-rollback")
	r.IMAP = &IMAPLocator{CollectionID: scope.Ref.CollectionID, MailboxID: "mailbox-a", UIDValidity: 10, UID: 7, EpochGeneration: 1}
	if _, e = a.UpsertMessage(ctx, scope, r); e != nil {
		t.Fatal(e)
	}
	if _, e = a.store.db.ExecContext(ctx, `create trigger invented_projection_failure before update of removed,folder on messages begin select raise(abort,'invented projection failure'); end`); e != nil {
		t.Fatal(e)
	}
	before, e := logicalDigest(ctx, a.store.db)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.InvalidateIMAPEpoch(ctx, scope.Ref, "mailbox-a", 11); e == nil || !strings.Contains(e.Error(), "invented projection failure") {
		t.Fatalf("projection failure not exercised: %v", e)
	}
	after, e := logicalDigest(ctx, a.store.db)
	if e != nil || before != after {
		t.Fatalf("failed reset partially committed identity/membership/epoch/checkpoint: %v", e)
	}
	if _, e = a.store.db.ExecContext(ctx, "drop trigger invented_projection_failure"); e != nil {
		t.Fatal(e)
	}
	if e = a.InvalidateIMAPEpoch(ctx, scope.Ref, "mailbox-a", 11); e != nil {
		t.Fatal(e)
	}
}

func TestGenericLocatorRotationRetainsIdentity(t *testing.T) {
	ctx := context.Background()
	account := fixtureAccount
	account.ProviderID = "generic"
	a, e := OpenAccount(ctx, inventedPath(t), account)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	scope := fixtureCollection("inbox")
	r := fixtureRecord("locator-rotation")
	ref, e := a.UpsertMessage(ctx, scope, r)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.SetBody(ctx, ref, "invented preserved body"); e != nil {
		t.Fatal(e)
	}
	id, e := a.CreateBoundPlan(ctx, "old locator", []PlanItem{{N: ref.N, Action: "delete"}})
	if e != nil {
		t.Fatal(e)
	}
	r.Message.Ref.Locator = "rotated-opaque-route"
	current, e := a.UpsertMessage(ctx, scope, r)
	if e != nil || current != ref {
		t.Fatalf("locator rotation changed identity: %+v %v", current, e)
	}
	m, e := a.GetMessage(ctx, ref)
	if e != nil || m.State.Ref.Locator != r.Message.Ref.Locator || m.Body == nil {
		t.Fatalf("rotated locator not persisted: %+v %v", m, e)
	}
	if _, e = a.LoadReviewedAction(ctx, id, ref, account); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("old route remains executable: %v", e)
	}
}

func TestReturnedMetadataAttribution(t *testing.T) {
	ctx := context.Background()
	a, e := OpenAccount(ctx, inventedPath(t), fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	scope := fixtureCollection("inbox")
	r := fixtureRecord("scoped-update")
	ref, e := a.UpsertMessage(ctx, scope, r)
	if e != nil {
		t.Fatal(e)
	}
	id, e := a.CreateBoundPlan(ctx, "scoped result", []PlanItem{{N: ref.N, Action: "delete"}})
	if e != nil {
		t.Fatal(e)
	}
	reviewed, e := a.LoadReviewedAction(ctx, id, ref, fixtureAccount)
	if e != nil {
		t.Fatal(e)
	}
	result := reviewed.Expected
	result.Metadata.Subject = "must not leak"
	wrong := ref
	wrong.AccountID = "account-b"
	if e = a.RecordBoundActions(ctx, []BoundAction{{PlanID: id, Local: wrong, Expected: reviewed.Expected, Result: &result, Action: "delete", Status: ActionMoved}}); !errors.Is(e, mailbox.ErrAccountMismatch) {
		t.Fatalf("wrong local account accepted: %v", e)
	}
	// Test the private helper's SQL fence independently of its already-validated
	// caller, using a mismatched private receiver unavailable to API consumers.
	for _, dimension := range []string{"account", "provider"} {
		t.Run(dimension, func(t *testing.T) {
			other := *a
			local := ref
			state := result
			if dimension == "account" {
				other.account.ID = "account-b"
				local.AccountID = other.account.ID
				state.Ref.AccountID = other.account.ID
				state.Memberships = append([]mailbox.Membership(nil), state.Memberships...)
				for i := range state.Memberships {
					state.Memberships[i].Collection.AccountID = other.account.ID
				}
			} else {
				other.account.ProviderID = "other-provider"
			}
			err := a.transaction(ctx, func(q sqlAccess) error { return other.writeReturnedMetadata(ctx, q, local, state) })
			if !errors.Is(err, mailbox.ErrStaleIdentity) {
				t.Fatalf("unattributed helper update succeeded: %v", err)
			}
		})
	}
	m, e := a.GetMessage(ctx, ref)
	if e != nil || m.State.Metadata.Subject != r.Message.Metadata.Subject {
		t.Fatalf("foreign update changed state: %+v %v", m, e)
	}
	if audits, e := a.Actions(ctx, id); e != nil || len(audits) != 0 {
		t.Fatalf("foreign action appended audit: %+v %v", audits, e)
	}
}
