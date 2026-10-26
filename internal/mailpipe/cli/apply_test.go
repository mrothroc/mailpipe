package cli_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/mrothroc/agentio"
)

// planned syncs, imports a delete verdict for g1 (Chip in) and stores plan 1.
func planned(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.stagesConfig("http://unused")
	e.run("sync")
	e.run("import-verdicts", writeFile(t, imported))
	if code, _, errOut := e.run("plan", "create", "--folder", "inbox"); code != 0 || !strings.Contains(errOut, "plan 1: 1 delete") {
		t.Fatalf("plan create: code %d err %q", code, errOut)
	}
	return e
}

func TestApplyChecksByDefaultAndMovesNothing(t *testing.T) {
	e := planned(t)
	code, out, errOut := e.run("apply", "1")
	if code != 0 || out != "n\tsender\tsubject\tstatus\tdetail\n1\tinfo@campaign.example.org\tChip in\twould_move\t\n" {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	if len(e.graph.moved) != 0 {
		t.Fatalf("check mode moved %v", e.graph.moved)
	}
	if !strings.Contains(errOut, "check only, nothing moved: 1 would move, 0 skipped, 0 failed") ||
		!strings.Contains(errOut, "Try: mailpipe apply 1 --execute") {
		t.Fatalf("hint %q", errOut)
	}
	if code, out, _ := e.run("actions", "1"); code != agentio.ExitNoData || out != "" {
		t.Fatalf("check mode wrote the action log: code %d out %q", code, out)
	}
}

func TestApplyExecuteMovesLogsAndReportsGraphTotals(t *testing.T) {
	e := planned(t)
	code, out, errOut := e.run("apply", "1", "--execute")
	if code != 0 || !strings.Contains(out, "\tChip in\tmoved\t\n") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	if strings.Join(e.graph.moved, ",") != "g1" || e.graph.server["g1"].folder != "deleteditems" {
		t.Fatalf("moved %v server %+v", e.graph.moved, e.graph.server)
	}
	if !strings.Contains(errOut, "moved 1 to deleteditems, skipped 0, failed 0") ||
		!strings.Contains(errOut, "Graph totals: inbox 2 -> 1, deleteditems 0 -> 1") || !strings.Contains(errOut, "Try: mailpipe sync") {
		t.Fatalf("hint %q", errOut)
	}
	code, out, _ = e.run("actions", "1")
	if code != 0 || !strings.HasPrefix(out, "n\tsender\tsubject\taction\tdestination\tstatus\tdetail\tat\n1\tinfo@campaign.example.org\tChip in\tdelete\tdeleteditems\tmoved\t\t") {
		t.Fatalf("actions code %d out %q", code, out)
	}
	code, out, errOut = e.run("apply", "1", "--execute")
	if code != agentio.ExitNoData || out != "" || !strings.Contains(errOut, "nothing left to apply in plan 1 (1 already moved)") {
		t.Fatalf("second run code %d out %q err %q", code, out, errOut)
	}
}

func TestApplySkipsMessagesThatChangedOnTheServer(t *testing.T) {
	e := planned(t)
	e.graph.server["g1"] = serverMsg{"archive", "<g1@example.org>", "info@campaign.example.org"}
	code, out, errOut := e.run("apply", "1", "--execute")
	if code != 0 || !strings.Contains(out, "\tskipped\tno longer in inbox\n") || len(e.graph.moved) != 0 {
		t.Fatalf("code %d out %q err %q moved %v", code, out, errOut, e.graph.moved)
	}
}

func TestApplyFailedMovesAreAnErrorWithARetryHint(t *testing.T) {
	e := planned(t)
	e.graph.moveFail = 503
	code, out, errOut := e.run("apply", "1", "--execute")
	if code != agentio.ExitRuntime || !strings.Contains(out, "\tfailed\tmove: graph 503") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "1 failed; failed items stay pending") || !strings.Contains(errOut, "Try: mailpipe apply 1 --execute") {
		t.Fatalf("err %q", errOut)
	}
	e.graph.moveFail = 0
	if code, out, _ := e.run("apply", "1", "--execute"); code != 0 || !strings.Contains(out, "\tmoved\t") {
		t.Fatalf("retry code %d out %q", code, out)
	}
}

func TestApplyRefusesPlansWithoutPlannedFolders(t *testing.T) {
	e := planned(t)
	db, err := sql.Open("sqlite", e.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("update plan_items set planned_folder=''"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	code, _, errOut := e.run("apply", "1")
	if code != agentio.ExitStale || !strings.Contains(errOut, "Try: mailpipe plan create") {
		t.Fatalf("code %d err %q", code, errOut)
	}
}

func TestApplyAndActionsArgumentErrors(t *testing.T) {
	e := planned(t)
	for _, args := range [][]string{{"apply", "x"}, {"apply", "0"}, {"apply", "1", "--max-items", "-1"}, {"actions", "x"}} {
		if code, _, errOut := e.run(args...); code != agentio.ExitUsage || !strings.Contains(errOut, "Try:") {
			t.Errorf("%v: code %d err %q", args, code, errOut)
		}
	}
	if code, _, errOut := e.run("apply", "9"); code != agentio.ExitRuntime || !strings.Contains(errOut, "no plan 9") || !strings.Contains(errOut, "Try: mailpipe plan list") {
		t.Fatalf("unknown plan: code %d err %q", code, errOut)
	}
}

func TestApplyCheckThatFailsBeforeAnyItemReportsOnlyTheErrorAndNeverSuggestsExecute(t *testing.T) {
	e := planned(t)
	e.graph.batchErr = errors.New("connection reset")
	code, out, errOut := e.run("apply", "1")
	if code != agentio.ExitRuntime || out != "" || !strings.Contains(errOut, "connection reset") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	if strings.Contains(errOut, "--execute") || strings.Contains(errOut, "check only") || !strings.Contains(errOut, "Try: mailpipe apply 1\n") {
		t.Fatalf("a failed check must suggest re-running the check, not executing, and print no summary: %q", errOut)
	}
}

func TestApplyCheckWithFailedItemsIsAnErrorAndNeverSuggestsExecute(t *testing.T) {
	e := planned(t)
	e.graph.lookupFail = 500
	code, out, errOut := e.run("apply", "1")
	if code != agentio.ExitRuntime || !strings.Contains(out, "\tfailed\tcheck: graph 500") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	if strings.Contains(errOut, "--execute") || !strings.Contains(errOut, "Try: mailpipe apply 1\n") {
		t.Fatalf("a check with failures must not suggest executing: %q", errOut)
	}
}

func TestApplyMaxItemsBoundsTheRun(t *testing.T) {
	e := newEnv(t)
	e.stagesConfig("http://unused")
	e.run("sync")
	e.run("import-verdicts", writeFile(t, imported+
		`{"internet_message_id":"<g2@example.org>","sender":"friend@example.org","stage":"imported-classifier","action":"delete","provenance":"p","at":"2026-10-21T12:00:00Z"}`+"\n"))
	if _, _, errOut := e.run("plan", "create", "--folder", "inbox"); !strings.Contains(errOut, "plan 1: 2 delete") {
		t.Fatalf("plan create: %q", errOut)
	}
	code, out, errOut := e.run("apply", "1", "--execute", "--max-items", "1")
	if code != 0 || strings.Count(out, "\tmoved\t") != 1 || len(e.graph.moved) != 1 {
		t.Fatalf("code %d out %q err %q moved %v", code, out, errOut, e.graph.moved)
	}
	if _, out, _ = e.run("apply", "1", "--execute"); strings.Count(out, "\tmoved\t") != 1 || len(e.graph.moved) != 2 {
		t.Fatalf("second run out %q moved %v", out, e.graph.moved)
	}
}

func TestActionsOnAnUnknownPlanSaysSo(t *testing.T) {
	e := planned(t)
	code, _, errOut := e.run("actions", "9")
	if code != agentio.ExitRuntime || !strings.Contains(errOut, "no plan 9") || !strings.Contains(errOut, "Try: mailpipe plan list") {
		t.Fatalf("code %d err %q", code, errOut)
	}
}

// Listings show a move at once: the local copy follows the action log, so
// nobody has to sync before trusting ls or digest.
func TestApplyExecuteMovesTheLocalCopyToo(t *testing.T) {
	e := planned(t)
	_, before, _ := e.run("digest")
	if !strings.Contains(before, "\tChip in\t") {
		t.Fatalf("before: %q", before)
	}
	if code, _, errOut := e.run("apply", "1", "--execute"); code != 0 {
		t.Fatalf("apply: code %d err %q", code, errOut)
	}
	if _, inbox, _ := e.run("digest"); strings.Contains(inbox, "\tChip in\t") {
		t.Fatalf("the Inbox digest still lists the moved message: %q", inbox)
	}
	if _, deleted, _ := e.run("ls", "--folder", "deleteditems"); !strings.Contains(deleted, "\tdeleteditems\tinfo@campaign.example.org\tChip in\n") {
		t.Fatalf("deleteditems: %q", deleted)
	}
}
