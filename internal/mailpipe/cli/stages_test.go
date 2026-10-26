package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/graph"
)

func (e *env) stagesConfig(jevURL string) { e.stagesConfigWithPolicy(jevURL, `{}`) }

func (e *env) stagesConfigWithPolicy(jevURL, policyJSON string) {
	e.t.Helper()
	newsletter, err := filepath.Abs("../../../examples/stages/newsletter.sh")
	if err != nil {
		e.t.Fatal(err)
	}
	cfg := map[string]any{
		"self_addresses": []string{"alex@example.com"},
		"self_domains":   []string{"example.com"},
		"honeypots":      []string{"trap@example.com"},
		"openrouter":     map[string]string{"keychain_service": "mailpipe-openrouter", "keychain_account": "default"},
		"stages": []map[string]any{
			{"name": "known", "type": "allowlist"},
			{"name": "political", "type": "jev", "url": jevURL, "question": map[string]string{
				"key": "political_appeal", "instructions": "Is this a political appeal?",
				"criteria_true": "asks to act", "criteria_false": "anything else"}},
			{"name": "newsletter", "type": "exec", "command": []string{newsletter}, "timeout_seconds": 10},
			{"name": "political-cached", "type": "sender_cache", "source": "imported-classifier", "label": "flagged", "min_verdicts": 1},
		},
		"policy": json.RawMessage(policyJSON),
	}
	data, _ := json.Marshal(cfg)
	path := filepath.Join(e.t.TempDir(), "stages.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		e.t.Fatal(err)
	}
	e.t.Setenv("MAILPIPE_STAGES", path)
}

func jevAPI(t *testing.T, cost float64) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct{ State string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		noul := 0.05
		if strings.Contains(req.State, "Chip in") {
			noul = 0.97
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"political_appeal": map[string]any{"noul": noul}},
			"usage": map[string]any{"cost": cost}})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestStagesListsTheConfiguredStages(t *testing.T) {
	e := newEnv(t)
	e.stagesConfig("http://unused")
	code, out, errOut := e.run("stages")
	if code != 0 || !strings.HasPrefix(out, "name\ttype\tneeds_body\n") ||
		!strings.Contains(out, "known\tallowlist\tfalse\n") || !strings.Contains(out, "political\tjev\ttrue\n") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

func TestRunAllowlistUsesSentItemsAndSkipsJudgedMessages(t *testing.T) {
	e := newEnv(t)
	e.stagesConfig("http://unused")
	e.run("sync")
	code, out, errOut := e.run("run", "known", "--folder", "inbox")
	if code != 0 {
		t.Fatalf("code %d err %q", code, errOut)
	}
	if !strings.Contains(out, "friend@example.org\tDinner Tuesday?\tknown_contact\t\tkeep\t") ||
		!strings.Contains(out, "info@campaign.example.org\tChip in\t\t\t\t") {
		t.Fatalf("out %q", out)
	}
	if !strings.Contains(errOut, "evaluated 2, failed 0, cost $0.0000") {
		t.Fatalf("summary missing: %q", errOut)
	}
	_, out, errOut = e.run("run", "known", "--folder", "inbox")
	if strings.Count(out, "\n") != 1 || !strings.Contains(errOut, "no messages pending for stage known") {
		t.Fatalf("rerun evaluated again: out %q err %q", out, errOut)
	}
	_, out, _ = e.run("run", "known", "--folder", "inbox", "--rerun")
	if strings.Count(out, "\n") != 3 {
		t.Fatalf("--rerun did not re-evaluate: %q", out)
	}
}

func TestRunJevRecordsScoresCostAndVerdicts(t *testing.T) {
	e := newEnv(t)
	srv, calls := jevAPI(t, 0.0002)
	e.stagesConfig(srv.URL)
	e.secrets = map[string]string{"mailpipe-openrouter/default": "sk-or-test-key"}
	e.run("sync")
	code, out, errOut := e.run("run", "political", "--folder", "inbox")
	if code != 0 || calls.Load() != 2 {
		t.Fatalf("code %d calls %d err %q", code, calls.Load(), errOut)
	}
	if !strings.Contains(out, "Chip in\tpolitical_appeal\tpolitical_appeal=0.97\t\t0.0002\t") || !strings.Contains(errOut, "cost $0.0004") {
		t.Fatalf("out %q err %q", out, errOut)
	}
	if strings.Contains(out+errOut, "sk-or-test-key") {
		t.Fatal("API key leaked")
	}
	_, ls, _ := e.run("ls", "--from", "campaign")
	code, out, _ = e.run("verdicts", firstN(t, ls))
	if code != 0 || !strings.HasPrefix(out, "stage\tat\tlabels\tscores\taction\treason\tcost_usd\tprovenance\terror\n") ||
		!strings.Contains(out, "political\t") || !strings.Contains(out, "jev:typesafe/jev-1.14") {
		t.Fatalf("verdicts out %q", out)
	}
}

func TestRunStopsAtTheBudgetWithAHint(t *testing.T) {
	e := newEnv(t)
	srv, calls := jevAPI(t, 0.6)
	e.stagesConfig(srv.URL)
	e.secrets = map[string]string{"mailpipe-openrouter/default": "sk-or-test-key"}
	e.run("sync")
	code, out, errOut := e.run("run", "political", "--folder", "inbox", "--max-cost-usd", "0.5", "--workers", "1")
	if code != agentio.ExitRuntime || calls.Load() != 1 || strings.Count(out, "\n") != 2 {
		t.Fatalf("code %d calls %d out %q", code, calls.Load(), out)
	}
	if !strings.Contains(errOut, "budget exhausted") || !strings.Contains(errOut, "Try: mailpipe run political --max-cost-usd") {
		t.Fatalf("err %q", errOut)
	}
}

func TestRunExecStage(t *testing.T) {
	e := newEnv(t)
	e.stagesConfig("http://unused")
	e.graph.pages["/me/mailFolders/inbox/messages/delta"][0].Value = append(e.graph.pages["/me/mailFolders/inbox/messages/delta"][0].Value,
		msg("g3", "editor@news.example.net", "Weekly Newsletter", "2026-10-22T07:00:00Z"))
	e.graph.totals["inbox"] = 3
	e.run("sync")
	code, out, errOut := e.run("run", "newsletter", "--folder", "inbox")
	if code != 0 || !strings.Contains(out, "Weekly Newsletter\tnewsletter\t") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

func TestRunErrorsNameTheFix(t *testing.T) {
	e := newEnv(t)
	e.run("sync")
	t.Setenv("MAILPIPE_STAGES", filepath.Join(t.TempDir(), "absent.json"))
	if code, _, errOut := e.run("run", "known"); code != agentio.ExitStale || !strings.Contains(errOut, "stages.example.json") {
		t.Fatalf("missing config: code %d err %q", code, errOut)
	}
	e.stagesConfig("http://unused")
	if code, _, errOut := e.run("run", "nope"); code != agentio.ExitUsage || !strings.Contains(errOut, "unknown stage \"nope\"") || !strings.Contains(errOut, "Try: mailpipe stages") {
		t.Fatalf("unknown stage: code %d err %q", code, errOut)
	}
	if code, _, errOut := e.run("run", "political"); code != agentio.ExitStale || !strings.Contains(errOut, "security add-generic-password -s mailpipe-openrouter -a default") {
		t.Fatalf("missing key: code %d err %q", code, errOut)
	}
	if code, _, errOut := e.run("run"); code != agentio.ExitUsage || !strings.Contains(errOut, "accepts 1 arg") {
		t.Fatalf("no stage: code %d err %q", code, errOut)
	}
}

func statusAPI(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	t.Cleanup(srv.Close)
	return srv
}

// Each fatal failure gets the hint for its actual cause.
func TestRunFailureHintsMatchTheirCause(t *testing.T) {
	cases := map[string]struct {
		status   int
		graphErr bool
		code     int
		want     string
		notWant  string
	}{
		"graph forbidden": {status: 200, graphErr: true, code: agentio.ExitStale, want: "Try: mailpipe auth status", notWant: "security add-generic-password"},
		"decisions 401":   {status: 401, code: agentio.ExitStale, want: "Try: security add-generic-password -U -s mailpipe-openrouter -a default -w"},
		"decisions 402":   {status: 402, code: agentio.ExitStale, want: "credits", notWant: "security add-generic-password"},
		"decisions 404":   {status: 404, code: agentio.ExitRuntime, want: "Try: mailpipe stages", notWant: "security add-generic-password"},
	}
	for name, c := range cases {
		e := newEnv(t)
		e.stagesConfig(statusAPI(t, c.status).URL)
		e.secrets = map[string]string{"mailpipe-openrouter/default": "sk-or-test-key"}
		e.run("sync")
		if c.graphErr {
			e.graph.getErr = &graph.APIError{Status: 403, Code: "ErrorAccessDenied", Message: "Access is denied."}
		}
		code, _, errOut := e.run("run", "political", "--folder", "inbox", "--workers", "1")
		if code != c.code || !strings.Contains(errOut, c.want) || (c.notWant != "" && strings.Contains(errOut, c.notWant)) {
			t.Errorf("%s: code %d err %q", name, code, errOut)
		}
	}
}

func TestRunSinceJudgesOnlyRecentMail(t *testing.T) {
	cases := map[string]struct {
		since string
		want  []string // senders judged
	}{
		"timestamp, inclusive": {"2026-10-21T09:30:00Z", []string{"friend@example.org"}},
		"date before both":     {"2026-01-01", []string{"friend@example.org", "info@campaign.example.org"}},
		"date after both":      {"2026-12-01", nil},
		"days":                 {"100000d", []string{"friend@example.org", "info@campaign.example.org"}},
		"duration":             {"1h", nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.stagesConfig("http://unused")
			e.run("sync")
			code, out, errOut := e.run("run", "known", "--since", c.since)
			if code != 0 {
				t.Fatalf("code %d err %q", code, errOut)
			}
			var judged []string
			for _, line := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
				judged = append(judged, strings.Split(line, "\t")[1])
			}
			if strings.Join(judged, ",") != strings.Join(c.want, ",") {
				t.Fatalf("judged %v, want %v", judged, c.want)
			}
		})
	}
}

func TestRunSinceRejectsWhatItCannotRead(t *testing.T) {
	for _, bad := range []string{"yesterday", "-2h", "0d", "2026-13-01"} {
		e := newEnv(t)
		e.stagesConfig("http://unused")
		code, _, errOut := e.run("run", "known", "--since", bad)
		if code != agentio.ExitUsage || !strings.Contains(errOut, "Try: mailpipe run known --since 24h") {
			t.Fatalf("%q: code %d err %q", bad, code, errOut)
		}
	}
}

func TestRunLabelledJudgesOnlyMessagesWithThatLabel(t *testing.T) {
	e := newEnv(t)
	e.stagesConfig("http://unused")
	e.run("sync")
	e.run("import-verdicts", writeFile(t, `{"internet_message_id":"<g1@example.org>","sender":"info@campaign.example.org","stage":"category","labels":["event"],"provenance":"p","at":"2026-10-21T12:00:00Z"}`+"\n"))
	code, out, errOut := e.run("run", "known", "--labelled", "category : event")
	if code != 0 || strings.Count(out, "\n") != 2 || !strings.Contains(out, "\tChip in\t") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	if code, _, errOut := e.run("run", "known", "--labelled", "event"); code != agentio.ExitUsage || !strings.Contains(errOut, "Try: mailpipe run known --labelled category:event") {
		t.Fatalf("bad --labelled: code %d err %q", code, errOut)
	}
}
