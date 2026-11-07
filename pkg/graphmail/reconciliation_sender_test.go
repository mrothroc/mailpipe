package graphmail_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func TestReconciliationSenderCasing(t *testing.T) {
	for _, wireField := range []string{"sender", "from"} {
		for _, combined := range []bool{false, true} {
			t.Run(wireField+map[bool]string{false: "/sender-only", true: "/combined"}[combined], func(t *testing.T) {
				before := []mailbox.Address{{Address: "reader@example.org", Name: "Reader"}}
				after := []mailbox.Address{{Address: "reader@example.org", Name: "Reader"}}
				if combined {
					before[0].Address = "Reader@Example.org"
				}
				proof, err := collectCasing(t, before, after, func(m map[string]any) {
					m[wireField] = map[string]any{"emailAddress": map[string]string{"address": "SENDER@Example.org"}}
				})
				if err != nil {
					t.Fatalf("same-ID address casing blocked: %v", err)
				}
				if proof.EnumeratedStates[1].Metadata.Sender.Address != "sender@example.org" || proof.States[1].Metadata.Sender.Address != "SENDER@Example.org" {
					t.Fatal("raw sender observations lost")
				}
				if proof.EnumeratedStates[1].Metadata.Recipients[0].Address != before[0].Address || proof.States[1].Metadata.Recipients[0].Address != after[0].Address {
					t.Fatal("raw recipient observations lost")
				}
			})
		}
	}
}

func TestReconciliationSenderDiagnostic(t *testing.T) {
	for _, tc := range []struct{ address, relation string }{{" sender@example.org ", "trim-only"}, {" SENDER@EXAMPLE.ORG ", "trim-and-case"}, {"other@example.org", "different"}} {
		t.Run(tc.relation, func(t *testing.T) {
			proof, err := collectCasing(t, nil, nil, func(m map[string]any) {
				m["sender"] = map[string]any{"emailAddress": map[string]string{"address": tc.address}}
			})
			var failure *graphmail.ReconciliationError
			if !errors.As(err, &failure) || !reflect.DeepEqual(proof, graphmail.LegacyEvidence{}) {
				t.Fatal("invalid sender produced evidence")
			}
			if !strings.Contains(failure.Reason, "fields=sender-address;") || !strings.Contains(failure.Reason, "sender-address="+tc.relation+";") {
				t.Fatal("missing safe sender relation")
			}
			if strings.Contains(failure.Error(), tc.address) || len(failure.Error()) > 1024 || failure.Unwrap() != failure.Cause {
				t.Fatal("diagnostic privacy/bound/cause changed")
			}
		})
	}
}

func TestReconciliationSenderUnicodeAndStrictMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		ok                  bool
	}{
		{"sharp-s", "straße@example.org", "STRAẞE@EXAMPLE.ORG", true},
		{"full-expansion", "straße@example.org", "STRASSE@example.org", false},
		{"ascii-i", "I@example.org", "i@example.org", true},
		{"dotted-i", "İ@example.org", "i@example.org", false},
		{"dotless-i", "I@example.org", "ı@example.org", false},
		{"sigma", "Σ@example.org", "ς@example.org", true},
		{"normalization", "é@example.org", "e\u0301@example.org", false},
		{"plus", "sender@example.org", "sender+tag@example.org", false},
		{"dot", "sender@example.org", "sen.der@example.org", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proof, err := collectSenderCasing(t, tc.before, nil, nil, func(m map[string]any) {
				m["sender"] = map[string]any{"emailAddress": map[string]string{"address": tc.after}}
			})
			if (err == nil) != tc.ok {
				t.Fatalf("Unicode/format boundary: %v", err)
			}
			if !tc.ok && !reflect.DeepEqual(proof, graphmail.LegacyEvidence{}) {
				t.Fatal("failed pair exposed evidence")
			}
			if tc.ok && (proof.EnumeratedStates[1].Metadata.Sender.Address != tc.before || proof.States[1].Metadata.Sender.Address != tc.after) {
				t.Fatal("Unicode spelling rewritten")
			}
		})
	}
	for _, field := range []string{"recipient-name", "order", "subject", "header", "membership", "revision", "id"} {
		t.Run(field, func(t *testing.T) {
			before := []mailbox.Address{{Address: "a@example.org", Name: "A"}, {Address: "b@example.org", Name: "B"}}
			after := append([]mailbox.Address(nil), before...)
			if field == "recipient-name" {
				after[0].Name = "a"
			}
			if field == "order" {
				after[0], after[1] = after[1], after[0]
			}
			proof, err := collectCasing(t, before, after, func(m map[string]any) {
				m["sender"] = map[string]any{"emailAddress": map[string]string{"address": "SENDER@example.org"}}
				switch field {
				case "subject":
					m["subject"] = "Changed"
				case "header":
					m["internetMessageId"] = "<other@example.org>"
				case "membership":
					m["parentFolderId"] = "other"
				case "revision":
					m["changeKey"] = "other"
				case "id":
					m["id"] = "other"
				}
			})
			if err == nil || !reflect.DeepEqual(proof, graphmail.LegacyEvidence{}) {
				t.Fatal("sender case masked another guard")
			}
		})
	}
}
