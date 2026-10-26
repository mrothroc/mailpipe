package apply_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/apply"
	"github.com/mrothroc/mailpipe/pkg/store"
)

// fakeMailbox is a mailbox of messages by Graph id, each in a folder id.
type fakeMailbox struct {
	msgs      map[string]apply.Remote
	folders   map[string]string // well-known name -> folder id
	lookups   [][]string
	moves     [][]string
	lookupErr map[string]error // per message
	moveErr   map[string]error // per message
	batchErr  error            // a whole Move call fails
}

func (f *fakeMailbox) FolderID(_ context.Context, name string) (string, error) {
	if id, ok := f.folders[name]; ok {
		return id, nil
	}
	return "", fmt.Errorf("no folder %s", name)
}

func (f *fakeMailbox) Lookup(_ context.Context, ids []string) ([]apply.Found, error) {
	f.lookups = append(f.lookups, ids)
	out := make([]apply.Found, len(ids))
	for i, id := range ids {
		if err := f.lookupErr[id]; err != nil {
			out[i].Err = err
		} else if r, ok := f.msgs[id]; ok {
			out[i].Remote = r
		} else {
			out[i].Missing = true
		}
	}
	return out, nil
}

func (f *fakeMailbox) Move(_ context.Context, ids []string, dest string) ([]error, error) {
	f.moves = append(f.moves, ids)
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	out := make([]error, len(ids))
	for i, id := range ids {
		if err := f.moveErr[id]; err != nil {
			out[i] = err
			continue
		}
		r := f.msgs[id]
		r.ParentFolderID = f.folders[dest]
		f.msgs[id] = r
	}
	return out, nil
}

type fixture struct {
	store *store.Store
	box   *fakeMailbox
	plan  int64
	items []store.PlanItem
}

// newFixture plans n deletes from the inbox, all still there on the server.
func newFixture(t *testing.T, n int) *fixture {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	box := &fakeMailbox{msgs: map[string]apply.Remote{}, folders: map[string]string{"inbox": "F-IN", "deleteditems": "F-DEL"},
		lookupErr: map[string]error{}, moveErr: map[string]error{}}
	var planned []store.PlanItem
	for i := range n {
		id := fmt.Sprintf("g%02d", i)
		rec := store.Record{GraphID: id, InternetMessageID: "<" + id + "@example.org>", Sender: "News@Campaign.example.org"}
		if _, err := s.Upsert(ctx, "inbox", rec); err != nil {
			t.Fatal(err)
		}
		box.msgs[id] = apply.Remote{InternetMessageID: rec.InternetMessageID, Sender: "news@campaign.example.org", ParentFolderID: "F-IN"}
	}
	ms, err := s.Messages(ctx, store.Filter{Folder: "inbox"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		planned = append(planned, store.PlanItem{N: m.N, Action: "delete"})
	}
	id, err := s.CreatePlan(ctx, "folder=inbox", planned)
	if err != nil {
		t.Fatal(err)
	}
	_, items, err := s.Plan(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{store: s, box: box, plan: id, items: items}
}

func (f *fixture) run(t *testing.T, opts apply.Options) (apply.Summary, map[string]string, error) {
	t.Helper()
	status := map[string]string{} // graph id -> status: detail
	sum, err := apply.Run(context.Background(), f.store, f.box, f.plan, f.items, opts, func(o apply.Outcome) error {
		status[o.Item.GraphID] = strings.TrimSuffix(o.Status+": "+o.Detail, ": ")
		return nil
	})
	return sum, status, err
}

var execute = apply.Options{Execute: true, Action: "delete", Destination: "deleteditems"}
var check = apply.Options{Action: "delete", Destination: "deleteditems"}

func TestCheckModeVerifiesEveryItemAndChangesNothing(t *testing.T) {
	f := newFixture(t, 3)
	sum, status, err := f.run(t, check)
	if err != nil || sum.WouldMove != 3 || sum.Moved != 0 || len(f.box.moves) != 0 {
		t.Fatalf("sum %+v moves %v err %v", sum, f.box.moves, err)
	}
	if status["g00"] != apply.WouldMove || len(f.box.lookups) != 1 {
		t.Fatalf("status %v lookups %v", status, f.box.lookups)
	}
	if log, _ := f.store.Actions(context.Background(), f.plan); len(log) != 0 {
		t.Fatalf("check mode wrote the action log: %+v", log)
	}
}

func TestExecuteMovesOnlyMessagesThatStillMatchThePlan(t *testing.T) {
	f := newFixture(t, 6)
	f.box.msgs["g01"] = apply.Remote{InternetMessageID: "<other@example.org>", Sender: "news@campaign.example.org", ParentFolderID: "F-IN"}
	f.box.msgs["g02"] = apply.Remote{InternetMessageID: "<g02@example.org>", Sender: "friend@example.org", ParentFolderID: "F-IN"}
	f.box.msgs["g03"] = apply.Remote{InternetMessageID: "<g03@example.org>", Sender: "news@campaign.example.org", ParentFolderID: "F-ARCHIVE"}
	delete(f.box.msgs, "g04")
	f.box.lookupErr["g05"] = errors.New("graph 500")
	sum, status, err := f.run(t, execute)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"g00": "moved",
		"g01": "skipped: Message-ID changed",
		"g02": "skipped: sender changed",
		"g03": "skipped: no longer in inbox",
		"g04": "skipped: no longer in the mailbox",
		"g05": "failed: check: graph 500",
	}
	for id, w := range want {
		if status[id] != w {
			t.Errorf("%s: %q, want %q", id, status[id], w)
		}
	}
	if sum.Moved != 1 || sum.Skipped != 4 || sum.Failed != 1 {
		t.Fatalf("sum %+v", sum)
	}
	if len(f.box.moves) != 1 || strings.Join(f.box.moves[0], ",") != "g00" {
		t.Fatalf("moved %v; only the verified message may move", f.box.moves)
	}
	log, _ := f.store.Actions(context.Background(), f.plan)
	if len(log) != 6 {
		t.Fatalf("action log has %d rows, want one per item", len(log))
	}
}

func TestFailedMovesStayPendingAndMovedItemsAreNotRetried(t *testing.T) {
	f := newFixture(t, 3)
	f.box.moveErr["g01"] = errors.New("graph 503")
	sum, status, _ := f.run(t, execute)
	if sum.Moved != 2 || sum.Failed != 1 || status["g01"] != "failed: move: graph 503" {
		t.Fatalf("sum %+v status %v", sum, status)
	}
	delete(f.box.moveErr, "g01")
	f.box.lookups, f.box.moves = nil, nil
	sum, status, _ = f.run(t, execute)
	if sum.Moved != 1 || sum.AlreadyMoved != 2 || len(status) != 1 || status["g01"] != "moved" {
		t.Fatalf("second run sum %+v status %v", sum, status)
	}
	if len(f.box.lookups) != 1 || len(f.box.lookups[0]) != 1 {
		t.Fatalf("second run looked up %v; moved items must not be touched again", f.box.lookups)
	}
}

func TestAWholeBatchFailureIsLoggedAsFailedAndStopsTheRun(t *testing.T) {
	f := newFixture(t, 25)
	f.box.batchErr = errors.New("connection reset")
	sum, _, err := f.run(t, execute)
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err %v", err)
	}
	if sum.Failed != 20 || len(f.box.moves) != 1 {
		t.Fatalf("sum %+v moves %d: the run must stop at the first failed batch", sum, len(f.box.moves))
	}
	if log, _ := f.store.Actions(context.Background(), f.plan); len(log) != 20 || log[0].Status != store.ActionFailed {
		t.Fatalf("log %+v", log)
	}
}

func TestBatchesOfTwentyAndMaxItems(t *testing.T) {
	f := newFixture(t, 25)
	sum, _, err := f.run(t, check)
	if err != nil || sum.WouldMove != 25 || len(f.box.lookups) != 2 || len(f.box.lookups[0]) != 20 || len(f.box.lookups[1]) != 5 {
		t.Fatalf("sum %+v lookups %d err %v", sum, len(f.box.lookups), err)
	}
	f.box.lookups = nil
	opts := execute
	opts.MaxItems = 10
	sum, status, err := f.run(t, opts)
	if err != nil || sum.Moved != 10 || len(status) != 10 || status["g00"] != "moved" || status["g09"] != "moved" || status["g10"] != "" {
		t.Fatalf("sum %+v status %v err %v: --max-items takes the first items in plan order", sum, status, err)
	}
}

func TestOnlyTheChosenActionIsApplied(t *testing.T) {
	f := newFixture(t, 2)
	f.items[1].Action = "digest"
	sum, status, err := f.run(t, check)
	if err != nil || sum.WouldMove != 1 || status["g01"] != "" {
		t.Fatalf("sum %+v status %v err %v", sum, status, err)
	}
}

func TestPlansWithoutAPlannedFolderAreRefused(t *testing.T) {
	f := newFixture(t, 1)
	f.items[0].PlannedFolder = ""
	_, _, err := f.run(t, check)
	if !errors.Is(err, apply.ErrStalePlan) || len(f.box.lookups) != 0 {
		t.Fatalf("err %v lookups %v", err, f.box.lookups)
	}
}

// failingLog accepts the read but cannot write the action log.
type failingLog struct{ *store.Store }

func (failingLog) LogActions(context.Context, []store.Action) error { return errors.New("disk full") }

func TestAnActionLogFailureStillReportsWhatMovedOnTheServer(t *testing.T) {
	f := newFixture(t, 3)
	var emitted []apply.Outcome
	sum, err := apply.Run(context.Background(), failingLog{f.store}, f.box, f.plan, f.items, execute, func(o apply.Outcome) error {
		emitted = append(emitted, o)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err %v", err)
	}
	if sum.Moved != 3 || len(emitted) != 3 || emitted[0].Status != store.ActionMoved {
		t.Fatalf("sum %+v emitted %+v: moves that happened must be reported even when the log cannot be written", sum, emitted)
	}
	if len(f.box.moves) != 1 {
		t.Fatalf("moves %v: the run must stop after the failed log write", f.box.moves)
	}
}
