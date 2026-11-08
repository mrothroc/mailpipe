package cli_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/store"
)

func TestReconcileInspectionSenderMatchesHistory(t *testing.T) {
	e := newEnv(t)
	seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
	e.graph.server = map[string]serverMsg{"original": {"inbox", "<original@example.org>", "sender@example.org"}}
	g := &provenanceGraph{senderNameGraph: &senderNameGraph{senderCasingGraph: &senderCasingGraph{casingGraph: &casingGraph{fakeGraph: e.graph, listed: "to@example.org", inspected: "to@example.org"}, sender: "sender@example.org"}}}
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
		t.Fatalf("history-matching inspection blocked: %d %s", code, stderr)
	}
	if !strings.Contains(out, `"version":6`) || !strings.Contains(out, `"sender_address_disagrees":true`) || !strings.Contains(out, `"sender_casing_changed":false`) || !strings.Contains(out, "enumerated-from@example.org") || !strings.Contains(out, "sender@example.org") {
		t.Fatal("missing directional raw evidence/flags", out)
	}
	if _, found, err := store.ReadLegacyReconciliationReceipt(context.Background(), e.db, subject.Account()); err != nil || found {
		t.Fatal("preview bound history", err)
	}
	if g.pages != 1 || g.batches != 1 || len(g.moved) != 0 || g.bodyCalls.Load() != 0 || g.rawCalls.Load() != 0 {
		t.Fatal("authority changed read/effect counts")
	}
}
