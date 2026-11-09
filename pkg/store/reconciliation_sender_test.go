package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func TestReconciliationSenderPairReview(t *testing.T) {
	for _, combined := range []bool{false, true} {
		t.Run(map[bool]string{false: "sender-only", true: "combined"}[combined], func(t *testing.T) {
			_, a, p, v := casingPreparation(t)
			n := p.Live[0].N
			e, s := v.EnumeratedStates[n], v.Verification.States[n]
			e.Metadata.Sender.Address = strings.ToUpper(s.Metadata.Sender.Address)
			if !combined {
				e.Metadata.Recipients = append(e.Metadata.Recipients[:0:0], s.Metadata.Recipients...)
			}
			v.EnumeratedStates[n] = e
			r, err := ReviewLegacyReconciliation(a, p, v)
			if err != nil {
				t.Fatalf("sender pair rejected: %v", err)
			}
			if r.Version != 8 {
				t.Fatal("new proposal must use v8")
			}
			raw, _ := json.Marshal(r.Items[0])
			var item struct {
				Sender    bool `json:"sender_casing_changed"`
				Name      bool `json:"sender_name_changed"`
				Recipient bool `json:"recipient_casing_changed"`
			}
			if err = json.Unmarshal(raw, &item); err != nil || !item.Sender || item.Name || item.Recipient != combined {
				t.Fatal("flags not independently derived")
			}
			if r.Items[0].EnumeratedState.Metadata.Sender.Address != e.Metadata.Sender.Address || r.Items[0].State.Metadata.Sender.Address != s.Metadata.Sender.Address {
				t.Fatal("sender raw pair lost")
			}
		})
	}
}

// Frozen v3 shape/domain. Adding sender evidence must not turn a pending v3 R
// into v4 authority, or rewrite a previously committed recipient-only receipt.
type reconciliationV3Item struct {
	RecipientCasingChanged bool           `json:"recipient_casing_changed"`
	EnumeratedState        *mailbox.State `json:"enumerated_state,omitempty"`
	reconciliationV2Item
}

func reconciliationV3Digest(t *testing.T, r LegacyReconciliationReceipt) string {
	t.Helper()
	old := reconciliationV2Items(r)
	items := make([]reconciliationV3Item, len(r.Items))
	for i, item := range r.Items {
		items[i] = reconciliationV3Item{item.RecipientCasingChanged, item.EnumeratedState, old[i]}
	}
	raw, err := json.Marshal(struct {
		Domain           string
		Account          mailbox.Account
		Source, Coverage string
		Folders          map[string]mailbox.CollectionRef
		Items            []reconciliationV3Item
	}{"mailpipe-legacy-reconciliation-v3", r.Account, r.SourceDigest, r.Coverage, r.Folders, items})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestReconciliationSenderPairValidationAndRawDigest(t *testing.T) {
	path, a, p, v := casingPreparation(t)
	n := p.Live[0].N
	e := v.EnumeratedStates[n]
	e.Metadata.Sender.Address = strings.ToUpper(e.Metadata.Sender.Address)
	v.EnumeratedStates[n] = e
	base, err := ReviewLegacyReconciliation(a, p, v)
	if err != nil {
		t.Fatal(err)
	}
	clone := func() LegacyReconciliation {
		var x LegacyReconciliation
		raw, _ := json.Marshal(v)
		if err := json.Unmarshal(raw, &x); err != nil {
			t.Fatal(err)
		}
		return x
	}
	for _, kind := range []string{"trim", "joint", "alias", "dot", "different", "name-invalid", "invalid", "header", "subject", "recipient-name-invalid", "recipient-order", "revision", "membership", "account", "id"} {
		t.Run(kind, func(t *testing.T) {
			x := clone()
			e := x.EnumeratedStates[n]
			switch kind {
			case "trim":
				e.Metadata.Sender.Address = " " + x.Verification.States[n].Metadata.Sender.Address + " "
			case "joint":
				e.Metadata.Sender.Address = " " + e.Metadata.Sender.Address + " "
			case "alias":
				e.Metadata.Sender.Address = "sender+tag@example.org"
			case "dot":
				e.Metadata.Sender.Address = "sen.der@example.org"
			case "different":
				e.Metadata.Sender.Address = "other@example.org"
			case "name-invalid":
				e.Metadata.Sender.Name = string([]byte{0xff})
			case "invalid":
				e.Metadata.Sender.Address = string([]byte{0xff})
			case "header":
				e.Metadata.InternetMessageID = "<other@example.org>"
			case "subject":
				e.Metadata.Subject = "Changed"
			case "recipient-name-invalid":
				e.Metadata.Recipients[0].Name = "\xff"
			case "recipient-order":
				e.Metadata.Recipients = append(e.Metadata.Recipients, mailbox.Address{Address: "second@example.org", Name: "Other"})
				s := x.Verification.States[n]
				s.Metadata.Recipients = append([]mailbox.Address{{Address: "second@example.org", Name: "Other"}}, s.Metadata.Recipients...)
				x.Verification.States[n] = s
			case "revision":
				e.Revision = "changed"
			case "membership":
				e.Memberships[0].Collection.CollectionID = "other"
			case "account":
				e.Ref.AccountID = "other"
			case "id":
				e.Ref.MessageID = "other"
			}
			x.EnumeratedStates[n] = e
			if r, err := ReviewLegacyReconciliation(a, p, x); err == nil || !reflect.DeepEqual(r, LegacyReconciliationReceipt{}) {
				t.Fatal("invalid sender pair produced authority")
			}
		})
	}
	for _, side := range []string{"enumeration", "inspection"} {
		t.Run(side, func(t *testing.T) {
			x := clone()
			states := x.EnumeratedStates
			if side == "inspection" {
				states = x.Verification.States
			}
			s := states[n]
			if side == "enumeration" {
				s.Metadata.Sender.Address = strings.ToLower(s.Metadata.Sender.Address[:1]) + s.Metadata.Sender.Address[1:]
			} else {
				s.Metadata.Sender.Address = strings.ToUpper(s.Metadata.Sender.Address[:1]) + s.Metadata.Sender.Address[1:]
			}
			states[n] = s
			r, err := ReviewLegacyReconciliation(a, p, x)
			if err != nil || (r.Digest == base.Digest) != (side == "enumeration") || r.AuditDigest == base.AuditDigest {
				t.Fatal("approval/audit sensitivity incorrect", err)
			}
			if side == "inspection" {
				b, err := ReconcileLegacy(context.Background(), path, a, p, base.Digest, reconcileLegacyFunc(func(context.Context, mailbox.Account, LegacyPreparation) (LegacyReconciliation, error) { return x, nil }))
				if b != nil {
					b.Close()
				}
				if !errors.Is(err, mailbox.ErrStaleIdentity) {
					t.Fatal("old R accepted changed sender spelling", err)
				}
			}
		})
	}
	owned := ownReconciliation(v)
	s := v.EnumeratedStates[n]
	s.Metadata.Sender.Address = "mutated@example.org"
	v.EnumeratedStates[n] = s
	if r, err := ReviewLegacyReconciliation(a, p, owned); err != nil || r.Digest != base.Digest || r.AuditDigest != base.AuditDigest || base.Items[0].EnumeratedState.Metadata.Sender.Address == "mutated@example.org" {
		t.Fatal("owned sender pair aliases provider", err)
	}
}

func TestStrictLegacySenderRuleUnchanged(t *testing.T) {
	for _, trim := range []bool{false, true} {
		t.Run(map[bool]string{false: "case", true: "existing-trim"}[trim], func(t *testing.T) {
			path, a, p, _ := casingPreparation(t)
			v := goodLegacyProof(a, p)
			n := p.Live[0].N
			s := v.States[n]
			s.Metadata.Sender.Address = strings.ToUpper(s.Metadata.Sender.Address)
			if trim {
				s.Metadata.Sender.Address = " " + s.Metadata.Sender.Address + " "
			}
			v.States[n] = s
			b, err := BindLegacy(context.Background(), path, a, p, verifyLegacyFunc(func(context.Context, mailbox.Account, LegacyPreparation) (LegacyVerification, error) { return v, nil }))
			if err != nil {
				t.Fatal("generic strict bind sender policy changed", err)
			}
			b.Close()
		})
	}
}
