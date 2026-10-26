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
)

// receiptTemplates file the campaign's receipts (g1, "Chip in") and know the
// friend (g2) only by an unfiled template that its subject does not match.
const receiptTemplates = `{"templates": [
  {"id": "campaign-receipt", "senders": ["@campaign.example.org"], "describe": "A receipt for a donation",
   "subject": "^Chip in$", "anchors": ["Thanks for your gift."],
   "fields": {"date": {"from": "received"}, "receipt": {"pattern": "Receipt number: (\\d+)"}},
   "file": {"folder": "Archive/Records/{date.year}", "name": "{date}_Campaign_Receipt_{receipt}"}},
  {"id": "friend-note", "senders": ["friend@example.org"], "describe": "A note about a party", "subject": "^Party"}
]}`

const receiptBody = "Thanks for your\n gift.\n\nReceipt number: 42\n"

func (e *env) templatesConfig(jevURL, templates string) string {
	e.t.Helper()
	dir := e.t.TempDir()
	path := filepath.Join(dir, "templates.json")
	if err := os.WriteFile(path, []byte(templates), 0o600); err != nil {
		e.t.Fatal(err)
	}
	e.t.Setenv("MAILPIPE_TEMPLATES", path)
	cfg := map[string]any{
		"self_addresses": []string{"alex@example.com"},
		"openrouter":     map[string]string{"keychain_service": "mailpipe-openrouter", "keychain_account": "default"},
		"stages":         []map[string]any{{"name": "template", "type": "template", "url": jevURL}},
	}
	data, _ := json.Marshal(cfg)
	stages := filepath.Join(dir, "stages.json")
	if err := os.WriteFile(stages, data, 0o600); err != nil {
		e.t.Fatal(err)
	}
	e.t.Setenv("MAILPIPE_STAGES", stages)
	e.secrets = map[string]string{"mailpipe-openrouter/default": "sk-or-test-key"}
	return path
}

// templateJev picks the first non-none option for "Chip in" and none for
// everything else.
func templateJev(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			State     string
			Questions map[string]struct{ Criteria map[string]string }
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		choice := "none"
		for k := range req.Questions["template"].Criteria {
			if k != "none" && strings.Contains(req.State, "Chip in") {
				choice = k
			}
		}
		probs := map[string]float64{}
		for k := range req.Questions["template"].Criteria {
			probs[k] = 0
		}
		probs[choice] = 1
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"template": map[string]any{"type": "choice", "choice": choice, "probabilities": probs}},
			"usage": map[string]any{"cost": 0.0002}})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestTemplatesListsTheTemplates(t *testing.T) {
	e := newEnv(t)
	e.templatesConfig("http://unused", receiptTemplates)
	code, out, errOut := e.run("templates")
	if code != 0 || !strings.HasPrefix(out, "id\tsenders\tfiles\tfolder\tname\n") ||
		!strings.Contains(out, "campaign-receipt\t@campaign.example.org\ttrue\tArchive/Records/{date.year}\t{date}_Campaign_Receipt_{receipt}\n") ||
		!strings.Contains(out, "friend-note\tfriend@example.org\tfalse\t\t\n") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

func TestTemplatesSaysHowToCreateAMissingOrBadFile(t *testing.T) {
	e := newEnv(t)
	path := e.templatesConfig("http://unused", receiptTemplates)
	os.Remove(path)
	if code, _, errOut := e.run("templates"); code != 3 || !strings.Contains(errOut, "templates.example.json") {
		t.Fatalf("missing: code %d err %q", code, errOut)
	}
	os.WriteFile(path, []byte(`{"templates": [{"id": "x"}]}`), 0o600)
	if code, _, errOut := e.run("templates"); code != 2 || !strings.Contains(errOut, path) {
		t.Fatalf("bad: code %d err %q", code, errOut)
	}
}

func TestTemplatesCheckFingerprintsStoredMailForFree(t *testing.T) {
	e := newEnv(t)
	e.templatesConfig("http://unused", receiptTemplates)
	e.graph.body = receiptBody
	e.run("sync")
	code, out, errOut := e.run("templates", "check")
	if code != 0 || !strings.HasPrefix(out, "n\treceived\tsender\tsubject\ttemplate\tfolder\tname\texpected\tdetail\n") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	if !strings.Contains(out, "\tinfo@campaign.example.org\tChip in\tcampaign-receipt\tArchive/Records/2026\t2026-10-20_Campaign_Receipt_42\t\t\n") {
		t.Errorf("g1 row missing: %q", out)
	}
	// The friend's template does not fit "Dinner Tuesday?": the row says why.
	if !strings.Contains(out, "\tfriend@example.org\tDinner Tuesday?\t-\t\t\t\tfriend-note: subject does not match") {
		t.Errorf("g2 row missing: %q", out)
	}
	if !strings.Contains(errOut, "2 messages, 1 matched") {
		t.Errorf("hint %q", errOut)
	}
}

func TestTemplatesCheckComparesWithAnEarlierManifest(t *testing.T) {
	e := newEnv(t)
	e.templatesConfig("http://unused", receiptTemplates)
	e.graph.body = receiptBody
	e.run("sync")
	same := manifest(t, e, "{g1}\tArchive/Records/2026\t2026-10-20_Campaign_Receipt_42")
	if _, out, errOut := e.run("templates", "check", "--expect", same); !strings.Contains(out, "2026-10-20_Campaign_Receipt_42\tsame\t") ||
		!strings.Contains(errOut, "1 same, 0 differ") {
		t.Fatalf("same: out %q err %q", out, errOut)
	}
	differs := manifest(t, e, "{g1}\tArchive/Records/2026\t2026-10-19_Campaign_Gift", "{g2}\tArchive/Notes\t2026-10-21_Friend_Note")
	_, out, errOut := e.run("templates", "check", "--expect", differs)
	if !strings.Contains(out, "\tdiffers: Archive/Records/2026/2026-10-19_Campaign_Gift.pdf\t") ||
		!strings.Contains(out, "\tfiled: Archive/Notes/2026-10-21_Friend_Note.pdf\t") || !strings.Contains(errOut, "0 same, 2 differ") {
		t.Fatalf("differs: out %q err %q", out, errOut)
	}
}

func TestTemplatesAnchorsListsTheLinesEveryExampleShares(t *testing.T) {
	e := newEnv(t)
	e.templatesConfig("http://unused", receiptTemplates)
	e.graph.body = receiptBody
	e.run("sync")
	code, out, errOut := e.run("templates", "anchors", "--from", "info@campaign.example.org")
	if code != 0 || out != "line\nThanks for your\ngift.\nReceipt number: 42\n" || !strings.Contains(errOut, "1 example") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	if code, _, errOut := e.run("templates", "anchors", "--from", "nobody@example.com"); code != 4 {
		t.Fatalf("no examples: code %d err %q", code, errOut)
	}
}

func TestTheTemplateStageDraftsAManifestThatSaveAccepts(t *testing.T) {
	e, root := savable(t)
	srv, calls := templateJev(t)
	e.templatesConfig(srv.URL, receiptTemplates)
	e.graph.body = receiptBody
	code, out, errOut := e.run("run", "template")
	if code != 0 || !strings.Contains(out, "\tChip in\tmatched,campaign-receipt,file\t") || !strings.Contains(out, "\tDinner Tuesday?\tnew\t") || calls.Load() != 2 {
		t.Fatalf("run: code %d out %q err %q", code, out, errOut)
	}
	draft := filepath.Join(t.TempDir(), "draft.tsv")
	code, out, errOut = e.run("file", "draft", "--out", draft)
	if code != 0 || !strings.Contains(out, "\tdrafted\tArchive/Records/2026\t2026-10-20_Campaign_Receipt_42\t") ||
		!strings.Contains(errOut, "mailpipe save --manifest "+draft) {
		t.Fatalf("draft: code %d out %q err %q", code, out, errOut)
	}
	body, _ := os.ReadFile(draft)
	if fi, _ := os.Stat(draft); !strings.HasPrefix(string(body), "n\tfolder\tname\n") || !strings.HasSuffix(string(body), "\tArchive/Records/2026\t2026-10-20_Campaign_Receipt_42\n") ||
		fi.Mode().Perm() != 0o600 {
		t.Fatalf("manifest %q", body)
	}
	if _, vs, _ := e.run("verdicts", firstN(t, out)); !strings.Contains(vs, "\nfiling\t") || !strings.Contains(vs, "\tpending\t") {
		t.Fatalf("no filing:pending verdict: %q", vs)
	}
	// A draft never replaces a file.
	if code, _, errOut := e.run("file", "draft", "--out", draft); code != 2 || !strings.Contains(errOut, "exists") {
		t.Fatalf("overwrite: code %d err %q", code, errOut)
	}
	if code, out, errOut := e.run("save", "--manifest", draft, "--root", root, "--execute"); code != 0 || !strings.Contains(out, "\tsaved\t") {
		t.Fatalf("save: code %d out %q err %q", code, out, errOut)
	}
	// Once saved, nothing is left to draft.
	if code, _, errOut := e.run("file", "draft", "--out", filepath.Join(t.TempDir(), "again.tsv")); code != 4 {
		t.Fatalf("after save: code %d err %q", code, errOut)
	}
}

func TestFileDraftSkipsAMatchTheTemplatesNoLongerFit(t *testing.T) {
	e := newEnv(t)
	srv, _ := templateJev(t)
	path := e.templatesConfig(srv.URL, receiptTemplates)
	e.graph.body = receiptBody
	e.run("sync")
	e.run("run", "template")
	os.WriteFile(path, []byte(strings.Replace(receiptTemplates, "Thanks for your gift.", "Thank you!", 1)), 0o600)
	code, out, errOut := e.run("file", "draft", "--out", filepath.Join(t.TempDir(), "d.tsv"))
	if code != 4 || !strings.Contains(out, "\tstale\t") || !strings.Contains(errOut, "mailpipe run template --rerun") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

func TestFileQueueListsReviewAndNewUntilTheOwnerDecides(t *testing.T) {
	e := newEnv(t)
	srv, _ := templateJev(t)
	e.templatesConfig(srv.URL, receiptTemplates)
	e.graph.body = receiptBody
	e.run("sync")
	e.run("run", "template")
	code, out, errOut := e.run("file", "queue")
	if code != 0 || !strings.HasPrefix(out, "n\treceived\tsender\tsubject\toutcome\treason\n") ||
		!strings.Contains(out, "\tfriend@example.org\tDinner Tuesday?\tnew\t") || strings.Contains(out, "Chip in") {
		t.Fatalf("queue: code %d out %q err %q", code, out, errOut)
	}
	// With --records, a new kind is listed only when it is a record: most of
	// a known sender's other mail (notices, policy updates) is not.
	if code, out, _ := e.run("file", "queue", "--records", "tx-kind:record"); code != 4 || strings.Contains(out, "Dinner") {
		t.Fatalf("not a record: code %d out %q", code, out)
	}
	e.run("import-verdicts", writeFile(t, `{"internet_message_id":"<g2@example.org>","sender":"friend@example.org","stage":"tx-kind","labels":["record"],"provenance":"p","at":"2026-10-25T11:00:00Z"}`+"\n"))
	if code, out, _ := e.run("file", "queue", "--records", "tx-kind:record"); code != 0 || !strings.Contains(out, "\tDinner Tuesday?\tnew\t") {
		t.Fatalf("a record: code %d out %q", code, out)
	}
	e.run("import-verdicts", writeFile(t, `{"internet_message_id":"<g2@example.org>","sender":"friend@example.org","stage":"template","labels":["dismissed"],"provenance":"owner","at":"2026-10-25T12:00:00Z"}`+"\n"))
	if code, out, _ := e.run("file", "queue"); code != 4 || strings.Contains(out, "Dinner") {
		t.Fatalf("after deciding: code %d out %q", code, out)
	}
}

// A record from a sender with no template joins the queue when asked for by
// the label that marks records.
func TestFileQueueAddsUntemplatedRecords(t *testing.T) {
	e := newEnv(t)
	srv, _ := templateJev(t)
	e.templatesConfig(srv.URL, strings.Replace(receiptTemplates, `"senders": ["friend@example.org"]`, `"senders": ["other@example.org"]`, 1))
	e.graph.body = receiptBody
	e.run("sync")
	e.run("run", "template")
	if code, out, _ := e.run("file", "queue"); code != 4 || strings.Contains(out, "Dinner") {
		t.Fatalf("without --records: code %d out %q", code, out)
	}
	e.run("import-verdicts", writeFile(t, `{"internet_message_id":"<g2@example.org>","sender":"friend@example.org","stage":"tx-kind","labels":["record"],"provenance":"p","at":"2026-10-25T12:00:00Z"}`+"\n"))
	code, out, errOut := e.run("file", "queue", "--records", "tx-kind:record")
	if code != 0 || !strings.Contains(out, "\tDinner Tuesday?\tno template\t") || strings.Contains(out, "Chip in") {
		t.Fatalf("with --records: code %d out %q err %q", code, out, errOut)
	}
	if code, _, errOut := e.run("file", "queue", "--records", "record"); code != 2 {
		t.Fatalf("bad --records: code %d err %q", code, errOut)
	}
	// Only the Inbox by default: expired mail is out of the owner's view.
	if code, out, _ := e.run("file", "queue", "--records", "tx-kind:record", "--folder", "deleteditems"); code != 4 || strings.Contains(out, "Dinner") {
		t.Fatalf("deleted items: code %d out %q", code, out)
	}
	// A record the owner has already saved is not waiting.
	e.run("import-verdicts", writeFile(t, `{"internet_message_id":"<g2@example.org>","sender":"friend@example.org","stage":"filing","labels":["saved"],"provenance":"p","at":"2026-10-25T13:00:00Z"}`+"\n"))
	if code, out, _ := e.run("file", "queue", "--records", "tx-kind:record"); code != 4 || strings.Contains(out, "Dinner") {
		t.Fatalf("after saving: code %d out %q", code, out)
	}
}

// file draft reads the templates the stage was configured with, not the
// default file.
func TestFileDraftUsesTheStagesTemplates(t *testing.T) {
	e := newEnv(t)
	srv, _ := templateJev(t)
	path := e.templatesConfig(srv.URL, receiptTemplates)
	e.graph.body = receiptBody
	e.run("sync")
	e.run("run", "template")
	stages := os.Getenv("MAILPIPE_STAGES")
	data, _ := os.ReadFile(stages)
	os.WriteFile(stages, []byte(strings.Replace(string(data), `"type":"template"`, `"type":"template","templates":"`+path+`"`, 1)), 0o600)
	t.Setenv("MAILPIPE_TEMPLATES", filepath.Join(t.TempDir(), "missing.json"))
	if code, out, errOut := e.run("file", "draft", "--out", filepath.Join(t.TempDir(), "d.tsv")); code != 0 || !strings.Contains(out, "\tdrafted\t") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

// A dismissal is the owner's decision: rerunning the stage does not bring the
// message back to the queue, and the record shelf life no longer spares it.
func TestFileDismissOutlastsAStageRerun(t *testing.T) {
	e := newEnv(t)
	srv, _ := templateJev(t)
	e.templatesConfig(srv.URL, receiptTemplates)
	e.graph.body = receiptBody
	e.run("sync")
	e.run("run", "template")
	_, out, _ := e.run("file", "queue")
	n := firstN(t, out)
	code, out, errOut := e.run("file", "dismiss", n, "--reason", "not needed")
	if code != 0 || !strings.Contains(out, n+"\tdismissed\t") {
		t.Fatalf("dismiss: code %d out %q err %q", code, out, errOut)
	}
	if _, vs, _ := e.run("verdicts", n); !strings.Contains(vs, "\nfiling\t") || !strings.Contains(vs, "\tdismissed\t") || !strings.Contains(vs, "not needed") {
		t.Fatalf("verdicts %q", vs)
	}
	e.run("run", "template", "--rerun")
	if code, out, _ := e.run("file", "queue"); code != 4 || strings.Contains(out, "Dinner") {
		t.Fatalf("after rerun: code %d out %q", code, out)
	}
	if code, _, errOut := e.run("file", "dismiss", "999"); code != 2 || !strings.Contains(errOut, "999") {
		t.Fatalf("unknown message: code %d err %q", code, errOut)
	}
}
