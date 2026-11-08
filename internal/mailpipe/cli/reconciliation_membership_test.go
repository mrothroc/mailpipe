package cli_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/store"
)

func reconciliationRows(t *testing.T, text string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err, text)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestReconciliationReviewsDistinctIDsAndMembershipChanges(t *testing.T) {
	e := newEnv(t)
	seedConflictHistory(t, e, []string{"first", "second"}, "<shared@example.org>", "sender@example.org")
	e.graph.server = map[string]serverMsg{"first": {"archive", "<shared@example.org>", "sender@example.org"}, "second": {"inbox", "<shared@example.org>", "sender@example.org"}}
	if code, _, stderr := e.run("account", "bind"); code != agentio.ExitStale {
		t.Fatalf("strict bind accepted moved collection: %d %s", code, stderr)
	}
	code, out, stderr := e.run("account", "reconcile", "--json")
	if code != 0 {
		t.Fatalf("exact originals blocked: %d %s", code, stderr)
	}
	rows := reconciliationRows(t, out)
	if len(rows) != 3 {
		t.Fatal("distinct IDs merged", out)
	}
	summary := rows[0]
	for _, row := range rows[1:] {
		if row["disposition"] != "verified" || row["original_folder"] != "inbox" || row["version"] != float64(7) {
			t.Fatal("incomplete original evidence", row)
		}
		old := row["original_memberships"].([]any)[0].(map[string]any)["Collection"].(map[string]any)["CollectionID"]
		current := row["state"].(map[string]any)["Memberships"].([]any)[0].(map[string]any)["Collection"].(map[string]any)["CollectionID"]
		if old != "F-inbox" {
			t.Fatal("old collection replaced", row)
		}
		if row["original_id"] == "first" {
			if current != "F-archive" || row["membership_changed"] != true {
				t.Fatal("move not explicit", row)
			}
		} else if current != "F-inbox" || row["membership_changed"] != false {
			t.Fatal("unchanged membership not explicit", row)
		}
	}
	args := []string{"account", "reconcile", "--execute", "--confirm-account", summary["account"].(string), "--confirm-history", summary["source_digest"].(string), "--confirm-reconciliation", summary["reconciliation_digest"].(string)}
	e.graph.server["first"] = serverMsg{"junkemail", "<shared@example.org>", "sender@example.org"}
	if code, _, stderr = e.run(args...); code != agentio.ExitStale {
		t.Fatalf("stale membership proposal executed: %d %s", code, stderr)
	}
	s, err := store.Open(e.db)
	if err != nil {
		t.Fatal("failed proposal bound history", err)
	}
	s.Close()
	e.graph.server["first"] = serverMsg{"archive", "<shared@example.org>", "sender@example.org"}
	if code, _, stderr = e.run(args...); code != 0 {
		t.Fatalf("reviewed membership rejected: %d %s", code, stderr)
	}
	e.auth.signedIn = false
	if code, out, stderr = e.run("account", "reconcile", "--json"); code != 0 || len(reconciliationRows(t, out)) != 3 {
		t.Fatalf("receipt not offline: %d %s", code, stderr)
	}
	if len(e.graph.moved) != 0 || e.graph.bodyCalls.Load() != 0 || e.graph.rawCalls.Load() != 0 {
		t.Fatal("reconciliation mutated mail or read content")
	}
}

// Reuse the source-validated noncase sender fixture, with independent sender
// name and recipient address casing evidence retained in the old v6 item.
type committedV6Graph struct{ *provenanceGraph }

func committedV6Presentation(m map[string]any, enumerated bool) {
	name, address := "Current Display", "to@example.org"
	if enumerated {
		name, address = "Original Display", "To@Example.org"
	}
	m["from"].(map[string]any)["emailAddress"].(map[string]any)["name"] = name
	m["toRecipients"].([]any)[0].(map[string]any)["emailAddress"].(map[string]any)["address"] = address
}
func (g *committedV6Graph) Get(ctx context.Context, path string, q url.Values, out any) error {
	if path != "/me/messages" {
		return g.provenanceGraph.Get(ctx, path, q, out)
	}
	var page map[string]any
	if err := g.provenanceGraph.Get(ctx, path, q, &page); err != nil {
		return err
	}
	for _, v := range page["value"].([]any) {
		committedV6Presentation(v.(map[string]any), true)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
func (g *committedV6Graph) Batch(ctx context.Context, reqs []graph.BatchRequest) ([]graph.BatchResponse, error) {
	out, err := g.provenanceGraph.Batch(ctx, reqs)
	if err != nil {
		return nil, err
	}
	for i := range out {
		var m map[string]any
		if err = json.Unmarshal(out[i].Body, &m); err != nil {
			return nil, err
		}
		committedV6Presentation(m, false)
		out[i].Body, err = json.Marshal(m)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func assertCommittedV6SenderEvidence(t *testing.T, item map[string]any) {
	t.Helper()
	for key, want := range map[string]bool{"sender_address_disagrees": true, "sender_casing_changed": false, "sender_name_changed": true, "recipient_casing_changed": true} {
		if item[key] != want {
			t.Fatalf("v6 %s not retained", key)
		}
	}
	for _, side := range []struct{ key, address, name, recipient string }{
		{"enumerated_state", "enumerated-from@example.org", "Original Display", "To@Example.org"},
		{"state", "sender@example.org", "Current Display", "to@example.org"},
	} {
		state, ok := item[side.key].(map[string]any)
		if !ok {
			t.Fatal("missing complete v6 state")
		}
		metadata := state["Metadata"].(map[string]any)
		sender := metadata["Sender"].(map[string]any)
		recipients := metadata["Recipients"].([]any)
		if sender["Address"] != side.address || sender["Name"] != side.name || len(recipients) != 2 || recipients[0].(map[string]any)["Address"] != side.recipient || recipients[0].(map[string]any)["Name"] != "Unexported Recipient" || recipients[1].(map[string]any)["Address"] != "cc@example.org" {
			t.Fatal("v6 raw observation pair lost")
		}
	}
}

func TestReconciliationOldReceiptsOfflineAndRoutingRepair(t *testing.T) {
	for _, version := range []int{1, 2, 3, 4, 5, 6} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			e := newEnv(t)
			sender := "info@campaign.example.org"
			if version == 6 {
				sender = "sender@example.org"
				e.graph.server = map[string]serverMsg{"g1": {"inbox", "<g1@example.org>", sender}}
			}
			seedConflictHistory(t, e, []string{"g1"}, "<g1@example.org>", sender)
			var v6 *committedV6Graph
			if version >= 4 {
				subject, err := e.auth.Subject(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				g := &senderCasingGraph{casingGraph: &casingGraph{fakeGraph: e.graph, listed: "Reader@Example.org", inspected: "reader@example.org"}, sender: "INFO@CAMPAIGN.EXAMPLE.ORG"}
				if version == 6 {
					v6 = &committedV6Graph{provenanceGraph: &provenanceGraph{senderNameGraph: &senderNameGraph{senderCasingGraph: g}}}
					e.provider, err = graphmail.New(v6, subject.Account())
				} else if version >= 5 {
					e.provider, err = graphmail.New(&senderNameGraph{senderCasingGraph: g, name: "Current Display"}, subject.Account())
				} else {
					e.provider, err = graphmail.New(g, subject.Account())
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			code, out, stderr := e.run("account", "reconcile", "--json")
			if code != 0 {
				t.Fatal(stderr)
			}
			summary := reconciliationRows(t, out)[0]
			args := []string{"account", "reconcile", "--execute", "--confirm-account", summary["account"].(string), "--confirm-history", summary["source_digest"].(string), "--confirm-reconciliation", summary["reconciliation_digest"].(string)}
			if version == 6 {
				args = append([]string{"account", "reconcile", "--json"}, args[2:]...)
			}
			if code, _, stderr = e.run(args...); code != 0 {
				t.Fatal(stderr)
			}
			// Construct an old stored receipt only in this isolated synthetic fixture.
			// The product has no receipt import or rewrite path.
			db, err := sql.Open("sqlite", "file:"+e.db)
			if err != nil {
				t.Fatal(err)
			}
			var raw string
			if err = db.QueryRow("select receipt from legacy_reconciliations").Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var receipt map[string]any
			if err = json.Unmarshal([]byte(raw), &receipt); err != nil {
				t.Fatal(err)
			}
			receipt["version"], receipt["reconciliation_digest"] = version, "stored-old-proposal"
			encoded, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("update legacy_reconciliations set receipt=?,reconciliation_digest=?", string(encoded), "stored-old-proposal"); err != nil {
				t.Fatal(err)
			}
			var item string
			if err = db.QueryRow("select item from legacy_reconciliation_items").Scan(&item); err != nil {
				t.Fatal(err)
			}
			var oldItem map[string]any
			if err = json.Unmarshal([]byte(item), &oldItem); err != nil {
				t.Fatal(err)
			}
			keys := []string{"recipient_name_changed"}
			if version < 6 {
				keys = append(keys, "sender_address_disagrees")
			}
			if version < 5 {
				keys = append(keys, "sender_name_changed")
			}
			if version < 4 {
				keys = append(keys, "sender_casing_changed")
			}
			if version < 3 {
				keys = append(keys, "recipient_casing_changed", "enumerated_state")
			}
			if version == 1 {
				keys = append(keys, "original_folder", "original_memberships", "membership_changed")
			}
			for _, key := range keys {
				delete(oldItem, key)
			}
			encoded, err = json.Marshal(oldItem)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("update legacy_reconciliation_items set item=?", string(encoded)); err != nil {
				t.Fatal(err)
			}
			frozenItem := string(encoded)
			if version == 6 {
				assertCommittedV6SenderEvidence(t, oldItem)
				if _, found := oldItem["recipient_name_changed"]; found {
					t.Fatal("new flag entered frozen v6 item")
				}
			}
			var frozenReceipt string
			if err = db.QueryRow("select receipt from legacy_reconciliations").Scan(&frozenReceipt); err != nil {
				t.Fatal(err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			pages, batches := 0, 0
			if v6 != nil {
				pages, batches = v6.pages, v6.batches
			}
			e.auth.signedIn = false
			if code, out, stderr = e.run("account", "reconcile", "--json"); code != 0 || reconciliationRows(t, out)[0]["version"] != float64(version) {
				t.Fatalf("old receipt requires auth: %d %s", code, stderr)
			}
			if version == 6 {
				assertCommittedV6SenderEvidence(t, reconciliationRows(t, out)[1])
				if v6.pages != pages || v6.batches != batches {
					t.Fatal("offline v6 receipt performed reads")
				}
			}
			e.auth.signedIn = true
			cfg, err := config.Load(config.DefaultPath())
			if err != nil {
				t.Fatal(err)
			}
			cfg.AccountID = ""
			if err = config.Save(config.DefaultPath(), cfg); err != nil {
				t.Fatal(err)
			}
			e.graph.pagesErr = sql.ErrConnDone // committed-receipt repair must not enumerate
			if code, _, _ = e.run(args...); code == 0 {
				t.Fatal("wrong committed receipt confirmation accepted")
			}
			args[len(args)-1] = "stored-old-proposal"
			if code, out, stderr = e.run(args...); code != 0 {
				t.Fatalf("v1 routing repair replayed binding: %d %s", code, stderr)
			}
			if version == 6 {
				assertCommittedV6SenderEvidence(t, reconciliationRows(t, out)[1])
				if v6.pages != pages || v6.batches != batches || v6.bodyCalls.Load() != 0 || v6.rawCalls.Load() != 0 {
					t.Fatal("v6 receipt repair read mailbox/content")
				}
			}
			cfg, err = config.Load(config.DefaultPath())
			if err != nil || cfg.AccountID != summary["account"] {
				t.Fatal("same-account routing not repaired", err)
			}
			if len(e.graph.moved) != 0 {
				t.Fatal("routing repair mutated mail")
			}
			db, err = sql.Open("sqlite", "file:"+e.db)
			if err != nil {
				t.Fatal(err)
			}
			var afterItem, afterReceipt string
			if err = db.QueryRow("select item from legacy_reconciliation_items").Scan(&afterItem); err != nil {
				t.Fatal(err)
			}
			if err = db.QueryRow("select receipt from legacy_reconciliations").Scan(&afterReceipt); err != nil {
				t.Fatal(err)
			}
			db.Close()
			if afterItem != frozenItem || afterReceipt != frozenReceipt {
				t.Fatal("old committed evidence rewritten")
			}
			if version >= 4 && version < 6 && (!strings.Contains(afterItem, `"sender_casing_changed":true`) || !strings.Contains(afterItem, `"recipient_casing_changed":true`) || !strings.Contains(afterItem, "Reader@Example.org")) {
				t.Fatal("v4 raw address pair lost")
			}
			if version == 6 {
				var saved map[string]any
				if err = json.Unmarshal([]byte(afterItem), &saved); err != nil {
					t.Fatal(err)
				}
				assertCommittedV6SenderEvidence(t, saved)
				if _, found := saved["recipient_name_changed"]; found {
					t.Fatal("stored v6 item acquired new semantics")
				}
			}
			if version == 5 && (!strings.Contains(afterItem, `"sender_name_changed":true`) || !strings.Contains(afterItem, "Current Display")) {
				t.Fatal("v5 raw sender-name pair lost")
			}

		})
	}
}
