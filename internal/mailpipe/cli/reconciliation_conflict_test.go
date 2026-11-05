package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/cli"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/store"
)

func TestReconciliationInjectedLegacyDiagnosticIdentifiesOriginalAndObserved(t *testing.T) {
	e := newEnv(t)
	s, err := store.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Upsert(context.Background(), "inbox", store.Record{GraphID: "original-id", InternetMessageID: "<shared@example.org>", Sender: "sender@example.org"})
	if err != nil {
		t.Fatal(err)
	}
	messages, err := s.Messages(context.Background(), store.Filter{})
	if err != nil || len(messages) != 1 {
		t.Fatalf("original inventory: %+v %v", messages, err)
	}
	n := messages[0].N
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	e.graph.server = map[string]serverMsg{"observed-id": {"inbox", "<shared@example.org>", "sender@example.org"}}
	installInjectedLegacyDiagnostic(t, e, messages, "observed-id", "<shared@example.org>", "sender@example.org", "inbox")
	code, out, stderr := e.run("account", "reconcile", "--json")
	if code != agentio.ExitStale {
		t.Fatalf("injected legacy error lost its blocked status: %d %s", code, stderr)
	}
	var row map[string]any
	if err = json.Unmarshal([]byte(strings.TrimSpace(out)), &row); err != nil {
		t.Fatalf("injected legacy diagnostic lacks original/observed references: %v; stdout=%q stderr=%q", err, out, stderr)
	}
	if row["n"] != float64(n) || row["original_id"] != "original-id" || row["observed_id"] != "observed-id" || row["status"] != "blocked" || row["enumeration_complete"] != false || row["original_inspection_complete"] != false {
		t.Fatalf("misleading conflict evidence: %s", out)
	}
	for _, key := range []string{"source_digest", "reconciliation_digest", "backup_digest", "state", "disposition"} {
		if _, ok := row[key]; ok {
			t.Fatalf("diagnostic acquired proposal field %s", key)
		}
	}
}

func seedConflictHistory(t *testing.T, e *env, ids []string, header, sender string) []store.Message {
	t.Helper()
	s, err := store.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err = s.Upsert(context.Background(), "inbox", store.Record{GraphID: id, InternetMessageID: header, Sender: sender}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.Messages(context.Background(), store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	return rows
}

func conflictJSONRows(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("invalid diagnostic JSON: %v %q", err, line)
		}
		if row["status"] != "blocked" || row["enumeration_complete"] != false || row["original_inspection_complete"] != false || row["diagnostic_scope"] != "first_conflicting_observation" {
			t.Fatalf("diagnostic claims complete evidence: %+v", row)
		}
		for _, key := range []string{"source_digest", "reconciliation_digest", "backup_digest", "state", "disposition", "history_confirmed", "receipt"} {
			if _, ok := row[key]; ok {
				t.Fatal("diagnostic acquired authority field", key)
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func assertConflictPreservedHistory(t *testing.T, e *env, before []store.Message, configBefore []byte) {
	t.Helper()
	s, err := store.Open(e.db)
	if err != nil {
		t.Fatal("conflict bound history", err)
	}
	after, err := s.Messages(context.Background(), store.Filter{})
	closeErr := s.Close()
	if err != nil || closeErr != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("conflict changed history: before=%+v after=%+v %v %v", before, after, err, closeErr)
	}
	cfg, err := os.ReadFile(config.DefaultPath())
	if err != nil || !bytes.Equal(cfg, configBefore) {
		t.Fatal("conflict saved routing", err)
	}
	subject, err := e.auth.Subject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.ReadLegacyReconciliationReceipt(context.Background(), e.db, subject.Account()); err != nil || found {
		t.Fatalf("conflict produced receipt: found=%t %v", found, err)
	}
	if len(e.graph.moved) != 0 || e.graph.rawCalls.Load() != 0 || e.graph.bodyCalls.Load() != 0 {
		t.Fatal("conflict requested content or mutated mail")
	}
}

func TestReconciliationInjectedLegacyDiagnosticPrivateOverflowAndOriginalCoverage(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		t.Run(fmt.Sprint(jsonMode), func(t *testing.T) {
			e := newEnv(t)
			outdir := filepath.Join(t.TempDir(), "out")
			t.Setenv("MAILPIPE_OUT_DIR", outdir)
			before := seedConflictHistory(t, e, []string{"original-one", "original-two"}, "<shared@example.org>", "sender@example.org")
			cfg, err := os.ReadFile(config.DefaultPath())
			if err != nil {
				t.Fatal(err)
			}
			e.graph.server = map[string]serverMsg{"observed-id": {"inbox", "<shared@example.org>", "sender@example.org"}}
			installInjectedLegacyDiagnostic(t, e, before, "observed-id", "<shared@example.org>", "sender@example.org", "inbox")
			args := []string{"account", "reconcile", "--limit", "1"}
			if jsonMode {
				args = append(args, "--json")
			}
			code, out, stderr := e.run(args...)
			if code != agentio.ExitStale || !strings.Contains(stderr, "full result:") || strings.Contains(stderr, "--confirm-") {
				t.Fatalf("overflow failure: %d %s", code, stderr)
			}
			lines := strings.Split(strings.TrimSpace(out), "\n")
			want := 2
			if jsonMode {
				want = 1
			}
			if len(lines) != want {
				t.Fatalf("stdout limit ignored: %q", out)
			}
			files, err := os.ReadDir(outdir)
			if err != nil || len(files) != 1 {
				t.Fatalf("missing retained report: %v %v", files, err)
			}
			path := filepath.Join(outdir, files[0].Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if jsonMode {
				rows := conflictJSONRows(t, string(raw))
				if len(rows) != 2 || rows[0]["n"] == rows[1]["n"] || rows[0]["original_id"] == rows[1]["original_id"] {
					t.Fatalf("originals collapsed: %s", raw)
				}
				for _, row := range rows {
					if row["matching_originals"] != float64(2) || row["retained_originals"] != float64(2) || row["omitted_originals"] != float64(0) || row["observed_id"] != "observed-id" {
						t.Fatal("incorrect scoped counts", row)
					}
					for _, original := range before {
						if row["n"] == float64(original.N) && row["original_id"] != original.GraphID {
							t.Fatal("local number was assigned another original identity", row)
						}
					}
				}
			} else if len(strings.Split(strings.TrimSpace(string(raw)), "\n")) != 3 || !strings.Contains(string(raw), "original-one") || !strings.Contains(string(raw), "original-two") {
				t.Fatalf("TSV overflow incomplete: %s", raw)
			}
			for _, name := range []string{outdir, path} {
				info, err := os.Stat(name)
				if err != nil {
					t.Fatal(err)
				}
				want := os.FileMode(0600)
				if info.IsDir() {
					want = 0700
				}
				if info.Mode().Perm() != want {
					t.Fatalf("private mode %s: %o", name, info.Mode().Perm())
				}
			}
			assertConflictPreservedHistory(t, e, before, cfg)
		})
	}
}

func TestReconciliationCounterpartDoesNotChangeExactProposal(t *testing.T) {
	e := newEnv(t)
	seedConflictHistory(t, e, []string{"g1"}, "<g1@example.org>", "info@campaign.example.org")
	before := seedConflictHistory(t, e, []string{"missing-original"}, "<shared@example.org>", "sender@example.org")
	code, out, stderr := e.run("account", "reconcile", "--json")
	if code != 0 {
		t.Fatalf("initial preview: %d %s", code, stderr)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(strings.Split(out, "\n")[0]), &summary); err != nil {
		t.Fatal(err)
	}
	e.graph.server["observed-id"] = serverMsg{"inbox", "<shared@example.org>", "sender@example.org"}
	args := []string{"account", "reconcile", "--json", "--execute", "--confirm-account", summary["account"].(string), "--confirm-history", summary["source_digest"].(string), "--confirm-reconciliation", summary["reconciliation_digest"].(string)}
	code, out, stderr = e.run(args...)
	if code != 0 {
		t.Fatalf("irrelevant counterpart blocked exact proposal: %d %s %s", code, out, stderr)
	}
	subject, err := e.auth.Subject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.OpenAccount(context.Background(), e.db, subject.Account())
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	receipt, err := bound.LegacyReconciliationReceipt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Items) != len(before) || len(e.graph.moved) != 0 {
		t.Fatal("counterpart published or server action attempted")
	}
	for _, row := range receipt.Items {
		if row.OriginalID == "observed-id" {
			t.Fatal("counterpart took original history")
		}
	}
}

func TestReconciliationInjectedLegacyDiagnosticPreservesSuppliedOmissionCounts(t *testing.T) {
	e := newEnv(t)
	outdir := filepath.Join(t.TempDir(), "out")
	t.Setenv("MAILPIPE_OUT_DIR", outdir)
	ids := []string{"original-one", "original-two"}
	before := seedConflictHistory(t, e, ids, "<shared@example.org>", "sender@example.org")
	e.graph.server = map[string]serverMsg{"observed-id": {"inbox", "<shared@example.org>", "sender@example.org"}}
	report := installInjectedLegacyDiagnostic(t, e, before, "observed-id", "<shared@example.org>", "sender@example.org", "inbox")
	report.MatchingOriginals = 5 // supplied legacy payload: two retained, three omitted
	code, out, stderr := e.run("account", "reconcile", "--json", "--limit", "1")
	if code != agentio.ExitStale || len(conflictJSONRows(t, out)) != 1 {
		t.Fatalf("bounded report failed: %d %s %s", code, out, stderr)
	}
	files, err := os.ReadDir(outdir)
	if err != nil || len(files) != 1 {
		t.Fatalf("bounded overflow missing: %v %v", files, err)
	}
	raw, err := os.ReadFile(filepath.Join(outdir, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	rows := conflictJSONRows(t, string(raw))
	if len(rows) != 2 {
		t.Fatalf("supplied retained rows lost: %d rows", len(rows))
	}
	for _, row := range rows {
		if row["matching_originals"] != float64(5) || row["retained_originals"] != float64(2) || row["omitted_originals"] != float64(3) {
			t.Fatal("overflow hid omissions or claimed total mailbox coverage", row)
		}
	}
}

type conflictOutputWriter struct {
	bytes.Buffer
	writes, allowed int
	short           bool
}

func (w *conflictOutputWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes <= w.allowed {
		return w.Buffer.Write(p)
	}
	if w.short {
		return len(p) - 1, nil
	}
	return 0, errors.New("unregistered-writer-secret\x1b\n")
}

func TestReconciliationInjectedLegacyDiagnosticOutputFailuresRemainBlocked(t *testing.T) {
	for _, kind := range []string{"write", "short", "partial", "overflow-file", "overflow-symlink", "overflow-unsafe"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			before := seedConflictHistory(t, e, []string{"original-one", "original-two"}, "<shared@example.org>", "sender@example.org")
			cfg, err := os.ReadFile(config.DefaultPath())
			if err != nil {
				t.Fatal(err)
			}
			e.graph.server = map[string]serverMsg{"observed-id": {"inbox", "<shared@example.org>", "sender@example.org"}}
			installInjectedLegacyDiagnostic(t, e, before, "observed-id", "<shared@example.org>", "sender@example.org", "inbox")
			var stderr bytes.Buffer
			writer := &conflictOutputWriter{short: kind == "short"}
			args := []string{"--db", e.db, "account", "reconcile", "--json"}
			if strings.HasPrefix(kind, "overflow-") {
				writer.allowed = 10
				outdir := filepath.Join(t.TempDir(), "out")
				switch kind {
				case "overflow-file":
					err = os.WriteFile(outdir, []byte("preserved-canary"), 0600)
				case "overflow-symlink":
					err = os.Symlink(t.TempDir(), outdir)
				case "overflow-unsafe":
					err = os.Mkdir(outdir, 0700)
					if err == nil {
						err = os.Chmod(outdir, 0777)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv("MAILPIPE_OUT_DIR", outdir)
				args = append(args, "--limit", "1")
			}
			if kind == "partial" {
				writer.allowed = 1
			}
			root := cli.NewRootCommand(cli.Deps{Auth: func() (cli.Authenticator, error) { return e.auth, nil }, Store: store.Open, Provider: func(context.Context) (mailbox.Provider, error) { return e.provider, nil }, Graph: func(cli.Authenticator) cli.Graph { return e.graph }})
			root.SetOut(writer)
			root.SetErr(&stderr)
			root.SetArgs(args)
			err = root.ExecuteContext(context.Background())
			if cli.ExitCode(err) != agentio.ExitStale || !strings.Contains(stderr.String(), "diagnostic output incomplete") || strings.Contains(stderr.String(), "writer-secret") || strings.Contains(stderr.String(), "full result:") || strings.Contains(stderr.String(), "exit=0") {
				t.Fatalf("output failure lost block or echoed cause: %v %s", err, stderr.String())
			}
			if writer.Len() != 0 {
				conflictJSONRows(t, writer.String())
			}
			if kind == "overflow-file" {
				raw, err := os.ReadFile(agentio.OutDir())
				if err != nil || string(raw) != "preserved-canary" {
					t.Fatal("overflow replaced existing file", err)
				}
			}
			assertConflictPreservedHistory(t, e, before, cfg)
		})
	}
}

var _ io.Writer = (*conflictOutputWriter)(nil)

func TestReconciliationInjectedLegacyDiagnosticUntrustedMetadataAndSuppliedOmissions(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		for _, fieldsOmitted := range []bool{false, true} {
			t.Run(fmt.Sprintf("json=%t/omitted=%t", jsonMode, fieldsOmitted), func(t *testing.T) {
				e := newEnv(t)
				outdir := filepath.Join(t.TempDir(), "out")
				t.Setenv("MAILPIPE_OUT_DIR", outdir)
				secret := "invented-conflict-secret"
				agentio.RegisterSecret(secret)
				metadata := "\x1b[31m\u009b\u202e\t\n'$(echo untrusted)' " + secret + " https://user:invented-password@host.example.org/path"
				if fieldsOmitted {
					// Supply an omitted legacy field; the renderer must retain its
					// byte count without reconstructing or displaying the value.
					metadata = strings.Repeat("x", 1010) + secret
				}
				ids := []string{"original-one-" + metadata, "original-two-" + metadata}
				header, sender := "<"+metadata+"@example.org>", metadata+"@example.org"
				observed, folder := "observed-"+metadata, "folder-"+metadata
				before := seedConflictHistory(t, e, ids, header, sender)
				cfg, err := os.ReadFile(config.DefaultPath())
				if err != nil {
					t.Fatal(err)
				}
				e.graph.server = map[string]serverMsg{observed: {folder, header, sender}}
				report := installInjectedLegacyDiagnostic(t, e, before, observed, header, sender, folder)
				if fieldsOmitted {
					// Omission is fixture input, not a field-size or correlation policy.
					fields := []*graphmail.ReconciliationDiagnosticField{&report.ObservedMessage.ID, &report.ObservedMessage.InternetMessageID, &report.ObservedMessage.Sender, &report.ObservedMessage.CollectionID}
					for j := range report.Originals {
						m := &report.Originals[j].Message
						fields = append(fields, &m.ID, &m.InternetMessageID, &m.Sender)
					}
					for _, field := range fields {
						*field = graphmail.ReconciliationDiagnosticField{OmittedBytes: len(field.Value)}
					}
				}
				args := []string{"account", "reconcile", "--limit", "1"}
				if jsonMode {
					args = append(args, "--json")
				}
				code, out, stderr := e.run(args...)
				if code != agentio.ExitStale {
					t.Fatalf("metadata changed refusal: %d %s", code, stderr)
				}
				files, err := os.ReadDir(outdir)
				if err != nil || len(files) != 1 {
					t.Fatalf("overflow missing: %v %v", files, err)
				}
				raw, err := os.ReadFile(filepath.Join(outdir, files[0].Name()))
				if err != nil {
					t.Fatal(err)
				}
				for _, text := range []string{out, stderr, string(raw)} {
					if strings.Contains(text, secret) || strings.Contains(text, "invented-password") || fieldsOmitted && strings.Contains(text, strings.Repeat("x", 50)) {
						t.Fatal("diagnostics leaked a secret or omitted prefix")
					}
				}
				for _, control := range []string{"\x1b", "\u009b", "\u202e"} {
					if strings.Contains(out, control) || strings.Contains(stderr, control) {
						t.Fatalf("unsafe terminal control %q", control)
					}
				}
				if jsonMode {
					rows := conflictJSONRows(t, string(raw))
					for _, row := range rows {
						omitted := row["fields_omitted_bytes"].(map[string]any)
						if fieldsOmitted {
							for name, original := range map[string]string{"original_header": header, "original_sender": sender, "observed_id": observed, "observed_header": header, "observed_sender": sender, "observed_collection": "F-" + folder} {
								if row[name] != "" || omitted[name] != float64(len(original)) {
									t.Fatalf("simultaneous omission not explicit for %s: %+v", name, row)
								}
							}
							if row["original_id"] != "" || omitted["original_id"] == nil || len(omitted) != 7 {
								t.Fatal("original identity omission missing", row)
							}
						} else if row["observed_id"] != agentio.Redact(observed) || row["original_header"] != agentio.Redact(header) || len(omitted) != 0 {
							t.Fatal("JSON failed decoded-value/redaction contract", row)
						}
					}
				} else {
					lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
					if len(lines) != 3 {
						t.Fatalf("metadata forged TSV rows: %q", raw)
					}
					columns := len(strings.Split(lines[0], "\t"))
					for _, line := range lines[1:] {
						if len(strings.Split(line, "\t")) != columns {
							t.Fatal("metadata shifted TSV columns")
						}
					}
				}
				assertConflictPreservedHistory(t, e, before, cfg)
			})
		}
	}
}

// These tests inject legacy diagnostic payloads only to exercise rendering
// compatibility. The real exact-ID collector does not produce these payloads.
// This fixture applies no correlation selection, row cap or field-size policy.
type injectedLegacyDiagnosticProvider struct {
	*graphmail.Provider
	report *graphmail.ReconciliationConflict
}

func (p injectedLegacyDiagnosticProvider) ReconcileLegacyReferences(context.Context, []graphmail.LegacyReference) (graphmail.LegacyEvidence, error) {
	return graphmail.LegacyEvidence{}, &graphmail.ReconciliationError{Structural: true, Reason: "identity evidence requires review", Conflict: p.report}
}
func installInjectedLegacyDiagnostic(t *testing.T, e *env, originals []store.Message, observed, header, sender, folder string) *graphmail.ReconciliationConflict {
	t.Helper()
	subject, err := e.auth.Subject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, err := graphmail.New(e.graph, subject.Account())
	if err != nil {
		t.Fatal(err)
	}
	field := func(v string) graphmail.ReconciliationDiagnosticField {
		return graphmail.ReconciliationDiagnosticField{Value: v}
	}
	message := func(id, h, s, f string) graphmail.ReconciliationConflictMessage {
		return graphmail.ReconciliationConflictMessage{ID: field(id), InternetMessageID: field(h), Sender: field(s), CollectionID: field(f)}
	}
	c := &graphmail.ReconciliationConflict{Account: field(string(subject.Account().ID)), ObservedMessage: message(observed, header, sender, "F-"+folder), MatchingOriginals: len(originals), Started: time.Now().UTC(), ObservedAt: time.Now().UTC(), Pages: 1, Observed: 1}
	for _, m := range originals {
		c.Originals = append(c.Originals, graphmail.ReconciliationConflictOriginal{N: m.N, Message: message(m.GraphID, m.InternetMessageID, m.Sender, "F-"+m.Folder)})
	}
	e.provider = injectedLegacyDiagnosticProvider{Provider: p, report: c}
	return c
}
