package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/store"
)

func TestLegacyStaleReferenceRecoveryEntry(t *testing.T) {
	e := newEnv(t)
	s, err := store.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Upsert(context.Background(), "inbox", store.Record{GraphID: "unavailable", InternetMessageID: "<unavailable@example.org>", Sender: "history@example.org"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"sync"}, {"sync", "--full"}} {
		if code, _, stderr := e.run(args...); code != agentio.ExitStale {
			t.Fatalf("unattributed sync: %d %s", code, stderr)
		}
	}
	code, _, stderr := e.run("account", "bind")
	if code != agentio.ExitStale || !strings.Contains(stderr, "account reconcile") || strings.Contains(stderr, "sync --full") {
		t.Fatalf("stale binding must leave the sync/binding loop with a reconciliation instruction: %d %s", code, stderr)
	}
}

func TestLegacyReviewedRecoveryAndSync(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s, err := store.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []store.Record{{GraphID: "g1", InternetMessageID: "<g1@example.org>", Sender: "info@campaign.example.org"}, {GraphID: "unavailable", InternetMessageID: "<unavailable@example.org>", Sender: "history@example.org"}} {
		if _, err = s.Upsert(ctx, "inbox", r); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.SetBody(ctx, 2, "preserved complete cached history"); err != nil {
		t.Fatal(err)
	}
	if err = s.AddVerdict(ctx, store.Verdict{N: 2, Stage: "filed", Provenance: "invented-filing"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreatePlan(ctx, "inbox", []store.PlanItem{{N: 2, Action: "delete"}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	code, out, stderr := e.run("account", "reconcile", "--json")
	if code != 0 {
		t.Fatalf("preview: %d %s", code, stderr)
	}
	var summary struct {
		Account string `json:"account"`
		D       string `json:"source_digest"`
		R       string `json:"reconciliation_digest"`
	}
	if err = json.Unmarshal([]byte(strings.Split(strings.TrimSpace(out), "\n")[0]), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.D == "" || summary.R == "" || !strings.Contains(out, `"disposition":"unavailable"`) {
		t.Fatalf("missing review evidence: %s", out)
	}
	s, err = store.Open(e.db)
	if err != nil {
		t.Fatal("preview bound history", err)
	}
	s.Close()
	args := []string{"account", "reconcile", "--execute", "--confirm-account", summary.Account, "--confirm-history", summary.D, "--confirm-reconciliation", summary.R}
	for _, flag := range []string{"--confirm-account", "--confirm-history", "--confirm-reconciliation"} {
		bad := append([]string(nil), args...)
		for i, v := range bad {
			if v == flag {
				bad[i+1] = "unreviewed"
			}
		}
		if code, _, stderr = e.run(bad...); code == 0 {
			t.Fatalf("unreviewed recovery accepted %s: %s", flag, stderr)
		}
	}
	if code, _, stderr = e.run(args...); code != 0 {
		t.Fatalf("execute: %d %s", code, stderr)
	}
	if code, _, stderr = e.run("sync", "--full"); code != 0 {
		t.Fatalf("post-recovery sync: %d %s", code, stderr)
	}
	if code, out, stderr = e.run("show", "2"); code != 0 || !strings.Contains(out, "preserved complete cached history") {
		t.Fatalf("history: %d %s %s", code, out, stderr)
	}
	if code, _, stderr = e.run("apply", "1"); code != agentio.ExitStale {
		t.Fatalf("old plan became executable %d %s", code, stderr)
	}
	e.graph.totals["inbox"] = 3
	e.graph.server["unavailable"] = serverMsg{"inbox", "<unavailable@example.org>", "history@example.org"}
	page := e.graph.pages["/me/mailFolders/inbox/messages/delta"][0]
	page.Value = append(page.Value, msg("unavailable", "history@example.org", "Restored invented history", "2026-10-20T10:00:00Z"))
	e.graph.pages["/me/mailFolders/inbox/messages/delta"] = []graph.Page{page}
	if code, _, stderr = e.run("sync", "--full"); code != 0 {
		t.Fatalf("reappearance sync: %d %s", code, stderr)
	}
	a := mailbox.Account{ID: mailbox.AccountID(summary.Account), ProviderID: store.GraphProvider}
	bound, err := store.OpenAccount(ctx, e.db, a)
	if err != nil {
		t.Fatal(err)
	}
	m, err := bound.GetMessage(ctx, mailbox.LocalRef{AccountID: a.ID, N: 2})
	if err != nil || string(m.State.Ref.MessageID) != "unavailable" || len(m.State.Memberships) != 1 {
		t.Fatalf("n changed on reappearance: %+v %v", m, err)
	}
	verdicts, err := bound.Verdicts(ctx, mailbox.LocalRef{AccountID: a.ID, N: 2})
	if err != nil || len(verdicts) != 1 || verdicts[0].Provenance != "invented-filing" {
		t.Fatalf("filing history lost: %+v %v", verdicts, err)
	}
	bound.Close()
	e.auth.signedIn = false
	if code, out, stderr = e.run("account", "reconcile", "--json"); code != 0 || !strings.Contains(out, `"disposition":"unavailable"`) {
		t.Fatalf("offline receipt: %d %s %s", code, out, stderr)
	}
	if len(e.graph.moved) != 0 {
		t.Fatal("reconciliation mutated mail")
	}
}

func TestReconciliationErrorsDistinguishIncompleteAndStructuralEvidence(t *testing.T) {
	for _, kind := range []string{"partial", "no-anchor", "ambiguous", "wrong-account"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			s, err := store.Open(e.db)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range []store.Record{{GraphID: "g1", InternetMessageID: "<g1@example.org>", Sender: "info@campaign.example.org"}, {GraphID: "missing", InternetMessageID: "<missing@example.org>", Sender: "history@example.org"}} {
				if _, err = s.Upsert(context.Background(), "inbox", r); err != nil {
					t.Fatal(err)
				}
			}
			s.Close()
			switch kind {
			case "partial":
				e.graph.pagesErr = &graph.APIError{Status: 403, Code: "ErrorAccessDenied"}
			case "no-anchor":
				e.graph.server = map[string]serverMsg{}
				e.graph.pages = map[string][]graph.Page{}
			case "ambiguous":
				e.graph.server["other"] = serverMsg{"inbox", "<missing@example.org>", "history@example.org"}
			case "wrong-account":
				cfg, err := config.Load(config.DefaultPath())
				if err != nil {
					t.Fatal(err)
				}
				cfg.AccountID = "other-account"
				if err = config.Save(config.DefaultPath(), cfg); err != nil {
					t.Fatal(err)
				}
			}
			code, out, stderr := e.run("account", "reconcile", "--json")
			if kind == "partial" {
				if code != agentio.ExitRuntime || !strings.Contains(stderr, "read failure") {
					t.Fatalf("transient error: %d %s", code, stderr)
				}
			} else if code != agentio.ExitStale {
				t.Fatalf("structural block: %d %s", code, stderr)
			}
			if out != "" || strings.Contains(stderr, "sync --full") {
				t.Fatalf("incomplete proposal or loop hint leaked: %s %s", out, stderr)
			}
			s, err = store.Open(e.db)
			if err != nil {
				t.Fatal("failed evidence attributed history", err)
			}
			s.Close()
		})
	}
}

type reconciliationRoutingProvider struct {
	*graphmail.Provider
	fail func(string) error
}

func (p reconciliationRoutingProvider) Resolve(ctx context.Context, name string) (mailbox.Collection, error) {
	if e := p.fail(name); e != nil {
		return mailbox.Collection{}, e
	}
	return p.Provider.Resolve(ctx, name)
}

func TestReconciliationRoutingPartialCompletionAndRetry(t *testing.T) {
	for _, kind := range []string{"resolve", "save"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			s, err := store.Open(e.db)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range []store.Record{{GraphID: "g1", InternetMessageID: "<g1@example.org>", Sender: "info@campaign.example.org"}, {GraphID: "missing", InternetMessageID: "<missing@example.org>", Sender: "history@example.org"}} {
				if _, err = s.Upsert(ctx, "inbox", r); err != nil {
					t.Fatal(err)
				}
			}
			s.Close()
			code, out, stderr := e.run("account", "reconcile", "--json")
			if code != 0 {
				t.Fatal(stderr)
			}
			summary := map[string]any{}
			if err = json.Unmarshal([]byte(strings.Split(out, "\n")[0]), &summary); err != nil {
				t.Fatal(err)
			}
			args := []string{"account", "reconcile", "--execute", "--confirm-account", summary["account"].(string), "--confirm-history", summary["source_digest"].(string), "--confirm-reconciliation", summary["reconciliation_digest"].(string)}
			subject, err := e.auth.Subject(ctx)
			if err != nil {
				t.Fatal(err)
			}
			p, err := graphmail.New(e.graph, subject.Account())
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(config.DefaultPath())
			if err != nil {
				t.Fatal(err)
			}
			e.provider = reconciliationRoutingProvider{Provider: p, fail: func(name string) error {
				if name != "junkemail" {
					return nil
				}
				if kind == "resolve" {
					return errors.New("invented resolution failure")
				}
				if err := os.Remove(config.DefaultPath()); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(config.DefaultPath(), 0700); err != nil {
					t.Fatal(err)
				}
				return nil
			}}
			if code, _, stderr = e.run(args...); code != agentio.ExitRuntime || !strings.Contains(stderr, "database reconciled but routing") {
				t.Fatalf("partial local outcome: %d %s", code, stderr)
			}
			bound, err := store.OpenAccount(ctx, e.db, subject.Account())
			if err != nil {
				t.Fatal("binding was lost", err)
			}
			r, err := bound.LegacyReconciliationReceipt(ctx)
			bound.Close()
			if err != nil || r.Digest != summary["reconciliation_digest"] {
				t.Fatalf("receipt lost: %+v %v", r, err)
			}
			if kind == "save" {
				if err = os.Remove(config.DefaultPath()); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(config.DefaultPath(), original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			e.provider = p
			e.graph.pagesErr = errors.New("retry must not enumerate again")
			if code, _, stderr = e.run(args...); code != 0 {
				t.Fatalf("same-account routing retry: %d %s", code, stderr)
			}
			if len(e.graph.moved) != 0 {
				t.Fatal("routing recovery executed mail actions")
			}
		})
	}
}

func TestReconciliationLimitedOutputRetainsCompletePrivateReport(t *testing.T) {
	e := newEnv(t)
	outdir := filepath.Join(t.TempDir(), "out")
	t.Setenv("MAILPIPE_OUT_DIR", outdir)
	s, err := store.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []store.Record{{GraphID: "g1", InternetMessageID: "<g1@example.org>", Sender: "info@campaign.example.org"}, {GraphID: "missing", InternetMessageID: "<missing@example.org>", Sender: "history@example.org"}} {
		if _, err = s.Upsert(context.Background(), "inbox", r); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	code, out, stderr := e.run("account", "reconcile", "--json", "--limit", "1")
	if code != 0 {
		t.Fatal(stderr)
	}
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("stdout limit ignored: %s", out)
	}
	files, err := os.ReadDir(outdir)
	if err != nil || len(files) != 1 {
		t.Fatalf("complete private report missing: %v %v", files, err)
	}
	path := filepath.Join(outdir, files[0].Name())
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"disposition":"unavailable"`) || !strings.Contains(string(raw), `"disposition":"verified"`) || !strings.Contains(string(raw), `"state":{`) {
		t.Fatalf("overflow lost review evidence: %s", raw)
	}
	for _, path := range []string{outdir, path} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0600)
		if info.IsDir() {
			want = 0700
		}
		if info.Mode().Perm() != want {
			t.Fatalf("private report mode: %s %o", path, info.Mode().Perm())
		}
	}
}
