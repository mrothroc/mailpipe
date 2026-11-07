package graphmail_test

import (
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func TestReconciliationSenderNameObservations(t *testing.T) {
	for _, pair := range [][2]string{{"Before", "After"}, {"Name", "NAME"}, {" Name ", "Name"}, {"", "Display"}, {"Display", ""}, {"Émetteur", "送信者"}} {
		for _, combined := range []bool{false, true} {
			t.Run(pair[0]+"/"+pair[1]+map[bool]string{false: "/name-only", true: "/combined"}[combined], func(t *testing.T) {
				before := []mailbox.Address{{Address: "reader@example.org", Name: "Reader"}}
				after := append([]mailbox.Address(nil), before...)
				address := "sender@example.org"
				if combined {
					address = "SENDER@Example.org"
					before[0].Address = "Reader@Example.org"
				}
				proof, err := collectObservations(t, "sender@example.org", before, after, func(m map[string]any) {
					m["sender"] = map[string]any{"emailAddress": map[string]string{"address": "sender@example.org", "name": pair[0]}}
				}, func(m map[string]any) {
					m["sender"] = map[string]any{"emailAddress": map[string]string{"address": address, "name": pair[1]}}
				})
				if err != nil {
					t.Fatalf("same-ID sender presentation blocked: %v", err)
				}
				if proof.EnumeratedStates[1].Metadata.Sender.Name != pair[0] || proof.States[1].Metadata.Sender.Name != pair[1] {
					t.Fatal("raw sender names lost")
				}
			})
		}
	}
}
