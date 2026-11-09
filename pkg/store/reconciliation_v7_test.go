package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"testing"
	"time"
)

// Frozen actual v7 wire shapes: no embedding of evolving receipt/item types.
type reconciliationV7Item struct {
	RecipientNameChanged   bool                 `json:"recipient_name_changed"`
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
type reconciliationV7Receipt struct {
	Version                        int                              `json:"version"`
	Account                        mailbox.Account                  `json:"account"`
	SourceDigest                   string                           `json:"source_digest"`
	Digest                         string                           `json:"reconciliation_digest"`
	Coverage                       string                           `json:"coverage"`
	Started                        time.Time                        `json:"started"`
	Finished                       time.Time                        `json:"finished"`
	Pages                          int                              `json:"pages"`
	Observed                       int                              `json:"observed"`
	HistoricalAttributionConfirmed bool                             `json:"historical_attribution_confirmed"`
	Backup                         string                           `json:"backup,omitempty"`
	BackupDigest                   string                           `json:"backup_digest,omitempty"`
	Folders                        map[string]mailbox.CollectionRef `json:"folders"`
	Items                          []reconciliationV7Item           `json:"items"`
}

func reconciliationV7Frozen(t *testing.T, r LegacyReconciliationReceipt) reconciliationV7Receipt {
	t.Helper()
	out := reconciliationV7Receipt{Version: r.Version, Account: r.Account, SourceDigest: r.SourceDigest, Digest: r.Digest, Coverage: r.Coverage, Started: r.Started, Finished: r.Finished, Pages: r.Pages, Observed: r.Observed, HistoricalAttributionConfirmed: r.HistoricalAttributionConfirmed, Backup: r.Backup, BackupDigest: r.BackupDigest, Folders: r.Folders}
	if r.Items != nil {
		out.Items = make([]reconciliationV7Item, len(r.Items))
	}
	for j, i := range r.Items {
		out.Items[j] = reconciliationV7Item{i.RecipientNameChanged, i.SenderAddressDisagrees, i.SenderNameChanged, i.SenderCasingChanged, i.RecipientCasingChanged, i.EnumeratedState, i.OriginalFolder, i.OriginalMemberships, i.MembershipChanged, i.N, i.OriginalID, i.Removed, i.Disposition, i.Reason, i.State}
	}
	return out
}
func reconciliationV7Digest(t *testing.T, r LegacyReconciliationReceipt) string {
	t.Helper()
	frozen := reconciliationV7Frozen(t, r)
	raw, err := json.Marshal(struct {
		Domain           string
		Account          mailbox.Account
		Source, Coverage string
		Folders          map[string]mailbox.CollectionRef
		Items            []reconciliationV7Item
	}{"mailpipe-legacy-reconciliation-v7", r.Account, r.SourceDigest, r.Coverage, r.Folders, frozen.Items})
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func TestReconciliationV7FrozenShape(t *testing.T) {
	for _, paired := range []bool{false, true} {
		_, a, p, v := senderAuthorityPreparation(t)
		if !paired {
			v.EnumeratedStates = nil
			v.SenderSourceIntegrity = nil
		}
		r := frozenV8Fixture(t, a, p, v)
		actual, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		frozen, err := json.Marshal(reconciliationV7Frozen(t, r))
		if err != nil {
			t.Fatal(err)
		}
		var projected reconciliationV7Receipt
		if err = json.Unmarshal(actual, &projected); err != nil {
			t.Fatal(err)
		}
		oldShape, err := json.Marshal(projected)
		if err != nil {
			t.Fatal(err)
		}
		if string(oldShape) != string(frozen) || string(actual) == string(frozen) || r.Version != 8 || r.Digest == reconciliationV7Digest(t, r) {
			t.Fatal("frozen v7 shape/domain or v8 separation changed")
		}
	}
}
