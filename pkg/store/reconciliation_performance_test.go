package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// inventedHistory is deliberately substantial: sparse stable numbers, two
// verdicts/message, plans/actions, varied cached bodies and two large cells.
func inventedHistory(b *testing.B, n, version int) string {
	b.Helper()
	ctx := context.Background()
	path := filepath.Join(b.TempDir(), "history.db")
	s, e := Open(path)
	if e != nil {
		b.Fatal(e)
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		b.Fatal(e)
	}
	insert, e := tx.Prepare("insert into messages(n,graph_id,internet_message_id,folder,received,sender,recipients,subject,preview,removed,body) values(?,?,?,'inbox','2026-01-01T00:00:00Z','sender@example.org','[]',?,?,?,?)")
	if e != nil {
		b.Fatal(e)
	}
	verdict, e := tx.Prepare("insert into verdicts(n,stage,provenance,at) values(?,?,'invented-history','2026-01-01T00:00:00Z')")
	if e != nil {
		b.Fatal(e)
	}
	for i := 0; i < n; i++ {
		body := strings.Repeat("x", 1024*(1+i%8))
		if i < 2 {
			body = strings.Repeat("x", 8<<20)
		}
		if _, e = insert.Exec(2*(i+1), fmt.Sprintf("invented-%d", i), fmt.Sprintf("<invented-%d@example.org>", i), "Invented history", strings.Repeat("p", 128), i%4 == 0, body); e != nil {
			b.Fatal(e)
		}
		for _, stage := range []string{"synthetic", "filed"} {
			if _, e = verdict.Exec(2*(i+1), stage); e != nil {
				b.Fatal(e)
			}
		}
		if i%100 == 0 {
			id := i/100 + 1
			n := 2 * (i + 1)
			if _, e = tx.Exec("insert into plans(id,scope,created_at) values(?,'invented','2026-01-01T00:00:00Z')", id); e != nil {
				b.Fatal(e)
			}
			if _, e = tx.Exec("insert into plan_items(plan_id,n,graph_id,internet_message_id,sender,action) values(?,?,?,?,?,'delete')", id, n, fmt.Sprintf("invented-%d", i), fmt.Sprintf("<invented-%d@example.org>", i), "sender@example.org"); e != nil {
				b.Fatal(e)
			}
			if _, e = tx.Exec("insert into actions(plan_id,n,graph_id,action,destination,status,at) values(?, ?,?,'delete','F-deleteditems','moved','2026-01-01T00:00:00Z')", id, n, fmt.Sprintf("invented-%d", i)); e != nil {
				b.Fatal(e)
			}
		}
	}
	insert.Close()
	verdict.Close()
	if e = tx.Commit(); e != nil {
		b.Fatal(e)
	}
	if version == 1 {
		if _, e = s.db.Exec("drop table legacy_reconciliation_items; drop table legacy_reconciliations; pragma user_version=1"); e != nil {
			b.Fatal(e)
		}
	}
	if e = s.Close(); e != nil {
		b.Fatal(e)
	}
	return path
}

type digestMetrics struct {
	sqlAccess
	directQueries, prepares int
	db                      *sql.DB
}

func (m *digestMetrics) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	m.directQueries++
	return m.sqlAccess.QueryRowContext(ctx, q, args...)
}
func (m *digestMetrics) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	m.directQueries++
	return m.sqlAccess.QueryContext(ctx, q, args...)
}
func (m *digestMetrics) PrepareContext(ctx context.Context, q string) (*sql.Stmt, error) {
	m.prepares++
	return m.db.PrepareContext(ctx, q)
}

func BenchmarkLegacyHistory(b *testing.B) {
	for _, n := range []int{10000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			path := inventedHistory(b, n, 2)
			info, e := os.Stat(path)
			if e != nil {
				b.Fatal(e)
			}
			b.Run("Prepare", func(b *testing.B) {
				b.ReportAllocs()
				b.ReportMetric(float64(info.Size()), "source-bytes")
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, e := PrepareLegacyBinding(context.Background(), path); e != nil {
						b.Fatal(e)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(info.Size()), "source-bytes")
			})
			b.Run("Digest", func(b *testing.B) {
				db, e := sqliteOpen(context.Background(), path)
				if e != nil {
					b.Fatal(e)
				}
				defer db.Close()
				m := &digestMetrics{sqlAccess: db, db: db}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, e = logicalDigest(context.Background(), m); e != nil {
						b.Fatal(e)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(m.directQueries)/float64(b.N), "direct-queries/digest")
				b.ReportMetric(float64(m.prepares)/float64(b.N), "explicit-prepares/digest")
			})
		})
	}
}

func BenchmarkLegacyMigration(b *testing.B) {
	for _, n := range []int{10000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				path := inventedHistory(b, n, 1)
				b.StartTimer()
				s, e := Open(path)
				if e != nil {
					b.Fatal(e)
				}
				if e = s.Close(); e != nil {
					b.Fatal(e)
				}
			}
		})
	}
}

func BenchmarkLegacyReconciliation(b *testing.B) {
	for _, n := range []int{10000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				path := inventedHistory(b, n, 2)
				p, e := PrepareLegacyBinding(context.Background(), path)
				if e != nil {
					b.Fatal(e)
				}
				a := mailbox.Account{ID: "invented-history-account", ProviderID: GraphProvider}
				v := reconcileLegacyFunc(func(_ context.Context, a mailbox.Account, p LegacyPreparation) (LegacyReconciliation, error) {
					proof := goodLegacyProof(a, p)
					missing := p.Live[0].N
					delete(proof.States, missing)
					now := time.Now().UTC()
					return LegacyReconciliation{Verification: proof, Unavailable: map[int64]string{missing: LegacyUnavailable}, Coverage: LegacyPrimaryMailbox, Started: now, Finished: now, Pages: 1, Observed: len(proof.States)}, nil
				})
				proof, e := v.ReconcileLegacy(context.Background(), a, p)
				if e != nil {
					b.Fatal(e)
				}
				r, e := ReviewLegacyReconciliation(a, p, proof)
				if e != nil {
					b.Fatal(e)
				}
				b.StartTimer()
				bound, e := ReconcileLegacy(context.Background(), path, a, p, r.Digest, v)
				if e != nil {
					b.Fatal(e)
				}
				if e = bound.Close(); e != nil {
					b.Fatal(e)
				}
			}
		})
	}
}

func BenchmarkLegacyCancellation(b *testing.B) {
	path := inventedHistory(b, 100000, 2)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		timer := time.AfterFunc(10*time.Millisecond, cancel)
		_, e := PrepareLegacyBinding(ctx, path)
		timer.Stop()
		cancel()
		if !errors.Is(e, context.Canceled) {
			b.Fatalf("cancellation did not stop preparation: %v", e)
		}
	}
}
