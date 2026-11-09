package store

import (
	"context"
	"encoding/json"
	"errors"
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

func TestReconciliationRecipientNameIndependentReview(t *testing.T) {
	for _, casing := range []bool{false, true} {
		t.Run(map[bool]string{false: "name-only", true: "combined"}[casing], func(t *testing.T) {
			_, a, p, v := recipientNamePreparation(t, casing)
			n := p.Live[0].N
			before, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			r, err := ReviewLegacyReconciliation(a, p, v)
			if err != nil {
				t.Fatal(err)
			}
			i := r.Items[0]
			if r.Version != 8 || !i.RecipientNameChanged || i.RecipientCasingChanged != casing || i.SenderNameChanged || i.SenderCasingChanged || i.SenderAddressDisagrees {
				t.Fatal("independent flags or version incorrect")
			}
			if !reflect.DeepEqual(i.EnumeratedState.Metadata.Recipients, v.EnumeratedStates[n].Metadata.Recipients) || !reflect.DeepEqual(i.State.Metadata.Recipients, v.Verification.States[n].Metadata.Recipients) || !recipientNameEvidenceUnchanged(t, v, before) {
				t.Fatal("raw observations changed or inputs mutated")
			}
			owned := ownReconciliation(v)
			v.EnumeratedStates[n].Metadata.Recipients[0].Name = "Mutated original"
			v.Verification.States[n].Metadata.Recipients[0].Name = "Mutated current"
			v.EnumeratedStates[n].Memberships[0].Collection.CollectionID = "mutated"
			again, err := ReviewLegacyReconciliation(a, p, owned)
			if err != nil || again.Digest != r.Digest || again.AuditDigest != r.AuditDigest || i.EnumeratedState.Metadata.Recipients[0].Name != "Original display" || i.State.Metadata.Recipients[0].Name != "Current display" {
				t.Fatal("owned evidence or receipt aliases verifier", err)
			}
			// The flag is independently derived even when duplicate address slots are
			// indistinguishable; comparison remains positional, never a cross-slot join.
			e := owned.EnumeratedStates[n]
			s := owned.Verification.States[n]
			s.Metadata.Recipients[0].Name, s.Metadata.Recipients[1].Name = e.Metadata.Recipients[1].Name, e.Metadata.Recipients[0].Name
			owned.Verification.States[n] = s
			swapped, err := ReviewLegacyReconciliation(a, p, owned)
			if err != nil || !swapped.Items[0].RecipientNameChanged || swapped.Digest == r.Digest {
				t.Fatal("duplicate slots lost indexed name evidence", err)
			}
		})
	}
}

func TestReconciliationRecipientNameEncodingAndRawDigest(t *testing.T) {
	for _, pair := range [][2]string{{"", "Display"}, {"Display", ""}, {" Name ", "Name"}, {"Équipe", "受信者"}, {"Before\x1b\u202e", "After\n\t"}, {"\xff", "Name"}, {"Name", "\xff"}, {"\xff", "\xfe"}, {"\xff", "\xff"}} {
		_, a, p, v := recipientNamePreparation(t, true)
		n := p.Live[0].N
		e, s := v.EnumeratedStates[n], v.Verification.States[n]
		e.Metadata.Recipients[0].Name, s.Metadata.Recipients[0].Name = pair[0], pair[1]
		v.EnumeratedStates[n], v.Verification.States[n] = e, s
		r, err := ReviewLegacyReconciliation(a, p, v)
		invalid := pair[0] != pair[1] && (pair[0] == "\xff" || pair[1] == "\xff" || pair[1] == "\xfe")
		if invalid {
			if err == nil || !reflect.DeepEqual(r, LegacyReconciliationReceipt{}) {
				t.Fatal("changed invalid name produced proof")
			}
			continue
		}
		if err != nil || r.Items[0].RecipientNameChanged != (pair[0] != pair[1]) || !r.Items[0].RecipientCasingChanged {
			t.Fatal("encoding or independent casing behavior changed", err)
		}
	}
	path, a, p, v := recipientNamePreparation(t, false)
	n := p.Live[0].N
	base, err := ReviewLegacyReconciliation(a, p, v)
	if err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"enumeration", "inspection"} {
		t.Run(side, func(t *testing.T) {
			x := ownReconciliation(v)
			states := x.EnumeratedStates
			if side == "inspection" {
				states = x.Verification.States
			}
			states[n].Metadata.Recipients[0].Name = "Other raw display"
			r, err := ReviewLegacyReconciliation(a, p, x)
			if err != nil || (r.Digest == base.Digest) != (side == "enumeration") || r.AuditDigest == base.AuditDigest {
				t.Fatal("approval/audit sensitivity incorrect", err)
			}
			if side == "inspection" {
				bound, err := ReconcileLegacy(context.Background(), path, a, p, base.Digest, reconcileLegacyFunc(func(context.Context, mailbox.Account, LegacyPreparation) (LegacyReconciliation, error) { return x, nil }))
				if bound != nil {
					bound.Close()
				}
				if !errors.Is(err, mailbox.ErrStaleIdentity) {
					t.Fatal("changed name reused reviewed R", err)
				}
			}
		})
	}
}

func TestReconciliationRecipientNameFinalTransaction(t *testing.T) {
	for _, kind := range []string{"valid", "enumeration", "inspection", "missing-pair", "name-flag", "casing-flag", "old-v6"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			path, a, p, v := recipientNamePreparation(t, false)
			n := p.Live[0].N
			r, err := ReviewLegacyReconciliation(a, p, v)
			if err != nil {
				t.Fatal(err)
			}
			owned := ownReconciliation(v)
			confirmation := r.Digest
			switch kind {
			case "enumeration":
				owned.EnumeratedStates[n].Metadata.Recipients[0].Name = "Tampered enumeration"
			case "inspection":
				owned.Verification.States[n].Metadata.Recipients[0].Name = "Tampered inspection"
			case "missing-pair":
				owned.EnumeratedStates = nil
			case "name-flag", "casing-flag":
				if kind == "name-flag" {
					r.Items[0].RecipientNameChanged = false
				} else {
					r.Items[0].RecipientCasingChanged = true
				}
				var e error
				r.Digest, r.AuditDigest, e = reconciliationReceiptDigests(r)
				if e != nil {
					t.Fatal(e)
				}
				confirmation = r.Digest
			case "old-v6":
				confirmation = reconciliationV6Digest(t, r)
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
					t.Fatal("final transaction accepted tampered name authority", err)
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
			if err = conn.QueryRowContext(ctx, "select item from legacy_reconciliation_items where n=?", n).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var item LegacyReconciliationItem
			if err = json.Unmarshal([]byte(raw), &item); err != nil || !item.RecipientNameChanged || item.RecipientCasingChanged || item.EnumeratedState.Metadata.Recipients[0].Name != "Original display" || item.State.Metadata.Recipients[0].Name != "Current display" {
				t.Fatal("committed raw pair or flags lost", err)
			}
		})
	}
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

func TestReconciliationRecipientNameDoesNotRelaxAddressShapeOrMetadata(t *testing.T) {
	for _, kind := range []string{"order", "nilness", "cardinality", "multiplicity", "trim", "alias", "full-fold", "normalization", "subject", "preview", "revision", "membership", "header", "account", "locator", "original-sender"} {
		t.Run(kind, func(t *testing.T) {
			_, a, p, v := recipientNamePreparation(t, false)
			n := p.Live[0].N
			e, s := v.EnumeratedStates[n], v.Verification.States[n]
			switch kind {
			case "order":
				s.Metadata.Recipients[0], s.Metadata.Recipients[2] = s.Metadata.Recipients[2], s.Metadata.Recipients[0]
			case "nilness":
				e.Metadata.Recipients = nil
				s.Metadata.Recipients = []mailbox.Address{}
			case "cardinality":
				s.Metadata.Recipients = s.Metadata.Recipients[:2]
			case "multiplicity":
				s.Metadata.Recipients[1].Address = "other@example.org"
			case "trim":
				s.Metadata.Recipients[0].Address = " reader@example.org "
			case "alias":
				s.Metadata.Recipients[0].Address = "reader+tag@example.org"
			case "full-fold":
				e.Metadata.Recipients[0].Address = "ß@example.org"
				s.Metadata.Recipients[0].Address = "ss@example.org"
			case "normalization":
				e.Metadata.Recipients[0].Address = "é@example.org"
				s.Metadata.Recipients[0].Address = "e\u0301@example.org"
			case "subject":
				s.Metadata.Subject = "Changed"
			case "preview":
				s.Metadata.Preview = "Changed"
			case "revision":
				e.Revision = "old"
				s.Revision = "new"
			case "membership":
				e.Memberships[0].Collection.CollectionID = "other"
			case "header":
				s.Metadata.InternetMessageID = "<other@example.org>"
			case "account":
				e.Ref.AccountID = "other"
			case "locator":
				e.Ref.Locator = "other"
			case "original-sender":
				s.Metadata.Sender.Address = "different@example.org"
			}
			v.EnumeratedStates[n], v.Verification.States[n] = e, s
			if r, err := ReviewLegacyReconciliation(a, p, v); err == nil || !reflect.DeepEqual(r, LegacyReconciliationReceipt{}) {
				t.Fatal("recipient name masked unrelated invalid evidence")
			}
		})
	}
}
