package graphmail_test

import (
	"reflect"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func TestReconciliationRecipientNameObservations(t *testing.T) {
	for _, names := range [][2]string{{"Before", "After"}, {"", "Display"}, {"Display", ""}, {" Name ", "Name"}, {"Équipe", "受信者"}, {"Before\x1b\u202e", "After\n\t"}} {
		for _, combined := range []bool{false, true} {
			t.Run(names[0]+"/"+names[1]+map[bool]string{false: "/name-only", true: "/combined"}[combined], func(t *testing.T) {
				before := []mailbox.Address{{Address: "reader@example.org", Name: names[0]}, {Address: "reader@example.org", Name: "Duplicate"}, {Address: "other@example.org", Name: "Other"}}
				after := append([]mailbox.Address(nil), before...)
				after[0].Name = names[1]
				if combined {
					after[0].Address = "READER@Example.org"
					after[1].Name = "Other duplicate display"
				}
				originalBefore, originalAfter := append([]mailbox.Address(nil), before...), append([]mailbox.Address(nil), after...)
				proof, err := collectCasing(t, before, after, nil)
				if err != nil {
					t.Fatalf("same-ID recipient presentation blocked: %v", err)
				}
				if !reflect.DeepEqual(proof.EnumeratedStates[1].Metadata.Recipients, before) || !reflect.DeepEqual(proof.States[1].Metadata.Recipients, after) {
					t.Fatalf("complete indexed raw observations lost: enum=%#v current=%#v before=%#v after=%#v", proof.EnumeratedStates[1].Metadata.Recipients, proof.States[1].Metadata.Recipients, before, after)
				}
				if !reflect.DeepEqual(before, originalBefore) || !reflect.DeepEqual(after, originalAfter) {
					t.Fatal("input slices mutated")
				}
			})
		}
	}
}
