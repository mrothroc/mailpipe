package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"hash"
	"strings"
	"testing"
)

func referenceDigestRows(ctx context.Context, q sqlAccess, h hash.Hash, tables []tableSpec) error {
	for _, t := range tables {
		hashPart(h, t)
		var last int64
		first := true
		for {
			if e := ctx.Err(); e != nil {
				return e
			}
			var row int64
			var e error
			if first {
				e = q.QueryRowContext(ctx, "select rowid from "+quote(t.Name)+" order by rowid limit 1").Scan(&row)
				first = false
			} else {
				e = q.QueryRowContext(ctx, "select rowid from "+quote(t.Name)+" where rowid>? order by rowid limit 1", last).Scan(&row)
			}
			if errors.Is(e, sql.ErrNoRows) {
				break
			}
			if e != nil {
				return e
			}
			last = row
			hashPart(h, "row")
			for _, col := range t.Columns {
				var typ string
				var length int64
				e = q.QueryRowContext(ctx, "select typeof("+quote(col)+"),coalesce(length(cast("+quote(col)+" as blob)),0) from "+quote(t.Name)+" where rowid=?", row).Scan(&typ, &length)
				if e != nil {
					return e
				}
				hashPart(h, typ)
				hashPart(h, length)
				for offset := int64(1); offset <= length; offset += 32768 {
					if e = ctx.Err(); e != nil {
						return e
					}
					var chunk []byte
					e = q.QueryRowContext(ctx, "select substr(cast("+quote(col)+" as blob),?,32768) from "+quote(t.Name)+" where rowid=?", offset, row).Scan(&chunk)
					if e != nil {
						return e
					}
					h.Write(chunk)
				}
			}
		}
		hashPart(h, "end")
	}
	return nil
}

func TestDigestStatementReuseMatchesReference(t *testing.T) {
	ctx := context.Background()
	path := frozenLegacy(t)
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.db.Exec("update messages set body=?,preview=NULL where n=7", strings.Repeat("a\x00☃", 20000)); e != nil {
		t.Fatal(e)
	}
	specs, e := tableSpecs(ctx, s.db)
	if e != nil {
		t.Fatal(e)
	}
	expected := sha256.New()
	if e = referenceDigestRows(ctx, s.db, expected, specs); e != nil {
		t.Fatal(e)
	}
	actual := sha256.New()
	if e = digestRows(ctx, s.db, actual, specs); e != nil {
		t.Fatal(e)
	}
	if fmt.Sprintf("%x", actual.Sum(nil)) != fmt.Sprintf("%x", expected.Sum(nil)) {
		t.Fatal("prepared digest changed the reference byte stream")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if e = digestRows(canceled, s.db, sha256.New(), specs); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancellation: %v", e)
	}
}

func TestDigestStatementReusePreservesTypedSparseValues(t *testing.T) {
	db, e := sql.Open("sqlite", ":memory:")
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, e = db.Exec("create table typed(value)"); e != nil {
		t.Fatal(e)
	}
	for i, value := range []any{nil, "", int64(-12), 1.25, []byte{0, 255, 0, 128}, strings.Repeat("☃\x00", 20000)} {
		if _, e = db.Exec("insert into typed(rowid,value) values(?,?)", int64(i*i*17-7), value); e != nil {
			t.Fatal(e)
		}
	}
	ctx := context.Background()
	specs, e := tableSpecs(ctx, db)
	if e != nil {
		t.Fatal(e)
	}
	a, b := sha256.New(), sha256.New()
	if e = referenceDigestRows(ctx, db, a, specs); e != nil {
		t.Fatal(e)
	}
	if e = digestRows(ctx, db, b, specs); e != nil {
		t.Fatal(e)
	}
	if fmt.Sprintf("%x", a.Sum(nil)) != fmt.Sprintf("%x", b.Sum(nil)) {
		t.Fatal("type/NULL/sparse-row digest changed")
	}
}

type failedDigestPreparer struct {
	sqlAccess
	failure error
}

func (f failedDigestPreparer) PrepareContext(context.Context, string) (*sql.Stmt, error) {
	return nil, f.failure
}
func TestDigestPreparationFailureDoesNotChangeHistory(t *testing.T) {
	ctx := context.Background()
	path := frozenLegacy(t)
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	before, e := logicalDigest(ctx, s.db)
	if e != nil {
		t.Fatal(e)
	}
	failure := errors.New("invented preparation failure")
	if _, e = logicalDigest(ctx, failedDigestPreparer{sqlAccess: s.db, failure: failure}); !errors.Is(e, failure) {
		t.Fatalf("preparation failure lost: %v", e)
	}
	after, e := logicalDigest(ctx, s.db)
	if e != nil || after != before {
		t.Fatalf("failed digest changed history: %v", e)
	}
}
