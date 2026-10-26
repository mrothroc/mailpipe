package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/store"
)

// applied is planned with plan 1 executed: g1 (info@campaign.example.org) is
// in Deleted Items on the server, and the owner has one rule of their own.
func applied(t *testing.T) *env {
	t.Helper()
	e := planned(t)
	if code, _, errOut := e.run("apply", "1", "--execute"); code != 0 {
		t.Fatalf("apply: %d %q", code, errOut)
	}
	e.graph.rules = []map[string]any{{"id": "U1", "displayName": "Family", "sequence": 1, "isEnabled": true,
		"conditions": map[string]any{"fromAddresses": []any{map[string]any{"emailAddress": map[string]any{"address": "info@campaign.example.org"}}}},
		"actions":    map[string]any{"moveToFolder": "F-FAM"}}}
	return e
}

const rulesHeader = "rule\top\tsenders\tadded\tremoved\n"

func TestRulesSendersBlocksASenderWithNothingKept(t *testing.T) {
	e := applied(t)
	code, out, errOut := e.run("rules", "senders")
	if code != 0 || out != "sender\tmoved\tkept\texpired\tstatus\treason\ninfo@campaign.example.org\t1\t0\t0\tblock\t\n" {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "1 of 1 senders blocked. Try: mailpipe rules diff\n") {
		t.Fatalf("hint %q", errOut)
	}
}

func TestRulesApplyChecksThenWritesThenDiffIsClean(t *testing.T) {
	e := applied(t)
	code, out, errOut := e.run("rules", "diff")
	if code != 0 || out != rulesHeader+"mailpipe blocked #1\tcreate\t1\t1\t0\n" || !strings.Contains(errOut, "Try: mailpipe rules apply --execute") {
		t.Fatalf("diff: code %d out %q err %q", code, out, errOut)
	}
	code, out, errOut = e.run("rules", "apply")
	if code != 0 || out != rulesHeader+"mailpipe blocked #1\tcreate\t1\t1\t0\n" || len(e.graph.rules) != 1 ||
		!strings.Contains(errOut, "check only, nothing written: 1 to change. Try: mailpipe rules apply --execute") {
		t.Fatalf("check: code %d out %q err %q rules %v", code, out, errOut, e.graph.rules)
	}
	code, _, errOut = e.run("rules", "apply", "--execute")
	if code != 0 || len(e.graph.rules) != 2 || !strings.Contains(errOut, "made 1 change; the server now matches the managed rules. Try: mailpipe rules list") {
		t.Fatalf("execute: code %d err %q rules %v", code, errOut, e.graph.rules)
	}
	code, out, errOut = e.run("rules", "list")
	if code != 0 || out != "name\tsequence\tenabled\tkind\tsenders\tmanaged_shape\nFamily\t1\ttrue\t\t1\tfalse\nmailpipe blocked #1\t2\ttrue\tmanaged\t1\ttrue\n" {
		t.Fatalf("list: code %d out %q err %q", code, out, errOut)
	}
	code, out, errOut = e.run("rules", "diff")
	if code != 0 || out != rulesHeader+"mailpipe blocked #1\tsame\t1\t0\t0\n" || !strings.Contains(errOut, "nothing to change") {
		t.Fatalf("diff after: code %d out %q err %q", code, out, errOut)
	}
	if e.graph.rules[0]["displayName"] != "Family" || e.graph.rules[0]["actions"].(map[string]any)["moveToFolder"] != "F-FAM" {
		t.Fatalf("the owner's own rule was touched: %v", e.graph.rules[0])
	}
}

func TestRulesUnblockASenderWhoseMailIsKeptAgain(t *testing.T) {
	e := applied(t)
	e.run("rules", "apply", "--execute")
	e.graph.server["g9"] = serverMsg{"archive", "<g9@example.org>", "INFO@campaign.example.org"}
	code, out, _ := e.run("rules", "senders")
	if code != 0 || out != "sender\tmoved\tkept\texpired\tstatus\treason\ninfo@campaign.example.org\t1\t1\t0\texclude\t1 kept outside Deleted Items\n" {
		t.Fatalf("senders: code %d out %q", code, out)
	}
	code, out, _ = e.run("rules", "diff")
	if code != 0 || out != rulesHeader+"mailpipe blocked #1\tdelete\t1\t0\t1\n" {
		t.Fatalf("diff: code %d out %q", code, out)
	}
	if code, _, _ := e.run("rules", "apply", "--execute"); code != 0 || len(e.graph.rules) != 1 {
		t.Fatalf("execute: code %d rules %v", code, e.graph.rules)
	}
}

func TestRulesNeverBlockKeepSendersOrContacts(t *testing.T) {
	e := applied(t)
	e.stagesConfigWithPolicy("http://unused", `{"keep_senders":["INFO@campaign.example.org"]}`)
	code, out, errOut := e.run("rules", "senders")
	if code != 0 || !strings.HasSuffix(out, "\t1\t0\t0\texclude\tkeep_sender\n") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	if code, _, errOut := e.run("rules", "diff"); code != agentio.ExitNoData || !strings.Contains(errOut, "Try: mailpipe rules senders") {
		t.Fatalf("diff with nothing to block: code %d err %q", code, errOut)
	}
}

func TestRulesNeedAnAppliedPlan(t *testing.T) {
	e := planned(t)
	if code, _, errOut := e.run("rules", "senders"); code != agentio.ExitNoData || !strings.Contains(errOut, "Try: mailpipe apply <plan-id> --execute") {
		t.Fatalf("unapplied: code %d err %q", code, errOut)
	}
	if code, _, _ := e.run("rules", "apply", "1"); code != agentio.ExitUsage {
		t.Fatalf("a plan id is no longer taken: code %d", code)
	}
}

// The blocklist replaces the per-plan rules mailpipe used to write: each is
// deleted once the blocklist that covers its senders exists.
func TestRulesApplyRetiresThePerPlanRules(t *testing.T) {
	e := applied(t)
	e.graph.rules = append(e.graph.rules, map[string]any{"id": "P4", "displayName": "mailpipe plan 4 #1", "sequence": 2, "isEnabled": true,
		"conditions": map[string]any{"fromAddresses": []any{map[string]any{"emailAddress": map[string]any{"address": "info@campaign.example.org"}}}},
		"actions":    map[string]any{"delete": true, "stopProcessingRules": true}})
	code, out, errOut := e.run("rules", "diff")
	if code != 0 || out != rulesHeader+"mailpipe blocked #1\tcreate\t1\t1\t0\nmailpipe plan 4 #1\tdelete\t1\t0\t1\n" {
		t.Fatalf("diff: code %d out %q err %q", code, out, errOut)
	}
	if code, _, errOut := e.run("rules", "apply", "--execute"); code != 0 {
		t.Fatalf("apply: code %d err %q", code, errOut)
	}
	var names []string
	for _, r := range e.graph.rules {
		names = append(names, r["displayName"].(string))
	}
	if strings.Join(names, ",") != "Family,mailpipe blocked #1" {
		t.Fatalf("rules on the server: %v", names)
	}
}

func TestRulesStopWhenASenderSearchFails(t *testing.T) {
	e := applied(t)
	e.graph.searchErr = 503
	code, out, errOut := e.run("rules", "apply", "--execute")
	if code != agentio.ExitRuntime || out != "" || len(e.graph.rules) != 1 || !strings.Contains(errOut, "info@campaign.example.org") {
		t.Fatalf("code %d out %q err %q rules %v", code, out, errOut, e.graph.rules)
	}
}

func TestRulesApplyReportsAFailedWriteWithARetry(t *testing.T) {
	e := applied(t)
	e.graph.ruleFail = "POST"
	code, _, errOut := e.run("rules", "apply", "--execute")
	if code != agentio.ExitRuntime || !strings.Contains(errOut, "create mailpipe blocked #1") ||
		!strings.Contains(errOut, "Try: mailpipe rules apply --execute") {
		t.Fatalf("code %d err %q", code, errOut)
	}
}

func TestRulesNeverSearchForABlankSender(t *testing.T) {
	e := newEnv(t)
	e.stagesConfig("http://unused")
	e.run("sync")
	// Imports refuse blank senders, so the plan is written to the store directly.
	s, err := store.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.Upsert(ctx, "inbox", store.Record{GraphID: "g3", Sender: "  "}); err != nil {
		t.Fatal(err)
	}
	m, _ := s.Messages(ctx, store.Filter{Folder: "inbox"})
	var n int64
	for _, x := range m {
		if x.GraphID == "g3" {
			n = x.N
		}
	}
	plan, err := s.CreatePlan(ctx, "folder=inbox", []store.PlanItem{{N: n, Action: "delete"}})
	if err == nil {
		err = s.LogActions(ctx, []store.Action{{PlanID: plan, N: n, GraphID: "g3", Action: "delete", Destination: "deleteditems", Status: store.ActionMoved}})
	}
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	e.graph.searchErr = 400 // any search at all fails
	code, out, errOut := e.run("rules", "senders")
	if code != 0 || out != "sender\tmoved\tkept\texpired\tstatus\treason\n\t1\t0\t0\texclude\tno sender address\n" {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

func TestRulesNeverBlockANeverBlockSender(t *testing.T) {
	e := applied(t)
	e.stagesConfigWithPolicy("http://unused", `{"never_block":["INFO@campaign.example.org"]}`)
	e.graph.searchErr = 500 // a never_block sender is never searched for
	code, out, errOut := e.run("rules", "senders")
	if code != 0 || !strings.HasSuffix(out, "\t1\t0\t0\texclude\tnever_block\n") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

// A failed write must still be reported when stdout fails too.
func TestRulesApplyKeepsAFailedWriteWhenStdoutAlsoFails(t *testing.T) {
	e := applied(t)
	e.graph.ruleFail = "POST"
	e.brokenStdout = true
	code, _, errOut := e.run("rules", "apply", "--execute")
	if code != agentio.ExitRuntime || !strings.Contains(errOut, "create mailpipe blocked #1") || !strings.Contains(errOut, "broken pipe") {
		t.Fatalf("code %d err %q", code, errOut)
	}
}

// A block sender joins the blocklist even when no plan has moved their mail
// (say every message expired under a no_block rule), and even with mail kept.
func TestABlockSenderIsBlockedWithoutAMove(t *testing.T) {
	e := applied(t)
	e.stagesConfigWithPolicy("http://unused", `{"block_senders":["Deals@Shop.example.net"]}`)
	e.graph.server["g8"] = serverMsg{"archive", "<g8@example.org>", "deals@shop.example.net"}
	code, out, errOut := e.run("rules", "senders")
	if code != 0 || !strings.Contains(out, "deals@shop.example.net\t0\t1\t0\tblock\t\n") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

// Junk senders get a managed rule that moves their mail to Junk, and are
// never put on the blocklist.
func TestJunkSendersGetAJunkRuleAndStayOffTheBlocklist(t *testing.T) {
	e := applied(t)
	e.stagesConfigWithPolicy("http://unused", `{"junk_senders":["@campaign.example.org","@radio.example.net"]}`)
	code, out, errOut := e.run("rules", "senders")
	if code != 0 || !strings.Contains(out, "info@campaign.example.org\t1\t0\t0\texclude\tjunk_sender\n") {
		t.Fatalf("senders: code %d out %q err %q", code, out, errOut)
	}
	code, out, errOut = e.run("rules", "diff")
	if code != 0 || out != rulesHeader+"mailpipe junk #1\tcreate\t2\t2\t0\n" {
		t.Fatalf("diff: code %d out %q err %q", code, out, errOut)
	}
	if code, _, errOut := e.run("rules", "apply", "--execute"); code != 0 {
		t.Fatalf("apply: code %d err %q", code, errOut)
	}
	last := e.graph.rules[len(e.graph.rules)-1]
	if last["displayName"] != "mailpipe junk #1" || last["actions"].(map[string]any)["moveToFolder"] != "F-junkemail" {
		t.Fatalf("junk rule on the server: %v", last)
	}
	if code, out, _ := e.run("rules", "diff"); code != 0 || out != rulesHeader+"mailpipe junk #1\tsame\t2\t0\t0\n" {
		t.Fatalf("diff after: code %d out %q", code, out)
	}
}
