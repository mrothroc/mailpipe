package graphmail

import (
	"reflect"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func TestCasingComparisonRepresentationAndOwnership(t *testing.T) {
	for _, kind := range []string{"nil-empty", "invalid-utf8", "canonical-unicode", "valid"} {
		t.Run(kind, func(t *testing.T) {
			a := mailbox.Metadata{Recipients: []mailbox.Address{{Address: "Reader@example.org", Name: "Reader"}}}
			b := mailbox.Metadata{Recipients: []mailbox.Address{{Address: "reader@example.org", Name: "Reader"}}}
			switch kind {
			case "nil-empty":
				a.Recipients = nil
				b.Recipients = []mailbox.Address{}
			case "invalid-utf8":
				a.Recipients[0].Address = string([]byte{0xff})
				b.Recipients[0].Address = string([]byte{0xfe})
			case "canonical-unicode":
				a.Recipients[0].Address = "é@example.org"
				b.Recipients[0].Address = "e\u0301@example.org"
			}
			beforeA, beforeB := reconciliationObservation(mailbox.State{Metadata: a}), reconciliationObservation(mailbox.State{Metadata: b})
			if reconciliationMetadataMatches(a, b) != (kind == "valid") {
				t.Fatal("incorrect scoped comparison")
			}
			if !reflect.DeepEqual(a, beforeA.Metadata) || !reflect.DeepEqual(b, beforeB.Metadata) {
				t.Fatal("comparison mutated recipients")
			}
			copy := reconciliationObservation(mailbox.State{Metadata: a, Memberships: []mailbox.Membership{{Collection: mailbox.CollectionRef{CollectionID: "original"}}}})
			if len(copy.Metadata.Recipients) > 0 {
				copy.Metadata.Recipients[0].Name = "changed"
				if a.Recipients[0].Name == "changed" {
					t.Fatal("snapshot aliases input")
				}
			}
		})
	}
}
