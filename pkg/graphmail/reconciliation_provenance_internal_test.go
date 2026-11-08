package graphmail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func originFixture(t *testing.T) (*Provider, map[string]any, mailbox.State) {
	t.Helper()
	p, e := New(&reconciliationTransport{}, mailbox.Account{ID: "invented-account", ProviderID: "graph"})
	if e != nil {
		t.Fatal(e)
	}
	m := conflictWire("one", "<one@example.org>", "agent@example.org", "inbox")
	m["from"] = map[string]any{"emailAddress": map[string]any{"address": "from@example.org", "name": "From Name"}}
	m["sender"] = map[string]any{"emailAddress": map[string]any{"address": "agent@example.org", "name": "Agent Name"}}
	m["toRecipients"] = []any{map[string]any{"emailAddress": map[string]any{"address": "to@example.org", "name": "To Name"}}, map[string]any{"emailAddress": map[string]any{"address": "to@example.org", "name": "To Name"}}}
	m["ccRecipients"] = []any{map[string]any{"emailAddress": map[string]any{"address": "cc@example.org", "name": "Cc Name"}}}
	raw, _ := json.Marshal(m)
	var w wireMessage
	if e = json.Unmarshal(raw, &w); e != nil {
		t.Fatal(e)
	}
	state, e := p.state(w)
	if e != nil {
		t.Fatal(e)
	}
	return p, m, state
}
func TestReconciliationOriginSelection(t *testing.T) {
	for _, kind := range []string{"both", "missing-from", "null-from", "empty-from", "missing-both", "null-both", "empty-recipients", "null-name", "hostile", "unknown-fields"} {
		t.Run(kind, func(t *testing.T) {
			p, m, _ := originFixture(t)
			selected := "from"
			presence := "present"
			switch kind {
			case "missing-from":
				delete(m, "from")
				selected = "sender"
				presence = "missing"
			case "null-from":
				m["from"] = nil
				selected = "sender"
				presence = "null"
			case "empty-from":
				m["from"].(map[string]any)["emailAddress"].(map[string]any)["address"] = ""
			case "missing-both":
				delete(m, "from")
				delete(m, "sender")
				selected = "none"
				presence = "missing"
			case "null-both":
				m["from"], m["sender"] = nil, nil
				selected = "none"
				presence = "null"
			case "empty-recipients":
				m["toRecipients"], m["ccRecipients"] = []any{}, []any{}
			case "null-name":
				m["from"].(map[string]any)["emailAddress"].(map[string]any)["name"] = nil
			case "hostile":
				m["from"].(map[string]any)["emailAddress"].(map[string]any)["address"] = "private-secret\x1b\nhttps://host.example.org/private"
				m["sender"].(map[string]any)["emailAddress"].(map[string]any)["address"] = "K@Example.org"
			case "unknown-fields":
				m["unselected-secret"] = map[string]any{"value": "excluded-private-value"}
			}
			raw, _ := json.Marshal(m)
			copyRaw := append([]byte(nil), raw...)
			var w wireMessage
			if e := json.Unmarshal(raw, &w); e != nil {
				t.Fatal(e)
			}
			state, e := p.state(w)
			if e != nil {
				t.Fatal(e)
			}
			before := reconciliationObservation(state)
			origin, e := reconciliationOrigin(context.Background(), raw, state)
			if e != nil || origin.SelectedSource != selected || origin.From.Presence != presence || !origin.MappedSenderMatchesWire || !origin.RecipientsMatchWire {
				t.Fatalf("source selection failed: %s %v", kind, e)
			}
			chosen, valid := origin.SenderAddress()
			if !valid || chosen != state.Metadata.Sender.Address {
				t.Fatal("source chooser changed raw value")
			}
			if !bytes.Equal(raw, copyRaw) || !reflect.DeepEqual(state, before) {
				t.Fatal("origin validation mutated input")
			}
			encoded, _ := json.Marshal(origin)
			for _, value := range []string{"From Name", "Agent Name", "To Name", "Cc Name", "to@example.org", "cc@example.org", "excluded-private-value", "excluded-subject", "excluded-preview"} {
				if strings.Contains(string(encoded), value) {
					t.Fatal("origin exported unrelated data")
				}
			}
		})
	}
}
func TestReconciliationOriginRejectsUnsupportedAndMismapped(t *testing.T) {
	_, m, state := originFixture(t)
	base, _ := json.Marshal(m)
	for _, kind := range []string{"duplicate", "case-alias", "nested-duplicate", "nested-alias", "address-null", "address-number", "address-missing", "email-null", "from-number", "name-number", "array-null", "array-object", "recipient-malformed", "invalid-utf8", "trailing", "not-object", "wrong-id", "wrong-locator", "sender-mapped-from-recipient", "sender-name", "recipient-order", "recipient-name", "recipient-multiplicity", "recipient-missing", "recipient-empty-shape"} {
		t.Run(kind, func(t *testing.T) {
			raw := append([]byte(nil), base...)
			s := reconciliationObservation(state)
			want := errOriginUnsupported
			switch kind {
			case "duplicate":
				raw = bytes.Replace(raw, []byte(`"from":`), []byte(`"from":null,"from":`), 1)
			case "case-alias":
				raw = bytes.Replace(raw, []byte(`"from":`), []byte(`"From":`), 1)
			case "nested-duplicate":
				raw = bytes.Replace(raw, []byte(`"address":"from@example.org"`), []byte(`"address":"other@example.org","address":"from@example.org"`), 1)
			case "nested-alias":
				raw = bytes.Replace(raw, []byte(`"address":"from@example.org"`), []byte(`"Address":"from@example.org"`), 1)
			case "address-null":
				raw = bytes.Replace(raw, []byte(`"address":"from@example.org"`), []byte(`"address":null`), 1)
			case "address-number":
				raw = bytes.Replace(raw, []byte(`"address":"from@example.org"`), []byte(`"address":7`), 1)
			case "address-missing":
				raw = bytes.Replace(raw, []byte(`"address":"from@example.org",`), nil, 1)
			case "email-null":
				raw = bytes.Replace(raw, []byte(`{"emailAddress":{"address":"from@example.org","name":"From Name"}}`), []byte(`{"emailAddress":null}`), 1)
			case "from-number":
				raw = bytes.Replace(raw, []byte(`{"emailAddress":{"address":"from@example.org","name":"From Name"}}`), []byte(`7`), 1)
			case "name-number":
				raw = bytes.Replace(raw, []byte(`"name":"From Name"`), []byte(`"name":7`), 1)
			case "array-null":
				raw = bytes.Replace(raw, []byte(`"ccRecipients":[{"emailAddress":{"address":"cc@example.org","name":"Cc Name"}}]`), []byte(`"ccRecipients":null`), 1)
			case "array-object":
				raw = bytes.Replace(raw, []byte(`"ccRecipients":[{"emailAddress":{"address":"cc@example.org","name":"Cc Name"}}]`), []byte(`"ccRecipients":{}`), 1)
			case "recipient-malformed":
				raw = bytes.Replace(raw, []byte(`"address":"cc@example.org"`), []byte(`"address":false`), 1)
			case "invalid-utf8":
				raw = bytes.Replace(raw, []byte("from@example.org"), []byte{0xff}, 1)
			case "trailing":
				raw = append(raw, []byte(` {}`)...)
			case "not-object":
				raw = []byte(`[]`)
			case "wrong-id":
				raw = bytes.Replace(raw, []byte(`"id":"one"`), []byte(`"id":"other"`), 1)
				want = errOriginMismatch
			case "wrong-locator":
				s.Ref.Locator = "other"
				want = errOriginMismatch
			case "sender-mapped-from-recipient":
				s.Metadata.Sender = s.Metadata.Recipients[0]
				want = errOriginMismatch
			case "sender-name":
				s.Metadata.Sender.Name = "wrong"
				want = errOriginMismatch
			case "recipient-order":
				s.Metadata.Recipients[0], s.Metadata.Recipients[2] = s.Metadata.Recipients[2], s.Metadata.Recipients[0]
				want = errOriginMismatch
			case "recipient-name":
				s.Metadata.Recipients[0].Name = "wrong"
				want = errOriginMismatch
			case "recipient-multiplicity":
				s.Metadata.Recipients[1] = s.Metadata.Recipients[2]
				want = errOriginMismatch
			case "recipient-empty-shape":
				var m map[string]any
				json.Unmarshal(raw, &m)
				m["toRecipients"], m["ccRecipients"] = []any{}, []any{}
				raw, _ = json.Marshal(m)
				s.Metadata.Recipients = []mailbox.Address{}
				want = errOriginMismatch
			case "recipient-missing":
				s.Metadata.Recipients = s.Metadata.Recipients[:1]
				want = errOriginMismatch
			}
			_, e := reconciliationOrigin(context.Background(), raw, s)
			if !errors.Is(e, want) {
				t.Fatalf("expected fixed provenance failure for %s, got %v", kind, e)
			}
			if strings.Contains(e.Error(), "@") || len(e.Error()) > 128 {
				t.Fatal("raw provenance error")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := reconciliationOrigin(ctx, base, state); !errors.Is(e, context.Canceled) {
		t.Fatal("cancellation lost")
	}
}
func TestReconciliationOriginBudgetAndSourceValidation(t *testing.T) {
	_, m, state := originFixture(t)
	raw, _ := json.Marshal(m)
	o, e := reconciliationOrigin(context.Background(), raw, state)
	if e != nil {
		t.Fatal(e)
	}
	budget := reconciliationOriginBudget{limit: 10}
	if e = budget.retain(context.Background(), "one", o, state.Metadata.Sender.Address); !errors.Is(e, errOriginLimit) {
		t.Fatal("origin budget failed open")
	}
	budget = reconciliationOriginBudget{limit: reconciliationOriginLimit}
	if e = budget.retain(context.Background(), "one", o, state.Metadata.Sender.Address); e != nil || budget.used <= 0 {
		t.Fatal("valid retained budget")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e = budget.retain(ctx, "one", o, ""); !errors.Is(e, context.Canceled) {
		t.Fatal("budget ignored cancellation")
	}
	for _, kind := range []string{"unknown-choice", "wrong-choice", "false-missing", "invalid-encoding", "unknown-presence"} {
		bad := o
		switch kind {
		case "unknown-choice":
			bad.SelectedSource = "untrusted-secret"
		case "wrong-choice":
			bad.SelectedSource = "sender"
		case "false-missing":
			bad.From.Presence = "missing"
		case "invalid-encoding":
			bad.From.Address = "\xff"
		case "unknown-presence":
			bad.From.Presence = "unknown"
		}
		if _, valid := bad.SenderAddress(); valid {
			t.Fatal("invalid source accepted")
		}
	}
	if originCategory(errOriginUnsupported) != "unsupported-provenance" || originCategory(errOriginMismatch) != "provenance-mismatch" || originCategory(errOriginLimit) != "provenance-limit" || originCategory(context.Canceled) != "" {
		t.Fatal("unsafe origin category")
	}
}

type originVariantTransport struct{ *evidenceBenchmarkTransport }

func (t *originVariantTransport) Batch(ctx context.Context, requests []graph.BatchRequest) ([]graph.BatchResponse, error) {
	out, e := t.evidenceBenchmarkTransport.Batch(ctx, requests)
	if e != nil {
		return nil, e
	}
	for i := range out {
		if out[i].Status == 200 {
			var m map[string]any
			if e = json.Unmarshal(out[i].Body, &m); e != nil {
				return nil, e
			}
			m["From"] = m["sender"]
			out[i].Body, e = json.Marshal(m)
			if e != nil {
				return nil, e
			}
		}
	}
	return out, nil
}
func TestPublicInspectionKeepsNilOriginObserver(t *testing.T) {
	a := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
	tr := &originVariantTransport{&evidenceBenchmarkTransport{n: 2}}
	p, e := New(tr, a)
	if e != nil {
		t.Fatal(e)
	}
	refs := batchOriginals(1, a)
	states, failures, e := p.InspectBatch(context.Background(), []mailbox.MessageRef{refs[0].Ref})
	if e != nil || len(states) != 1 || failures[0] != nil || states[0].Metadata.Sender.Address != "sender@example.org" {
		t.Fatal("ordinary inspection changed decoder policy", e)
	}
	proof, e := p.ReconcileLegacyReferences(context.Background(), refs)
	if e != nil || len(proof.States) != 1 {
		t.Fatal("default recovery changed", e)
	}
	proof, e = p.ReconcileLegacyReferencesWithSenderAddressDiagnostics(context.Background(), refs)
	var failure *ReconciliationError
	if e == nil || !errors.As(e, &failure) || !errors.Is(e, errOriginUnsupported) || failure.SenderAddressDiagnostic != nil || !reflect.DeepEqual(proof, LegacyEvidence{}) || !strings.Contains(e.Error(), "phase=inspection; category=unsupported-provenance") {
		t.Fatal("opt-in ambiguous origin not blocked", e)
	}
	fault := errors.New("observer-private-secret")
	_, _, e = p.inspectBatch(context.Background(), []mailbox.MessageRef{refs[0].Ref}, func(mailbox.MessageRef, json.RawMessage, mailbox.State) error { return fault })
	if !errors.Is(e, fault) {
		t.Fatal("private observer cause lost")
	}
}

func TestReconciliationOriginHandoffIntegrity(t *testing.T) {
	_, m, state := originFixture(t)
	raw, _ := json.Marshal(m)
	origin, e := reconciliationOrigin(context.Background(), raw, state)
	if e != nil {
		t.Fatal(e)
	}
	if !origin.MatchesMappedSender(state.Metadata.Sender.Address) {
		t.Fatal("valid source handoff rejected")
	}
	if origin.MatchesMappedSender("copied-from-recipient@example.org") {
		t.Fatal("changed collector value inherited wire provenance")
	}
	var absent *ReconciliationSenderAddressOrigin
	if absent.MatchesMappedSender("") {
		t.Fatal("missing source inherited provenance")
	}
	for _, kind := range []string{"mapping", "recipient", "choice"} {
		bad := origin
		switch kind {
		case "mapping":
			bad.MappedSenderMatchesWire = false
		case "recipient":
			bad.RecipientsMatchWire = false
		case "choice":
			bad.SelectedSource = "unknown"
		}
		if bad.MatchesMappedSender(state.Metadata.Sender.Address) {
			t.Fatal("unverified handoff accepted")
		}
	}
}
