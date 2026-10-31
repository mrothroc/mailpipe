package cli_test

import (
	"context"
	"database/sql"
	"errors"
	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/msauth"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/store"
)

func TestGraphProviderCompatibility(t *testing.T) {
	e := newEnv(t)
	code, out, stderr := e.run("sync")
	if code != 0 || !strings.Contains(out, "folder\tadded\tupdated\tremoved\tlocal\tgraph\n") {
		t.Fatalf("sync %d %q %q", code, out, stderr)
	}
	legacy, err := store.Open(e.db)
	if err == nil {
		_ = legacy.Close()
		t.Fatal("fresh sync still used an unbound database")
	}
	code, out, stderr = e.run("ls", "--json")
	if code != 0 || !strings.Contains(out, `"graph_id":"g1"`) {
		t.Fatalf("list %d %q %q", code, out, stderr)
	}
	e.stagesConfig("http://unused")
	for _, args := range [][]string{{"import-verdicts", writeFile(t, imported)}, {"plan", "create", "--folder", "inbox"}, {"apply", "1"}} {
		if code, _, stderr := e.run(args...); code != 0 {
			t.Fatalf("pipeline %v: %d %s", args, code, stderr)
		}
	}
	if len(e.graph.moved) != 0 {
		t.Fatal("check mode moved mail")
	}
	if code, _, stderr := e.run("apply", "1", "--execute"); code != 0 || len(e.graph.moved) != 1 {
		t.Fatalf("reviewed execute %d %s", code, stderr)
	}
	original := e.graph.pages["/me/mailFolders/inbox/messages/delta"][0].Value
	e.graph.pages["/me/mailFolders/inbox/messages/delta"][0].Value = original[1:]
	e.graph.pages["/me/mailFolders/deleteditems/messages/delta"][0].Value = original[:1]
	if code, _, stderr := e.run("sync", "--full"); code != 0 {
		t.Fatal(stderr)
	}
	_, moved, _ := e.run("ls", "--folder", "deleteditems", "--json")
	if !strings.Contains(moved, `"graph_id":"g1"`) || !strings.Contains(moved, `"n":1`) {
		t.Fatal("move or sync renumbered message", moved)
	}
	for _, args := range [][]string{{"actions", "1"}, {"rules", "diff"}, {"rules", "apply", "--execute"}} {
		if code, _, stderr := e.run(args...); code != 0 {
			t.Fatalf("pipeline %v: %d %s", args, code, stderr)
		}
	}
	e.graph.mime = map[string]string{"g2": "From: friend@example.org\r\nSubject: Fixture\r\nContent-Type: text/plain\r\n\r\nComplete filing tail"}
	root := t.TempDir()
	file := manifest(t, e, "{g2}\tRecords\tFixture")
	if code, _, stderr := e.run("save", "--manifest", file, "--root", root); code != 0 || e.graph.rawCalls.Load() != 0 {
		t.Fatalf("filing dry run %d %s", code, stderr)
	}
	if code, _, stderr := e.run("save", "--manifest", file, "--root", root, "--execute"); code != 0 || len(e.rendered) != 1 {
		t.Fatalf("offline filing %d %s", code, stderr)
	}
	if info, err := os.Stat(filepath.Join(root, "Records", "Fixture.pdf")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private PDF %v %v", info, err)
	}
	if code, _, stderr := e.run("show", "2"); code != 0 {
		t.Fatal(stderr)
	}
	e.auth.signedIn = false
	if code, _, stderr := e.run("show", "2"); code != 0 {
		t.Fatalf("offline cache %d %s", code, stderr)
	}
	if code, _, err := e.run("ls"); code != 0 {
		t.Fatalf("offline list: %d %s", code, err)
	}
}

func TestLegacyBindingRequiresExactHistoricalAttribution(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	legacy, err := store.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.Upsert(ctx, "inbox", store.Record{GraphID: "g1", InternetMessageID: "<g1@example.org>", Sender: "info@campaign.example.org"}); err != nil {
		t.Fatal(err)
	}
	if err = legacy.SetBody(ctx, 1, "complete cached history"); err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.CreatePlan(ctx, "inbox", []store.PlanItem{{N: 1, Action: "delete"}}); err != nil {
		t.Fatal(err)
	}
	_ = legacy.Close()
	if code, _, _ := e.run("sync"); code != agentio.ExitStale {
		t.Fatalf("historical sync silently bound: %d", code)
	}
	code, out, stderr := e.run("account", "bind")
	if code != 0 {
		t.Fatalf("preview %d %s", code, stderr)
	}
	rows := strings.Split(strings.TrimSpace(out), "\n")
	cols := strings.Split(rows[1], "\t")
	digest := cols[len(cols)-1]
	stillLegacy, err := store.Open(e.db)
	if err != nil {
		t.Fatal("preview changed attribution", err)
	}
	_ = stillLegacy.Close()
	if code, _, _ := e.run("account", "bind", "--execute"); code != agentio.ExitUsage {
		t.Fatalf("missing attribution: %d", code)
	}
	if code, _, _ := e.run("account", "bind", "--execute", "--confirm-history", strings.Repeat("0", 64)); code != agentio.ExitUsage {
		t.Fatalf("stale attribution: %d", code)
	}
	if code, _, stderr := e.run("account", "bind", "--execute", "--confirm-history", digest); code != 0 {
		t.Fatalf("bind %d %s", code, stderr)
	}
	if code, _, stderr := e.run("apply", "1"); code != agentio.ExitStale || !strings.Contains(stderr, "mailpipe plan create") {
		t.Fatalf("legacy plan executable %d %s", code, stderr)
	}
	e.auth.signedIn = false
	if code, out, stderr := e.run("show", "1"); code != 0 || !strings.Contains(out, "complete cached history") {
		t.Fatalf("lost cache/offline history %d %q %s", code, out, stderr)
	}
	if len(e.graph.moved) != 0 {
		t.Fatal("binding executed mail actions")
	}
}

func TestBoundAccountSwitchRefusedWithoutReassignment(t *testing.T) {
	e := newEnv(t)
	if code, _, stderr := e.run("sync"); code != 0 {
		t.Fatal(stderr)
	}
	e.auth.subject = msauth.Subject{TenantID: "22222222-2222-4222-8222-222222222222", ObjectID: "44444444-4444-4444-8444-444444444444"}
	if code, _, stderr := e.run("sync"); code != agentio.ExitStale || !strings.Contains(stderr, "account mismatch") {
		t.Fatalf("switch %d %s", code, stderr)
	}
	if code, _, stderr := e.run("show", "1"); code != agentio.ExitStale {
		t.Fatalf("cross account content %d %s", code, stderr)
	}
	if e.graph.bodyCalls.Load() != 0 {
		t.Fatal("cross-account body requested")
	}
	if code, out, stderr := e.run("ls"); code != 0 || !strings.Contains(out, "info@campaign.example.org") {
		t.Fatalf("mirror reassigned %d %q %s", code, out, stderr)
	}
}

type readOnlyMailbox struct{ source mailbox.Provider }

func (p readOnlyMailbox) Account() mailbox.Account { return p.source.Account() }
func (p readOnlyMailbox) Collections(ctx context.Context) ([]mailbox.Collection, error) {
	return p.source.Collections(ctx)
}
func (p readOnlyMailbox) Sync(ctx context.Context, r mailbox.SyncRequest) (mailbox.SyncPage, error) {
	return p.source.Sync(ctx, r)
}
func (p readOnlyMailbox) Metadata(ctx context.Context, r mailbox.MessageRef) (mailbox.Metadata, error) {
	return p.source.Metadata(ctx, r)
}
func (p readOnlyMailbox) Text(ctx context.Context, r mailbox.MessageRef) (string, error) {
	return p.source.Text(ctx, r)
}
func (p readOnlyMailbox) WriteMIME(ctx context.Context, r mailbox.MessageRef, w io.Writer) error {
	return p.source.WriteMIME(ctx, r, w)
}
func (p readOnlyMailbox) Inspect(ctx context.Context, r mailbox.MessageRef) (mailbox.State, error) {
	return p.source.Inspect(ctx, r)
}

func TestReadOnlyMethodSetCannotExecuteGraphActionsOrRules(t *testing.T) {
	e := planned(t)
	subject, err := e.auth.Subject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, err := graphmail.New(e.graph, subject.Account())
	if err != nil {
		t.Fatal(err)
	}
	e.provider = readOnlyMailbox{p}
	for _, args := range [][]string{{"apply", "1"}, {"rules", "list"}, {"sync"}} {
		code, _, stderr := e.run(args...)
		if code == 0 || !strings.Contains(stderr, "operation unsupported") {
			t.Fatalf("unsupported %v: %d %s", args, code, stderr)
		}
	}
	if len(e.graph.moved) != 0 || e.graph.ruleSeq != 0 {
		t.Fatal("read-only wrapper retained writes")
	}
}

func TestCLIMultiMessageVerdictsRemainAtomic(t *testing.T) {
	for _, operation := range []string{"import", "dismiss", "save", "draft"} {
		t.Run(operation, func(t *testing.T) {
			e := newEnv(t)
			if code, _, stderr := e.run("sync"); code != 0 {
				t.Fatal(stderr)
			}
			stage := "filing"
			var args []string
			switch operation {
			case "import":
				stage = "batch-fixture"
				args = []string{"import-verdicts", writeFile(t, `{"internet_message_id":"<g1@example.org>","sender":"info@campaign.example.org","stage":"batch-fixture","labels":["review"],"provenance":"fixture","at":"2026-10-20T10:00:00Z"}`+"\n"+`{"internet_message_id":"<g2@example.org>","sender":"friend@example.org","stage":"batch-fixture","labels":["review"],"provenance":"fixture","at":"2026-10-20T10:00:00Z"}`+"\n")}
			case "dismiss":
				args = []string{"file", "dismiss", "1", "2", "--reason", "invented test decision"}
			case "save":
				args = []string{"save", "--manifest", manifest(t, e, "{g1}\tRecords\tfirst", "{g2}\tRecords\tsecond"), "--root", t.TempDir(), "--execute"}
			case "draft":
				templates := `{"templates":[{"id":"one","senders":["info@campaign.example.org"],"describe":"A fixture","subject":"^Chip in$","fields":{"date":{"from":"received"}},"file":{"folder":"Records","name":"{date}_one"}},{"id":"two","senders":["friend@example.org"],"describe":"Another fixture","subject":"^Dinner","fields":{"date":{"from":"received"}},"file":{"folder":"Records","name":"{date}_two"}}]}`
				e.templatesConfig("http://unused", templates)
				s, err := e.boundStore()
				if err != nil {
					t.Fatal(err)
				}
				err = s.AddVerdictsBatch(context.Background(), []store.BoundVerdict{{Local: mailbox.LocalRef{AccountID: s.Account().ID, N: 1}, Verdict: store.Verdict{Stage: "template", Labels: []string{"matched", "one", "file"}, Scores: map[string]float64{}, Reason: "fixture", Provenance: `{"template_id":"one"}`}}, {Local: mailbox.LocalRef{AccountID: s.Account().ID, N: 2}, Verdict: store.Verdict{Stage: "template", Labels: []string{"matched", "two", "file"}, Scores: map[string]float64{}, Reason: "fixture", Provenance: `{"template_id":"two"}`}}})
				_ = s.Close()
				if err != nil {
					t.Fatal(err)
				}
				args = []string{"file", "draft", "--out", filepath.Join(t.TempDir(), "draft.tsv")}
			}
			e.openAccount = func(ctx context.Context, path string, account mailbox.Account) (*store.AccountStore, error) {
				opened, err := store.OpenAccount(ctx, path, account)
				if err != nil {
					return nil, err
				}
				db, err := sql.Open("sqlite", path)
				if err == nil {
					_, err = db.Exec("create trigger invented_cli_batch_failure before insert on verdicts when new.n=2 and new.stage='" + stage + "' begin select raise(abort,'invented second-row failure'); end")
					_ = db.Close()
				}
				if err != nil {
					_ = opened.Close()
					return nil, err
				}
				return opened, nil
			}
			code, _, stderr := e.run(args...)
			e.openAccount = nil
			db, err := sql.Open("sqlite", e.db)
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec("drop trigger if exists invented_cli_batch_failure")
			_ = db.Close()
			if err != nil {
				t.Fatal(err)
			}
			if code != agentio.ExitRuntime || !strings.Contains(stderr, "invented second-row failure") {
				t.Fatalf("%s: %d %s", operation, code, stderr)
			}
			s, err := e.boundStore()
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			for _, n := range []int64{1, 2} {
				vs, err := s.Verdicts(context.Background(), mailbox.LocalRef{AccountID: s.Account().ID, N: n})
				if err != nil {
					t.Fatal(err)
				}
				for _, v := range vs {
					if v.Stage == stage {
						t.Fatalf("partial %s verdict persisted for %d: %+v", operation, n, v)
					}
				}
			}
			if operation == "save" && e.graph.rawCalls.Load() != 0 {
				t.Fatal("source fetched after failed pending transaction")
			}
		})
	}
}

func TestGraphReceivedPrecisionAndEmptyBodyCache(t *testing.T) {
	e := newEnv(t)
	const precise = "2026-10-20T10:00:00.1234567Z"
	e.graph.pages["/me/mailFolders/inbox/messages/delta"][0].Value[0] = msg("g1", "info@campaign.example.org", "Chip in", precise)
	if code, _, stderr := e.run("sync"); code != 0 {
		t.Fatal(stderr)
	}
	if code, out, stderr := e.run("ls", "--json"); code != 0 || !strings.Contains(out, precise) {
		t.Fatalf("timestamp precision lost %d %s %s", code, out, stderr)
	}
	e.graph.body = ""
	if code, _, stderr := e.run("show", "1"); code != 0 {
		t.Fatal(stderr)
	}
	before := e.graph.bodyCalls.Load()
	e.auth.signedIn = false
	if code, _, stderr := e.run("show", "1"); code != 0 || e.graph.bodyCalls.Load() != before {
		t.Fatalf("empty cache fetched again %d %s", code, stderr)
	}
}

func TestHistoryOnlyBindingPopulatesOfflineRoutes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s, err := store.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Upsert(ctx, "inbox", store.Record{GraphID: "retired", Sender: "history@example.org"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Remove(ctx, "inbox", "retired"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	prep, err := store.PrepareLegacyBinding(ctx, e.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(prep.Live) != 0 {
		t.Fatal("fixture has live rows")
	}
	if code, _, stderr := e.run("account", "bind", "--execute", "--confirm-history", prep.Digest()); code != 0 {
		t.Fatalf("bind %d %s", code, stderr)
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"inbox", "deleteditems", "sentitems", "junkemail"} {
		if cfg.Collections[name] != "F-"+name {
			t.Fatalf("missing route %s: %+v", name, cfg.Collections)
		}
	}
	e.auth.signedIn = false
	for _, folder := range []string{"inbox", "deleteditems", "sentitems", "junkemail"} {
		if code, _, stderr := e.run("ls", "--folder", folder); code != agentio.ExitNoData {
			t.Fatalf("offline %s: %d %s", folder, code, stderr)
		}
	}
	if _, err = store.Open(e.db); err == nil {
		t.Fatal("permanent binding lost")
	}
}

type bindingResolver struct {
	mailbox.Provider
	resolve func(context.Context, string) (mailbox.Collection, error)
}

func (p bindingResolver) Resolve(ctx context.Context, name string) (mailbox.Collection, error) {
	return p.resolve(ctx, name)
}

func TestBindingPreservesVerifiedHistoricalAlias(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s, err := store.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Upsert(ctx, "Archive", store.Record{GraphID: "custom", InternetMessageID: "<custom@example.org>", Sender: "history@example.org"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	e.graph.server["custom"] = serverMsg{"Archive", "<custom@example.org>", "history@example.org"}
	prep, err := store.PrepareLegacyBinding(ctx, e.db)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := e.run("account", "bind", "--execute", "--confirm-history", prep.Digest()); code != 0 {
		t.Fatalf("bind %d %s", code, stderr)
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Collections["Archive"] != "F-Archive" {
		t.Fatalf("verified alias lost %+v", cfg.Collections)
	}
	e.auth.signedIn = false
	if code, out, stderr := e.run("ls", "--folder", "Archive"); code != 0 || !strings.Contains(out, "history@example.org") || !strings.Contains(out, "Archive") {
		t.Fatalf("offline alias %d %q %s", code, out, stderr)
	}
}
func TestBindingRouteFailureRecoversOnlySameAccount(t *testing.T) {
	for _, mode := range []string{"resolve", "save"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			s, err := store.Open(e.db)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Upsert(ctx, "inbox", store.Record{GraphID: "retired", Sender: "history@example.org"}); err != nil {
				t.Fatal(err)
			}
			_, _ = s.Remove(ctx, "inbox", "retired")
			_ = s.Close()
			prep, err := store.PrepareLegacyBinding(ctx, e.db)
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(config.DefaultPath())
			if err != nil {
				t.Fatal(err)
			}
			subject, err := e.auth.Subject(ctx)
			if err != nil {
				t.Fatal(err)
			}
			p, err := graphmail.New(e.graph, subject.Account())
			if err != nil {
				t.Fatal(err)
			}
			e.provider = bindingResolver{Provider: p, resolve: func(ctx context.Context, name string) (mailbox.Collection, error) {
				if name == "junkemail" {
					if mode == "resolve" {
						return mailbox.Collection{}, errors.New("invented resolution failure")
					}
					if err := os.Remove(config.DefaultPath()); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(config.DefaultPath(), 0700); err != nil {
						t.Fatal(err)
					}
				}
				return p.Resolve(ctx, name)
			}}
			if code, _, stderr := e.run("account", "bind", "--execute", "--confirm-history", prep.Digest()); code != agentio.ExitRuntime || !strings.Contains(stderr, "retry account bind") {
				t.Fatalf("partial failure %d %s", code, stderr)
			}
			if mode == "save" {
				if err := os.Remove(config.DefaultPath()); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(config.DefaultPath(), original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			bound, err := store.OpenAccount(ctx, e.db, subject.Account())
			if err != nil {
				t.Fatal("binding was not permanent", err)
			}
			_ = bound.Close()
			other := msauth.Subject{TenantID: subject.TenantID, ObjectID: "55555555-5555-4555-8555-555555555555"}
			wrong, err := graphmail.New(e.graph, other.Account())
			if err != nil {
				t.Fatal(err)
			}
			e.provider = wrong
			if code, _, _ := e.run("account", "bind", "--execute"); code == 0 {
				t.Fatal("different-account recovery succeeded")
			}
			e.provider = p
			if code, _, stderr := e.run("account", "bind", "--execute"); code != 0 {
				t.Fatalf("recovery %d %s", code, stderr)
			}
			cfg, err := config.Load(config.DefaultPath())
			if err != nil {
				t.Fatal(err)
			}
			if cfg.AccountID != string(subject.Account().ID) {
				t.Fatal("binding changed")
			}
			for _, name := range []string{"inbox", "deleteditems", "sentitems", "junkemail"} {
				if cfg.Collections[name] != "F-"+name {
					t.Fatalf("recovery route %s %+v", name, cfg.Collections)
				}
			}
		})
	}
}
