package graphmail_test

import (
	"encoding/json"
	"errors"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"reflect"
	"strings"
	"testing"
)

func TestSenderAddressDiagnosticHTTP(t *testing.T) {
	for _, capture := range []bool{false, true} {
		proof, err := collectDiagnosticObservations(t, capture, "sender@example.org", nil, nil, nil, func(m map[string]any) {
			m["sender"].(map[string]any)["emailAddress"].(map[string]any)["address"] = "other@example.org"
		})
		var failure *graphmail.ReconciliationError
		if !errors.As(err, &failure) || !reflect.DeepEqual(proof, graphmail.LegacyEvidence{}) {
			t.Fatal("mismatch did not fail with zero evidence")
		}
		if strings.Contains(err.Error(), "@") {
			t.Fatal("raw address in normal error")
		}
		d := failure.SenderAddressDiagnostic
		if (d != nil) != capture {
			t.Fatalf("opt-in diagnostic capture = %v, want %v", d != nil, capture)
		}
		if d != nil && (d.AccountID != account.ID || d.N != 1 || d.ImmutableID != "one" || d.EnumeratedSenderAddress != "sender@example.org" || d.InspectedSenderAddress != "other@example.org") {
			t.Fatal("wrong scalar observation pair")
		}
		raw, e := json.Marshal(failure)
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(raw), "sender@example.org") || strings.Contains(string(raw), "other@example.org") || strings.Contains(string(raw), "SenderAddressDiagnostic") {
			t.Fatal("ordinary error JSON exposed opt-in payload")
		}
		if errors.Unwrap(failure) == nil {
			t.Fatal("cause identity lost")
		}
	}
}

func TestSenderAddressDiagnosticDoesNotBroadenCapture(t *testing.T) {
	for _, kind := range []string{"recipient", "allowed-casing-with-subject", "different-reference", "historical-sender"} {
		t.Run(kind, func(t *testing.T) {
			var before func(map[string]any)
			change := func(m map[string]any) { m["subject"] = "Changed" }
			switch kind {
			case "allowed-casing-with-subject":
				change = func(m map[string]any) {
					m["subject"] = "Changed"
					m["sender"].(map[string]any)["emailAddress"].(map[string]any)["address"] = "SENDER@example.org"
				}
			case "different-reference":
				change = func(m map[string]any) {
					m["id"] = "another"
					m["sender"].(map[string]any)["emailAddress"].(map[string]any)["address"] = "other@example.org"
				}
			case "historical-sender":
				before = func(m map[string]any) {
					m["sender"].(map[string]any)["emailAddress"].(map[string]any)["address"] = "other@example.org"
				}
				change = before
			}
			proof, err := collectDiagnosticObservations(t, true, "sender@example.org", nil, nil, before, change)
			var failure *graphmail.ReconciliationError
			if !errors.As(err, &failure) || failure.SenderAddressDiagnostic != nil || !reflect.DeepEqual(proof, graphmail.LegacyEvidence{}) {
				t.Fatal("unrelated failure fabricated sender diagnostic or evidence")
			}
		})
	}
	proof, err := collectDiagnosticObservations(t, true, "sender@example.org", nil, nil, nil, nil)
	if err != nil || len(proof.States) != 1 {
		t.Fatal("opt-in changed successful evidence", err)
	}
}
