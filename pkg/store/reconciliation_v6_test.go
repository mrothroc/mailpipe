package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// Frozen v6 wire shape: never embed the evolving current item DTO here.
type reconciliationV6Item struct {
	SenderAddressDisagrees bool                 `json:"sender_address_disagrees"`
	SenderNameChanged      bool                 `json:"sender_name_changed"`
	SenderCasingChanged    bool                 `json:"sender_casing_changed"`
	RecipientCasingChanged bool                 `json:"recipient_casing_changed"`
	EnumeratedState        *mailbox.State       `json:"enumerated_state,omitempty"`
	OriginalFolder         string               `json:"original_folder,omitempty"`
	OriginalMemberships    []mailbox.Membership `json:"original_memberships,omitempty"`
	MembershipChanged      bool                 `json:"membership_changed"`
	N                      int64                `json:"n"`
	OriginalID             string               `json:"original_id"`
	Removed                bool                 `json:"original_removed"`
	Disposition            string               `json:"disposition"`
	Reason                 string               `json:"reason,omitempty"`
	State                  *mailbox.State       `json:"state,omitempty"`
}

func reconciliationV6Items(r LegacyReconciliationReceipt) []reconciliationV6Item {
	out := make([]reconciliationV6Item, len(r.Items))
	for j, i := range r.Items {
		out[j] = reconciliationV6Item{i.SenderAddressDisagrees, i.SenderNameChanged, i.SenderCasingChanged, i.RecipientCasingChanged, i.EnumeratedState, i.OriginalFolder, i.OriginalMemberships, i.MembershipChanged, i.N, i.OriginalID, i.Removed, i.Disposition, i.Reason, i.State}
	}
	return out
}
func reconciliationV6Digest(t *testing.T, r LegacyReconciliationReceipt) string {
	t.Helper()
	raw, err := json.Marshal(struct {
		Domain           string
		Account          mailbox.Account
		Source, Coverage string
		Folders          map[string]mailbox.CollectionRef
		Items            []reconciliationV6Item
	}{"mailpipe-legacy-reconciliation-v6", r.Account, r.SourceDigest, r.Coverage, r.Folders, reconciliationV6Items(r)})
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// Prove the literal v6 shape and domain against the actual current compiler DTO
// before that DTO evolves; subsequent versions retain this independent helper.
func TestReconciliationV6FrozenShape(t *testing.T) {
	for _, paired := range []bool{false, true} {
		_, a, p, v := casingPreparation(t)
		if !paired {
			v.EnumeratedStates = nil
		}
		r, err := ReviewLegacyReconciliation(a, p, v)
		if err != nil {
			t.Fatal(err)
		}
		current, err := json.Marshal(r.Items)
		if err != nil {
			t.Fatal(err)
		}
		frozen, err := json.Marshal(reconciliationV6Items(r))
		if err != nil {
			t.Fatal(err)
		}
		var decoded []reconciliationV6Item
		if err = json.Unmarshal(current, &decoded); err != nil {
			t.Fatal(err)
		}
		retained, err := json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if string(retained) != string(frozen) || string(current) == string(frozen) || r.Version != 7 || r.Digest == reconciliationV6Digest(t, r) {
			t.Fatal("frozen v6 shape or v7 domain separation changed")
		}
	}
}
