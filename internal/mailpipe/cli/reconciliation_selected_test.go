package cli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type selectedOriginalGraph struct {
	*provenanceGraph
	allEqual bool
}

func (g *selectedOriginalGraph) Get(ctx context.Context, path string, q url.Values, out any) error {
	if path != "/me/messages" {
		return g.provenanceGraph.Get(ctx, path, q, out)
	}
	var page map[string]any
	if err := g.provenanceGraph.Get(ctx, path, q, &page); err != nil {
		return err
	}
	for _, v := range page["value"].([]any) {
		m := v.(map[string]any)
		if g.allEqual || m["id"] == "first" {
			m["from"] = map[string]any{"emailAddress": map[string]string{"address": "sender@example.org", "name": "Unexported From Name"}}
		}
		if !g.allEqual && m["id"] == "first" {
			m["subject"] = "An unrelated invented mismatch"
		}
	}
	raw, err := json.Marshal(page)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func TestReconcileSelectedOriginalCommand(t *testing.T) {
	e := newEnv(t)
	rows := seedConflictHistory(t, e, []string{"first", "target"}, "<original@example.org>", "sender@example.org")
	e.graph.server = map[string]serverMsg{"first": {"inbox", "<original@example.org>", "sender@example.org"}, "target": {"inbox", "<original@example.org>", "sender@example.org"}}
	g := &selectedOriginalGraph{provenanceGraph: &provenanceGraph{senderNameGraph: &senderNameGraph{senderCasingGraph: &senderCasingGraph{casingGraph: &casingGraph{fakeGraph: e.graph, listed: "to@example.org", inspected: "to@example.org"}, sender: "sender@example.org"}}}}
	subject, err := e.auth.Subject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e.provider, err = graphmail.New(g, subject.Account())
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, row := range rows {
		if row.GraphID == "target" {
			n = row.N
		}
	}
	dir := t.TempDir()
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "selected.json")
	code, out, stderr := e.run("account", "reconcile", "--json", "--sender-address-diagnostic-out", path)
	if code != 1 || out != "" || !strings.Contains(stderr, "fields=subject") {
		t.Fatalf("default first failure changed: %d %s", code, stderr)
	}
	if _, err = os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("first failure exported a later pair")
	}
	code, out, stderr = e.run("account", "reconcile", "--json", "--sender-address-diagnostic-out", path, "--sender-address-diagnostic-n", fmt.Sprint(n))
	if code != 1 || out != "" {
		t.Fatalf("selected observation must exit runtime1/empty stdout: %d %s", code, stderr)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("supported command cannot diagnose selected later original", err)
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["format"] != float64(3) || doc["diagnostic_scope"] != "requested_original_observation" || doc["status"] != "diagnostic_only" || doc["describes_first_failure"] != false || doc["admission_evaluated"] != false || doc["historical_identity_validated"] != false || doc["reconciliation_complete"] != false || doc["requested_immutable_id"] != "target" || doc["n"] != float64(n) {
		t.Fatal("diagnostic claimed first failure or validation", string(raw))
	}
	for _, s := range []string{"@example.org", path, "Unexported", "--confirm-reconciliation"} {
		if strings.Contains(out+stderr, s) {
			t.Fatal("private value/authority leaked")
		}
	}
	if g.pages != 2 || g.batches != 2 || len(g.moved) != 0 || g.bodyCalls.Load() != 0 || g.rawCalls.Load() != 0 {
		t.Fatal("selected observation changed read/effect counts")
	}
	if _, found, err := store.ReadLegacyReconciliationReceipt(context.Background(), e.db, subject.Account()); err != nil || found {
		t.Fatal("selected observation created receipt", err)
	}
}

func TestReconcileSelectedEqualCommand(t *testing.T) {
	e := newEnv(t)
	rows := seedConflictHistory(t, e, []string{"target"}, "<original@example.org>", "sender@example.org")
	e.graph.server = map[string]serverMsg{"target": {"inbox", "<original@example.org>", "sender@example.org"}}
	g := &selectedOriginalGraph{provenanceGraph: &provenanceGraph{senderNameGraph: &senderNameGraph{senderCasingGraph: &senderCasingGraph{casingGraph: &casingGraph{fakeGraph: e.graph, listed: "to@example.org", inspected: "to@example.org"}, sender: "sender@example.org"}}}, allEqual: true}
	subject, err := e.auth.Subject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e.provider, err = graphmail.New(g, subject.Account())
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr := e.run("account", "reconcile", "--json")
	if code != 0 || !strings.Contains(out, "reconciliation_digest") {
		t.Fatalf("normal v5 preview regressed: %d %s", code, stderr)
	}
	dir := t.TempDir()
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "equal.json")
	code, out, stderr = e.run("account", "reconcile", "--json", "--sender-address-diagnostic-out", path, "--sender-address-diagnostic-n", fmt.Sprint(rows[0].N))
	if code != 1 || out != "" || !strings.Contains(stderr, "saved privately") || strings.Contains(stderr, "--confirm-") {
		t.Fatalf("equal diagnostic acquired authority: %d %s", code, stderr)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["enumerated_sender_address"] != doc["inspected_sender_address"] || doc["status"] != "diagnostic_only" || doc["admission_evaluated"] != false {
		t.Fatal("equal diagnostic mislabelled")
	}
	if g.pages != 1 || g.batches != 2 || g.bodyCalls.Load() != 0 || g.rawCalls.Load() != 0 || len(g.moved) != 0 {
		t.Fatal("equal diagnostic changed effects")
	}
	if _, found, err := store.ReadLegacyReconciliationReceipt(context.Background(), e.db, subject.Account()); err != nil || found {
		t.Fatal("equal diagnostic bound history", err)
	}
}
