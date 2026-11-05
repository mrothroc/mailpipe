package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
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

func TestReconciliationCasingReceipt(t *testing.T) {
	_, a, p, v := casingPreparation(t)
	r, err := ReviewLegacyReconciliation(a, p, v)
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != 3 {
		t.Fatalf("new casing review needs v3, got %d", r.Version)
	}
	raw, _ := json.Marshal(r.Items[0])
	var item struct {
		Changed     bool           `json:"recipient_casing_changed"`
		Enumeration *mailbox.State `json:"enumerated_state"`
	}
	if err = json.Unmarshal(raw, &item); err != nil || !item.Changed || item.Enumeration == nil || item.Enumeration.Metadata.Recipients[0].Address != "Reader@Example.org" || r.Items[0].State.Metadata.Recipients[0].Address != "reader@example.org" {
		t.Fatal("receipt lost exact observations")
	}
}

func TestReconciliationCasingValidationAndDigest(t *testing.T) {
	path, a, p, v := casingPreparation(t)
	n := p.Live[0].N
	base, err := ReviewLegacyReconciliation(a, p, v)
	if err != nil {
		t.Fatal(err)
	}
	clone := func() LegacyReconciliation {
		var out LegacyReconciliation
		raw, _ := json.Marshal(v)
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, kind := range []string{"unknown", "historical", "unavailable", "redundant", "account", "locator", "membership", "revision", "name", "order", "cardinality", "multiplicity", "trim", "address", "invalid-utf8", "subject", "header", "sender", "received", "preview", "read", "attachments"} {
		t.Run(kind, func(t *testing.T) {
			x := clone()
			e := x.EnumeratedStates[n]
			s := x.Verification.States[n]
			switch kind {
			case "unknown":
				x.EnumeratedStates[999] = e
			case "historical":
				x.EnumeratedStates[p.Removed[0].N] = e
			case "unavailable":
				delete(x.Verification.States, n)
				x.Unavailable = map[int64]string{n: LegacyUnavailable}
			case "redundant":
				e = s
			case "account":
				e.Ref.AccountID = "other-account"
			case "locator":
				e.Ref.Locator = "other"
			case "membership":
				e.Memberships[0].Collection.CollectionID = "other"
			case "revision":
				e.Revision = "other"
			case "name":
				e.Metadata.Recipients[0].Name = "READER"
			case "order":
				e.Metadata.Recipients = append(e.Metadata.Recipients, mailbox.Address{Address: "second@example.org", Name: "Other"})
				s.Metadata.Recipients = append([]mailbox.Address{{Address: "second@example.org", Name: "Other"}}, s.Metadata.Recipients...)
			case "cardinality":
				e.Metadata.Recipients = append(e.Metadata.Recipients, e.Metadata.Recipients[0])
			case "multiplicity":
				e.Metadata.Recipients = append(e.Metadata.Recipients, e.Metadata.Recipients[0])
				s.Metadata.Recipients = append(s.Metadata.Recipients, mailbox.Address{Address: "second@example.org", Name: "Reader"})
			case "trim":
				e.Metadata.Recipients[0].Address = " Reader@Example.org "
			case "address":
				e.Metadata.Recipients[0].Address = "other@example.org"
			case "invalid-utf8":
				e.Metadata.Recipients[0].Address = string([]byte{0xff})
				s.Metadata.Recipients[0].Address = string([]byte{0xfe})
			case "subject":
				e.Metadata.Subject = "changed"
			case "header":
				e.Metadata.InternetMessageID = "<other@example.org>"
			case "sender":
				e.Metadata.Sender.Name = "other"
			case "received":
				e.Metadata.Received = time.Now()
			case "preview":
				e.Metadata.Preview = "changed"
			case "read":
				e.Metadata.IsRead = !s.Metadata.IsRead
			case "attachments":
				e.Metadata.HasAttachments = !s.Metadata.HasAttachments
			}
			x.EnumeratedStates[n] = e
			if kind != "unavailable" {
				x.Verification.States[n] = s
			}
			if r, err := ReviewLegacyReconciliation(a, p, x); err == nil || !reflect.DeepEqual(r, LegacyReconciliationReceipt{}) {
				t.Fatal("invalid casing proof returned proposal")
			}
		})
	}
	for _, side := range []string{"enumeration", "inspection", "timestamps"} {
		t.Run(side, func(t *testing.T) {
			x := clone()
			if side == "enumeration" {
				s := x.EnumeratedStates[n]
				s.Metadata.Recipients[0].Address = "READER@EXAMPLE.ORG"
				x.EnumeratedStates[n] = s
			}
			if side == "inspection" {
				s := x.Verification.States[n]
				s.Metadata.Recipients[0].Address = "reader@EXAMPLE.ORG"
				x.Verification.States[n] = s
			}
			if side == "timestamps" {
				x.Started = x.Started.Add(time.Second)
				x.Finished = x.Finished.Add(time.Second)
			}
			r, err := ReviewLegacyReconciliation(a, p, x)
			if err != nil {
				t.Fatal(err)
			}
			if (r.Digest == base.Digest) != (side == "timestamps") {
				t.Fatal("digest does not cover raw observation pair")
			}
			if side != "timestamps" {
				b, err := ReconcileLegacy(context.Background(), path, a, p, base.Digest, reconcileLegacyFunc(func(context.Context, mailbox.Account, LegacyPreparation) (LegacyReconciliation, error) { return x, nil }))
				if b != nil {
					b.Close()
				}
				if !errors.Is(err, mailbox.ErrStaleIdentity) {
					t.Fatal("changed raw spelling executed old proposal", err)
				}
			}
		})
	}
	// Neither a returned receipt nor the frozen owned evidence aliases the
	// verifier's recipient arrays, membership arrays, folder maps or states.
	owned := ownReconciliation(v)
	e := v.EnumeratedStates[n]
	e.Metadata.Recipients[0].Address = "mutated@example.org"
	e.Memberships[0].Collection.CollectionID = "mutated"
	v.EnumeratedStates[n] = e
	s := v.Verification.States[n]
	s.Metadata.Recipients[0].Name = "mutated"
	v.Verification.States[n] = s
	v.Verification.Folders[p.Live[0].Folder] = mailbox.CollectionRef{}
	if base.Items[0].EnumeratedState.Metadata.Recipients[0].Address != "Reader@Example.org" || base.Items[0].State.Metadata.Recipients[0].Name != "Reader" {
		t.Fatal("receipt aliases verifier")
	}
	if r, err := ReviewLegacyReconciliation(a, p, owned); err != nil || r.Digest != base.Digest {
		t.Fatal("owned proof aliases verifier", err)
	}
}

// Frozen v2 item JSON order and domain: pending v2 confirmations must never
// inherit v3 authority, including when no casing pair is present.
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

func TestReconciliationCasingTransactionRevalidatesOwnedPair(t *testing.T) {
	for _, kind := range []string{"valid", "changed-pair", "missing-pair", "v1", "v2"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			path, a, p, v := casingPreparation(t)
			if kind == "v1" || kind == "v2" {
				v.EnumeratedStates = nil
			}
			r, err := ReviewLegacyReconciliation(a, p, v)
			if err != nil {
				t.Fatal(err)
			}
			confirmation := r.Digest
			if kind == "v1" {
				confirmation = reconciliationV1Digest(t, r)
			}
			if kind == "v2" {
				confirmation = reconciliationV2Digest(t, r)
			}
			owned := ownReconciliation(v)
			if kind == "changed-pair" {
				n := p.Live[0].N
				s := owned.EnumeratedStates[n]
				s.Metadata.Recipients[0].Address = "READER@EXAMPLE.ORG"
				owned.EnumeratedStates[n] = s
			}
			if kind == "missing-pair" {
				owned.EnumeratedStates = nil
			}
			life, db, err := maintenanceDB(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer closeMaintenance(life, db)
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			before, err := logicalDigest(ctx, conn)
			if err != nil {
				t.Fatal(err)
			}
			bound := &AccountStore{account: a}
			err = immediate(ctx, conn, func(q sqlAccess) error {
				return bound.bindLegacyTransaction(ctx, q, p, v.Verification, v.Unavailable, &legacyReconciliationRequest{receipt: r, confirmation: confirmation, evidence: owned}, "invented-backup", p.Digest())
			})
			if kind != "valid" {
				if !errors.Is(err, mailbox.ErrStaleIdentity) {
					t.Fatal("final transaction accepted stale casing evidence", err)
				}
				after, e := logicalDigest(ctx, conn)
				if e != nil || after != before {
					t.Fatal("failed transaction changed history", e)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var raw string
			if err = conn.QueryRowContext(ctx, "select item from legacy_reconciliation_items where n=?", p.Live[0].N).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var item LegacyReconciliationItem
			if err = json.Unmarshal([]byte(raw), &item); err != nil || !item.RecipientCasingChanged || item.EnumeratedState.Metadata.Recipients[0].Address != "Reader@Example.org" {
				t.Fatal("committed pair changed", err)
			}
		})
	}
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
	if err != nil || m.State.Metadata.Recipients[0].Address != "reader@example.org" {
		t.Fatal("inspection spelling not published exactly", err)
	}
	if err = bound.Close(); err != nil {
		t.Fatal(err)
	}
	read, found, err := ReadLegacyReconciliationReceipt(ctx, path, a)
	if err != nil || !found || read.Version != 3 || read.Digest != r.Digest || read.Items[0].EnumeratedState.Metadata.Recipients[0].Address != "Reader@Example.org" {
		t.Fatal("receipt pair not retained offline", err)
	}
}
