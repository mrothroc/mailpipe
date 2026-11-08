package cli_test

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type casingGraph struct {
	*fakeGraph
	listed, inspected string
	pages, batches    int
}

func casingWire(address string) []any {
	return []any{map[string]any{"emailAddress": map[string]string{"address": address, "name": "Reader"}}}
}

func (g *casingGraph) Get(ctx context.Context, path string, q url.Values, out any) error {
	if path != "/me/messages" {
		return g.fakeGraph.Get(ctx, path, q, out)
	}
	g.pages++
	var page map[string]any
	if err := g.fakeGraph.Get(ctx, path, q, &page); err != nil {
		return err
	}
	for _, value := range page["value"].([]any) {
		value.(map[string]any)["toRecipients"] = casingWire(g.listed)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (g *casingGraph) Batch(ctx context.Context, requests []graph.BatchRequest) ([]graph.BatchResponse, error) {
	g.batches++
	responses, err := g.fakeGraph.Batch(ctx, requests)
	if err != nil {
		return nil, err
	}
	for i := range responses {
		var message map[string]any
		if err = json.Unmarshal(responses[i].Body, &message); err != nil {
			return nil, err
		}
		message["toRecipients"] = casingWire(g.inspected)
		responses[i].Body, err = json.Marshal(message)
		if err != nil {
			return nil, err
		}
	}
	return responses, nil
}

func TestReconcileCasingReview(t *testing.T) {
	e := newEnv(t)
	seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
	e.graph.server = map[string]serverMsg{"original": {"inbox", "<original@example.org>", "sender@example.org"}}
	g := &casingGraph{fakeGraph: e.graph, listed: "Reader@Example.org", inspected: "reader@example.org"}
	subject, err := e.auth.Subject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e.provider, err = graphmail.New(g, subject.Account())
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr := e.run("account", "reconcile", "--json")
	if code != 0 {
		t.Fatalf("casing preview: %d %s", code, stderr)
	}
	if !strings.Contains(out, `"recipient_casing_changed":true`) || !strings.Contains(out, `"enumerated_state":`) || !strings.Contains(out, "Reader@Example.org") || !strings.Contains(out, "reader@example.org") || !strings.Contains(out, `"version":7`) {
		t.Fatal("missing exact casing review evidence")
	}
	if g.pages != 1 || g.batches != 1 || len(g.moved) != 0 {
		t.Fatal("casing policy added requests or mutations")
	}
	legacy, err := store.Open(e.db)
	if err != nil {
		t.Fatal("preview attributed history", err)
	}
	legacy.Close()
	summary := reconciliationRows(t, out)[0]
	args := []string{"account", "reconcile", "--execute", "--confirm-account", summary["account"].(string), "--confirm-history", summary["source_digest"].(string), "--confirm-reconciliation", summary["reconciliation_digest"].(string)}
	g.inspected = "READER@example.org"
	if code, out, stderr = e.run(args...); code != agentio.ExitStale || out != "" {
		t.Fatalf("raw casing changed without fresh review: %d %s", code, stderr)
	}
	g.inspected = "reader@example.org"
	if code, _, stderr = e.run(args...); code != 0 {
		t.Fatalf("reviewed casing execute: %d %s", code, stderr)
	}
	e.auth.signedIn = false
	pages, batches := g.pages, g.batches
	if code, out, stderr = e.run("account", "reconcile", "--json"); code != 0 || !strings.Contains(out, "Reader@Example.org") {
		t.Fatalf("offline pair: %d %s", code, stderr)
	}
	if g.pages != pages || g.batches != batches || len(g.moved) != 0 || g.bodyCalls.Load() != 0 || g.rawCalls.Load() != 0 {
		t.Fatal("receipt read or casing attribution added remote actions/content")
	}
}

func TestReconcileCasingWhitespaceStillFailsWithoutProposal(t *testing.T) {
	e := newEnv(t)
	seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
	e.graph.server = map[string]serverMsg{"original": {"inbox", "<original@example.org>", "sender@example.org"}}
	g := &casingGraph{fakeGraph: e.graph, listed: "Reader@Example.org", inspected: " reader@example.org "}
	subject, err := e.auth.Subject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e.provider, err = graphmail.New(g, subject.Account())
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr := e.run("account", "reconcile", "--json")
	if code != agentio.ExitRuntime || out != "" || !strings.Contains(stderr, "category=metadata-changed") || strings.Contains(stderr, "--confirm-reconciliation") {
		t.Fatalf("noncasing mismatch emitted authority: %d %s", code, stderr)
	}
	if g.pages != 1 || g.batches != 1 || len(g.moved) != 0 {
		t.Fatal("failed mismatch added requests")
	}
	legacy, err := store.Open(e.db)
	if err != nil {
		t.Fatal("failed preview bound store", err)
	}
	legacy.Close()
	if _, found, err := store.ReadLegacyReconciliationReceipt(context.Background(), e.db, subject.Account()); err != nil || found {
		t.Fatal("failed preview wrote receipt", err)
	}
}
