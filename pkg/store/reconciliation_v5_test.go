package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// Frozen v5 wire shape: never embed the evolving current item DTO here.
type reconciliationV5Item struct {
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

func reconciliationV5Items(r LegacyReconciliationReceipt) []reconciliationV5Item {
	out := make([]reconciliationV5Item, len(r.Items))
	for j, i := range r.Items {
		out[j] = reconciliationV5Item{i.SenderNameChanged, i.SenderCasingChanged, i.RecipientCasingChanged, i.EnumeratedState, i.OriginalFolder, i.OriginalMemberships, i.MembershipChanged, i.N, i.OriginalID, i.Removed, i.Disposition, i.Reason, i.State}
	}
	return out
}
func reconciliationV5Digest(t *testing.T, r LegacyReconciliationReceipt) string {
	t.Helper()
	raw, err := json.Marshal(struct {
		Domain           string
		Account          mailbox.Account
		Source, Coverage string
		Folders          map[string]mailbox.CollectionRef
		Items            []reconciliationV5Item
	}{"mailpipe-legacy-reconciliation-v5", r.Account, r.SourceDigest, r.Coverage, r.Folders, reconciliationV5Items(r)})
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func TestReconciliationV5FrozenShape(t *testing.T) {
	_, a, p, v := casingPreparation(t)
	r := frozenV8Fixture(t, a, p, v)
	current, err := json.Marshal(r.Items)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := json.Marshal(reconciliationV5Items(r))
	if err != nil {
		t.Fatal(err)
	}
	var decoded []reconciliationV5Item
	if err = json.Unmarshal(current, &decoded); err != nil {
		t.Fatal(err)
	}
	retained, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(retained) != string(frozen) || string(current) == string(frozen) || r.Version != 8 || r.Digest == reconciliationV5Digest(t, r) {
		t.Fatal("v5 frozen fields or current domain separation changed")
	}
}
