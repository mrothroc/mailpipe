package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// Approval is an explicit allowlist: enumeration, pair presence and all five
// presentation flags belong only to audit. Complete inspected State remains
// approval-bound, including future inspected metadata fields.
type reconciliationApprovalItem struct {
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

// Declaration order and explicit names define these versioned preimages.
// encoding/json sorts string map keys; no alternate canonicalizer is used.
type reconciliationApprovalPayload struct {
	Domain   string                           `json:"Domain"`
	Account  mailbox.Account                  `json:"Account"`
	Source   string                           `json:"Source"`
	Coverage string                           `json:"Coverage"`
	Folders  map[string]mailbox.CollectionRef `json:"Folders"`
	Items    []reconciliationApprovalItem     `json:"Items"`
}

type reconciliationAuditPayload struct {
	Domain         string                           `json:"Domain"`
	ApprovalDigest string                           `json:"ApprovalDigest"`
	Account        mailbox.Account                  `json:"Account"`
	Source         string                           `json:"Source"`
	Coverage       string                           `json:"Coverage"`
	Folders        map[string]mailbox.CollectionRef `json:"Folders"`
	Started        time.Time                        `json:"Started"`
	Finished       time.Time                        `json:"Finished"`
	Pages          int                              `json:"Pages"`
	Observed       int                              `json:"Observed"`
	Items          []LegacyReconciliationItem       `json:"Items"`
}

// Called only after independent evidence validation, or to check the integrity
// of that execution's frozen receipt. It supplies no evidence or admission.
func reconciliationReceiptDigests(r LegacyReconciliationReceipt) (string, string, error) {
	if r.Version != 8 {
		return "", "", stale("reconciliation receipt version changed before binding")
	}
	items := make([]reconciliationApprovalItem, len(r.Items))
	for j, i := range r.Items {
		items[j] = reconciliationApprovalItem{N: i.N, OriginalID: i.OriginalID, Removed: i.Removed, Disposition: i.Disposition, Reason: i.Reason, OriginalFolder: i.OriginalFolder, OriginalMemberships: i.OriginalMemberships, MembershipChanged: i.MembershipChanged, State: i.State}
	}
	approval, err := reconciliationPayloadDigest(reconciliationApprovalPayload{Domain: "mailpipe-legacy-reconciliation-approval-v8", Account: r.Account, Source: r.SourceDigest, Coverage: r.Coverage, Folders: r.Folders, Items: items})
	if err != nil {
		return "", "", err
	}
	audit, err := reconciliationPayloadDigest(reconciliationAuditPayload{Domain: "mailpipe-legacy-reconciliation-audit-v8", ApprovalDigest: approval, Account: r.Account, Source: r.SourceDigest, Coverage: r.Coverage, Folders: r.Folders, Started: r.Started, Finished: r.Finished, Pages: r.Pages, Observed: r.Observed, Items: r.Items})
	if err != nil {
		return "", "", err
	}
	return approval, audit, nil
}

func reconciliationPayloadDigest(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), nil
}

func checkReconciliationReceiptDigests(r LegacyReconciliationReceipt) error {
	approval, audit, err := reconciliationReceiptDigests(r)
	if err != nil {
		return err
	}
	if approval != r.Digest || audit != r.AuditDigest {
		return stale("reconciliation snapshot changed before binding")
	}
	return nil
}
