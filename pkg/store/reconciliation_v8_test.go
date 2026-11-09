package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// Frozen actual v8 wire shapes: no embedding of evolving receipt/item types.
type reconciliationV8Item struct {
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
type reconciliationV8Receipt struct {
	Version                        int                              `json:"version"`
	Account                        mailbox.Account                  `json:"account"`
	SourceDigest                   string                           `json:"source_digest"`
	Digest                         string                           `json:"reconciliation_digest"`
	AuditDigest                    string                           `json:"audit_digest,omitempty"`
	Coverage                       string                           `json:"coverage"`
	Started                        time.Time                        `json:"started"`
	Finished                       time.Time                        `json:"finished"`
	Pages                          int                              `json:"pages"`
	Observed                       int                              `json:"observed"`
	HistoricalAttributionConfirmed bool                             `json:"historical_attribution_confirmed"`
	Backup                         string                           `json:"backup,omitempty"`
	BackupDigest                   string                           `json:"backup_digest,omitempty"`
	Folders                        map[string]mailbox.CollectionRef `json:"folders"`
	Items                          []reconciliationV8Item           `json:"items"`
}

func reconciliationV8Frozen(t *testing.T, r LegacyReconciliationReceipt) reconciliationV8Receipt {
	t.Helper()
	out := reconciliationV8Receipt{Version: r.Version, Account: r.Account, SourceDigest: r.SourceDigest, Digest: r.Digest, AuditDigest: r.AuditDigest, Coverage: r.Coverage, Started: r.Started, Finished: r.Finished, Pages: r.Pages, Observed: r.Observed, HistoricalAttributionConfirmed: r.HistoricalAttributionConfirmed, Backup: r.Backup, BackupDigest: r.BackupDigest, Folders: r.Folders}
	if r.Items != nil {
		out.Items = make([]reconciliationV8Item, len(r.Items))
	}
	for j, i := range r.Items {
		out.Items[j] = reconciliationV8Item{i.RecipientNameChanged, i.SenderAddressDisagrees, i.SenderNameChanged, i.SenderCasingChanged, i.RecipientCasingChanged, i.EnumeratedState, i.OriginalFolder, i.OriginalMemberships, i.MembershipChanged, i.N, i.OriginalID, i.Removed, i.Disposition, i.Reason, i.State}
	}
	return out
}

type reconciliationV8ApprovalItem struct {
	N                   int64                `json:"n"`
	OriginalID          string               `json:"original_id"`
	Removed             bool                 `json:"original_removed"`
	Disposition         string               `json:"disposition"`
	Reason              string               `json:"reason,omitempty"`
	OriginalFolder      string               `json:"original_folder,omitempty"`
	OriginalMemberships []mailbox.Membership `json:"original_memberships,omitempty"`
	MembershipChanged   bool                 `json:"membership_changed"`
	State               *mailbox.State       `json:"state,omitempty"`
}

func reconciliationV8Digests(t *testing.T, r LegacyReconciliationReceipt) (string, string) {
	t.Helper()
	frozen := reconciliationV8Frozen(t, r)
	items := make([]reconciliationV8ApprovalItem, len(frozen.Items))
	for j, i := range frozen.Items {
		items[j] = reconciliationV8ApprovalItem{N: i.N, OriginalID: i.OriginalID, Removed: i.Removed, Disposition: i.Disposition, Reason: i.Reason, OriginalFolder: i.OriginalFolder, OriginalMemberships: i.OriginalMemberships, MembershipChanged: i.MembershipChanged, State: i.State}
	}
	digest := func(v any) string {
		raw, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		h := sha256.Sum256(raw)
		return hex.EncodeToString(h[:])
	}
	approval := digest(struct {
		Domain           string
		Account          mailbox.Account
		Source, Coverage string
		Folders          map[string]mailbox.CollectionRef
		Items            []reconciliationV8ApprovalItem
	}{"mailpipe-legacy-reconciliation-approval-v8", frozen.Account, frozen.SourceDigest, frozen.Coverage, frozen.Folders, items})
	audit := digest(struct {
		Domain, ApprovalDigest string
		Account                mailbox.Account
		Source, Coverage       string
		Folders                map[string]mailbox.CollectionRef
		Started, Finished      time.Time
		Pages, Observed        int
		Items                  []reconciliationV8Item
	}{"mailpipe-legacy-reconciliation-audit-v8", approval, frozen.Account, frozen.SourceDigest, frozen.Coverage, frozen.Folders, frozen.Started, frozen.Finished, frozen.Pages, frozen.Observed, frozen.Items})
	return approval, audit
}
func TestReconciliationV8FrozenReceiptShape(t *testing.T) {
	for _, kind := range []string{"paired-disagreement", "paired-casing", "no-pair"} {
		t.Run(kind, func(t *testing.T) {
			_, a, p, v := senderAuthorityPreparation(t)
			n := p.Live[0].N
			if kind == "no-pair" {
				v.EnumeratedStates = nil
				v.SenderSourceIntegrity = nil
			} else {
				e, s := v.EnumeratedStates[n], v.Verification.States[n]
				e.Metadata.Recipients[0].Name = "Before Reader"
				s.Metadata.Recipients[0].Name = "After Reader"
				if kind == "paired-casing" {
					e.Metadata.Sender.Address = "SENDER@EXAMPLE.ORG"
					v.SenderSourceIntegrity = nil
				}
				v.EnumeratedStates[n], v.Verification.States[n] = e, s
			}
			r := frozenV8Fixture(t, a, p, v)
			raw, e := json.Marshal(r)
			if e != nil {
				t.Fatal(e)
			}
			frozen, e := json.Marshal(reconciliationV8Frozen(t, r))
			if e != nil {
				t.Fatal(e)
			}
			if string(raw) != string(frozen) || r.Version != 8 {
				t.Fatal("literal v8 shape differs from actual baseline")
			}
			approval, audit := reconciliationV8Digests(t, r)
			if approval != r.Digest || audit != r.AuditDigest {
				t.Fatal("literal v8 domain/order differs")
			}
			i := r.Items[0]
			if kind != "no-pair" && (!i.RecipientNameChanged || !i.RecipientCasingChanged || !i.SenderNameChanged || i.EnumeratedState == nil || i.SenderCasingChanged != (kind == "paired-casing") || i.SenderAddressDisagrees != (kind == "paired-disagreement")) {
				t.Fatal("paired true flags not proven")
			}
		})
	}
}

func TestReconciliationV8LiteralPreimages(t *testing.T) {
	// Encoding only: this deliberately minimal payload is not admitted evidence.
	r := LegacyReconciliationReceipt{Version: 8, Account: mailbox.Account{ID: "account", ProviderID: "graph"}, SourceDigest: "source", Coverage: "coverage", Folders: map[string]mailbox.CollectionRef{"b": {AccountID: "account", CollectionID: "b"}, "a": {AccountID: "account", CollectionID: "a"}}, Started: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Finished: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Pages: 1, Observed: 1, Items: []LegacyReconciliationItem{{N: 3, OriginalID: "opaque", OriginalFolder: "inbox", Disposition: "verified"}}}
	const approvalJSON = `{"Domain":"mailpipe-legacy-reconciliation-approval-v8","Account":{"ID":"account","ProviderID":"graph"},"Source":"source","Coverage":"coverage","Folders":{"a":{"AccountID":"account","CollectionID":"a"},"b":{"AccountID":"account","CollectionID":"b"}},"Items":[{"n":3,"original_id":"opaque","original_removed":false,"disposition":"verified","original_folder":"inbox","membership_changed":false}]}`
	sum := sha256.Sum256([]byte(approvalJSON))
	expectedApproval := hex.EncodeToString(sum[:])
	auditJSON := `{"Domain":"mailpipe-legacy-reconciliation-audit-v8","ApprovalDigest":"` + expectedApproval + `","Account":{"ID":"account","ProviderID":"graph"},"Source":"source","Coverage":"coverage","Folders":{"a":{"AccountID":"account","CollectionID":"a"},"b":{"AccountID":"account","CollectionID":"b"}},"Started":"2026-01-01T00:00:00Z","Finished":"2026-01-01T00:00:00Z","Pages":1,"Observed":1,"Items":[{"recipient_name_changed":false,"sender_address_disagrees":false,"sender_name_changed":false,"sender_casing_changed":false,"recipient_casing_changed":false,"original_folder":"inbox","membership_changed":false,"n":3,"original_id":"opaque","original_removed":false,"disposition":"verified"}]}`
	sum = sha256.Sum256([]byte(auditJSON))
	expectedAudit := hex.EncodeToString(sum[:])
	a, d := reconciliationV8Digests(t, r)
	var err error
	if err != nil || a != expectedApproval || d != expectedAudit {
		t.Fatal("typed preimage names/order/map sorting/domain changed", err)
	}
}

// A frozen committed-record fixture, not an alternative admission validator.
// Its literal wire shapes/domains were proven against actual v8 before evolution.
func frozenV8Fixture(t *testing.T, a mailbox.Account, p LegacyPreparation, v LegacyReconciliation) LegacyReconciliationReceipt {
	t.Helper()
	r := LegacyReconciliationReceipt{Version: 8, Account: a, SourceDigest: p.Digest(), Coverage: LegacyPrimaryMailbox, Started: v.Started, Finished: v.Finished, Pages: 1, Observed: len(v.Verification.States), Folders: v.Verification.Folders, HistoricalAttributionConfirmed: v.Verification.HistoricalAttributionConfirmed}
	for _, m := range p.Live {
		scope := v.Verification.Folders[m.Folder]
		i := LegacyReconciliationItem{N: m.N, OriginalID: m.GraphID, OriginalFolder: m.Folder, OriginalMemberships: []mailbox.Membership{{Collection: scope}}}
		if state, ok := v.Verification.States[m.N]; ok {
			state = reconciliationSnapshot(state)
			i.State = &state
			i.Disposition = "verified"
			i.MembershipChanged = state.Memberships[0].Collection != scope
			if before, ok := v.EnumeratedStates[m.N]; ok {
				before = reconciliationSnapshot(before)
				i.EnumeratedState = &before
				i.SenderNameChanged = before.Metadata.Sender.Name != state.Metadata.Sender.Name
				i.SenderAddressDisagrees = !strings.EqualFold(before.Metadata.Sender.Address, state.Metadata.Sender.Address)
				i.SenderCasingChanged = !i.SenderAddressDisagrees && before.Metadata.Sender.Address != state.Metadata.Sender.Address
				for n, left := range before.Metadata.Recipients {
					right := state.Metadata.Recipients[n]
					i.RecipientNameChanged = i.RecipientNameChanged || left.Name != right.Name
					i.RecipientCasingChanged = i.RecipientCasingChanged || left.Address != right.Address
				}
			}
		} else {
			i.Disposition = "unavailable"
			i.Reason = LegacyUnavailable
		}
		r.Items = append(r.Items, i)
	}
	for _, m := range p.Removed {
		r.Items = append(r.Items, LegacyReconciliationItem{N: m.N, OriginalID: m.GraphID, Removed: true, Disposition: "historical"})
	}
	sort.Slice(r.Items, func(i, j int) bool { return r.Items[i].N < r.Items[j].N })
	r.Digest, r.AuditDigest = reconciliationV8Digests(t, r)
	return r
}
