package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// This allowlist binds complete inspected state, not acquisition measurements.
type reconciliationApprovalItem struct {
	N                   int64                `json:"n"`
	OriginalID          string               `json:"original_id"`
	Removed             bool                 `json:"original_removed"`
	Disposition         string               `json:"disposition"`
	Reason              string               `json:"reason,omitempty"`
	OriginalFolder      string               `json:"original_folder,omitempty"`
	OriginalMemberships []mailbox.Membership `json:"original_memberships,omitempty"`
	MembershipChanged   bool                 `json:"membership_changed"`
	ReadRef             *mailbox.MessageRef  `json:"read_ref,omitempty"`
	ReadOutcome         string               `json:"read_outcome,omitempty"`
	State               *mailbox.State       `json:"state,omitempty"`
}
type reconciliationApprovalPayload struct {
	Domain   string                           `json:"Domain"`
	Method   string                           `json:"Method"`
	Account  mailbox.Account                  `json:"Account"`
	Source   string                           `json:"Source"`
	Coverage string                           `json:"Coverage"`
	Folders  map[string]mailbox.CollectionRef `json:"Folders"`
	Items    []reconciliationApprovalItem     `json:"Items"`
}
type reconciliationAuditPayload struct {
	Domain           string                           `json:"Domain"`
	ApprovalDigest   string                           `json:"ApprovalDigest"`
	Method           string                           `json:"Method"`
	Account          mailbox.Account                  `json:"Account"`
	Source           string                           `json:"Source"`
	Coverage         string                           `json:"Coverage"`
	Folders          map[string]mailbox.CollectionRef `json:"Folders"`
	Started          time.Time                        `json:"Started"`
	Finished         time.Time                        `json:"Finished"`
	ReferenceContext *LegacyReferenceContext          `json:"ReferenceContext"`
	Items            []LegacyReconciliationItem       `json:"Items"`
}

func reconciliationReceiptShape(r LegacyReconciliationReceipt) error {
	bad := func() error { return stale("reconciliation receipt shape changed before binding") }
	if r.Version != 9 || r.Method != LegacyReferenceMethod || r.Coverage != LegacyReferenceCoverage || r.ReferenceContext == nil || r.Pages != 0 || r.Observed != 0 {
		return bad()
	}
	for _, i := range r.Items {
		if i.EnumeratedState != nil || i.RecipientNameChanged || i.RecipientCasingChanged || i.SenderCasingChanged || i.SenderNameChanged || i.SenderAddressDisagrees {
			return bad()
		}
		switch i.Disposition {
		case "verified":
			if i.Removed || i.ReadRef == nil || !referenceRefValid(r.Account.ID, *i.ReadRef) || i.State == nil || i.State.Ref != *i.ReadRef || string(i.ReadRef.MessageID) != i.OriginalID || i.ReadOutcome != LegacyReferenceFound || !i.SourceValidated {
				return bad()
			}
		case "unavailable":
			if i.Removed || i.ReadRef == nil || !referenceRefValid(r.Account.ID, *i.ReadRef) || string(i.ReadRef.MessageID) != i.OriginalID || i.State != nil || i.SourceValidated || i.ReadOutcome != LegacyReferenceNotFound || i.Reason != LegacyReferenceUnavailable {
				return bad()
			}
		case "historical":
			if !i.Removed || i.ReadRef != nil || i.ReadOutcome != "" || i.SourceValidated || i.State != nil {
				return bad()
			}
		default:
			return bad()
		}
	}
	return nil
}
func reconciliationReceiptDigests(r LegacyReconciliationReceipt) (string, string, error) {
	if e := reconciliationReceiptShape(r); e != nil {
		return "", "", e
	}
	items := make([]reconciliationApprovalItem, len(r.Items))
	for j, i := range r.Items {
		items[j] = reconciliationApprovalItem{N: i.N, OriginalID: i.OriginalID, Removed: i.Removed, Disposition: i.Disposition, Reason: i.Reason, OriginalFolder: i.OriginalFolder, OriginalMemberships: i.OriginalMemberships, MembershipChanged: i.MembershipChanged, ReadRef: i.ReadRef, ReadOutcome: i.ReadOutcome, State: i.State}
	}
	approval, e := reconciliationPayloadDigest(reconciliationApprovalPayload{"mailpipe-legacy-reconciliation-approval-v9", r.Method, r.Account, r.SourceDigest, r.Coverage, r.Folders, items})
	if e != nil {
		return "", "", e
	}
	audit, e := reconciliationPayloadDigest(reconciliationAuditPayload{"mailpipe-legacy-reconciliation-audit-v9", approval, r.Method, r.Account, r.SourceDigest, r.Coverage, r.Folders, r.Started, r.Finished, r.ReferenceContext, r.Items})
	if e != nil {
		return "", "", e
	}
	return approval, audit, nil
}
func reconciliationPayloadDigest(v any) (string, error) {
	raw, e := json.Marshal(v)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), nil
}
func checkReconciliationReceiptDigests(r LegacyReconciliationReceipt) error {
	approval, audit, e := reconciliationReceiptDigests(r)
	if e != nil {
		return e
	}
	if approval != r.Digest || audit != r.AuditDigest {
		return stale("reconciliation snapshot changed before binding")
	}
	return nil
}
