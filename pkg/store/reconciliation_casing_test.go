package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func casingPreparation(t *testing.T) (string, mailbox.Account, LegacyPreparation, LegacyReconciliation) {
	t.Helper()
	path := frozenLegacy(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := PrepareLegacyBinding(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	a := mailbox.Account{ID: "casing-account", ProviderID: GraphProvider}
	proof := goodLegacyProof(a, p)
	n := p.Live[0].N
	current := proof.States[n]
	current.Metadata.Recipients = []mailbox.Address{{Address: "reader@example.org", Name: "Reader"}}
	proof.States[n] = current
	enumerated := current
	enumerated.Metadata.Recipients = []mailbox.Address{{Address: "Reader@Example.org", Name: "Reader"}}
	now := time.Now().UTC()
	v := LegacyReconciliation{Verification: proof, Coverage: LegacyPrimaryMailbox, Started: now, Finished: now, Pages: 1, Observed: len(proof.States)}
	// Decode through the actual evidence DTO so the baseline regression is a
	// behavioral failure, before the additive evidence field exists.
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["EnumeratedStates"] = map[int64]mailbox.State{n: enumerated}
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return path, a, p, v
}

// Frozen v2 item JSON order and domain: pending v2 confirmations must never
// inherit current authority, including when no casing pair is present.
type reconciliationV2Item struct {
	OriginalFolder      string               `json:"original_folder,omitempty"`
	OriginalMemberships []mailbox.Membership `json:"original_memberships,omitempty"`
	MembershipChanged   bool                 `json:"membership_changed"`
	N                   int64                `json:"n"`
	OriginalID          string               `json:"original_id"`
	Removed             bool                 `json:"original_removed"`
	Disposition         string               `json:"disposition"`
	Reason              string               `json:"reason,omitempty"`
	State               *mailbox.State       `json:"state,omitempty"`
}

func reconciliationV2Items(r LegacyReconciliationReceipt) []reconciliationV2Item {
	out := make([]reconciliationV2Item, len(r.Items))
	for j, i := range r.Items {
		out[j] = reconciliationV2Item{i.OriginalFolder, i.OriginalMemberships, i.MembershipChanged, i.N, i.OriginalID, i.Removed, i.Disposition, i.Reason, i.State}
	}
	return out
}
func reconciliationV2Digest(t *testing.T, r LegacyReconciliationReceipt) string {
	t.Helper()
	raw, err := json.Marshal(struct {
		Domain           string
		Account          mailbox.Account
		Source, Coverage string
		Folders          map[string]mailbox.CollectionRef
		Items            []reconciliationV2Item
	}{"mailpipe-legacy-reconciliation-v2", r.Account, r.SourceDigest, r.Coverage, r.Folders, reconciliationV2Items(r)})
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func TestReconciliationCasingBindingPreservesHistoryAndAuthoritativeInventory(t *testing.T) {
	ctx := context.Background()
	original := rawFixtureDB(t, frozenLegacy(t))
	specs, err := tableSpecs(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	original.Close()
	path, a, p, v := casingPreparation(t)
	db := rawFixtureDB(t, path)
	before, err := dataDigest(ctx, db, specs)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	enumerated := v.EnumeratedStates[p.Live[0].N]
	enumerated.Metadata.Sender.Address = strings.ToUpper(enumerated.Metadata.Sender.Address)
	enumerated.Metadata.Sender.Name = "Before Display"
	current := v.Verification.States[p.Live[0].N]
	current.Metadata.Sender.Name = "After Display"
	v.Verification.States[p.Live[0].N] = current
	v.EnumeratedStates[p.Live[0].N] = enumerated
	v = referenceFixture(a, p, v)
	r, err := ReviewLegacyReconciliation(a, p, v)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := ReconcileLegacy(ctx, path, a, p, r.Digest, reconcileLegacyFunc(func(_ context.Context, _ mailbox.Account, exported LegacyPreparation) (LegacyReconciliation, error) {
		// The provider sees exported copies, never the transaction's authority.
		exported.Live[0].GraphID = "forged"
		exported.Removed[0].GraphID = "forged-removed"
		return v, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	after, err := dataDigest(ctx, bound.store.db, specs)
	if err != nil || after != before {
		t.Fatal("casing attribution replaced original history", err)
	}
	m, err := bound.GetMessage(ctx, mailbox.LocalRef{AccountID: a.ID, N: p.Live[0].N})
	if err != nil || m.State.Metadata.Recipients[0].Address != "reader@example.org" || m.State.Metadata.Sender.Name != "After Display" {
		t.Fatal("inspection spelling not published exactly", err)
	}
	if err = bound.Close(); err != nil {
		t.Fatal(err)
	}
	read, found, err := ReadLegacyReconciliationReceipt(ctx, path, a)
	if err != nil || !found || read.Version != 9 || read.Digest != r.Digest || read.Items[0].EnumeratedState != nil || read.Items[0].ReadRef == nil || !read.Items[0].SourceValidated || read.Items[0].State.Metadata.Sender.Name != "After Display" {
		t.Fatal("receipt pair not retained offline", err)
	}
}
