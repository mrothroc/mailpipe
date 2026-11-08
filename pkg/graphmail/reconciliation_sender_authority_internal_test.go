package graphmail

import (
	"context"
	"reflect"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func TestReconciliationSenderAuthorityIntegrityAndStrictGuards(t *testing.T) {
	for _, kind := range []string{"valid", "sender-fallback", "unknown-enumeration", "unknown-inspection", "unknown-exact", "unknown-case", "empty-before", "empty-after", "original-sender", "original-header", "header", "subject", "recipient-name", "membership", "revision"} {
		t.Run(kind, func(t *testing.T) {
			p, g, refs := selectedObservationFixture(t)
			from := func(address string) map[string]any {
				return map[string]any{"emailAddress": map[string]string{"address": address}}
			}
			g.listed[0]["from"] = from("enumerated@example.org")
			switch kind {
			case "sender-fallback":
				delete(g.listed[0], "from")
				g.listed[0]["sender"] = from("enumerated@example.org")
			case "unknown-enumeration":
				g.listed[0]["From"] = g.listed[0]["from"]
				delete(g.listed[0], "from")
			case "unknown-inspection":
				g.inspected[0]["From"] = from("sender@example.org")
			case "unknown-exact":
				g.listed[0]["From"] = from("sender@example.org")
				delete(g.listed[0], "from")
			case "unknown-case":
				g.listed[0]["From"] = from("SENDER@EXAMPLE.ORG")
				delete(g.listed[0], "from")
			case "empty-before":
				g.listed[0]["from"] = from("")
			case "empty-after":
				g.inspected[0]["sender"] = from("")
			case "original-sender":
				refs[0].Sender = "different@example.org"
			case "original-header":
				refs[0].InternetMessageID = "<different@example.org>"
			case "header":
				g.inspected[0]["internetMessageId"] = "<different@example.org>"
			case "subject":
				g.inspected[0]["subject"] = "Different"
			case "recipient-name":
				g.listed[0]["toRecipients"] = []any{from("to@example.org")}
				g.inspected[0]["toRecipients"] = []any{map[string]any{"emailAddress": map[string]string{"address": "to@example.org", "name": "Different"}}}
			case "membership":
				g.inspected[0]["parentFolderId"] = "archive"
			case "revision":
				g.listed[0]["changeKey"], g.inspected[0]["changeKey"] = "old", "new"
			}
			proof, err := p.ReconcileLegacyReferences(context.Background(), refs)
			valid := kind == "valid" || kind == "sender-fallback" || kind == "unknown-exact" || kind == "unknown-case"
			if (err == nil) != valid {
				t.Fatal("directional source-integrity guard", kind, err)
			}
			if !valid && !reflect.DeepEqual(proof, LegacyEvidence{}) {
				t.Fatal("failed pair returned partial proof")
			}
			if valid && proof.SenderSourceIntegrity[1] != (kind == "valid" || kind == "sender-fallback") {
				t.Fatal("unknown provenance supplied new authority")
			}
			if g.calls != 1 || g.batchCalls != 1 || !reflect.DeepEqual(g.requests, []string{"target", "other"}) {
				t.Fatal("authority changed request protocol")
			}
		})
	}
}

func TestReconciliationSenderAuthorityRejectsInvalidChangedStrings(t *testing.T) {
	for _, address := range []string{"", "\xff"} {
		for _, first := range []bool{false, true} {
			a := mailbox.Metadata{Sender: mailbox.Address{Address: "enumerated@example.org"}}
			b := mailbox.Metadata{Sender: mailbox.Address{Address: "sender@example.org"}}
			if first {
				a.Sender.Address = address
			} else {
				b.Sender.Address = address
			}
			if reconciliationMetadataMatchesSender(a, b, true) {
				t.Fatal("invalid changed address accepted")
			}
		}
	}
}
