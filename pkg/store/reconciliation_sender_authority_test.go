package store

import (
	"context"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func senderAuthorityPreparation(t *testing.T) (string, mailbox.Account, LegacyPreparation, LegacyReconciliation) {
	t.Helper()
	path, a, p, v := casingPreparation(t)
	n := p.Live[0].N
	e, s := v.EnumeratedStates[n], v.Verification.States[n]
	e.Metadata.Sender.Address = "enumerated@example.org"
	e.Metadata.Sender.Name, s.Metadata.Sender.Name = "Before Display", "After Display"
	v.EnumeratedStates[n], v.Verification.States[n] = e, s
	v.SenderSourceIntegrity = map[int64]bool{n: true}
	return path, a, p, v
}

func TestReconciliationSenderAuthorityPreservesCompleteHistory(t *testing.T) {
	ctx := context.Background()
	original := rawFixtureDB(t, frozenLegacy(t))
	specs, err := tableSpecs(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	original.Close()
	path, a, p, v := senderAuthorityPreparation(t)
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
		return v, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	after, err := dataDigest(ctx, bound.store.db, specs)
	if err != nil || after != before {
		t.Fatal("attribution replaced historical data", err)
	}
	m, err := bound.GetMessage(ctx, mailbox.LocalRef{AccountID: a.ID, N: p.Live[0].N})
	if err != nil || m.State.Metadata.Sender.Address != v.Verification.States[p.Live[0].N].Metadata.Sender.Address {
		t.Fatal("inspection not published exactly", err)
	}
	if err = bound.Close(); err != nil {
		t.Fatal(err)
	}
	read, found, err := ReadLegacyReconciliationReceipt(ctx, path, a)
	if err != nil || !found || read.Version != 9 || read.Digest != r.Digest || read.Items[0].SenderAddressDisagrees || read.Items[0].SenderCasingChanged || read.Items[0].ReadRef == nil || !read.Items[0].SourceValidated {
		t.Fatal("current offline receipt lost flags", err)
	}
}
