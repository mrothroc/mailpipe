package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type reconciliationReadFailureTransport struct {
	*fakeGraph
	phase          string
	change         string
	cause          error
	reads, batches int
}

func (g *reconciliationReadFailureTransport) Get(ctx context.Context, path string, query url.Values, out any) error {
	if path == "/me/messages" {
		g.reads++
		if g.phase == "enumeration" {
			return g.cause
		}
		if g.phase == "consistency" && g.change == "reference-appeared" {
			raw, _ := json.Marshal(map[string]any{"value": []any{}})
			return json.Unmarshal(raw, out)
		}
	}
	return g.fakeGraph.Get(ctx, path, query, out)
}

func (g *reconciliationReadFailureTransport) Batch(ctx context.Context, requests []graph.BatchRequest) ([]graph.BatchResponse, error) {
	g.batches++
	if g.phase == "inspection" {
		return nil, g.cause
	}
	out, err := g.fakeGraph.Batch(ctx, requests)
	if err == nil && g.phase == "consistency" {
		var wire map[string]any
		if err = json.Unmarshal(out[0].Body, &wire); err != nil {
			return nil, err
		}
		switch g.change {
		case "precondition-changed":
			wire["parentFolderId"] = "different-collection"
		case "metadata-changed":
			wire["subject"] = "Changed invented metadata"
		}
		out[0].Body, err = json.Marshal(wire)
	}
	return out, err
}

func TestReconciliationReadFailureCategoriesReachCommandOutput(t *testing.T) {
	const hostile = "unregistered-diagnostic-secret\n\x1b[31m https://host.example.org/private"
	for _, tc := range []struct {
		phase, category string
		cause           error
	}{
		{"enumeration", "deadline", context.DeadlineExceeded},
		{"enumeration", "canceled", context.Canceled},
		{"enumeration", "read-failure", io.ErrUnexpectedEOF},
		{"enumeration", "authorization-http-401", &graph.APIError{Status: 401, Code: "untrusted-code", Message: hostile}},
		{"enumeration", "authorization-http-403", &graph.APIError{Status: 403, Code: "untrusted-code", Message: hostile}},
		{"inspection", "unknown-api-response", &graph.APIError{Status: 799, Code: "untrusted-code", Message: hostile}},
		{"enumeration", "unknown-read-failure", errors.New(hostile)},
		{"inspection", "throttled-http-429", &graph.APIError{Status: 429, Code: "untrusted-code", Message: hostile}},
		{"inspection", "not-found-http-404", &graph.APIError{Status: 404, Code: "ErrorItemNotFound", Message: hostile}},
		{"consistency", "metadata-changed", nil},
		{"consistency", "precondition-changed", nil},
		{"consistency", "reference-appeared", nil},
	} {
		t.Run(tc.phase+"/"+tc.category, func(t *testing.T) {
			e := newEnv(t)
			seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
			e.graph.server = map[string]serverMsg{"original": {"inbox", "<original@example.org>", "sender@example.org"}}
			tr := &reconciliationReadFailureTransport{fakeGraph: e.graph, phase: tc.phase, change: tc.category, cause: tc.cause}
			subject, err := e.auth.Subject(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			e.provider, err = graphmail.New(tr, subject.Account())
			if err != nil {
				t.Fatal(err)
			}
			code, out, stderr := e.run("account", "reconcile", "--json")
			if code != agentio.ExitRuntime || out != "" {
				t.Fatalf("failed evidence produced success/output: %d %q %s", code, out, stderr)
			}
			if !strings.Contains(stderr, "phase="+tc.phase) || !strings.Contains(stderr, "category="+tc.category) {
				t.Fatalf("missing actionable category: %s", stderr)
			}
			for _, private := range []string{"unregistered-diagnostic-secret", "untrusted-code", "ErrorItemNotFound", "host.example.org", "original@example.org", "F-inbox", e.db, "reconciliation_digest", "--confirm-"} {
				if strings.Contains(stderr, private) {
					t.Fatal("failure leaked private data or proposal authority")
				}
			}
			if tr.reads != 1 || (tc.phase == "enumeration" && tr.batches != 0) || (tc.phase != "enumeration" && tr.batches != 1) {
				t.Fatal("diagnosis added mailbox requests")
			}
			s, err := store.Open(e.db)
			if err != nil {
				t.Fatal("failure attributed history", err)
			}
			s.Close()
			if _, found, err := store.ReadLegacyReconciliationReceipt(context.Background(), e.db, subject.Account()); err != nil || found {
				t.Fatal("failure produced receipt", err)
			}
			if len(e.graph.moved) != 0 || e.graph.bodyCalls.Load() != 0 || e.graph.rawCalls.Load() != 0 {
				t.Fatal("failure requested content or mutations")
			}
		})
	}
}
