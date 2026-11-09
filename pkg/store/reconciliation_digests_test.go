package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestReconciliationAuditEncodingFailureReturnsNoApproval(t *testing.T) {
	_, a, p, v := referencePreparation(t)
	v.Started = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	v.Finished = v.Started
	if r, err := ReviewLegacyReconciliation(a, p, v); err == nil || !reflect.DeepEqual(r, LegacyReconciliationReceipt{}) {
		t.Fatal("audit encoding failure returned partial approval")
	}
}

func TestReferenceStateEncodingFailureReturnsNoReceipt(t *testing.T) {
	_, a, p, v := referencePreparation(t)
	// No enumeration pair can mask the intended selected-state codec failure.
	v.EnumeratedStates = nil
	if _, err := ReviewLegacyReconciliation(a, p, v); err != nil {
		t.Fatal("control evidence is not admitted", err)
	}
	n := p.Live[0].N
	state := v.Verification.States[n]
	state.Metadata.Received = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	v.Verification.States[n] = state
	original := p.Live[0]
	if _, _, err := reconciliationState(a.ID, original, v.Verification.Folders[original.Folder], state); err != nil {
		t.Fatal("invalid encoding date failed earlier state validation", err)
	}
	_, codecErr := json.Marshal(state.Metadata.Received)
	var expected *json.MarshalerError
	if !errors.As(codecErr, &expected) || expected.Type != reflect.TypeOf((*time.Time)(nil)) {
		t.Fatal("fixture does not reach the time codec", codecErr)
	}
	r, err := ReviewLegacyReconciliation(a, p, v)
	var actual *json.MarshalerError
	if !errors.As(err, &actual) || actual.Type != expected.Type || actual.Err.Error() != expected.Err.Error() {
		t.Fatal("review failed before the approval time codec", err)
	}
	if !reflect.DeepEqual(r, LegacyReconciliationReceipt{}) {
		t.Fatal("approval encoding failure returned a partial receipt")
	}
}
