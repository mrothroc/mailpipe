package store_test

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/store"
)

func TestActionLogRecordsEveryAttemptAndOnlyMovesCountAsDone(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ms := seed(t, s)
	plan, err := s.CreatePlan(ctx, "folder=inbox", []store.PlanItem{{N: ms[0].N, Action: "delete"}, {N: ms[1].N, Action: "delete"}})
	if err != nil {
		t.Fatal(err)
	}
	err = s.LogActions(ctx, []store.Action{
		{PlanID: plan, N: ms[0].N, GraphID: ms[0].GraphID, Action: "delete", Destination: "deleteditems", Status: store.ActionMoved},
		{PlanID: plan, N: ms[1].N, GraphID: ms[1].GraphID, Action: "delete", Destination: "deleteditems", Status: store.ActionFailed, Detail: "graph 503"},
	})
	if err != nil {
		t.Fatal(err)
	}
	done, err := s.Moved(ctx, plan)
	if err != nil || len(done) != 1 || !done[ms[0].N] {
		t.Fatalf("moved %v err %v: a failed action stays pending", done, err)
	}
	// A retry that succeeds adds a row; the history keeps both attempts.
	if err := s.LogActions(ctx, []store.Action{{PlanID: plan, N: ms[1].N, GraphID: ms[1].GraphID, Action: "delete",
		Destination: "deleteditems", Status: store.ActionMoved}}); err != nil {
		t.Fatal(err)
	}
	if done, _ := s.Moved(ctx, plan); len(done) != 2 {
		t.Fatalf("moved %v after the retry", done)
	}
	log, err := s.Actions(ctx, plan)
	if err != nil || len(log) != 3 {
		t.Fatalf("log %+v err %v", log, err)
	}
	if log[1].Status != store.ActionFailed || log[1].Detail != "graph 503" || log[1].At == "" || log[1].Sender != ms[1].Sender {
		t.Fatalf("row %+v", log[1])
	}
	if other, _ := s.Moved(ctx, plan+1); len(other) != 0 {
		t.Fatalf("another plan sees this plan's moves: %v", other)
	}
}

func TestLogActionsIsAllOrNothing(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ms := seed(t, s)
	plan, _ := s.CreatePlan(ctx, "folder=inbox", []store.PlanItem{{N: ms[0].N, Action: "delete"}})
	err := s.LogActions(ctx, []store.Action{
		{PlanID: plan, N: ms[0].N, GraphID: ms[0].GraphID, Action: "delete", Destination: "deleteditems", Status: store.ActionMoved},
		{PlanID: plan, N: ms[0].N, GraphID: ms[0].GraphID, Action: "delete", Destination: "deleteditems", Status: "exploded"},
	})
	if err == nil {
		t.Fatal("an unknown status was accepted")
	}
	if log, _ := s.Actions(ctx, plan); len(log) != 0 {
		t.Fatalf("partial log written: %+v", log)
	}
	if m, _, _ := s.Get(ctx, ms[0].N); m.Folder != "inbox" {
		t.Fatalf("a rolled-back move changed the local folder to %q", m.Folder)
	}
}

// The local copy follows every logged move, so listings are current before
// the next sync; a failed or skipped attempt leaves the message where it was.
func TestALoggedMoveMovesTheLocalCopy(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ms := seed(t, s)
	plan, _ := s.CreatePlan(ctx, "folder=inbox", []store.PlanItem{{N: ms[0].N, Action: "delete"}, {N: ms[1].N, Action: "delete"}})
	if err := s.LogActions(ctx, []store.Action{
		{PlanID: plan, N: ms[0].N, GraphID: ms[0].GraphID, Action: "delete", Destination: "deleteditems", Status: store.ActionMoved},
		{PlanID: plan, N: ms[1].N, GraphID: ms[1].GraphID, Action: "delete", Destination: "deleteditems", Status: store.ActionFailed},
		{PlanID: plan, N: ms[1].N, GraphID: ms[1].GraphID, Action: "delete", Destination: "deleteditems", Status: store.ActionSkipped},
	}); err != nil {
		t.Fatal(err)
	}
	if m, _, _ := s.Get(ctx, ms[0].N); m.Folder != "deleteditems" {
		t.Fatalf("moved message is in %q locally", m.Folder)
	}
	if m, _, _ := s.Get(ctx, ms[1].N); m.Folder != "inbox" {
		t.Fatalf("an unmoved message is in %q locally", m.Folder)
	}
	// The next sync's removal from the Inbox must not touch the moved copy.
	if gone, err := s.Remove(ctx, "inbox", ms[0].GraphID); err != nil || gone {
		t.Fatalf("Remove = %v, %v", gone, err)
	}
	if n, _ := s.Count(ctx, store.Filter{Folder: "deleteditems"}); n != 1 {
		t.Fatalf("deleteditems holds %d live messages", n)
	}
}

func TestMovedSendersCountsEveryPlansMovesOncePerMessage(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ms := seed(t, s)
	plan, _ := s.CreatePlan(ctx, "folder=inbox", []store.PlanItem{{N: ms[0].N, Action: "delete"}, {N: ms[1].N, Action: "delete"}})
	other, _ := s.CreatePlan(ctx, "folder=inbox", []store.PlanItem{{N: ms[0].N, Action: "delete"}, {N: ms[1].N, Action: "delete"}})
	if _, err := s.Upsert(ctx, "inbox", store.Record{GraphID: "planned-only", Sender: "late@example.net"}); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Messages(ctx, store.Filter{Folder: "inbox", SenderLike: "late@"})
	_, _ = s.CreatePlan(ctx, "folder=inbox", []store.PlanItem{{N: all[0].N, Action: "delete"}}) // planned, never applied
	if err := s.LogActions(ctx, []store.Action{
		{PlanID: plan, N: ms[0].N, GraphID: ms[0].GraphID, Action: "delete", Destination: "deleteditems", Status: store.ActionFailed},
		{PlanID: plan, N: ms[0].N, GraphID: ms[0].GraphID, Action: "delete", Destination: "deleteditems", Status: store.ActionMoved},
		{PlanID: plan, N: ms[1].N, GraphID: ms[1].GraphID, Action: "delete", Destination: "deleteditems", Status: store.ActionSkipped},
		{PlanID: other, N: ms[0].N, GraphID: ms[0].GraphID, Action: "delete", Destination: "deleteditems", Status: store.ActionMoved},
		{PlanID: other, N: ms[1].N, GraphID: ms[1].GraphID, Action: "delete", Destination: "deleteditems", Status: store.ActionMoved},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.MovedSenders(ctx)
	want := []store.SenderMoves{{Sender: ms[0].Sender, Moved: 1}, {Sender: ms[1].Sender, Moved: 1}}
	sort.Slice(want, func(i, j int) bool { return want[i].Sender < want[j].Sender })
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v err %v, want %+v: every plan's moves count, once per message", got, err, want)
	}
}

func TestMovedSendersNormalisesCaseAndSpacing(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	for _, r := range []store.Record{{GraphID: "p1", Sender: " News@Campaign.example.org "}, {GraphID: "p2", Sender: "news@campaign.example.org"}} {
		if _, err := s.Upsert(ctx, "inbox", r); err != nil {
			t.Fatal(err)
		}
	}
	ms, _ := s.Messages(ctx, store.Filter{Folder: "inbox"})
	var items []store.PlanItem
	var log []store.Action
	for _, m := range ms {
		items = append(items, store.PlanItem{N: m.N, Action: "delete"})
	}
	plan, _ := s.CreatePlan(ctx, "folder=inbox", items)
	for _, m := range ms {
		log = append(log, store.Action{PlanID: plan, N: m.N, GraphID: m.GraphID, Action: "delete", Destination: "deleteditems", Status: store.ActionMoved})
	}
	if err := s.LogActions(ctx, log); err != nil {
		t.Fatal(err)
	}
	got, err := s.MovedSenders(ctx)
	if err != nil || len(got) != 1 || got[0] != (store.SenderMoves{Sender: "news@campaign.example.org", Moved: 2}) {
		t.Fatalf("got %+v err %v: one sender, however it was spelt", got, err)
	}
}

// A shelf-life deletion (no_block) never puts its sender on the blocklist.
func TestMovedSendersSkipsNoBlockItems(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ms := seed(t, s)
	plan, _ := s.CreatePlan(ctx, "folder=inbox", []store.PlanItem{{N: ms[0].N, Action: "delete", NoBlock: true}, {N: ms[1].N, Action: "delete"}})
	if err := s.LogActions(ctx, []store.Action{
		{PlanID: plan, N: ms[0].N, GraphID: ms[0].GraphID, Action: "delete", Destination: "deleteditems", Status: store.ActionMoved},
		{PlanID: plan, N: ms[1].N, GraphID: ms[1].GraphID, Action: "delete", Destination: "deleteditems", Status: store.ActionMoved},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.MovedSenders(ctx)
	if err != nil || len(got) != 1 || got[0].Sender != ms[1].Sender {
		t.Fatalf("got %+v err %v", got, err)
	}
	expired, err := s.ExpiredSenders(ctx)
	if err != nil || len(expired) != 1 || expired[ms[0].Sender] != 1 {
		t.Fatalf("expired %v err %v: a no_block move counts as expired mail for its sender", expired, err)
	}
	_, items, _ := s.Plan(ctx, plan)
	for _, it := range items {
		if it.NoBlock != (it.N == ms[0].N) {
			t.Fatalf("no_block not stored as planned: %+v", items)
		}
	}
}
