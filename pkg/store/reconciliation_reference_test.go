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

// Synthetic supported-reader evidence, not an alternate validator. Call before
// mutations so negatives exercise the actual independent store boundary.
func referenceFixture(a mailbox.Account, p LegacyPreparation, v LegacyReconciliation) LegacyReconciliation {
	v.Method, v.Coverage = LegacyReferenceMethod, LegacyReferenceCoverage
	v.Pages, v.Observed = 0, 0
	v.EnumeratedStates = nil
	v.SenderSourceIntegrity = nil
	v.ReferenceOutcomes = map[int64]LegacyReferenceOutcome{}
	for _, m := range p.Live {
		ref := mailbox.MessageRef{AccountID: a.ID, MessageID: mailbox.MessageID(m.GraphID), Locator: mailbox.Locator(m.GraphID)}
		if _, ok := v.Verification.States[m.N]; ok {
			v.ReferenceOutcomes[m.N] = LegacyReferenceOutcome{Ref: ref, Kind: LegacyReferenceFound, SourceValidated: true}
		} else if reason, ok := v.Unavailable[m.N]; ok {
			if reason == LegacyUnavailable {
				v.Unavailable[m.N] = LegacyReferenceUnavailable
			}
			v.ReferenceOutcomes[m.N] = LegacyReferenceOutcome{Ref: ref, Kind: LegacyReferenceNotFound}
		}
	}
	v.ReferenceContext = LegacyReferenceContext{RequestedReferences: len(p.Live), CompletedReferences: len(v.ReferenceOutcomes), InspectionBatches: (len(p.Live) + 19) / 20}
	for _, s := range v.Verification.States {
		raw, e := json.Marshal(s)
		if e != nil {
			panic(e)
		}
		v.ReferenceContext.StateBytes += int64(len(raw))
	}
	return v
}
func referencePreparation(t *testing.T) (string, mailbox.Account, LegacyPreparation, LegacyReconciliation) {
	t.Helper()
	path, a, p, v := casingPreparation(t)
	return path, a, p, referenceFixture(a, p, v)
}
func TestReferenceStoreIndependentContract(t *testing.T) {
	for _, kind := range []string{"valid", "method", "coverage", "nonce", "account", "missing-outcome", "unknown-outcome", "foreign-n", "ref", "locator", "invalid-ref", "stamp", "missing-state", "extra-state", "overlap", "legacy-enum", "legacy-stamp", "pages", "observed", "requested", "completed", "batches", "bytes", "window", "sender", "header", "membership", "invalid-revision", "invalid-membership", "invalid-metadata"} {
		t.Run(kind, func(t *testing.T) {
			_, a, p, v := referencePreparation(t)
			n := p.Live[0].N
			s := v.Verification.States[n]
			o := v.ReferenceOutcomes[n]
			switch kind {
			case "method":
				v.Method = "unknown"
			case "coverage":
				v.Coverage = LegacyPrimaryMailbox
			case "nonce":
				v.Verification.Nonce = "other"
			case "account":
				v.Verification.Account.ID = "other"
			case "missing-outcome":
				delete(v.ReferenceOutcomes, n)
			case "unknown-outcome":
				o.Kind = "unknown"
			case "foreign-n":
				v.ReferenceOutcomes[999] = o
			case "ref":
				o.Ref.MessageID = "other"
			case "locator":
				o.Ref.Locator = "other"
			case "invalid-ref":
				o.Ref.MessageID = "\xff"
				o.Ref.Locator = "\xff"
			case "stamp":
				o.SourceValidated = false
			case "missing-state":
				delete(v.Verification.States, n)
			case "extra-state":
				v.Verification.States[999] = s
			case "overlap":
				v.Unavailable = map[int64]string{n: LegacyReferenceUnavailable}
			case "legacy-enum":
				v.EnumeratedStates = map[int64]mailbox.State{n: s}
			case "legacy-stamp":
				v.SenderSourceIntegrity = map[int64]bool{n: true}
			case "pages":
				v.Pages = 1
			case "observed":
				v.Observed = 1
			case "requested":
				v.ReferenceContext.RequestedReferences++
			case "completed":
				v.ReferenceContext.CompletedReferences++
			case "batches":
				v.ReferenceContext.InspectionBatches = 0
			case "bytes":
				v.ReferenceContext.StateBytes++
			case "window":
				v.Finished = v.Started.Add(LegacyReferenceReadTimeout + time.Second)
			case "sender":
				s.Metadata.Sender.Address = "other@example.org"
			case "header":
				s.Metadata.InternetMessageID = "<other@example.org>"
			case "membership":
				s.Memberships = nil
			case "invalid-revision":
				s.Revision = "\xff"
			case "invalid-membership":
				s.Memberships[0].Collection.CollectionID = "\xff"
			case "invalid-metadata":
				s.Metadata.Subject = "\xff"
			}
			if kind != "missing-outcome" {
				v.ReferenceOutcomes[n] = o
			}
			if kind != "missing-state" {
				v.Verification.States[n] = s
			}
			// Encoding cases must reach their UTF-8 guard with valid accounting.
			// Keep the separate "bytes" case deliberately inconsistent.
			if kind == "invalid-revision" || kind == "invalid-membership" || kind == "invalid-metadata" {
				v.ReferenceContext.StateBytes = 0
				for _, state := range v.Verification.States {
					raw, err := json.Marshal(state)
					if err != nil {
						t.Fatal(err)
					}
					v.ReferenceContext.StateBytes += int64(len(raw))
				}
				// Match the encoder's replacement length with valid ASCII so
				// this control admits with identical byte accounting.
				invalidJSON, err := json.Marshal("\xff")
				if err != nil {
					t.Fatal(err)
				}
				validValue := strings.Repeat("x", len(invalidJSON)-2)
				control := ownReconciliation(v)
				current := control.Verification.States[n]
				switch kind {
				case "invalid-revision":
					current.Revision = validValue
				case "invalid-membership":
					current.Memberships[0].Collection.CollectionID = mailbox.CollectionID(validValue)
				case "invalid-metadata":
					current.Metadata.Subject = validValue
				}
				control.Verification.States[n] = current
				var controlBytes int64
				for _, state := range control.Verification.States {
					raw, err := json.Marshal(state)
					if err != nil {
						t.Fatal(err)
					}
					controlBytes += int64(len(raw))
				}
				if controlBytes != v.ReferenceContext.StateBytes {
					t.Fatal("encoding control must preserve exact byte accounting")
				}
				if _, err := ReviewLegacyReconciliation(a, p, control); err != nil {
					t.Fatal("otherwise-valid encoding control rejected", err)
				}
			}
			r, e := ReviewLegacyReconciliation(a, p, v)
			if kind == "valid" {
				if e != nil || r.Version != 9 || r.Method != LegacyReferenceMethod || r.Items[0].ReadRef == nil || !r.Items[0].SourceValidated || r.Items[0].State == nil || r.Items[0].EnumeratedState != nil {
					t.Fatal("valid new contract rejected", e)
				}
			} else if e == nil || !reflect.DeepEqual(r, LegacyReconciliationReceipt{}) {
				t.Fatal("invalid evidence returned authority", e)
			}
		})
	}
}
func TestReferenceStoreUnavailableAndHistorical(t *testing.T) {
	ctx := context.Background()
	path, a, _, _ := referencePreparation(t)
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Upsert(ctx, "inbox", Record{GraphID: "anchor", InternetMessageID: "<anchor@example.org>", Sender: "anchor@example.org"}); e != nil {
		t.Fatal(e)
	}
	s.Close()
	p, e := PrepareLegacyBinding(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	proof := goodLegacyProof(a, p)
	missing := p.Live[0].N
	delete(proof.States, missing)
	v := referenceFixture(a, p, LegacyReconciliation{Verification: proof, Unavailable: map[int64]string{missing: LegacyReferenceUnavailable}, Started: time.Now().UTC(), Finished: time.Now().UTC()})
	for _, kind := range []string{"valid", "bad-ref", "invalid-encoding", "stamp", "state", "reason", "no-anchor"} {
		t.Run(kind, func(t *testing.T) {
			x := ownReconciliation(v)
			o := x.ReferenceOutcomes[missing]
			switch kind {
			case "bad-ref":
				o.Ref.MessageID = "other"
			case "invalid-encoding":
				o.Ref.MessageID = "\xff"
				o.Ref.Locator = "\xff"
			case "stamp":
				o.SourceValidated = true
			case "state":
				x.Verification.States[missing] = goodLegacyProof(a, p).States[missing]
			case "reason":
				x.Unavailable[missing] = LegacyUnavailable
			case "no-anchor":
				for _, m := range p.Live {
					delete(x.Verification.States, m.N)
					x.Unavailable[m.N] = LegacyReferenceUnavailable
					x.ReferenceOutcomes[m.N] = LegacyReferenceOutcome{Ref: mailbox.MessageRef{AccountID: a.ID, MessageID: mailbox.MessageID(m.GraphID), Locator: mailbox.Locator(m.GraphID)}, Kind: LegacyReferenceNotFound}
				}
				x.ReferenceContext.StateBytes = 0
			}
			if kind != "no-anchor" {
				x.ReferenceOutcomes[missing] = o
			}
			r, err := ReviewLegacyReconciliation(a, p, x)
			if kind == "valid" {
				if err != nil || r.Items[0].State != nil || r.Items[0].Reason != LegacyReferenceUnavailable {
					t.Fatal("scoped unavailable rejected", err)
				}
			} else if err == nil || !reflect.DeepEqual(r, LegacyReconciliationReceipt{}) {
				t.Fatal("invalid unavailable acquired authority", err)
			}
		})
	}
	// Zero live refs still covers the complete removed inventory without an anchor.
	p.Live = nil
	proof = goodLegacyProof(a, p)
	v = referenceFixture(a, p, LegacyReconciliation{Verification: proof, Started: time.Now().UTC(), Finished: time.Now().UTC()})
	r, e := ReviewLegacyReconciliation(a, p, v)
	if e != nil || r.ReferenceContext.CompletedReferences != 0 {
		t.Fatal("historical-only invented reads", e)
	}
	for _, i := range r.Items {
		if i.Disposition != "historical" || i.State != nil || i.ReadRef != nil {
			t.Fatal("historical item acquired state")
		}
	}
}
func TestReferenceStoreSnapshotAndPendingCompatibility(t *testing.T) {
	for _, kind := range []string{"valid", "state", "outcome", "context", "receipt", "resigned-audit", "v8-paired", "v8-no-pair", "v1", "v2", "v3", "v4", "v5", "v6", "v7"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			path, a, p, v := referencePreparation(t)
			r, e := ReviewLegacyReconciliation(a, p, v)
			if e != nil {
				t.Fatal(e)
			}
			owned := ownReconciliation(v)
			confirmation := r.Digest
			n := p.Live[0].N
			switch kind {
			case "state":
				s := owned.Verification.States[n]
				s.Metadata.Subject = "Changed"
				owned.Verification.States[n] = s
			case "outcome":
				o := owned.ReferenceOutcomes[n]
				o.SourceValidated = false
				owned.ReferenceOutcomes[n] = o
			case "context":
				owned.Started = owned.Started.Add(-time.Second)
			case "receipt":
				r.Items[0].SourceValidated = false
			case "resigned-audit":
				r.Started = r.Started.Add(-time.Second)
				r.Digest, r.AuditDigest, e = reconciliationReceiptDigests(r)
				if e != nil {
					t.Fatal(e)
				}
			case "v8-paired", "v8-no-pair", "v1", "v2", "v3", "v4", "v5", "v6", "v7":
				_, _, _, legacy := senderAuthorityPreparation(t)
				legacy.Verification.Account = a
				legacy.Verification.Nonce = p.Challenge()
				legacy.Verification.States = v.Verification.States
				legacy.Verification.Folders = v.Verification.Folders
				if kind != "v8-paired" {
					legacy.EnumeratedStates = nil
					legacy.SenderSourceIntegrity = nil
				}
				old := frozenV8Fixture(t, a, p, legacy)
				confirmation, _ = reconciliationV8Digests(t, old)
				switch kind {
				case "v1":
					confirmation = reconciliationV1Digest(t, old)
				case "v2":
					confirmation = reconciliationV2Digest(t, old)
				case "v3":
					confirmation = reconciliationV3Digest(t, old)
				case "v4":
					confirmation = reconciliationV4Digest(t, old)
				case "v5":
					confirmation = reconciliationV5Digest(t, old)
				case "v6":
					confirmation = reconciliationV6Digest(t, old)
				case "v7":
					confirmation = reconciliationV7Digest(t, old)
				}
				if confirmation == r.Digest {
					t.Fatal("old pending authority reinterpreted")
				}
			}
			life, db, e := maintenanceDB(ctx, path)
			if e != nil {
				t.Fatal(e)
			}
			defer closeMaintenance(life, db)
			conn, e := db.Conn(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer conn.Close()
			before, e := logicalDigest(ctx, conn)
			if e != nil {
				t.Fatal(e)
			}
			bound := &AccountStore{account: a}
			e = immediate(ctx, conn, func(q sqlAccess) error {
				return bound.bindLegacyTransaction(ctx, q, p, v.Verification, v.Unavailable, &legacyReconciliationRequest{receipt: r, confirmation: confirmation, evidence: owned}, "invented-backup", p.Digest())
			})
			if kind == "valid" {
				if e != nil {
					t.Fatal(e)
				}
			} else {
				if !errors.Is(e, mailbox.ErrStaleIdentity) {
					t.Fatal("invalid current/old snapshot committed", e)
				}
				after, err := logicalDigest(ctx, conn)
				if err != nil || before != after {
					t.Fatal("failed transaction changed history", err)
				}
			}
		})
	}
}
func TestReferenceApprovalRawStateSensitivity(t *testing.T) {
	_, a, p, v := referencePreparation(t)
	original := v.Verification.States[p.Live[0].N]
	original.Metadata.Recipients = append(original.Metadata.Recipients, mailbox.Address{Address: "second@example.org", Name: "Second"})
	v.Verification.States[p.Live[0].N] = original
	v = referenceFixture(a, p, v)
	base, e := ReviewLegacyReconciliation(a, p, v)
	if e != nil {
		t.Fatal(e)
	}
	n := p.Live[0].N
	for _, kind := range []string{"name", "recipient-name", "recipient-address", "subject", "preview", "received", "read", "attachment", "revision", "membership", "recipient-nil", "recipient-order", "recipient-duplicate", "times"} {
		t.Run(kind, func(t *testing.T) {
			x := ownReconciliation(v)
			s := x.Verification.States[n]
			switch kind {
			case "name":
				s.Metadata.Sender.Name = "New"
			case "recipient-name":
				s.Metadata.Recipients[0].Name = "New"
			case "recipient-address":
				s.Metadata.Recipients[0].Address = strings.ToUpper(s.Metadata.Recipients[0].Address)
			case "subject":
				s.Metadata.Subject = "New"
			case "preview":
				s.Metadata.Preview = "New"
			case "received":
				s.Metadata.Received = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			case "read":
				s.Metadata.IsRead = !s.Metadata.IsRead
			case "attachment":
				s.Metadata.HasAttachments = !s.Metadata.HasAttachments
			case "revision":
				s.Revision = "new"
			case "membership":
				s.Memberships[0].Collection.CollectionID = "other"
			case "recipient-nil":
				s.Metadata.Recipients = nil
			case "recipient-order":
				s.Metadata.Recipients[0], s.Metadata.Recipients[1] = s.Metadata.Recipients[1], s.Metadata.Recipients[0]
			case "recipient-duplicate":
				s.Metadata.Recipients = append(s.Metadata.Recipients, s.Metadata.Recipients[0])
			case "times":
				x.Started = x.Started.Add(-time.Second)
			}
			x.Verification.States[n] = s
			x.ReferenceContext.StateBytes = 0
			for _, state := range x.Verification.States {
				raw, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				x.ReferenceContext.StateBytes += int64(len(raw))
			}
			r, err := ReviewLegacyReconciliation(a, p, x)
			if err != nil || (r.Digest == base.Digest) != (kind == "times") || r.AuditDigest == base.AuditDigest {
				t.Fatal("approval/audit sensitivity incorrect", err)
			}
		})
	}
}

func TestReferenceDigestLiteralPreimages(t *testing.T) {
	// Encoding-only vector: it supplies no admission/history capability.
	r := LegacyReconciliationReceipt{Version: 9, Method: LegacyReferenceMethod, Account: mailbox.Account{ID: "account", ProviderID: "graph"}, SourceDigest: "source", Coverage: LegacyReferenceCoverage, Folders: map[string]mailbox.CollectionRef{"b": {AccountID: "account", CollectionID: "b"}, "a": {AccountID: "account", CollectionID: "a"}}, Started: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Finished: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ReferenceContext: &LegacyReferenceContext{}, Items: []LegacyReconciliationItem{{N: 3, OriginalID: "opaque", Removed: true, Disposition: "historical"}}}
	const approvalJSON = `{"Domain":"mailpipe-legacy-reconciliation-approval-v9","Method":"graph-exact-original-inspections-v1","Account":{"ID":"account","ProviderID":"graph"},"Source":"source","Coverage":"graph-original-references-v1","Folders":{"a":{"AccountID":"account","CollectionID":"a"},"b":{"AccountID":"account","CollectionID":"b"}},"Items":[{"n":3,"original_id":"opaque","original_removed":true,"disposition":"historical","membership_changed":false}]}`
	h := sha256.Sum256([]byte(approvalJSON))
	approval := hex.EncodeToString(h[:])
	auditJSON := `{"Domain":"mailpipe-legacy-reconciliation-audit-v9","ApprovalDigest":"` + approval + `","Method":"graph-exact-original-inspections-v1","Account":{"ID":"account","ProviderID":"graph"},"Source":"source","Coverage":"graph-original-references-v1","Folders":{"a":{"AccountID":"account","CollectionID":"a"},"b":{"AccountID":"account","CollectionID":"b"}},"Started":"2026-01-01T00:00:00Z","Finished":"2026-01-01T00:00:00Z","ReferenceContext":{"requested_references":0,"completed_references":0,"inspection_batches":0,"state_bytes":0},"Items":[{"recipient_name_changed":false,"sender_address_disagrees":false,"sender_name_changed":false,"sender_casing_changed":false,"recipient_casing_changed":false,"membership_changed":false,"n":3,"original_id":"opaque","original_removed":true,"disposition":"historical"}]}`
	h = sha256.Sum256([]byte(auditJSON))
	audit := hex.EncodeToString(h[:])
	gotA, gotD, e := reconciliationReceiptDigests(r)
	if e != nil || gotA != approval || gotD != audit {
		t.Fatal("v9 literal typed preimage/domain differs", e)
	}
	included := map[string]bool{"N": true, "OriginalID": true, "Removed": true, "Disposition": true, "Reason": true, "OriginalFolder": true, "OriginalMemberships": true, "MembershipChanged": true, "ReadRef": true, "ReadOutcome": true, "State": true}
	excluded := map[string]bool{"SourceValidated": true, "RecipientNameChanged": true, "SenderAddressDisagrees": true, "SenderNameChanged": true, "SenderCasingChanged": true, "RecipientCasingChanged": true, "EnumeratedState": true}
	typ := reflect.TypeFor[LegacyReconciliationItem]()
	projection := reflect.TypeFor[reconciliationApprovalItem]()
	if typ.NumField() != len(included)+len(excluded) || projection.NumField() != len(included) {
		t.Fatal("unclassified future item/projection field")
	}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if !included[name] && !excluded[name] {
			t.Fatal("unclassified item field", name)
		}
	}
	for i := 0; i < projection.NumField(); i++ {
		if !included[projection.Field(i).Name] {
			t.Fatal("audit context entered approval")
		}
	}
}
func TestReferenceReceiptLegacyAndCodecTamper(t *testing.T) {
	_, a, p, v := referencePreparation(t)
	base, e := ReviewLegacyReconciliation(a, p, v)
	if e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"name", "case", "sender-name", "sender-case", "sender-disagrees", "enum", "pages", "method", "version", "first-codec", "second-codec"} {
		t.Run(kind, func(t *testing.T) {
			raw, _ := json.Marshal(base)
			var r LegacyReconciliationReceipt
			if e := json.Unmarshal(raw, &r); e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "name":
				r.Items[0].RecipientNameChanged = true
			case "case":
				r.Items[0].RecipientCasingChanged = true
			case "sender-name":
				r.Items[0].SenderNameChanged = true
			case "sender-case":
				r.Items[0].SenderCasingChanged = true
			case "sender-disagrees":
				r.Items[0].SenderAddressDisagrees = true
			case "enum":
				r.Items[0].EnumeratedState = r.Items[0].State
			case "pages":
				r.Pages = 1
			case "method":
				r.Method = "other"
			case "version":
				r.Version = 8
			case "first-codec":
				r.Items[0].State.Metadata.Received = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "second-codec":
				r.Started = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			approval, audit, err := reconciliationReceiptDigests(r)
			if err == nil || approval != "" || audit != "" {
				t.Fatal("tampered/failed codec returned partial digests")
			}
		})
	}
}

func TestReferenceReviewOwnsCallerEvidence(t *testing.T) {
	_, a, p, v := referencePreparation(t)
	r, err := ReviewLegacyReconciliation(a, p, v)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	owned := ownReconciliation(v)
	ownedJSON, err := json.Marshal(owned)
	if err != nil {
		t.Fatal(err)
	}
	n := p.Live[0].N
	s := v.Verification.States[n]
	s.Metadata.Recipients[0].Name = "caller mutation"
	s.Memberships[0].Collection.CollectionID = "caller collection"
	v.Verification.States[n] = s
	v.Verification.Folders[p.Live[0].Folder] = mailbox.CollectionRef{AccountID: a.ID, CollectionID: "caller folder"}
	v.ReferenceOutcomes[n] = LegacyReferenceOutcome{Kind: "caller outcome"}
	v.ReferenceContext.StateBytes++
	after, err := json.Marshal(r)
	if err != nil || string(encoded) != string(after) {
		t.Fatal("review receipt aliases caller state", err)
	}
	after, err = json.Marshal(owned)
	if err != nil || string(ownedJSON) != string(after) {
		t.Fatal("owned final evidence aliases caller containers", err)
	}
}

// Adding a field requires an explicit admission, ownership and digest decision.
// Full State remains projected as a value; its nested slices still need ownership.
func TestReferenceSchemaFieldContracts(t *testing.T) {
	contracts := []struct {
		typ    reflect.Type
		groups map[string]string
	}{
		{reflect.TypeFor[LegacyReferenceContext](), map[string]string{"validated audit measurements; value-owned": "RequestedReferences CompletedReferences InspectionBatches StateBytes"}},
		{reflect.TypeFor[LegacyReferenceOutcome](), map[string]string{"exact identity/disposition; approval+audit value-owned": "Ref Kind", "validated capability; audit-only value-owned": "SourceValidated"}},
		{reflect.TypeFor[LegacyVerification](), map[string]string{"validated account/challenge/confirmation; value-owned": "Account Nonce HistoricalAttributionConfirmed", "authoritative input; maps and nested state slices owned": "States Folders"}},
		{reflect.TypeFor[LegacyReconciliation](), map[string]string{"validated approval+audit context; value-owned": "Method Coverage", "audit-only; value-owned": "Started Finished ReferenceContext", "validated complete outcome/disposition input; map-owned": "ReferenceOutcomes Unavailable", "validated history/state input; deeply owned": "Verification", "legacy authority forbidden; empty/zero required": "EnumeratedStates SenderSourceIntegrity Pages Observed"}},
		{reflect.TypeFor[LegacyReconciliationReceipt](), map[string]string{"fixed new semantics; separately shape-validated": "Version", "approval+audit context; nested containers owned": "Method Account SourceDigest Coverage Folders Items", "audit-only; context pointer owned": "ReferenceContext Started Finished", "derived digests, not incoming authority": "Digest AuditDigest", "legacy counts forbidden for new scope": "Pages Observed", "separate confirmation/verified backup outcome guards": "HistoricalAttributionConfirmed Backup BackupDigest"}},
		{reflect.TypeFor[reconciliationApprovalPayload](), map[string]string{"fixed domain and complete stable approval projection": "Domain Method Account Source Coverage Folders Items"}},
		{reflect.TypeFor[reconciliationAuditPayload](), map[string]string{"fixed domain, computed approval and full current-execution audit": "Domain ApprovalDigest Method Account Source Coverage Folders Started Finished ReferenceContext Items"}},
		{reflect.TypeFor[mailbox.State](), map[string]string{"full approval+audit projection; value-owned": "Ref Revision", "full projection; nested recipients owned": "Metadata", "full projection; slice-owned": "Memberships"}},
		{reflect.TypeFor[mailbox.Metadata](), map[string]string{"full approval+audit projection; value-owned": "InternetMessageID Received Sender Subject Preview HasAttachments IsRead", "full projection; slice-owned, exact shape/order": "Recipients"}},
	}
	for _, c := range contracts {
		t.Run(c.typ.Name(), func(t *testing.T) {
			classified := map[string]bool{}
			for role, fields := range c.groups {
				if role == "" {
					t.Fatal("missing field semantics")
				}
				for _, name := range strings.Fields(fields) {
					if classified[name] {
						t.Fatal("duplicate classification", name)
					}
					classified[name] = true
				}
			}
			if len(classified) != c.typ.NumField() {
				t.Fatal("schema changed; review admission, ownership and hash classification")
			}
			for i := 0; i < c.typ.NumField(); i++ {
				if !classified[c.typ.Field(i).Name] {
					t.Fatal("unclassified field", c.typ.Field(i).Name)
				}
			}
		})
	}
}
