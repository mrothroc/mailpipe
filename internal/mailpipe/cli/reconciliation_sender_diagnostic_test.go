package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/store"
)

func TestReconcileSenderAddressDiagnosticCommand(t *testing.T) {
	e := newEnv(t)
	seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
	e.graph.server = map[string]serverMsg{"original": {"inbox", "<original@example.org>", "sender@example.org"}}
	g := &senderNameGraph{senderCasingGraph: &senderCasingGraph{casingGraph: &casingGraph{fakeGraph: e.graph, listed: "reader@example.org", inspected: "reader@example.org"}, sender: "other@example.org"}}
	subject, err := e.auth.Subject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e.provider, err = graphmail.New(g, subject.Account())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "diagnostic.json")
	code, out, stderr := e.run("account", "reconcile", "--json", "--sender-address-diagnostic-out", path)
	if code != 1 || out != "" {
		t.Fatalf("diagnostic preview exit=%d stdout=%q stderr=%s", code, out, stderr)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing explicit private diagnostic: %v (%s)", err, stderr)
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["enumerated_sender_address"] != "sender@example.org" || doc["inspected_sender_address"] != "other@example.org" || doc["reconciliation_complete"] != false {
		t.Fatal("wrong raw observations/scope")
	}
	for _, v := range []string{"sender@example.org", "other@example.org", path, "--confirm-reconciliation"} {
		if strings.Contains(stderr, v) {
			t.Fatal("diagnostic leaked through normal error")
		}
	}
	if g.pages != 1 || g.batches != 1 || len(g.moved) != 0 || g.bodyCalls.Load() != 0 || g.rawCalls.Load() != 0 {
		t.Fatal("diagnostic changed reads/actions")
	}
	legacy, err := store.Open(e.db)
	if err != nil {
		t.Fatal("diagnostic bound history", err)
	}
	legacy.Close()
	if _, found, err := store.ReadLegacyReconciliationReceipt(context.Background(), e.db, subject.Account()); err != nil || found {
		t.Fatal("diagnostic produced receipt", err)
	}
}

func TestReconcileSenderDiagnosticControls(t *testing.T) {
	for _, kind := range []string{"no-flag", "success", "subject", "allowed-casing-with-subject", "revision", "hostile"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
			e.graph.server = map[string]serverMsg{"original": {"inbox", "<original@example.org>", "sender@example.org"}}
			sender := "sender@example.org"
			if kind == "no-flag" {
				sender = "other@example.org"
			}
			if kind == "hostile" {
				sender = "raw-secret\x1b\nhttps://host.example.org/private"
			}
			if kind == "allowed-casing-with-subject" {
				sender = "SENDER@example.org"
			}
			g := &senderNameGraph{senderCasingGraph: &senderCasingGraph{casingGraph: &casingGraph{fakeGraph: e.graph, listed: "reader@example.org", inspected: "reader@example.org"}, sender: sender}}
			if kind == "subject" || kind == "allowed-casing-with-subject" {
				g.edit = func(m map[string]any) { m["subject"] = "Changed" }
			}
			if kind == "revision" {
				g.edit = func(m map[string]any) { m["changeKey"] = "other" }
			}
			subject, err := e.auth.Subject(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			e.provider, err = graphmail.New(g, subject.Account())
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			os.Chmod(dir, 0700)
			path := filepath.Join(dir, "diagnostic.json")
			args := []string{"account", "reconcile", "--json"}
			if kind != "no-flag" {
				args = append(args, "--sender-address-diagnostic-out", path)
			}
			code, out, stderr := e.run(args...)
			if kind == "success" {
				if code != 0 || !strings.Contains(out, `"version":6`) {
					t.Fatalf("diagnostic option changed successful preview %d %s", code, stderr)
				}
				first := reconciliationRows(t, out)[0]["reconciliation_digest"]
				code, out, stderr = e.run("account", "reconcile", "--json")
				if code != 0 || reconciliationRows(t, out)[0]["reconciliation_digest"] != first {
					t.Fatal("diagnostic changed proposal digest")
				}
			} else if code == 0 || out != "" {
				t.Fatal("diagnostic changed rejection/authority")
			}
			raw, err := os.ReadFile(path)
			if kind == "hostile" {
				var doc map[string]any
				if err != nil || json.Unmarshal(raw, &doc) != nil || doc["inspected_sender_address"] != sender {
					t.Fatal("raw hostile text not preserved privately", err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("ineligible/successful preview wrote diagnostic", err)
			}
			for _, v := range []string{path, "raw-secret", "host.example.org", "\x1b"} {
				if strings.Contains(stderr, v) {
					t.Fatal("normal output leaked raw content/path")
				}
			}
			expected := 1
			if kind == "success" {
				expected = 2
			}
			if g.pages != expected || g.batches != expected || len(g.moved) != 0 || g.bodyCalls.Load() != 0 || g.rawCalls.Load() != 0 {
				t.Fatal("diagnostics expanded mailbox reads/actions")
			}
			legacy, err := store.Open(e.db)
			if err != nil {
				t.Fatal("diagnostic bound history", err)
			}
			legacy.Close()
		})
	}
}

func TestReconcileSenderDiagnosticRejectsCommittedReceiptOffline(t *testing.T) {
	e := newEnv(t)
	seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
	e.graph.server = map[string]serverMsg{"original": {"inbox", "<original@example.org>", "sender@example.org"}}
	code, out, stderr := e.run("account", "reconcile", "--json")
	if code != 0 {
		t.Fatal(stderr)
	}
	summary := reconciliationRows(t, out)[0]
	code, _, stderr = e.run("account", "reconcile", "--execute", "--confirm-account", summary["account"].(string), "--confirm-history", summary["source_digest"].(string), "--confirm-reconciliation", summary["reconciliation_digest"].(string))
	if code != 0 {
		t.Fatal(stderr)
	}
	e.auth.signedIn = false
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	path := filepath.Join(dir, "diagnostic.json")
	code, out, stderr = e.run("account", "reconcile", "--json", "--sender-address-diagnostic-out", path)
	if code != 2 || out != "" || !strings.Contains(stderr, "fresh unbound preview") {
		t.Fatalf("offline diagnostic not refused before auth: %d %s", code, stderr)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("offline receipt created raw diagnostic")
	}
	code, out, stderr = e.run("account", "reconcile", "--json", "--sender-address-diagnostic-out", path, "--sender-address-diagnostic-n", "1")
	if code != 2 || out != "" || !strings.Contains(stderr, "fresh unbound preview") {
		t.Fatalf("selected offline diagnostic not refused before auth: %d %s", code, stderr)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("selected offline receipt created raw diagnostic")
	}
}
