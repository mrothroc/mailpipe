package store

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Failures are actual SQLite errors after prior migration statements have run.
// No fault behavior or callback is compiled into production.
type failSQL struct {
	sqlAccess
	needle string
	fired  bool
}

func (f *failSQL) ExecContext(ctx context.Context, stmt string, args ...any) (sql.Result, error) {
	if !f.fired && strings.Contains(strings.ToLower(stmt), f.needle) {
		f.fired = true
		return f.sqlAccess.ExecContext(ctx, "insert into absent_invented_failure_table values(1)")
	}
	return f.sqlAccess.ExecContext(ctx, stmt, args...)
}
func rawFixtureDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, e := sql.Open("sqlite", sqliteURI(path, "rw", false))
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}
func testMigrationRollback(t *testing.T) {
	for _, needle := range []string{"create table messages_replacement", "insert into messages_replacement", "drop table messages", "alter table messages_replacement", "create unique index", "alter table plans", "create table if not exists store_binding", "pragma user_version=", "commit"} {
		t.Run(needle, func(t *testing.T) {
			ctx := context.Background()
			path := frozenLegacy(t)
			life, e := acquireLifecycle(ctx, path, true, diskFiles{})
			if e != nil {
				t.Fatal(e)
			}
			defer life.release()
			db := rawFixtureDB(t, path)
			defer db.Close()
			conn, e := db.Conn(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer conn.Close()
			before, e := logicalDigest(ctx, conn)
			if e != nil {
				t.Fatal(e)
			}
			snapshot, digest, e := backupDatabase(ctx, life, conn)
			if e != nil {
				t.Fatal(e)
			}
			if digest != before {
				t.Fatal("backup differs")
			}
			f := &failSQL{sqlAccess: conn, needle: needle}
			if e = migrateSchema(ctx, f, digest); e == nil || !f.fired {
				t.Fatalf("failure not exercised: %v fired=%v", e, f.fired)
			}
			after, e := logicalDigest(ctx, conn)
			if e != nil || after != before {
				t.Fatalf("partial migration: %s %s %v", before, after, e)
			}
			var fk int
			if e = conn.QueryRowContext(ctx, "pragma foreign_keys").Scan(&fk); e != nil || fk != 1 {
				t.Fatalf("FK not restored %d %v", fk, e)
			}
			restored := restoreInvented(t, life, snapshot, filepath.Join(filepath.Dir(path), "restored.db"))
			if restored != before {
				t.Fatalf("restore differs %s %s", before, restored)
			}
		})
	}
}

// The helper only restores a quiescent, test-owned snapshot to a new temp path.
// It never operates on configuration paths or a live source inode.
func restoreInvented(t *testing.T, source *lifecycleLease, snapshot, destination string) string {
	t.Helper()
	if !source.entry.exclusive {
		t.Fatal("restore source is not exclusive")
	}
	if b, e := os.ReadFile(filepath.Join(filepath.Dir(source.entry.path), ".invented-store-test")); e != nil || string(b) != "invented example.org fixture\n" {
		t.Fatal("restore is not test owned")
	}
	ctx := context.Background()
	target, e := acquireLifecycle(ctx, destination, true, diskFiles{})
	if e != nil {
		t.Fatal(e)
	}
	defer target.release()
	input, e := os.Open(snapshot)
	if e != nil {
		t.Fatal(e)
	}
	defer input.Close()
	out, e := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		t.Fatal(e)
	}
	_, copyErr := io.Copy(out, input)
	e = errors.Join(copyErr, out.Sync(), out.Close())
	if e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadDir(filepath.Dir(destination))
	if e != nil {
		t.Fatal(e)
	}
	digest, e := offlineSnapshot(ctx, destination, target)
	if e != nil {
		t.Fatal(e)
	}
	after, e := os.ReadDir(filepath.Dir(destination))
	if e != nil || len(before) != len(after) {
		t.Fatalf("verification wrote directory: %v", e)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, e = os.Lstat(destination + suffix); !errors.Is(e, os.ErrNotExist) {
			t.Fatalf("verification created %s", suffix)
		}
	}
	return digest
}
func testMigrationRefusesUnknownSchema(t *testing.T) {
	for name, change := range map[string]string{"future": "pragma user_version=999", "trigger": "create trigger invented_trigger after insert on messages begin select 1; end", "index": "create index invented_extension on messages(subject)", "column": "alter table messages add column invented_extension text"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := frozenLegacy(t)
			db := rawFixtureDB(t, path)
			if _, e := db.Exec(change); e != nil {
				t.Fatal(e)
			}
			before, e := logicalDigest(ctx, db)
			if e != nil {
				t.Fatal(e)
			}
			db.Close()
			s, e := Open(path)
			if e == nil {
				s.Close()
				t.Fatal("unknown schema accepted")
			}
			db = rawFixtureDB(t, path)
			after, e := logicalDigest(ctx, db)
			if e != nil || before != after {
				t.Fatalf("refusal changed data %v", e)
			}
		})
	}
}
func testMigrationPreservesAllHistoricalColumns(t *testing.T) {
	ctx := context.Background()
	path := frozenLegacy(t)
	db := rawFixtureDB(t, path)
	specs, e := tableSpecs(ctx, db)
	if e != nil {
		t.Fatal(e)
	}
	before, e := dataDigest(ctx, db, specs)
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	after, e := dataDigest(ctx, s.db, specs)
	if e != nil || after != before {
		t.Fatalf("historical values differ %v", e)
	}
	var nullable int
	rows, e := s.db.Query("pragma table_info(messages)")
	if e != nil {
		t.Fatal(e)
	}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def any
		if e = rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); e != nil {
			t.Fatal(e)
		}
		if name == "graph_id" {
			nullable = 1 - notnull
		}
	}
	rows.Close()
	if nullable != 1 {
		t.Fatal("graph_id remains NOT NULL")
	}
	s.Close()
	backups, _ := filepath.Glob(path + ".backup-*")
	if len(backups) != 1 {
		t.Fatalf("backup count %v", backups)
	}
	info, e := os.Stat(filepath.Join(backups[0], "snapshot.db"))
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("backup mode %v", e)
	}
	info, e = os.Stat(backups[0])
	if e != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("backup directory mode %v", e)
	}
}
func TestFreshMainRefusesRecoverySet(t *testing.T) {
	for _, empty := range []bool{false, true} {
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			t.Run(suffix+map[bool]string{false: "missing", true: "empty"}[empty], func(t *testing.T) {
				path := inventedPath(t)
				if empty {
					if e := os.WriteFile(path, nil, 0600); e != nil {
						t.Fatal(e)
					}
				}
				data := []byte("invented recovery bytes")
				if e := os.WriteFile(path+suffix, data, 0600); e != nil {
					t.Fatal(e)
				}
				if s, e := Open(path); e == nil {
					s.Close()
					t.Fatal("recovery set accepted")
				}
				b, e := os.ReadFile(path + suffix)
				if e != nil || string(b) != string(data) {
					t.Fatal("recovery bytes changed")
				}
				if !empty {
					if _, e = os.Stat(path); !errors.Is(e, os.ErrNotExist) {
						t.Fatal("main created beside orphan journals")
					}
				}
			})
		}
	}
}
func TestDigestCancellationAndLargeValues(t *testing.T) {
	ctx := context.Background()
	path := frozenLegacy(t)
	db := rawFixtureDB(t, path)
	if _, e := db.Exec("update messages set body=? where n=7", strings.Repeat("invented example.org ", 100000)); e != nil {
		t.Fatal(e)
	}
	first, e := logicalDigest(ctx, db)
	if e != nil {
		t.Fatal(e)
	}
	second, e := logicalDigest(ctx, db)
	if e != nil || first != second {
		t.Fatal("digest not deterministic")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = logicalDigest(canceled, db); !errors.Is(e, context.Canceled) {
		t.Fatalf("digest cancellation %v", e)
	}
	if _, e = db.Exec("update messages set body=NULL where n=7"); e != nil {
		t.Fatal(e)
	}
	nullDigest, e := logicalDigest(ctx, db)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("update messages set body='' where n=7"); e != nil {
		t.Fatal(e)
	}
	emptyDigest, e := logicalDigest(ctx, db)
	if e != nil || nullDigest == emptyDigest {
		t.Fatal("NULL and empty conflated")
	}
}
