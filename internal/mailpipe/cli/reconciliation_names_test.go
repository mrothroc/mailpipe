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
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type senderNameGraph struct {
	*senderCasingGraph
	name string
	edit func(map[string]any)
}

// Pin a recorded revision in both synthetic observations so revision changes
// exercise the actual optional precondition rather than an absent token.
func (g *senderNameGraph) Get(ctx context.Context, path string, q url.Values, out any) error {
	if path != "/me/messages" {
		return g.senderCasingGraph.Get(ctx, path, q, out)
	}
	var page map[string]any
	if err := g.senderCasingGraph.Get(ctx, path, q, &page); err != nil {
		return err
	}
	for _, v := range page["value"].([]any) {
		v.(map[string]any)["changeKey"] = "revision-1"
	}
	raw, err := json.Marshal(page)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
func (g *senderNameGraph) Batch(ctx context.Context, reqs []graph.BatchRequest) ([]graph.BatchResponse, error) {
	out, err := g.senderCasingGraph.Batch(ctx, reqs)
	if err != nil {
		return nil, err
	}
	for i := range out {
		var m map[string]any
		if err = json.Unmarshal(out[i].Body, &m); err != nil {
			return nil, err
		}
		m["from"].(map[string]any)["emailAddress"].(map[string]any)["name"] = g.name
		m["changeKey"] = "revision-1"
		if g.edit != nil {
			g.edit(m)
		}
		out[i].Body, err = json.Marshal(m)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func TestReconcileSenderNameReview(t *testing.T) {
	for _, combined := range []bool{false, true} {
		t.Run(map[bool]string{false: "name-only", true: "combined"}[combined], func(t *testing.T) {
			e := newEnv(t)
			seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
			e.graph.server = map[string]serverMsg{"original": {"inbox", "<original@example.org>", "sender@example.org"}}
			address, listed := "sender@example.org", "reader@example.org"
			if combined {
				address = "SENDER@Example.org"
				listed = "Reader@Example.org"
			}
			g := &senderNameGraph{senderCasingGraph: &senderCasingGraph{casingGraph: &casingGraph{fakeGraph: e.graph, listed: listed, inspected: "reader@example.org"}, sender: address}, name: "Current Display"}
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
				t.Fatalf("name preview: %d %s", code, stderr)
			}
			rows := reconciliationRows(t, out)
			item := rows[1]
			if item["sender_name_changed"] != true || item["sender_casing_changed"] != combined || item["recipient_casing_changed"] != combined || rows[0]["version"] != float64(8) {
				t.Fatal("presentation flags/version not derived independently")
			}
			if g.pages != 1 || g.batches != 1 || len(g.moved) != 0 {
				t.Fatal("name exception changed reads/actions")
			}
			legacy, err := store.Open(e.db)
			if err != nil {
				t.Fatal("preview bound history", err)
			}
			legacy.Close()
			summary := rows[0]
			args := []string{"account", "reconcile", "--execute", "--confirm-account", summary["account"].(string), "--confirm-history", summary["source_digest"].(string), "--confirm-reconciliation", summary["reconciliation_digest"].(string)}
			g.name = "Another Display"
			if code, out, stderr = e.run(args...); code != agentio.ExitStale || out != "" {
				t.Fatalf("changed name reused R: %d %s", code, stderr)
			}
			g.name = "Current Display"
			if code, _, stderr = e.run(args...); code != 0 {
				t.Fatalf("reviewed name binding: %d %s", code, stderr)
			}
			bound, err := store.OpenAccount(context.Background(), e.db, subject.Account())
			if err != nil {
				t.Fatal(err)
			}
			message, err := bound.GetMessage(context.Background(), mailbox.LocalRef{AccountID: subject.Account().ID, N: int64(item["n"].(float64))})
			if err != nil || message.State.Metadata.Sender.Name != g.name {
				t.Fatal("inspection name not published exactly", err)
			}
			bound.Close()
			e.auth.signedIn = false
			pages, batches := g.pages, g.batches
			if code, out, stderr = e.run("account", "reconcile", "--json"); code != 0 || !strings.Contains(out, `"sender_name_changed":true`) || !strings.Contains(out, "Current Display") {
				t.Fatalf("offline name receipt: %d %s", code, stderr)
			}
			if pages != g.pages || batches != g.batches || len(g.moved) != 0 || g.bodyCalls.Load() != 0 || g.rawCalls.Load() != 0 {
				t.Fatal("receipt/attribution added reads or mail actions")
			}

		})
	}
}

func TestReconcileSenderNameDoesNotMaskFailure(t *testing.T) {
	for _, kind := range []string{"recipient-name-and-preview", "address-content", "subject", "membership", "revision", "header", "id"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
			e.graph.server = map[string]serverMsg{"original": {"inbox", "<original@example.org>", "sender@example.org"}}
			const hostile = "invented-name-secret\x1b\nhttps://host.example.org/private"
			g := &senderNameGraph{senderCasingGraph: &senderCasingGraph{casingGraph: &casingGraph{fakeGraph: e.graph, listed: "reader@example.org", inspected: "reader@example.org"}, sender: "sender@example.org"}, name: hostile}
			g.edit = func(m map[string]any) {
				switch kind {
				case "recipient-name-and-preview":
					m["bodyPreview"] = "Changed"
					m["toRecipients"].([]any)[0].(map[string]any)["emailAddress"].(map[string]any)["name"] = "reader"
				case "address-content":
					m["from"].(map[string]any)["emailAddress"].(map[string]any)["address"] = "sender+tag@example.org"
				case "subject":
					m["subject"] = "Changed"
				case "membership":
					m["parentFolderId"] = "F-archive"
				case "revision":
					m["changeKey"] = "other"
				case "header":
					m["internetMessageId"] = "<other@example.org>"
				case "id":
					m["id"] = "other"
				}
			}
			subject, err := e.auth.Subject(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			e.provider, err = graphmail.New(g, subject.Account())
			if err != nil {
				t.Fatal(err)
			}
			code, out, stderr := e.run("account", "reconcile", "--json")
			if code == 0 || out != "" {
				t.Fatal("failed evidence emitted authority")
			}
			for _, value := range []string{"invented-name-secret", "host.example.org", e.db, "--confirm-reconciliation", "\x1b"} {
				if strings.Contains(stderr, value) {
					t.Fatal("failed name pair leaked data or confirmation")
				}
			}
			if len(stderr) > 2048 || g.pages != 1 || g.batches != 1 || len(g.moved) != 0 || g.bodyCalls.Load() != 0 || g.rawCalls.Load() != 0 {
				t.Fatal("failed name pair expanded output/reads/actions")
			}
			legacy, err := store.Open(e.db)
			if err != nil {
				t.Fatal("failed pair bound history", err)
			}
			legacy.Close()
			if _, found, err := store.ReadLegacyReconciliationReceipt(context.Background(), e.db, subject.Account()); err != nil || found {
				t.Fatal("failed pair produced receipt", err)
			}
		})
	}
}
