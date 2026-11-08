package cli_test

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type provenanceGraph struct {
	*senderNameGraph
	originMode string
}

func provenanceWire(m map[string]any, enumerated bool) {
	from, sender := "sender@example.org", "inspected-agent@example.org"
	if enumerated {
		from, sender = "enumerated-from@example.org", "enumerated-agent@example.org"
	}
	m["from"] = map[string]any{"emailAddress": map[string]string{"address": from, "name": "Unexported From Name"}}
	m["sender"] = map[string]any{"emailAddress": map[string]string{"address": sender, "name": "Unexported Sender Name"}}
	m["toRecipients"] = []any{map[string]any{"emailAddress": map[string]string{"address": "to@example.org", "name": "Unexported Recipient"}}}
	m["ccRecipients"] = []any{map[string]any{"emailAddress": map[string]string{"address": "cc@example.org", "name": "Unexported Recipient"}}}
}
func (g *provenanceGraph) Get(ctx context.Context, path string, q url.Values, out any) error {
	if path != "/me/messages" {
		return g.senderNameGraph.Get(ctx, path, q, out)
	}
	var page map[string]any
	if e := g.senderNameGraph.Get(ctx, path, q, &page); e != nil {
		return e
	}
	for _, item := range page["value"].([]any) {
		m := item.(map[string]any)
		provenanceWire(m, true)
		if g.originMode == "enumeration" {
			m["From"] = m["from"]
			delete(m, "from")
		}
	}
	raw, e := json.Marshal(page)
	if e != nil {
		return e
	}
	return json.Unmarshal(raw, out)
}
func (g *provenanceGraph) Batch(ctx context.Context, requests []graph.BatchRequest) ([]graph.BatchResponse, error) {
	out, e := g.senderNameGraph.Batch(ctx, requests)
	if e != nil {
		return nil, e
	}
	for i := range out {
		var m map[string]any
		if e = json.Unmarshal(out[i].Body, &m); e != nil {
			return nil, e
		}
		provenanceWire(m, false)
		if g.originMode == "inspection" {
			m["From"] = m["from"]
			delete(m, "from")
		}
		out[i].Body, e = json.Marshal(m)
		if e != nil {
			return nil, e
		}
	}
	return out, nil
}
func TestReconcileProvenanceCommand(t *testing.T) {
	e := newEnv(t)
	seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
	e.graph.server = map[string]serverMsg{"original": {"inbox", "<original@example.org>", "sender@example.org"}}
	g := &provenanceGraph{senderNameGraph: &senderNameGraph{senderCasingGraph: &senderCasingGraph{casingGraph: &casingGraph{fakeGraph: e.graph, listed: "to@example.org", inspected: "to@example.org"}, sender: "sender@example.org"}}}
	g.edit = func(m map[string]any) { m["subject"] = "Changed invented subject" }
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
	path := filepath.Join(dir, "origin.json")
	code, out, stderr := e.run("account", "reconcile", "--json", "--sender-address-diagnostic-out", path)
	if code != 1 || out != "" {
		t.Fatalf("provenance changed strict rejection %d %s", code, stderr)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("missing private diagnostic", err)
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["format"] != float64(3) {
		t.Fatalf("mapped-only report cannot establish raw provenance: format=%v", doc["format"])
	}
	for _, key := range []string{"enumerated_origin", "inspected_origin"} {
		o, ok := doc[key].(map[string]any)
		if !ok || o["selected_source"] != "from" || o["mapped_sender_matches_wire"] != true || o["recipients_match_wire"] != true {
			t.Fatal("missing raw source classification")
		}
	}
	for _, v := range []string{"Unexported", "to@example.org", "cc@example.org"} {
		if strings.Contains(string(raw), v) {
			t.Fatal("raw diagnostic exported unrelated fields")
		}
	}
	for _, v := range []string{path, "@example.org", "Unexported", "--confirm-reconciliation"} {
		if strings.Contains(stderr, v) {
			t.Fatal("private provenance leaked or granted authority")
		}
	}
	if g.pages != 1 || g.batches != 1 || len(g.moved) != 0 || g.bodyCalls.Load() != 0 || g.rawCalls.Load() != 0 {
		t.Fatal("provenance changed read/action count")
	}
	if _, found, err := store.ReadLegacyReconciliationReceipt(context.Background(), e.db, subject.Account()); err != nil || found {
		t.Fatal("provenance created receipt", err)
	}
}

func TestReconcileUnsupportedProvenanceHasNoAuthority(t *testing.T) {
	for _, phase := range []string{"enumeration", "inspection"} {
		t.Run(phase, func(t *testing.T) {
			e := newEnv(t)
			seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
			e.graph.server = map[string]serverMsg{"original": {"inbox", "<original@example.org>", "sender@example.org"}}
			g := &provenanceGraph{senderNameGraph: &senderNameGraph{senderCasingGraph: &senderCasingGraph{casingGraph: &casingGraph{fakeGraph: e.graph, listed: "to@example.org", inspected: "to@example.org"}, sender: "sender@example.org"}}, originMode: phase}
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
			path := filepath.Join(dir, "origin.json")
			code, out, stderr := e.run("account", "reconcile", "--json", "--sender-address-diagnostic-out", path)
			if code != 1 || out != "" || !strings.Contains(stderr, "phase="+phase+"; category=unsupported-provenance") || strings.Contains(stderr, "saved privately") {
				t.Fatalf("unsupported origin produced authority: %d %s", code, stderr)
			}
			if _, err = os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("unsupported source produced private claim")
			}
			for _, v := range []string{"@example.org", path, "Unexported", "--confirm-reconciliation"} {
				if strings.Contains(stderr, v) {
					t.Fatal("unsupported source leaked values/authority")
				}
			}
			expectedBatches := 0
			if phase == "inspection" {
				expectedBatches = 1
			}
			if g.pages != 1 || g.batches != expectedBatches || len(g.moved) != 0 || g.bodyCalls.Load() != 0 || g.rawCalls.Load() != 0 {
				t.Fatal("unsupported origin added reads/actions")
			}
			if _, found, err := store.ReadLegacyReconciliationReceipt(context.Background(), e.db, subject.Account()); err != nil || found {
				t.Fatal("unsupported source created receipt", err)
			}
		})
	}
}
