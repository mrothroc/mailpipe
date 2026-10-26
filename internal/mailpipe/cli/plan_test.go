package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrothroc/agentio"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "verdicts.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const imported = `{"internet_message_id":"<g1@example.org>","sender":"info@campaign.example.org","stage":"imported-classifier","labels":["flagged"],"scores":{"political_appeal":0.99},"action":"delete","provenance":"jev:typesafe/jev-1.14 2026-10-21","at":"2026-10-21T12:00:00Z"}
{"internet_message_id":"<g2@example.org>","sender":"someone-else@example.net","stage":"imported-classifier","labels":["flagged_high"],"action":"delete","provenance":"p","at":"2026-10-21T12:00:00Z"}
{"internet_message_id":"<absent@example.org>","sender":"x@example.org","stage":"imported-classifier","action":"delete","provenance":"p","at":"2026-10-21T12:00:00Z"}
`

func TestImportVerdictsMatchesOnMessageIDAndSender(t *testing.T) {
	e := newEnv(t)
	e.stagesConfig("http://unused")
	e.run("sync")
	file := writeFile(t, imported)
	code, out, errOut := e.run("import-verdicts", file)
	want := "records\tadded\talready_present\tunmatched\tsender_mismatch\n3\t1\t0\t1\t1\n"
	if code != 0 || out != want {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	_, out, _ = e.run("import-verdicts", file)
	if !strings.HasSuffix(out, "3\t0\t1\t1\t1\n") {
		t.Fatalf("re-import duplicated verdicts: %q", out)
	}
	_, ls, _ := e.run("ls", "--from", "campaign")
	_, vs, _ := e.run("verdicts", firstN(t, ls))
	if !strings.Contains(vs, "imported-classifier\t2026-10-21T12:00:00Z\tflagged\tpolitical_appeal=0.99\tdelete\t") {
		t.Fatalf("verdicts %q", vs)
	}
}

func TestImportVerdictsIsAllOrNothing(t *testing.T) {
	e := newEnv(t)
	e.run("sync")
	file := writeFile(t, imported+`{"internet_message_id":"<g1@example.org>","sender":"info@campaign.example.org","stage":"imported-classifier","action":"explode","provenance":"p","at":"2026-10-21T12:00:00Z"}`+"\n")
	code, out, errOut := e.run("import-verdicts", file)
	if code != agentio.ExitUsage || out != "" || !strings.Contains(errOut, "line 4") || !strings.Contains(errOut, "explode") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	_, ls, _ := e.run("ls", "--from", "campaign")
	if _, vs, _ := e.run("verdicts", firstN(t, ls)); strings.Count(vs, "\n") != 1 {
		t.Fatalf("partial import happened: %q", vs)
	}
}

func TestPlanCreateShowAndListHonourPolicy(t *testing.T) {
	e := newEnv(t)
	e.stagesConfigWithPolicy("http://unused", `{"keep_senders": [], "rules": [{"stage": "newsletter", "label": "newsletter", "action": "digest"}]}`)
	e.run("sync")
	e.run("import-verdicts", writeFile(t, imported+
		`{"internet_message_id":"<g2@example.org>","sender":"friend@example.org","stage":"imported-classifier","action":"delete","provenance":"p","at":"2026-10-21T12:00:00Z"}`+"\n"))
	e.run("run", "known", "--folder", "inbox") // friend@example.org is a Sent Items contact: final keep
	code, out, errOut := e.run("plan", "create", "--folder", "inbox")
	if code != 0 || !strings.HasPrefix(out, "n\tfolder\tsender\tsubject\taction\treasons\n") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	if !strings.Contains(out, "inbox\tinfo@campaign.example.org\tChip in\tdelete\timported-classifier:action=delete\n") || strings.Contains(out, "friend@example.org") {
		t.Fatalf("plan rows %q", out)
	}
	if !strings.Contains(errOut, "plan 1: 1 delete") || !strings.Contains(errOut, "1 kept") || !strings.Contains(errOut, "Try: mailpipe plan show 1") {
		t.Fatalf("hint %q", errOut)
	}
	_, shown, _ := e.run("plan", "show", "1")
	if shown != out {
		t.Fatalf("plan show differs from what was created:\n%q\n%q", shown, out)
	}
	_, list, _ := e.run("plan", "list")
	if !strings.HasPrefix(list, "id\tcreated_at\tscope\tdelete\tquarantine\tdigest\n1\t") || !strings.Contains(list, "\tfolder=inbox\t1\t0\t0\n") {
		t.Fatalf("plan list %q", list)
	}
	if code, _, errOut := e.run("plan", "show", "9"); code != agentio.ExitRuntime || !strings.Contains(errOut, "no plan 9. Try: mailpipe plan list") {
		t.Fatalf("missing plan: %d %q", code, errOut)
	}
}

func TestRunSenderCacheStageHitsAndPlanActsOnIt(t *testing.T) {
	e := newEnv(t)
	e.stagesConfigWithPolicy("http://unused", `{"rules": [{"stage": "political-cached", "label": "flagged", "action": "delete"}]}`)
	inbox := "/me/mailFolders/inbox/messages/delta"
	e.graph.pages[inbox][0].Value = append(e.graph.pages[inbox][0].Value,
		msg("g4", "info@campaign.example.org", "Chip in again", "2026-10-22T10:00:00Z"),
		msg("g5", "info@campaign.example.org", "Last chance", "2026-10-23T10:00:00Z"))
	e.graph.totals["inbox"] = 4
	e.run("sync")
	var lines []string
	for _, id := range []string{"g4", "g5"} {
		lines = append(lines, `{"internet_message_id":"<`+id+`@example.org>","sender":"info@campaign.example.org","stage":"imported-classifier","labels":["flagged"],"provenance":"p","at":"2026-10-21T12:00:00Z"}`)
	}
	e.run("import-verdicts", writeFile(t, strings.Join(lines, "\n")+"\n"))
	code, out, errOut := e.run("run", "political-cached", "--folder", "inbox", "--from", "campaign")
	if code != 0 || !strings.Contains(out, "\tChip in\tflagged\t") {
		t.Fatalf("no cache hit for g1 (two other verdicts from its sender): code %d out %q err %q", code, out, errOut)
	}
	_, plan, _ := e.run("plan", "create", "--folder", "inbox")
	if !strings.Contains(plan, "\tChip in\tdelete\tpolitical-cached:flagged\n") {
		t.Fatalf("plan did not act on the cached verdict: %q", plan)
	}
}

func TestImportAcceptsMailpipesOwnFinalFlag(t *testing.T) {
	e := newEnv(t)
	e.run("sync")
	file := writeFile(t, `{"internet_message_id":"<g2@example.org>","sender":"friend@example.org","stage":"known","labels":["known_contact"],"action":"keep","final":true,"provenance":"allowlist","at":"2026-10-21T12:00:00Z"}`+"\n")
	if code, _, errOut := e.run("import-verdicts", file); code != 0 {
		t.Fatalf("code %d err %q", code, errOut)
	}
	_, ls, _ := e.run("ls", "--from", "friend")
	_, vs, _ := e.run("verdicts", firstN(t, ls), "--json")
	if !strings.Contains(vs, `"final":true`) {
		t.Fatalf("final flag lost: %q", vs)
	}
}
