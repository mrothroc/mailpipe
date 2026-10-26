package store_test

import (
	"context"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/store"
)

func TestByMessageIDFindsLiveCopies(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	for _, r := range []struct {
		folder string
		rec    store.Record
	}{
		{"inbox", store.Record{GraphID: "a", InternetMessageID: "<m1@example.org>", Sender: "x@example.org"}},
		{"deleteditems", store.Record{GraphID: "b", InternetMessageID: "<m1@example.org>", Sender: "x@example.org"}},
		{"inbox", store.Record{GraphID: "c", InternetMessageID: "<m2@example.org>", Sender: "y@example.org"}},
	} {
		if _, err := s.Upsert(ctx, r.folder, r.rec); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ByMessageID(ctx, "<m1@example.org>")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v err %v", got, err)
	}
	if got, _ := s.ByMessageID(ctx, "<absent@example.org>"); len(got) != 0 {
		t.Fatalf("absent id matched %+v", got)
	}
}

func TestPlansAreStoredSnapshots(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ms := seed(t, s)
	items := []store.PlanItem{
		{N: ms[0].N, Action: "delete", Reasons: []string{"imported-classifier:flagged"}},
		{N: ms[1].N, Action: "keep", Reasons: []string{"known:known_contact"}},
	}
	id, err := s.CreatePlan(ctx, "folder=inbox", items)
	if err != nil || id < 1 {
		t.Fatalf("id %d err %v", id, err)
	}
	p, got, err := s.Plan(ctx, id)
	if err != nil || p.ID != id || p.Scope != "folder=inbox" || p.CreatedAt == "" || len(got) != 2 {
		t.Fatalf("plan %+v items %+v err %v", p, got, err)
	}
	byN := map[int64]store.PlanItem{}
	for _, it := range got {
		byN[it.N] = it
	}
	del := byN[ms[0].N]
	if del.Action != "delete" || del.Reasons[0] != "imported-classifier:flagged" || del.GraphID != ms[0].GraphID || del.Sender != ms[0].Sender {
		t.Fatalf("item %+v: items carry the message identity they were planned against", del)
	}
	plans, err := s.Plans(ctx)
	if err != nil || len(plans) != 1 || plans[0].Counts["delete"] != 1 || plans[0].Counts["keep"] != 1 {
		t.Fatalf("plans %+v err %v", plans, err)
	}
	if _, _, err := s.Plan(ctx, 999); err == nil {
		t.Fatal("missing plan returned no error")
	}
}

func TestLatestVerdictsPerStage(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ms := seed(t, s)
	n := ms[0].N
	_ = s.AddVerdict(ctx, store.Verdict{N: n, Stage: "political", Labels: []string{"old"}})
	_ = s.AddVerdict(ctx, store.Verdict{N: n, Stage: "political", Labels: []string{"new"}})
	_ = s.AddVerdict(ctx, store.Verdict{N: n, Stage: "political", Error: "timed out"}) // failures never replace a verdict
	_ = s.AddVerdict(ctx, store.Verdict{N: n, Stage: "known"})
	got, err := s.LatestVerdicts(ctx, store.Filter{Folder: "inbox"})
	if err != nil {
		t.Fatal(err)
	}
	vs := got[n]
	if len(vs) != 2 {
		t.Fatalf("latest %+v", vs)
	}
	for _, v := range vs {
		if v.Stage == "political" && v.Labels[0] != "new" {
			t.Fatalf("political latest %+v", v)
		}
	}
}

func TestLatestBySenderGroupsEachMessagesLatestVerdict(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	for i, r := range []store.Record{
		{GraphID: "a", Sender: "Loud@Campaign.example.org"},
		{GraphID: "b", Sender: "loud@campaign.example.org"},
		{GraphID: "c", Sender: "quiet@example.net"},
	} {
		r.Received = "2026-10-0" + string(rune('1'+i)) + "T00:00:00Z"
		if _, err := s.Upsert(ctx, "inbox", r); err != nil {
			t.Fatal(err)
		}
	}
	ms, _ := s.Messages(ctx, store.Filter{})
	n := map[string]int64{}
	for _, m := range ms {
		n[m.GraphID] = m.N
	}
	_ = s.AddVerdict(ctx, store.Verdict{N: n["a"], Stage: "political", Labels: []string{"old"}})
	_ = s.AddVerdict(ctx, store.Verdict{N: n["a"], Stage: "political", Labels: []string{"political_appeal"}})
	_ = s.AddVerdict(ctx, store.Verdict{N: n["b"], Stage: "political", Labels: []string{"political_appeal"}})
	_ = s.AddVerdict(ctx, store.Verdict{N: n["b"], Stage: "political", Error: "timed out"})
	_ = s.AddVerdict(ctx, store.Verdict{N: n["c"], Stage: "other", Labels: []string{"x"}})
	got, err := s.LatestBySender(ctx, "political")
	if err != nil {
		t.Fatal(err)
	}
	loud := got["loud@campaign.example.org"]
	if len(got) != 1 || len(loud) != 2 || loud[0].Labels[0] != "political_appeal" || loud[1].Labels[0] != "political_appeal" {
		t.Fatalf("got %+v", got)
	}
}

func TestLatestBySenderToleratesANullSender(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ms := seed(t, s)
	_ = s.AddVerdict(ctx, store.Verdict{N: ms[0].N, Stage: "political", Labels: []string{"political_appeal"}})
	if err := s.Exec(ctx, "update messages set sender=null where n=?", ms[0].N); err != nil {
		t.Fatal(err)
	}
	_ = s.AddVerdict(ctx, store.Verdict{N: ms[1].N, Stage: "political", Labels: []string{"political_appeal"}})
	if err := s.Exec(ctx, "update messages set sender='  ' where n=?", ms[1].N); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestBySender(ctx, "political")
	if err != nil {
		t.Fatalf("NULL sender broke the query: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("messages without a sender were grouped together: %+v", got)
	}
}

func TestCreatePlanRejectsAMissingMessage(t *testing.T) {
	s, _ := open(t)
	if _, err := s.CreatePlan(context.Background(), "x", []store.PlanItem{{N: 999, Action: "delete"}}); err == nil {
		t.Fatal("a plan item for a missing message was silently dropped")
	}
	if plans, _ := s.Plans(context.Background()); len(plans) != 0 {
		t.Fatalf("a failed plan was partly stored: %+v", plans)
	}
}

func TestAddVerdictsIsAllOrNothing(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ms := seed(t, s)
	err := s.AddVerdicts(ctx, []store.Verdict{{N: ms[0].N, Stage: "a"}, {N: 999999, Stage: "a"}})
	if err == nil {
		t.Fatal("a verdict for a missing message was accepted")
	}
	if vs, _ := s.Verdicts(ctx, ms[0].N); len(vs) != 0 {
		t.Fatalf("partial batch committed: %+v", vs)
	}
	if err := s.AddVerdicts(ctx, []store.Verdict{{N: ms[0].N, Stage: "a"}, {N: ms[1].N, Stage: "a"}}); err != nil {
		t.Fatal(err)
	}
}

func TestCreatePlanRejectsARemovedMessage(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ms := seed(t, s)
	if _, err := s.Remove(ctx, ms[0].Folder, ms[0].GraphID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePlan(ctx, "x", []store.PlanItem{{N: ms[0].N, Action: "delete"}}); err == nil {
		t.Fatal("planned an action for a message that is no longer in the mailbox")
	}
}

func TestLatestBySenderKeysAreTrimmed(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ms := seed(t, s)
	_ = s.AddVerdict(ctx, store.Verdict{N: ms[0].N, Stage: "political", Labels: []string{"x"}})
	if err := s.Exec(ctx, "update messages set sender='  Padded@Example.org ' where n=?", ms[0].N); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LatestBySender(ctx, "political")
	if len(got["padded@example.org"]) != 1 {
		t.Fatalf("keys %v", got)
	}
}

func TestByMessageIDIgnoresBlankIDs(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	seed(t, s) // seeded messages have no Message-ID
	for _, id := range []string{"", "  "} {
		if got, err := s.ByMessageID(ctx, id); err != nil || len(got) != 0 {
			t.Fatalf("ByMessageID(%q) = %d messages, err %v", id, len(got), err)
		}
	}
}

func TestRemoveUnseenMarksOnlyThatFoldersMissingMessages(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	for _, r := range []struct{ folder, id string }{{"inbox", "a"}, {"inbox", "b"}, {"inbox", "c"}, {"deleteditems", "d"}} {
		if _, err := s.Upsert(ctx, r.folder, store.Record{GraphID: r.id}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.RemoveUnseen(ctx, "inbox", map[string]bool{"b": true})
	if err != nil || n != 2 {
		t.Fatalf("removed %d err %v", n, err)
	}
	if c, _ := s.Count(ctx, store.Filter{Folder: "inbox"}); c != 1 {
		t.Fatalf("inbox count %d", c)
	}
	if c, _ := s.Count(ctx, store.Filter{Folder: "deleteditems"}); c != 1 {
		t.Fatal("another folder's message was removed")
	}
	if n, _ := s.RemoveUnseen(ctx, "inbox", map[string]bool{"b": true}); n != 0 {
		t.Fatalf("second pass removed %d", n)
	}
}

// A plan item records the folder it was planned in, and reports whether the
// message has left the mailbox since, so apply can skip anything that moved.
func TestPlanItemsRecordTheirFolderAndCurrentState(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ms := seed(t, s)
	id, err := s.CreatePlan(ctx, "x", []store.PlanItem{{N: ms[0].N, Action: "delete"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(ctx, "deleteditems", store.Record{GraphID: ms[0].GraphID, Sender: ms[0].Sender}); err != nil {
		t.Fatal(err) // the message moved after planning
	}
	_, items, err := s.Plan(ctx, id)
	if err != nil || len(items) != 1 {
		t.Fatal(err)
	}
	it := items[0]
	if it.PlannedFolder != "inbox" || it.Folder != "deleteditems" || it.Removed {
		t.Fatalf("item %+v", it)
	}
	if _, err := s.Remove(ctx, "deleteditems", ms[0].GraphID); err != nil {
		t.Fatal(err)
	}
	_, items, _ = s.Plan(ctx, id)
	if !items[0].Removed {
		t.Fatalf("removal not reported: %+v", items[0])
	}
}

func TestOldDatabasesGainThePlanFolderColumn(t *testing.T) {
	path := t.TempDir() + "/old.db"
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Recreate plan_items as the first schema had it, without planned_folder.
	if err := s.Exec(ctx, "drop table plan_items"); err != nil {
		t.Fatal(err)
	}
	if err := s.Exec(ctx, `create table plan_items (plan_id integer not null references plans(id), n integer not null references messages(n),
		graph_id text not null, internet_message_id text not null, sender text not null, action text not null,
		reasons text not null default '[]', primary key (plan_id, n))`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	ms := seed(t, s)
	if _, err := s.CreatePlan(ctx, "x", []store.PlanItem{{N: ms[0].N, Action: "delete"}}); err != nil {
		t.Fatalf("plan on a migrated database: %v", err)
	}
}
