package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

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
