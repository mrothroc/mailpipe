package store

import (
	"context"
	"errors"
	"reflect"
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

func TestReconciliationSenderAuthorityIndependentReview(t *testing.T) {
	_, a, p, v := senderAuthorityPreparation(t)
	n := p.Live[0].N
	r, err := ReviewLegacyReconciliation(a, p, v)
	if err != nil {
		t.Fatal(err)
	}
	i := r.Items[0]
	if r.Version != 8 || !i.SenderAddressDisagrees || i.SenderCasingChanged || !i.SenderNameChanged || !i.RecipientCasingChanged || i.EnumeratedState.Metadata.Sender.Address != "enumerated@example.org" || i.State.Metadata.Sender.Address != v.Verification.States[n].Metadata.Sender.Address {
		t.Fatal("raw directional flags not independently derived")
	}
	owned := ownReconciliation(v)
	v.SenderSourceIntegrity[n] = false
	e := v.EnumeratedStates[n]
	e.Metadata.Sender.Address = "mutated@example.org"
	e.Metadata.Recipients[0].Name = "Mutated"
	v.EnumeratedStates[n] = e
	again, err := ReviewLegacyReconciliation(a, p, owned)
	if err != nil || again.Digest != r.Digest || again.AuditDigest != r.AuditDigest || !owned.SenderSourceIntegrity[n] {
		t.Fatal("owned evidence aliases verifier", err)
	}
	for _, field := range []string{"sender", "name", "recipient", "flag"} {
		changed := ownReconciliation(owned)
		e := changed.EnumeratedStates[n]
		switch field {
		case "sender":
			e.Metadata.Sender.Address = "other-enumerated@example.org"
		case "name":
			e.Metadata.Sender.Name = "Other Display"
		case "recipient":
			e.Metadata.Recipients[0].Address = "READER@EXAMPLE.ORG"
		case "flag":
			e.Metadata.Sender.Address = changed.Verification.States[n].Metadata.Sender.Address
			delete(changed.SenderSourceIntegrity, n)
		}
		changed.EnumeratedStates[n] = e
		modified, err := ReviewLegacyReconciliation(a, p, changed)
		if err != nil || modified.Digest != r.Digest || modified.AuditDigest == r.AuditDigest {
			t.Fatal("raw evidence/derived flag not digest-bound", field, err)
		}
	}
}

func TestReconciliationSenderAuthorityRejectsUntrustedPairs(t *testing.T) {
	for _, kind := range []string{"missing-stamp", "false-stamp", "wrong-stamp", "extra-stamp", "no-pair", "case-stamp", "empty-before", "empty-after", "invalid-before", "invalid-after", "original-sender", "header", "ref", "locator", "account", "membership", "revision", "recipient-name-invalid", "recipient-order", "recipient-shape", "subject"} {
		t.Run(kind, func(t *testing.T) {
			_, a, p, v := senderAuthorityPreparation(t)
			n := p.Live[0].N
			e, s := v.EnumeratedStates[n], v.Verification.States[n]
			switch kind {
			case "missing-stamp":
				v.SenderSourceIntegrity = nil
			case "false-stamp":
				v.SenderSourceIntegrity[n] = false
			case "wrong-stamp":
				v.SenderSourceIntegrity = map[int64]bool{n + 100: true}
			case "extra-stamp":
				v.SenderSourceIntegrity[n+100] = true
			case "no-pair":
				v.EnumeratedStates = nil
			case "case-stamp":
				e.Metadata.Sender.Address = s.Metadata.Sender.Address
			case "empty-before":
				e.Metadata.Sender.Address = ""
			case "empty-after":
				s.Metadata.Sender.Address = ""
			case "invalid-before":
				e.Metadata.Sender.Address = "\xff"
			case "invalid-after":
				s.Metadata.Sender.Address = "\xff"
			case "original-sender":
				s.Metadata.Sender.Address = "different@example.org"
			case "header":
				s.Metadata.InternetMessageID = "<other@example.org>"
			case "ref":
				e.Ref.MessageID = "different"
			case "locator":
				e.Ref.Locator = "different"
			case "account":
				e.Ref.AccountID = "different"
			case "membership":
				e.Memberships = []mailbox.Membership{{Collection: mailbox.CollectionRef{AccountID: a.ID, CollectionID: "different"}}}
			case "revision":
				e.Revision = "old"
				s.Revision = "new"
			case "recipient-name-invalid":
				e.Metadata.Recipients[0].Name = "\xff"
			case "recipient-order":
				e.Metadata.Recipients = append(e.Metadata.Recipients, mailbox.Address{Address: "second@example.org", Name: "Second"})
				s.Metadata.Recipients = []mailbox.Address{e.Metadata.Recipients[1], s.Metadata.Recipients[0]}
			case "recipient-shape":
				e.Metadata.Recipients = nil
			case "subject":
				e.Metadata.Subject = "Changed"
			}
			if v.EnumeratedStates != nil {
				v.EnumeratedStates[n] = e
			}
			v.Verification.States[n] = s
			if r, err := ReviewLegacyReconciliation(a, p, v); err == nil || !reflect.DeepEqual(r, LegacyReconciliationReceipt{}) {
				t.Fatal("untrusted pair produced proposal")
			}
		})
	}
}

func TestReconciliationSenderAuthorityFinalTransaction(t *testing.T) {
	for _, kind := range []string{"valid", "stamp", "raw-pair", "receipt-flag", "old-v5"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			path, a, p, v := senderAuthorityPreparation(t)
			n := p.Live[0].N
			r, err := ReviewLegacyReconciliation(a, p, v)
			if err != nil {
				t.Fatal(err)
			}
			owned := ownReconciliation(v)
			confirmation := r.Digest
			switch kind {
			case "stamp":
				owned.SenderSourceIntegrity[n] = false
			case "raw-pair":
				e := owned.EnumeratedStates[n]
				e.Metadata.Sender.Address = "different-enumerated@example.org"
				owned.EnumeratedStates[n] = e
			case "receipt-flag":
				r.Items[0].SenderAddressDisagrees = false
				var e error
				r.Digest, r.AuditDigest, e = reconciliationReceiptDigests(r)
				if e != nil {
					t.Fatal(e)
				}
				confirmation = r.Digest
			case "old-v5":
				confirmation = reconciliationV5Digest(t, r)
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
					t.Fatal("final transaction accepted tampered evidence", err)
				}
				after, e := logicalDigest(ctx, conn)
				if e != nil || before != after {
					t.Fatal("rejected transaction mutated history", e)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
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
	if err != nil || !found || read.Version != 8 || read.Digest != r.Digest || !read.Items[0].SenderAddressDisagrees || read.Items[0].SenderCasingChanged {
		t.Fatal("current offline receipt lost flags", err)
	}
}
