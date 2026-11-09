package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func TestReconciliationReviewsMovedMembership(t *testing.T) {
	ctx := context.Background()
	path := frozenLegacy(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := PrepareLegacyBinding(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	a := mailbox.Account{ID: "invented-account", ProviderID: GraphProvider}
	proof := goodLegacyProof(a, p)
	n := p.Live[0].N
	state := proof.States[n]
	old := state.Memberships[0].Collection
	state.Memberships = []mailbox.Membership{{Collection: mailbox.CollectionRef{AccountID: a.ID, CollectionID: "current-collection"}}}
	proof.States[n] = state
	if bound, err := BindLegacy(ctx, path, a, p, verifyLegacyFunc(func(context.Context, mailbox.Account, LegacyPreparation) (LegacyVerification, error) {
		return proof, nil
	})); err == nil {
		bound.Close()
		t.Fatal("strict binding accepted changed collection")
	}
	now := time.Now().UTC()
	evidence := referenceFixture(a, p, LegacyReconciliation{Verification: proof, Started: now, Finished: now})
	r, err := ReviewLegacyReconciliation(a, p, evidence)
	if err != nil {
		t.Fatalf("reviewable exact-ID collection change blocked: %v", err)
	}
	if r.Version != 9 {
		t.Fatalf("membership proposal has version %d", r.Version)
	}
	raw, _ := json.Marshal(r.Items[0])
	var item map[string]json.RawMessage
	_ = json.Unmarshal(raw, &item)
	var previous []mailbox.Membership
	if err = json.Unmarshal(item["original_memberships"], &previous); err != nil || len(previous) != 1 || previous[0].Collection != old || string(item["membership_changed"]) != "true" {
		t.Fatalf("missing old/current review evidence: %s", raw)
	}
	bound, err := ReconcileLegacy(ctx, path, a, p, r.Digest, reconcileLegacyFunc(func(context.Context, mailbox.Account, LegacyPreparation) (LegacyReconciliation, error) {
		return evidence, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	m, err := bound.GetMessage(ctx, mailbox.LocalRef{AccountID: a.ID, N: n})
	if err != nil || m.State.Memberships[0].Collection != state.Memberships[0].Collection {
		t.Fatalf("current membership not published: %+v %v", m, err)
	}
	var folder string
	if err = bound.store.db.QueryRowContext(ctx, "select folder from messages where n=?", n).Scan(&folder); err != nil || folder != p.Live[0].Folder {
		t.Fatalf("original folder changed: %q %v", folder, err)
	}
}

// Frozen v1 item shape: new review fields must not alter the old digest domain.
type reconciliationV1Item struct {
	N           int64          `json:"n"`
	OriginalID  string         `json:"original_id"`
	Removed     bool           `json:"original_removed"`
	Disposition string         `json:"disposition"`
	Reason      string         `json:"reason,omitempty"`
	State       *mailbox.State `json:"state,omitempty"`
}

func reconciliationV1Items(r LegacyReconciliationReceipt) []reconciliationV1Item {
	items := make([]reconciliationV1Item, len(r.Items))
	for j, i := range r.Items {
		items[j] = reconciliationV1Item{i.N, i.OriginalID, i.Removed, i.Disposition, i.Reason, i.State}
	}
	return items
}

func reconciliationV1Digest(t *testing.T, r LegacyReconciliationReceipt) string {
	t.Helper()
	raw, err := json.Marshal(struct {
		Domain           string
		Account          mailbox.Account
		Source, Coverage string
		Folders          map[string]mailbox.CollectionRef
		Items            []reconciliationV1Item
	}{"mailpipe-legacy-reconciliation-v1", r.Account, r.SourceDigest, r.Coverage, r.Folders, reconciliationV1Items(r)})
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func TestReconciliationMembershipDigestAndValidation(t *testing.T) {
	ctx := context.Background()
	path := frozenLegacy(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := PrepareLegacyBinding(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	a := mailbox.Account{ID: "invented-account", ProviderID: GraphProvider}
	makeEvidence := func() LegacyReconciliation {
		now := time.Now().UTC()
		return referenceFixture(a, p, LegacyReconciliation{Verification: goodLegacyProof(a, p), Started: now, Finished: now})
	}
	base, err := ReviewLegacyReconciliation(a, p, makeEvidence())
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"current-membership", "old-membership", "revision", "metadata", "cross-account", "duplicate", "zero", "multiple", "old-pending-v1"} {
		t.Run(kind, func(t *testing.T) {
			e := makeEvidence()
			n := p.Live[0].N
			state := e.Verification.States[n]
			switch kind {
			case "current-membership":
				state.Memberships[0].Collection.CollectionID = "moved"
			case "old-membership":
				e.Verification.Folders[p.Live[0].Folder] = mailbox.CollectionRef{AccountID: a.ID, CollectionID: "different-old"}
			case "revision":
				state.Revision = "changed-revision"
			case "metadata":
				state.Metadata.Subject = "Changed synthetic metadata"
			case "cross-account":
				state.Memberships[0].Collection.AccountID = "other-account"
			case "duplicate":
				state.Memberships = append(state.Memberships, state.Memberships[0])
			case "zero":
				state.Memberships = nil
			case "multiple":
				state.Memberships = append(state.Memberships, mailbox.Membership{Collection: mailbox.CollectionRef{AccountID: a.ID, CollectionID: "second"}})
			}
			e.Verification.States[n] = state
			e.ReferenceContext.StateBytes = 0
			for _, current := range e.Verification.States {
				raw, _ := json.Marshal(current)
				e.ReferenceContext.StateBytes += int64(len(raw))
			}
			r, err := ReviewLegacyReconciliation(a, p, e)
			invalid := kind == "cross-account" || kind == "duplicate" || kind == "zero" || kind == "multiple"
			if invalid {
				if err == nil {
					t.Fatal("invalid Graph membership accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			confirmation := base.Digest
			if kind == "old-pending-v1" {
				confirmation = reconciliationV1Digest(t, base)
				if confirmation == base.Digest {
					t.Fatal("digest domains overlap")
				}
			} else if r.Digest == base.Digest {
				t.Fatal("review-relevant change omitted from digest")
			}
			if bound, err := ReconcileLegacy(ctx, path, a, p, confirmation, reconcileLegacyFunc(func(context.Context, mailbox.Account, LegacyPreparation) (LegacyReconciliation, error) { return e, nil })); err == nil {
				bound.Close()
				t.Fatal("old proposal authorized changed evidence")
			}
			s, err := Open(path)
			if err != nil {
				t.Fatal("failed review attributed history", err)
			}
			s.Close()
		})
	}
}

func TestReconciliationV1StoredReceiptRemainsReadable(t *testing.T) {
	ctx := context.Background()
	path := frozenLegacy(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	p, err := PrepareLegacyBinding(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	a := mailbox.Account{ID: "invented-account", ProviderID: GraphProvider}
	v := goodLegacyProof(a, p)
	now := time.Now().UTC()
	r := frozenV8Fixture(t, a, p, LegacyReconciliation{Verification: v, Started: now, Finished: now})
	r.Version = 1
	r.Digest = reconciliationV1Digest(t, r)
	oldItems := reconciliationV1Items(r)
	r.Items = nil
	bound, err := BindLegacy(ctx, path, a, p, verifyLegacyFunc(func(context.Context, mailbox.Account, LegacyPreparation) (LegacyVerification, error) { return v, nil }))
	if err != nil {
		t.Fatal(err)
	}
	// Populate a synthetic old receipt using its frozen JSON shape. No product
	// path rewrites receipts or exposes a receipt-import operation.
	err = bound.transaction(ctx, func(q sqlAccess) error {
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err = q.ExecContext(ctx, "insert into legacy_reconciliations values(1,?,?,?,?,?)", a.ID, a.ProviderID, r.SourceDigest, r.Digest, string(raw)); err != nil {
			return err
		}
		for _, i := range oldItems {
			raw, err = json.Marshal(i)
			if err != nil {
				return err
			}
			if _, err = q.ExecContext(ctx, "insert into legacy_reconciliation_items values(?,?,?,?)", i.N, a.ID, a.ProviderID, string(raw)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	bound.Close()
	read, found, err := ReadLegacyReconciliationReceipt(ctx, path, a)
	if err != nil || !found || read.Version != 1 || read.Digest != r.Digest || len(read.Items) != len(oldItems) {
		t.Fatalf("old receipt changed: %+v %v", read, err)
	}
	for _, i := range read.Items {
		if i.OriginalFolder != "" || i.OriginalMemberships != nil || i.MembershipChanged {
			t.Fatal("old receipt invented prior membership")
		}
	}
}

func TestReconciliationReservedIdentitiesRetainTheirOwnHistory(t *testing.T) {
	ctx := context.Background()
	path := frozenLegacy(t)
	originalDB := rawFixtureDB(t, path)
	// Make the already removed counterpart correlate with the unavailable row.
	if _, err := originalDB.Exec("update messages set internet_message_id='<copy@example.org>',sender='sender@example.org' where n=77"); err != nil {
		t.Fatal(err)
	}
	specs, err := tableSpecs(ctx, originalDB)
	if err != nil {
		t.Fatal(err)
	}
	originalDB.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Upsert(ctx, "inbox", Record{GraphID: "anchor", InternetMessageID: "<copy@example.org>", Sender: "sender@example.org"}); err != nil {
		t.Fatal(err)
	}
	if err = s.AddVerdict(ctx, Verdict{N: 77, Stage: "filed", Provenance: "removed-fixture"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	db := rawFixtureDB(t, path)
	before, err := dataDigest(ctx, db, specs)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	p, err := PrepareLegacyBinding(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	a := mailbox.Account{ID: "invented-account", ProviderID: GraphProvider}
	e := reconciliationProof(a, p)
	r, err := ReviewLegacyReconciliation(a, p, e)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := ReconcileLegacy(ctx, path, a, p, r.Digest, reconcileLegacyFunc(func(context.Context, mailbox.Account, LegacyPreparation) (LegacyReconciliation, error) { return e, nil }))
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	after, err := dataDigest(ctx, bound.store.db, specs)
	if err != nil || after != before {
		t.Fatal("attribution changed original columns/history", err)
	}
	for _, n := range []int64{7, 77} {
		m, err := bound.GetMessage(ctx, mailbox.LocalRef{AccountID: a.ID, N: n})
		if err != nil || len(m.State.Memberships) != 0 || m.State.Revision != "" {
			t.Fatalf("reserved identity invented live evidence: %+v %v", m, err)
		}
	}
	scope := mailbox.Collection{Ref: e.Verification.Folders["inbox"]}
	sync := func(ids ...string) {
		t.Helper()
		var records []MessageRecord
		var messages []mailbox.Message
		for _, id := range ids {
			m := mailbox.Message{Ref: mailbox.MessageRef{AccountID: a.ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator(id)}, Metadata: mailbox.Metadata{InternetMessageID: "<copy@example.org>", Sender: mailbox.Address{Address: "sender@example.org"}}}
			messages = append(messages, m)
			records = append(records, MessageRecord{Message: m, GraphID: id})
		}
		pass, err := bound.BeginSync(ctx, scope, mailbox.Cursor{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = bound.StagePage(ctx, pass, mailbox.Cursor{}, mailbox.SyncPage{Mode: mailbox.Snapshot, Complete: true, Messages: messages}, records); err != nil {
			t.Fatal(err)
		}
		if err = bound.CommitSync(ctx, pass); err != nil {
			t.Fatal(err)
		}
	}
	sync("anchor", "opaque-removed", "new-counterpart")
	missing, err := bound.GetMessage(ctx, mailbox.LocalRef{AccountID: a.ID, N: 7})
	if err != nil || len(missing.State.Memberships) != 0 || missing.State.Ref.MessageID != "opaque-old" {
		t.Fatal("alternate ID stole unavailable row", err)
	}
	removed, err := bound.GetMessage(ctx, mailbox.LocalRef{AccountID: a.ID, N: 77})
	if err != nil || len(removed.State.Memberships) != 1 || removed.State.Ref.MessageID != "opaque-removed" {
		t.Fatal("removed counterpart lost its own reservation", err)
	}
	rows, err := bound.ByMessageID(ctx, "<copy@example.org>")
	if err != nil {
		t.Fatal(err)
	}
	var fresh mailbox.LocalRef
	for _, m := range rows {
		if m.State.Ref.MessageID == "new-counterpart" {
			fresh = m.Local
		}
	}
	if fresh.N <= 0 || fresh.N == 7 || fresh.N == 77 {
		t.Fatal("new opaque ID reused historical N")
	}
	verdicts, err := bound.Verdicts(ctx, fresh)
	if err != nil || len(verdicts) != 0 {
		t.Fatal("new identity inherited verdicts", err)
	}
	sync("anchor", "opaque-removed", "new-counterpart", "opaque-old")
	original, err := bound.GetMessage(ctx, mailbox.LocalRef{AccountID: a.ID, N: 7})
	if err != nil || original.State.Ref.MessageID != "opaque-old" || len(original.State.Memberships) != 1 || original.Body == nil || *original.Body != "invented body" {
		t.Fatalf("same ID failed to revive own N/body: %+v %v", original, err)
	}
	for n, provenance := range map[int64]string{7: "fixture", 77: "removed-fixture"} {
		vs, err := bound.Verdicts(ctx, mailbox.LocalRef{AccountID: a.ID, N: n})
		if err != nil || len(vs) != 1 || vs[0].Provenance != provenance {
			t.Fatalf("history crossed opaque identities: %d %+v %v", n, vs, err)
		}
	}
	if _, err = bound.LoadReviewedAction(ctx, 11, mailbox.LocalRef{AccountID: a.ID, N: 7}, a); err == nil {
		t.Fatal("old pending plan became executable")
	}
	acts, err := bound.Actions(ctx, 11)
	if err != nil || len(acts) != 1 || acts[0].Audit.ID != 19 {
		t.Fatal("old action history lost", err)
	}
}
