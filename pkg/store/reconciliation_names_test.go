package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

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
