package cli_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type senderCasingGraph struct {
	*casingGraph
	sender string
}

func (g *senderCasingGraph) Batch(ctx context.Context, reqs []graph.BatchRequest) ([]graph.BatchResponse, error) {
	out, err := g.casingGraph.Batch(ctx, reqs)
	if err != nil {
		return nil, err
	}
	for i := range out {
		var m map[string]any
		if err = json.Unmarshal(out[i].Body, &m); err != nil {
			return nil, err
		}
		from := m["from"].(map[string]any)["emailAddress"].(map[string]any)
		from["address"] = g.sender
		out[i].Body, err = json.Marshal(m)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func TestReconcileExactSenderStateReview(t *testing.T) {
	for _, combined := range []bool{false, true} {
		t.Run(map[bool]string{false: "sender-only", true: "combined"}[combined], func(t *testing.T) {
			e := newEnv(t)
			seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
			e.graph.server = map[string]serverMsg{"original": {"inbox", "<original@example.org>", "sender@example.org"}}
			listed := "reader@example.org"
			if combined {
				listed = "Reader@Example.org"
			}
			g := &senderCasingGraph{casingGraph: &casingGraph{fakeGraph: e.graph, listed: listed, inspected: "reader@example.org"}, sender: "SENDER@Example.org"}
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
				t.Fatalf("sender preview: %d %s", code, stderr)
			}
			if strings.Contains(out, `"sender_casing_changed"`) || !strings.Contains(out, `"version":9`) || !strings.Contains(out, "SENDER@Example.org") {
				t.Fatal("missing raw sender review")
			}
			rows := reconciliationRows(t, out)
			if rows[1]["recipient_casing_changed"] != nil {
				t.Fatal("recipient flag was not independently derived")
			}
			if g.pages != 0 || g.batches != 1 || len(g.moved) != 0 {
				t.Fatal("sender policy changed read/actions")
			}
			legacy, err := store.Open(e.db)
			if err != nil {
				t.Fatal("preview bound history", err)
			}
			legacy.Close()
			summary := rows[0]
			args := []string{"account", "reconcile", "--execute", "--confirm-account", summary["account"].(string), "--confirm-history", summary["source_digest"].(string), "--confirm-reconciliation", summary["reconciliation_digest"].(string)}
			g.sender = "Sender@example.org"
			if code, out, stderr = e.run(args...); code != agentio.ExitStale || out != "" {
				t.Fatalf("raw sender change reused R: %d %s", code, stderr)
			}
			g.sender = "SENDER@Example.org"
			if code, _, stderr = e.run(args...); code != 0 {
				t.Fatalf("reviewed sender execute: %d %s", code, stderr)
			}
			b, err := store.OpenAccount(context.Background(), e.db, subject.Account())
			if err != nil {
				t.Fatal(err)
			}
			m, err := b.GetMessage(context.Background(), mailbox.LocalRef{AccountID: subject.Account().ID, N: int64(rows[1]["n"].(float64))})
			if err != nil || m.State.Metadata.Sender.Address != g.sender {
				t.Fatal("inspection sender not preserved exactly", err)
			}
			b.Close()
			e.auth.signedIn = false
			pages, batches := g.pages, g.batches
			if code, out, stderr = e.run("account", "reconcile", "--json"); code != 0 || !strings.Contains(out, "SENDER@Example.org") {
				t.Fatalf("offline sender receipt: %d %s", code, stderr)
			}
			if g.pages != pages || g.batches != batches || len(g.moved) != 0 || g.bodyCalls.Load() != 0 || g.rawCalls.Load() != 0 {
				t.Fatal("sender receipt/attribution added content/server actions")
			}
		})
	}
}

func TestReconcileSenderMismatchSafeFailure(t *testing.T) {
	for _, tc := range []struct{ label, sender, relation string }{{"whitespace", " sender@example.org ", "trim-only"}, {"joint", " SENDER@EXAMPLE.ORG ", "trim-and-case"}, {"alias", "sender+tag@example.org", "different"}, {"hostile", "invented-sender-secret\x1b\nhttps://host.example.org/private", "different"}} {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
			e.graph.server = map[string]serverMsg{"original": {"inbox", "<original@example.org>", "sender@example.org"}}
			g := &senderNameGraph{senderCasingGraph: &senderCasingGraph{casingGraph: &casingGraph{fakeGraph: e.graph, listed: "reader@example.org", inspected: "reader@example.org"}, sender: tc.sender}}
			fields := "fields=sender-address;"
			if tc.label == "whitespace" || tc.label == "joint" {
				g.edit = func(m map[string]any) { m["subject"] = "Changed invented subject" }
				fields = "fields=sender-address,subject;"
			}
			subject, err := e.auth.Subject(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			e.provider, err = graphmail.New(g, subject.Account())
			if err != nil {
				t.Fatal(err)
			}
			code, out, stderr := e.run(legacyDiagnosticArgs(t)...)
			if code != agentio.ExitRuntime || out != "" || !strings.Contains(stderr, fields) || !strings.Contains(stderr, "sender-address="+tc.relation+";") {
				t.Fatalf("missing bounded failed comparison: %d %s", code, stderr)
			}
			for _, value := range []string{tc.sender, "invented-sender-secret", "host.example.org", "original@example.org", e.db, "--confirm-reconciliation", "\x1b"} {
				if strings.Contains(stderr, value) {
					t.Fatal("failure leaked value or proposal authority")
				}
			}
			if len(stderr) > 2048 || g.pages != 1 || g.batches != 1 || len(g.moved) != 0 || g.bodyCalls.Load() != 0 || g.rawCalls.Load() != 0 {
				t.Fatal("failure added output/requests/actions")
			}
			s, err := store.Open(e.db)
			if err != nil {
				t.Fatal("failed preview bound history", err)
			}
			s.Close()
			if _, found, err := store.ReadLegacyReconciliationReceipt(context.Background(), e.db, subject.Account()); err != nil || found {
				t.Fatal("failed preview produced receipt", err)
			}
		})
	}
}
