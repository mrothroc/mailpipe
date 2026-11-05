package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

type reconcileLegacyFunc func(context.Context, mailbox.Account, LegacyPreparation) (LegacyReconciliation, error)

func (f reconcileLegacyFunc) ReconcileLegacy(ctx context.Context, a mailbox.Account, p LegacyPreparation) (LegacyReconciliation, error) {
	return f(ctx, a, p)
}

func reconciliationProof(a mailbox.Account, p LegacyPreparation) LegacyReconciliation {
	v := goodLegacyProof(a, p)
	delete(v.States, 7)
	return LegacyReconciliation{Verification: v, Unavailable: map[int64]string{7: LegacyUnavailable}, Coverage: LegacyPrimaryMailbox, Started: time.Now().UTC(), Finished: time.Now().UTC(), Pages: 1, Observed: len(v.States)}
}

func TestReconciliationRejectsIncompleteAuthority(t *testing.T) {
	ctx := context.Background()
	path := frozenLegacy(t)
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	// The frozen fixture has only one live message. A missing reference alone
	// cannot establish an anchor in an unrelated mailbox.
	p, e := PrepareLegacyBinding(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	a := mailbox.Account{ID: "history-account", ProviderID: GraphProvider}
	proof := reconciliationProof(a, p)
	if _, e = ReviewLegacyReconciliation(a, p, proof); e == nil {
		t.Fatal("no-anchor evidence accepted")
	}
	if bound, e := BindLegacy(ctx, path, a, p, verifyLegacyFunc(func(context.Context, mailbox.Account, LegacyPreparation) (LegacyVerification, error) {
		return proof.Verification, nil
	})); e == nil {
		bound.Close()
		t.Fatal("strict bind accepted missing state")
	}
	if _, e = OpenAccount(ctx, path, a); e == nil {
		t.Fatal("failed proof attributed history")
	}
}

func TestReviewedReconciliationPreservesHistory(t *testing.T) {
	ctx := context.Background()
	path := frozenLegacy(t)
	originalDB := rawFixtureDB(t, path)
	originalSpecs, e := tableSpecs(ctx, originalDB)
	if e != nil {
		t.Fatal(e)
	}
	originalDB.Close()

	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Upsert(ctx, "inbox", Record{GraphID: "anchor", InternetMessageID: "<anchor@example.org>", Sender: "anchor@example.org"}); e != nil {
		t.Fatal(e)
	}
	s.Close()
	beforeDB := rawFixtureDB(t, path)
	before, e := dataDigest(ctx, beforeDB, originalSpecs)
	if e != nil {
		t.Fatal(e)
	}
	beforeDB.Close()

	p, e := PrepareLegacyBinding(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	a := mailbox.Account{ID: "history-account", ProviderID: GraphProvider}
	proof := reconciliationProof(a, p)
	report, e := ReviewLegacyReconciliation(a, p, proof)
	if e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"account", "challenge", "missing", "overlap", "coverage", "consent", "digest", "callback", "sender", "header", "locator", "folder", "reason", "extra"} {
		t.Run(kind, func(t *testing.T) {
			verifier := reconcileLegacyFunc(func(_ context.Context, a mailbox.Account, p LegacyPreparation) (LegacyReconciliation, error) {
				v := reconciliationProof(a, p)
				switch kind {
				case "account":
					v.Verification.Account.ID = "wrong-account"
				case "challenge":
					v.Verification.Nonce = "wrong"
				case "missing":
					delete(v.Unavailable, 7)
				case "overlap":
					v.Verification.States[7] = goodLegacyProof(a, p).States[7]
				case "coverage":
					v.Coverage = "partial"
				case "consent":
					v.Verification.HistoricalAttributionConfirmed = false
				case "sender", "header", "locator":
					for n, state := range v.Verification.States {
						switch kind {
						case "sender":
							state.Metadata.Sender.Address = "other@example.net"
						case "header":
							state.Metadata.InternetMessageID = "<other@example.net>"
						case "locator":
							state.Ref.Locator = "other"
						}
						v.Verification.States[n] = state
						break
					}
				case "folder":
					delete(v.Verification.Folders, "inbox")
				case "reason":
					v.Unavailable[7] = "unknown"
				case "extra":
					v.Unavailable[999] = LegacyUnavailable
				case "callback":
					return v, errors.New("incomplete remote inventory")
				}
				return v, nil
			})
			digest := report.Digest
			if kind == "digest" {
				digest = "unreviewed"
			}
			b, e := ReconcileLegacy(ctx, path, a, p, digest, verifier)
			if e == nil {
				b.Close()
				t.Fatal("incomplete authority accepted")
			}
			s, e := Open(path)
			if e != nil {
				t.Fatal("failed reconciliation changed attribution", e)
			}
			s.Close()
		})
	}
	verifier := reconcileLegacyFunc(func(_ context.Context, a mailbox.Account, p LegacyPreparation) (LegacyReconciliation, error) {
		return reconciliationProof(a, p), nil
	})
	bound, e := ReconcileLegacy(ctx, path, a, p, report.Digest, verifier)
	if e != nil {
		t.Fatal(e)
	}
	defer bound.Close()
	after, e := dataDigest(ctx, bound.store.db, originalSpecs)
	if e != nil || after != before {
		t.Fatalf("original historical columns changed: %v", e)
	}

	receipt, e := bound.LegacyReconciliationReceipt(ctx)
	if e != nil || receipt.Digest != report.Digest || receipt.BackupDigest != p.Digest() {
		t.Fatalf("receipt: %+v %v", receipt, e)
	}
	for _, items := range [][]LegacyReconciliationItem{report.Items, receipt.Items} {
		historical := map[int64]string{}
		for _, item := range items {
			if item.Disposition == "historical" {
				historical[item.N] = item.OriginalID
				if !item.Removed || item.State != nil {
					t.Fatalf("removed receipt invented current evidence: %+v", item)
				}
			}
		}
		if len(historical) != 1 || historical[77] != "opaque-removed" {
			t.Fatalf("frozen removed identity missing from report/receipt: %v", historical)
		}
		if len(historical) != len(p.Removed) {
			t.Fatalf("removed rows omitted: %v", historical)
		}
		for _, ref := range p.Removed {
			if id, ok := historical[ref.N]; !ok || id != ref.GraphID {
				t.Fatalf("removed identity omitted or changed: %+v", ref)
			}
		}
	}
	m, e := bound.GetMessage(ctx, mailbox.LocalRef{AccountID: a.ID, N: 7})
	if e != nil || !m.Valid || len(m.State.Memberships) != 0 {
		t.Fatalf("historical message: %+v %v", m, e)
	}
	history, e := bound.PlanHistory(ctx, 11)
	if e != nil || len(history.Items) != 1 || history.Items[0].Expected != nil {
		t.Fatalf("plan history: %+v %v", history, e)
	}
	if _, e = bound.LoadReviewedAction(ctx, 11, mailbox.LocalRef{AccountID: a.ID, N: 7}, a); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("old plan became executable: %v", e)
	}
	acts, e := bound.Actions(ctx, 11)
	if e != nil || len(acts) != 1 || acts[0].Audit.ID != 19 {
		t.Fatalf("action history: %+v %v", acts, e)
	}
}

func TestReconciliationTransactionFailuresRollBack(t *testing.T) {
	for _, needle := range []string{"insert into store_binding", "update messages", "insert into memberships", "insert into legacy_reconciliations", "insert into legacy_reconciliation_items", "commit"} {
		t.Run(needle, func(t *testing.T) {
			ctx := context.Background()
			path := frozenLegacy(t)
			s, e := Open(path)
			if e != nil {
				t.Fatal(e)
			}
			s.Close()
			p, e := PrepareLegacyBinding(ctx, path)
			if e != nil {
				t.Fatal(e)
			}
			life, db, e := maintenanceDB(ctx, path)
			if e != nil {
				t.Fatal(e)
			}
			defer closeMaintenance(life, db)
			a := mailbox.Account{ID: "invented-account", ProviderID: GraphProvider}
			proof := goodLegacyProof(a, p)
			now := time.Now().UTC()
			evidence := LegacyReconciliation{Verification: proof, Unavailable: map[int64]string{}, Coverage: LegacyPrimaryMailbox, Started: now, Finished: now, Pages: 1, Observed: len(proof.States)}
			receipt, e := ReviewLegacyReconciliation(a, p, evidence)
			if e != nil {
				t.Fatal(e)
			}
			conn, e := db.Conn(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer conn.Close()
			before, e := logicalDigest(ctx, conn)
			if e != nil {
				t.Fatal(e)
			}
			f := &failSQL{sqlAccess: conn, needle: needle}
			bound := &AccountStore{account: a}
			e = immediate(ctx, f, func(q sqlAccess) error {
				return bound.bindLegacyTransaction(ctx, q, p, proof, evidence.Unavailable, &legacyReconciliationRequest{receipt: receipt}, "invented-backup", p.Digest())
			})
			if e == nil || !f.fired {
				t.Fatalf("failure not exercised: %v %v", e, f.fired)
			}
			after, e := logicalDigest(ctx, conn)
			if e != nil || after != before {
				t.Fatalf("partial attribution or receipt survived: %v", e)
			}
		})
	}
}

func TestReconciliationPreparationAndLifecycleGuards(t *testing.T) {
	ctx := context.Background()
	path := frozenLegacy(t)
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	p, e := PrepareLegacyBinding(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	a := mailbox.Account{ID: "invented-account", ProviderID: GraphProvider}
	verifier := reconcileLegacyFunc(func(_ context.Context, a mailbox.Account, p LegacyPreparation) (LegacyReconciliation, error) {
		now := time.Now().UTC()
		return LegacyReconciliation{Verification: goodLegacyProof(a, p), Coverage: LegacyPrimaryMailbox, Started: now, Finished: now, Pages: 1, Observed: len(p.Live)}, nil
	})
	proof, _ := verifier.ReconcileLegacy(ctx, a, p)
	r, e := ReviewLegacyReconciliation(a, p, proof)
	if e != nil {
		t.Fatal(e)
	}
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ReconcileLegacy(ctx, path, a, p, r.Digest, verifier); !errors.Is(e, ErrLifecycleBusy) {
		t.Fatalf("shared lifecycle bypassed: %v", e)
	}
	s.Close()
	other := frozenLegacy(t)
	s, e = Open(other)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	if b, e := ReconcileLegacy(ctx, other, a, p, r.Digest, verifier); e == nil {
		b.Close()
		t.Fatal("foreign path accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = ReconcileLegacy(canceled, path, a, p, r.Digest, verifier); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancellation: %v", e)
	}
	db := rawFixtureDB(t, path)
	if _, e = db.Exec("insert into plans(scope,created_at) values('changed','2026-01-01T00:00:00Z')"); e != nil {
		t.Fatal(e)
	}
	db.Close()
	if b, e := ReconcileLegacy(ctx, path, a, p, r.Digest, verifier); e == nil {
		b.Close()
		t.Fatal("stale preparation accepted")
	}
}

func TestReconciliationReceiptSchemaV1Upgrade(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(fmt.Sprint(bound), func(t *testing.T) {
			ctx := context.Background()
			path := frozenLegacy(t)
			s, e := Open(path)
			if e != nil {
				t.Fatal(e)
			}
			s.Close()
			a := mailbox.Account{ID: "invented-account", ProviderID: GraphProvider}
			if bound {
				p, e := PrepareLegacyBinding(ctx, path)
				if e != nil {
					t.Fatal(e)
				}
				b, e := BindLegacy(ctx, path, a, p, verifyLegacyFunc(func(_ context.Context, a mailbox.Account, p LegacyPreparation) (LegacyVerification, error) {
					return goodLegacyProof(a, p), nil
				}))
				if e != nil {
					t.Fatal(e)
				}
				b.Close()
			}
			db := rawFixtureDB(t, path)
			if _, e = db.Exec("drop table legacy_reconciliation_items; drop table legacy_reconciliations; pragma user_version=1"); e != nil {
				t.Fatal(e)
			}
			specs, e := tableSpecs(ctx, db)
			if e != nil {
				t.Fatal(e)
			}
			before, e := dataDigest(ctx, db, specs)
			if e != nil {
				t.Fatal(e)
			}
			db.Close()
			if bound {
				b, e := OpenAccount(ctx, path, a)
				if e != nil {
					t.Fatal(e)
				}
				b.Close()
			} else {
				s, e := Open(path)
				if e != nil {
					t.Fatal(e)
				}
				s.Close()
			}
			db = rawFixtureDB(t, path)
			defer db.Close()
			after, e := dataDigest(ctx, db, specs)
			if e != nil || before != after {
				t.Fatalf("v1 history changed: %v", e)
			}
			var version int
			if e = db.QueryRow("pragma user_version").Scan(&version); e != nil || version != 2 {
				t.Fatalf("version: %d %v", version, e)
			}
			var count int
			if e = db.QueryRow("select count(*) from legacy_reconciliations").Scan(&count); e != nil || count != 0 {
				t.Fatalf("migration invented a receipt: %d %v", count, e)
			}
		})
	}
}

func TestReconciliationRejectsTamperedExportedInventory(t *testing.T) {
	ctx := context.Background()
	path := frozenLegacy(t)
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	p, e := PrepareLegacyBinding(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	a := mailbox.Account{ID: "invented-account", ProviderID: GraphProvider}
	p.Live[0].GraphID = "forged-reference"
	proof := goodLegacyProof(a, p)
	now := time.Now().UTC()
	evidence := LegacyReconciliation{Verification: proof, Coverage: LegacyPrimaryMailbox, Started: now, Finished: now, Pages: 1, Observed: len(p.Live)}
	r, e := ReviewLegacyReconciliation(a, p, evidence)
	if e != nil {
		t.Fatal(e)
	}
	verifier := reconcileLegacyFunc(func(_ context.Context, a mailbox.Account, p LegacyPreparation) (LegacyReconciliation, error) {
		p.Live[0].GraphID = "forged-reference" // Even the trusted callback cannot replace the stored inventory.
		v := evidence
		v.Verification.Nonce = p.Challenge()
		return v, nil
	})
	if b, e := ReconcileLegacy(ctx, path, a, p, r.Digest, verifier); e == nil {
		b.Close()
		t.Fatal("exported inventory supplied binding authority")
	}
	s, e = Open(path)
	if e != nil {
		t.Fatal("failed proof bound history", e)
	}
	defer s.Close()
	m, found, e := s.Get(ctx, 7)
	if e != nil || !found || m.GraphID == "forged-reference" {
		t.Fatalf("original identity changed: %+v %v", m, e)
	}
}
