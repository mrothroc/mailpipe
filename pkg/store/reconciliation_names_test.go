package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func TestReconciliationSenderNamePair(t *testing.T) {
	for _, combined := range []bool{false, true} {
		t.Run(map[bool]string{false: "name-only", true: "combined"}[combined], func(t *testing.T) {
			_, a, p, v := casingPreparation(t)
			n := p.Live[0].N
			e, s := v.EnumeratedStates[n], v.Verification.States[n]
			e.Metadata.Sender.Name = "Original Display"
			s.Metadata.Sender.Name = "Current Display"
			if !combined {
				e.Metadata.Recipients = append([]mailbox.Address(nil), s.Metadata.Recipients...)
			} else {
				e.Metadata.Sender.Address = "SENDER@Example.org"
			}
			v.EnumeratedStates[n], v.Verification.States[n] = e, s
			r, err := ReviewLegacyReconciliation(a, p, v)
			if err != nil {
				t.Fatal("sender name pair rejected", err)
			}
			raw, err := json.Marshal(r.Items[0])
			if err != nil {
				t.Fatal(err)
			}
			var flags map[string]any
			if err = json.Unmarshal(raw, &flags); err != nil {
				t.Fatal(err)
			}
			if r.Version != 8 || flags["sender_name_changed"] != true || flags["sender_casing_changed"] != combined || flags["recipient_casing_changed"] != combined {
				t.Fatal("flags or v8 not derived")
			}
			if r.Items[0].EnumeratedState.Metadata.Sender.Name != e.Metadata.Sender.Name || r.Items[0].State.Metadata.Sender.Name != s.Metadata.Sender.Name {
				t.Fatal("raw names lost")
			}
		})
	}
}

// Frozen v4 item order/domain, independent of the current receipt DTO.
type reconciliationV4Item struct {
	SenderCasingChanged bool `json:"sender_casing_changed"`
	reconciliationV3Item
}

func reconciliationV4Digest(t *testing.T, r LegacyReconciliationReceipt) string {
	t.Helper()
	old := reconciliationV2Items(r)
	items := make([]reconciliationV4Item, len(r.Items))
	for i, item := range r.Items {
		items[i] = reconciliationV4Item{item.SenderCasingChanged, reconciliationV3Item{item.RecipientCasingChanged, item.EnumeratedState, old[i]}}
	}
	raw, err := json.Marshal(struct {
		Domain           string
		Account          mailbox.Account
		Source, Coverage string
		Folders          map[string]mailbox.CollectionRef
		Items            []reconciliationV4Item
	}{"mailpipe-legacy-reconciliation-v4", r.Account, r.SourceDigest, r.Coverage, r.Folders, items})
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func TestReconciliationSenderNameRawDigestAndEncoding(t *testing.T) {
	path, a, p, v := casingPreparation(t)
	n := p.Live[0].N
	e, s := v.EnumeratedStates[n], v.Verification.States[n]
	e.Metadata.Sender.Name, s.Metadata.Sender.Name = "Before Display", "After Display"
	v.EnumeratedStates[n], v.Verification.States[n] = e, s
	base, err := ReviewLegacyReconciliation(a, p, v)
	if err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"enumerated", "inspection"} {
		t.Run(side, func(t *testing.T) {
			x := ownReconciliation(v)
			states := x.EnumeratedStates
			if side == "inspection" {
				states = x.Verification.States
			}
			s := states[n]
			s.Metadata.Sender.Name = "Changed Display"
			states[n] = s
			r, err := ReviewLegacyReconciliation(a, p, x)
			if err != nil || (r.Digest == base.Digest) != (side == "enumerated") || r.AuditDigest == base.AuditDigest {
				t.Fatal("approval/audit sensitivity incorrect", err)
			}
			if side == "inspection" {
				b, err := ReconcileLegacy(context.Background(), path, a, p, base.Digest, reconcileLegacyFunc(func(context.Context, mailbox.Account, LegacyPreparation) (LegacyReconciliation, error) { return x, nil }))
				if b != nil {
					b.Close()
				}
				if !errors.Is(err, mailbox.ErrStaleIdentity) {
					t.Fatal("changed permitted name reused confirmation", err)
				}
			}
			s.Metadata.Sender.Name = string([]byte{0xff})
			states[n] = s
			if r, err = ReviewLegacyReconciliation(a, p, x); err == nil || !reflect.DeepEqual(r, LegacyReconciliationReceipt{}) {
				t.Fatal("invalid UTF8 name produced authority")
			}
		})
	}
	owned := ownReconciliation(v)
	e = v.EnumeratedStates[n]
	e.Metadata.Sender.Name = "Mutated"
	v.EnumeratedStates[n] = e
	if r, err := ReviewLegacyReconciliation(a, p, owned); err != nil || r.Digest != base.Digest || r.AuditDigest != base.AuditDigest || base.Items[0].EnumeratedState.Metadata.Sender.Name != "Before Display" {
		t.Fatal("raw name snapshot aliases verifier", err)
	}
	// Identical name encodings retain the prior address-only behavior.
	e = owned.EnumeratedStates[n]
	s = owned.Verification.States[n]
	e.Metadata.Sender.Name = string([]byte{0xff})
	s.Metadata.Sender.Name = e.Metadata.Sender.Name
	owned.EnumeratedStates[n], owned.Verification.States[n] = e, s
	if _, err := ReviewLegacyReconciliation(a, p, owned); err != nil {
		t.Fatal("unchanged name changed address-only behavior", err)
	}
}
