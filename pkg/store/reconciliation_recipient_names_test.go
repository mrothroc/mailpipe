package store

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func recipientNamePreparation(t *testing.T, casing bool) (string, mailbox.Account, LegacyPreparation, LegacyReconciliation) {
	t.Helper()
	path, a, p, v := casingPreparation(t)
	n := p.Live[0].N
	e, s := v.EnumeratedStates[n], v.Verification.States[n]
	e.Metadata.Recipients = []mailbox.Address{{Address: "reader@example.org", Name: "Original display"}, {Address: "reader@example.org", Name: "Duplicate display"}, {Address: "other@example.org", Name: "Other"}}
	s.Metadata.Recipients = append([]mailbox.Address(nil), e.Metadata.Recipients...)
	s.Metadata.Recipients[0].Name = "Current display"
	if casing {
		s.Metadata.Recipients[1].Address = "READER@Example.org"
	}
	v.EnumeratedStates[n], v.Verification.States[n] = e, s
	return path, a, p, v
}

func TestReconciliationRecipientNamesPreserveCompleteHistory(t *testing.T) {
	ctx := context.Background()
	original := rawFixtureDB(t, frozenLegacy(t))
	specs, err := tableSpecs(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	original.Close()
	path, a, p, v := recipientNamePreparation(t, true)
	db := rawFixtureDB(t, path)
	before, err := dataDigest(ctx, db, specs)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	v = referenceFixture(a, p, v)
	r, err := ReviewLegacyReconciliation(a, p, v)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := ReconcileLegacy(ctx, path, a, p, r.Digest, reconcileLegacyFunc(func(_ context.Context, _ mailbox.Account, exported LegacyPreparation) (LegacyReconciliation, error) {
		exported.Live[0].GraphID = "forged"
		exported.Removed[0].GraphID = "forged-removed"
		return v, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	after, err := dataDigest(ctx, bound.store.db, specs)
	if err != nil || before != after {
		t.Fatal("recipient review replaced original history", err)
	}
	m, err := bound.GetMessage(ctx, mailbox.LocalRef{AccountID: a.ID, N: p.Live[0].N})
	if err != nil || !reflect.DeepEqual(m.State.Metadata.Recipients, v.Verification.States[p.Live[0].N].Metadata.Recipients) {
		t.Fatal("current raw observations not published", err)
	}
	bound.Close()
}

func recipientNameEvidenceUnchanged(t *testing.T, v LegacyReconciliation, before []byte) bool {
	t.Helper()
	after, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(after) == string(before)
}
