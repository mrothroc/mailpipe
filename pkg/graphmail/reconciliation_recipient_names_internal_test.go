package graphmail

import (
	"reflect"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func TestMetadataRecipientNamesEncodingAndOwnership(t *testing.T) {
	for _, pair := range [][2]string{{"Before", "After"}, {"", "Display"}, {" Display ", "Display"}, {"Équipe", "受信者"}, {"Before\x1b\u202e", "After\n"}, {"\xff", "Name"}, {"Name", "\xff"}, {"\xff", "\xfe"}, {"\xff", "\xff"}} {
		a := mailbox.Metadata{Sender: mailbox.Address{Address: "sender@example.org"}, Recipients: []mailbox.Address{{Address: "Reader@Example.org", Name: pair[0]}, {Address: "reader@example.org", Name: "Duplicate"}}}
		b := reconciliationObservation(mailbox.State{Metadata: a}).Metadata
		b.Recipients[0].Name = pair[1]
		b.Recipients[0].Address = "reader@example.org"
		oldA, oldB := reconciliationObservation(mailbox.State{Metadata: a}).Metadata, reconciliationObservation(mailbox.State{Metadata: b}).Metadata
		invalid := pair[0] != pair[1] && (pair[0] == "\xff" || pair[1] == "\xff" || pair[1] == "\xfe")
		if reconciliationMetadataMatches(a, b) == invalid {
			t.Fatal("wrong changed-name encoding behavior")
		}
		if !reflect.DeepEqual(a, oldA) || !reflect.DeepEqual(b, oldB) {
			t.Fatal("recipient input mutated")
		}
		b.Preview = "Changed"
		if reconciliationMetadataMatches(a, b) {
			t.Fatal("name exception masked unrelated metadata")
		}
	}
}

func TestMetadataRecipientNamesSharedBackingSlice(t *testing.T) {
	backing := []mailbox.Address{{Address: "reader@example.org", Name: "First"}, {Address: "reader@example.org", Name: "Second"}, {Address: "reader@example.org", Name: "Third"}}
	before := append([]mailbox.Address(nil), backing...)
	a := mailbox.Metadata{Sender: mailbox.Address{Address: "sender@example.org"}, Recipients: backing[:2]}
	b := a
	b.Recipients = backing[1:]
	if !reconciliationMetadataMatches(a, b) || !reflect.DeepEqual(backing, before) {
		t.Fatal("position-only name comparison mutated shared backing storage")
	}
}
