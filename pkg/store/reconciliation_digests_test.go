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
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func TestReconciliationDigestPreimages(t *testing.T) {
	// Encoding only: this deliberately minimal payload is not admitted evidence.
	r := LegacyReconciliationReceipt{Version: 8, Account: mailbox.Account{ID: "account", ProviderID: "graph"}, SourceDigest: "source", Coverage: "coverage", Folders: map[string]mailbox.CollectionRef{"b": {AccountID: "account", CollectionID: "b"}, "a": {AccountID: "account", CollectionID: "a"}}, Started: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Finished: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Pages: 1, Observed: 1, Items: []LegacyReconciliationItem{{N: 3, OriginalID: "opaque", OriginalFolder: "inbox", Disposition: "verified"}}}
	const approvalJSON = `{"Domain":"mailpipe-legacy-reconciliation-approval-v8","Account":{"ID":"account","ProviderID":"graph"},"Source":"source","Coverage":"coverage","Folders":{"a":{"AccountID":"account","CollectionID":"a"},"b":{"AccountID":"account","CollectionID":"b"}},"Items":[{"n":3,"original_id":"opaque","original_removed":false,"disposition":"verified","original_folder":"inbox","membership_changed":false}]}`
	sum := sha256.Sum256([]byte(approvalJSON))
	expectedApproval := hex.EncodeToString(sum[:])
	auditJSON := `{"Domain":"mailpipe-legacy-reconciliation-audit-v8","ApprovalDigest":"` + expectedApproval + `","Account":{"ID":"account","ProviderID":"graph"},"Source":"source","Coverage":"coverage","Folders":{"a":{"AccountID":"account","CollectionID":"a"},"b":{"AccountID":"account","CollectionID":"b"}},"Started":"2026-01-01T00:00:00Z","Finished":"2026-01-01T00:00:00Z","Pages":1,"Observed":1,"Items":[{"recipient_name_changed":false,"sender_address_disagrees":false,"sender_name_changed":false,"sender_casing_changed":false,"recipient_casing_changed":false,"original_folder":"inbox","membership_changed":false,"n":3,"original_id":"opaque","original_removed":false,"disposition":"verified"}]}`
	sum = sha256.Sum256([]byte(auditJSON))
	expectedAudit := hex.EncodeToString(sum[:])
	a, d, err := reconciliationReceiptDigests(r)
	if err != nil || a != expectedApproval || d != expectedAudit {
		t.Fatal("typed preimage names/order/map sorting/domain changed", err)
	}
	// Every item field must be deliberately classified. A future unclassified
	// field must trigger review rather than silently enter or leave approval.
	included := map[string]bool{"N": true, "OriginalID": true, "Removed": true, "Disposition": true, "Reason": true, "OriginalFolder": true, "OriginalMemberships": true, "MembershipChanged": true, "State": true}
	excluded := map[string]bool{"EnumeratedState": true, "RecipientNameChanged": true, "SenderAddressDisagrees": true, "SenderNameChanged": true, "SenderCasingChanged": true, "RecipientCasingChanged": true}
	typ := reflect.TypeFor[LegacyReconciliationItem]()
	if typ.NumField() != len(included)+len(excluded) {
		t.Fatal("unclassified item field")
	}
	projection := reflect.TypeFor[reconciliationApprovalItem]()
	if projection.NumField() != len(included) {
		t.Fatal("approval projection changed")
	}
	for i := 0; i < typ.NumField(); i++ {
		n := typ.Field(i).Name
		if !included[n] && !excluded[n] {
			t.Fatal("unclassified item field", n)
		}
	}
	for i := 0; i < projection.NumField(); i++ {
		if !included[projection.Field(i).Name] {
			t.Fatal("audit field entered approval")
		}
	}
}

func TestReconciliationStableApprovalRawAudit(t *testing.T) {
	_, a, p, v := casingPreparation(t)
	n := p.Live[0].N
	v.EnumeratedStates = nil
	base, err := ReviewLegacyReconciliation(a, p, v)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"sender-name", "sender-case", "sender-disagrees", "recipient-name", "recipient-case", "combined-case", "combined-disagreement", "times", "pages", "observed", "nil-empty-map"} {
		t.Run(kind, func(t *testing.T) {
			x := ownReconciliation(v)
			s := x.Verification.States[n]
			e := reconciliationSnapshot(s)
			flags := map[string]bool{}
			switch kind {
			case "sender-name":
				e.Metadata.Sender.Name = "Other display"
				flags["sender_name_changed"] = true
			case "sender-case":
				e.Metadata.Sender.Address = strings.ToUpper(s.Metadata.Sender.Address)
				flags["sender_casing_changed"] = true
			case "sender-disagrees":
				e.Metadata.Sender.Address = "enumerated@example.org"
				x.SenderSourceIntegrity[n] = true
				flags["sender_address_disagrees"] = true
			case "recipient-name":
				e.Metadata.Recipients[0].Name = "Other reader"
				flags["recipient_name_changed"] = true
			case "recipient-case":
				e.Metadata.Recipients[0].Address = "READER@Example.org"
				flags["recipient_casing_changed"] = true
			case "combined-case", "combined-disagreement":
				e.Metadata.Sender.Name = "Other display"
				e.Metadata.Recipients[0] = mailbox.Address{Address: "READER@Example.org", Name: "Other reader"}
				flags["sender_name_changed"], flags["recipient_name_changed"], flags["recipient_casing_changed"] = true, true, true
				if kind == "combined-case" {
					e.Metadata.Sender.Address = strings.ToUpper(s.Metadata.Sender.Address)
					flags["sender_casing_changed"] = true
				} else {
					e.Metadata.Sender.Address = "enumerated@example.org"
					x.SenderSourceIntegrity[n] = true
					flags["sender_address_disagrees"] = true
				}
			case "times":
				x.Started = x.Started.Add(-time.Second)
				x.Finished = x.Finished.Add(time.Second)
			case "pages":
				x.Pages++
			case "observed":
				x.Observed++
			}
			if len(flags) > 0 {
				x.EnumeratedStates[n] = e
			}
			r, err := ReviewLegacyReconciliation(a, p, x)
			if err != nil || r.Digest != base.Digest || ((r.AuditDigest == base.AuditDigest) != (kind == "nil-empty-map")) {
				t.Fatal("approval/audit separation failed", err)
			}
			raw, _ := json.Marshal(r.Items[0])
			var fields map[string]any
			if err = json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"sender_name_changed", "sender_casing_changed", "sender_address_disagrees", "recipient_name_changed", "recipient_casing_changed"} {
				if fields[key] != flags[key] {
					t.Fatal("flag not independently derived", key)
				}
			}
			if len(flags) > 0 && !reflect.DeepEqual(r.Items[0].EnumeratedState.Metadata, e.Metadata) {
				t.Fatal("raw enumeration lost")
			}
			if !reflect.DeepEqual(r.Items[0].State.Metadata, s.Metadata) {
				t.Fatal("inspection changed")
			}
		})
	}
}

func TestReconciliationApprovalSensitivity(t *testing.T) {
	_, a, p, v := recipientNamePreparation(t, true)
	n := p.Live[0].N
	base, err := ReviewLegacyReconciliation(a, p, v)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"sender-name", "sender-address", "recipient-name", "recipient-address", "nil-shape", "order", "received", "subject", "preview", "read", "attachments", "revision", "membership", "ref", "locator", "account", "provider", "history", "original-id", "disposition", "folder-map", "coverage"} {
		t.Run(kind, func(t *testing.T) {
			x := ownReconciliation(v)
			prep := p
			prep.Live = append([]Message(nil), p.Live...)
			account := a
			s := x.Verification.States[n]
			switch kind {
			case "sender-name":
				s.Metadata.Sender.Name = "Changed inspection"
			case "sender-address":
				s.Metadata.Sender.Address = strings.ToUpper(s.Metadata.Sender.Address)
			case "recipient-name":
				s.Metadata.Recipients[0].Name = "Changed inspection"
			case "recipient-address":
				s.Metadata.Recipients[0].Address = "READER@EXAMPLE.ORG"
			case "nil-shape":
				s.Metadata.Recipients = nil
			case "order":
				s.Metadata.Recipients[0], s.Metadata.Recipients[2] = s.Metadata.Recipients[2], s.Metadata.Recipients[0]
			case "received":
				s.Metadata.Received = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			case "subject":
				s.Metadata.Subject = "Changed inspection"
			case "preview":
				s.Metadata.Preview = "Changed inspection"
			case "read":
				s.Metadata.IsRead = !s.Metadata.IsRead
			case "attachments":
				s.Metadata.HasAttachments = !s.Metadata.HasAttachments
			case "revision":
				s.Revision = "Changed"
			case "membership":
				s.Memberships[0].Collection.CollectionID = "other"
			case "ref":
				s.Ref.MessageID = "other"
			case "locator":
				s.Ref.Locator = "other"
			case "account":
				account.ID = "other"
			case "provider":
				account.ProviderID = "other"
			case "history":
				prep.digest = "other-source"
			case "original-id":
				prep.Live[0].GraphID = "other"
			case "disposition":
				delete(x.Verification.States, n)
				delete(x.EnumeratedStates, n)
				x.Unavailable = map[int64]string{n: LegacyUnavailable}
			case "folder-map":
				x.Verification.Folders[prep.Live[0].Folder] = mailbox.CollectionRef{AccountID: a.ID, CollectionID: "other"}
			case "coverage":
				x.Coverage = "other"
			}
			if kind != "disposition" {
				x.Verification.States[n] = s
			}
			// For metadata-only sensitivity permit enumeration to follow the same new
			// inspection, so rejection cannot mask an omitted inspected approval field.
			if kind == "sender-name" || kind == "sender-address" || kind == "recipient-name" || kind == "recipient-address" || kind == "received" || kind == "subject" || kind == "preview" || kind == "read" || kind == "attachments" || kind == "revision" || kind == "membership" || kind == "nil-shape" || kind == "order" {
				delete(x.EnumeratedStates, n)
			}
			r, err := ReviewLegacyReconciliation(account, prep, x)
			if err == nil && r.Digest == base.Digest {
				t.Fatal("changed approval input retained R", kind)
			}
			if err != nil && !reflect.DeepEqual(r, LegacyReconciliationReceipt{}) {
				t.Fatal("failure returned partial identity")
			}
		})
	}
}

func TestReconciliationExcludedInvalidEvidenceHasNoDigests(t *testing.T) {
	for _, kind := range []string{"subject", "invalid-name", "wrong-ref", "wrong-account", "wrong-membership", "wrong-revision", "recipient-order", "redundant-pair", "unknown-pair", "false-integrity"} {
		t.Run(kind, func(t *testing.T) {
			_, a, p, v := recipientNamePreparation(t, true)
			n := p.Live[0].N
			e := v.EnumeratedStates[n]
			switch kind {
			case "subject":
				e.Metadata.Subject = "Excluded invalid subject"
			case "invalid-name":
				e.Metadata.Recipients[0].Name = "\xff"
			case "wrong-ref":
				e.Ref.MessageID = "other"
			case "wrong-account":
				e.Ref.AccountID = "other"
			case "wrong-membership":
				e.Memberships[0].Collection.CollectionID = "other"
			case "wrong-revision":
				e.Revision = "old"
				s := v.Verification.States[n]
				s.Revision = "new"
				v.Verification.States[n] = s
			case "recipient-order":
				e.Metadata.Recipients[0], e.Metadata.Recipients[2] = e.Metadata.Recipients[2], e.Metadata.Recipients[0]
			case "redundant-pair":
				e = reconciliationSnapshot(v.Verification.States[n])
			case "unknown-pair":
				v.EnumeratedStates[n+100] = e
			case "false-integrity":
				e.Metadata.Sender.Address = "enumerated@example.org"
				v.SenderSourceIntegrity = map[int64]bool{n: false}
			}
			v.EnumeratedStates[n] = e
			if r, err := ReviewLegacyReconciliation(a, p, v); err == nil || !reflect.DeepEqual(r, LegacyReconciliationReceipt{}) {
				t.Fatal("excluded invalid evidence acquired authority")
			}
		})
	}
}

func TestReconciliationCurrentExecutionSnapshotIntegrity(t *testing.T) {
	for _, kind := range []string{"valid", "owned-enumeration", "owned-inspection", "owned-pair-absent", "receipt-raw", "receipt-flag-name", "receipt-flag-case", "receipt-flag-sender-name", "receipt-flag-sender-case", "receipt-flag-sender-disagrees", "receipt-audit", "receipt-approval", "receipt-version", "forged-audit"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			path, a, p, v := recipientNamePreparation(t, true)
			n := p.Live[0].N
			r, err := ReviewLegacyReconciliation(a, p, v)
			if err != nil {
				t.Fatal(err)
			}
			owned := ownReconciliation(v)
			switch kind {
			case "owned-enumeration":
				owned.EnumeratedStates[n].Metadata.Recipients[0].Name = "Other permitted enumeration"
			case "owned-inspection":
				owned.Verification.States[n].Metadata.Recipients[0].Name = "Other inspection"
			case "owned-pair-absent":
				owned.EnumeratedStates = nil
			case "receipt-raw":
				r.Items[0].EnumeratedState.Metadata.Recipients[0].Name = "Tampered raw receipt"
			case "receipt-flag-name":
				r.Items[0].RecipientNameChanged = !r.Items[0].RecipientNameChanged
			case "receipt-flag-case":
				r.Items[0].RecipientCasingChanged = !r.Items[0].RecipientCasingChanged
			case "receipt-flag-sender-name":
				r.Items[0].SenderNameChanged = !r.Items[0].SenderNameChanged
			case "receipt-flag-sender-case":
				r.Items[0].SenderCasingChanged = !r.Items[0].SenderCasingChanged
			case "receipt-flag-sender-disagrees":
				r.Items[0].SenderAddressDisagrees = !r.Items[0].SenderAddressDisagrees
			case "receipt-audit":
				r.AuditDigest = "forged"
			case "receipt-approval":
				r.Digest = "forged"
			case "receipt-version":
				r.Version = 7
			case "forged-audit":
				r.Items[0].RecipientNameChanged = false
				r.Digest, r.AuditDigest, err = reconciliationReceiptDigests(r)
				if err != nil {
					t.Fatal(err)
				}
			}
			// Owned enumeration mutation is valid and approval-stable, but is stale
			// within THIS execution. The raw snapshot audit must catch it.
			if kind == "owned-enumeration" || kind == "owned-pair-absent" {
				changed, e := ReviewLegacyReconciliation(a, p, owned)
				if e != nil || changed.Digest != r.Digest || changed.AuditDigest == r.AuditDigest {
					t.Fatal("fixture is not a valid audit-only change", e)
				}
			}
			life, db, err := maintenanceDB(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer closeMaintenance(life, db)
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			before, err := logicalDigest(ctx, conn)
			if err != nil {
				t.Fatal(err)
			}
			bound := &AccountStore{account: a}
			err = immediate(ctx, conn, func(q sqlAccess) error {
				return bound.bindLegacyTransaction(ctx, q, p, v.Verification, v.Unavailable, &legacyReconciliationRequest{receipt: r, confirmation: r.Digest, evidence: owned}, "invented-backup", p.Digest())
			})
			if kind != "valid" {
				if !errors.Is(err, mailbox.ErrStaleIdentity) {
					t.Fatal("tampered snapshot accepted", err)
				}
				after, e := logicalDigest(ctx, conn)
				if e != nil || before != after {
					t.Fatal("failed final review changed history", e)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReconciliationAuditEncodingFailureReturnsNoApproval(t *testing.T) {
	_, a, p, v := casingPreparation(t)
	v.Started = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	v.Finished = v.Started
	if r, err := ReviewLegacyReconciliation(a, p, v); err == nil || !reflect.DeepEqual(r, LegacyReconciliationReceipt{}) {
		t.Fatal("audit encoding failure returned partial approval")
	}
}

func TestReconciliationApprovalEncodingFailureReturnsNoReceipt(t *testing.T) {
	_, a, p, v := casingPreparation(t)
	// No enumeration pair can mask the intended first-hash codec failure.
	v.EnumeratedStates = nil
	if _, err := ReviewLegacyReconciliation(a, p, v); err != nil {
		t.Fatal("control evidence is not admitted", err)
	}
	n := p.Live[0].N
	state := v.Verification.States[n]
	state.Metadata.Received = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	v.Verification.States[n] = state
	original := p.Live[0]
	if _, _, err := reconciliationState(a.ID, original, v.Verification.Folders[original.Folder], state); err != nil {
		t.Fatal("invalid encoding date failed earlier state validation", err)
	}
	_, codecErr := json.Marshal(state.Metadata.Received)
	var expected *json.MarshalerError
	if !errors.As(codecErr, &expected) || expected.Type != reflect.TypeOf((*time.Time)(nil)) {
		t.Fatal("fixture does not reach the time codec", codecErr)
	}
	r, err := ReviewLegacyReconciliation(a, p, v)
	var actual *json.MarshalerError
	if !errors.As(err, &actual) || actual.Type != expected.Type || actual.Err.Error() != expected.Err.Error() {
		t.Fatal("review failed before the approval time codec", err)
	}
	if !reflect.DeepEqual(r, LegacyReconciliationReceipt{}) {
		t.Fatal("approval encoding failure returned a partial receipt")
	}
}

func TestReconciliationApprovalProjectionFieldSensitivity(t *testing.T) {
	_, a, p, v := casingPreparation(t)
	base, err := ReviewLegacyReconciliation(a, p, v)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"n", "id", "removed", "disposition", "reason", "folder", "old-membership", "membership-flag", "state", "source", "account", "provider", "folders", "coverage"} {
		t.Run(kind, func(t *testing.T) {
			// Pure preimage sensitivity, not an admission path. The real Review tests
			// independently reject forged combinations and derive the authoritative item.
			raw, err := json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			var r LegacyReconciliationReceipt
			if err = json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			i := &r.Items[0]
			switch kind {
			case "n":
				i.N++
			case "id":
				i.OriginalID = "other"
			case "removed":
				i.Removed = !i.Removed
			case "disposition":
				i.Disposition = "other"
			case "reason":
				i.Reason = "other"
			case "folder":
				i.OriginalFolder = "other"
			case "old-membership":
				i.OriginalMemberships[0].Collection.CollectionID = "other"
			case "membership-flag":
				i.MembershipChanged = !i.MembershipChanged
			case "state":
				i.State = nil
			case "source":
				r.SourceDigest = "other"
			case "account":
				r.Account.ID = "other"
			case "provider":
				r.Account.ProviderID = "other"
			case "folders":
				r.Folders[i.OriginalFolder] = mailbox.CollectionRef{AccountID: a.ID, CollectionID: "other"}
			case "coverage":
				r.Coverage = "other"
			}
			approval, audit, err := reconciliationReceiptDigests(r)
			if err != nil || approval == base.Digest || audit == base.AuditDigest {
				t.Fatal("stable input not bound to both identities", err)
			}
		})
	}
}
