package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

type descriptorEvent struct {
	path   string
	flags  int
	closed bool
}
type traceFiles struct {
	diskFiles
	events      []*descriptorEvent
	active      map[int]*descriptorEvent
	directories map[int]string
}

func (f *traceFiles) openat(dir int, name string, flags int, mode uint32) (int, error) {
	fd, e := f.diskFiles.openat(dir, name, flags, mode)
	if e != nil {
		return fd, e
	}
	path := name
	if !filepath.IsAbs(name) {
		path = filepath.Join(f.directories[dir], name)
	}
	event := &descriptorEvent{path: path, flags: flags}
	f.events = append(f.events, event)
	f.active[fd] = event
	if flags&unix.O_DIRECTORY != 0 {
		f.directories[fd] = path
	}
	return fd, nil
}
func (f *traceFiles) close(fd int) error {
	if event := f.active[fd]; event != nil {
		event.closed = true
		delete(f.active, fd)
	}
	delete(f.directories, fd)
	return f.diskFiles.close(fd)
}
func newTraceFiles() *traceFiles {
	return &traceFiles{active: map[int]*descriptorEvent{}, directories: map[int]string{}}
}

type vacuumOrder struct {
	sqlAccess
	t      *testing.T
	files  *traceFiles
	source string
	saw    bool
}

func (v *vacuumOrder) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.EqualFold(strings.TrimSpace(query), "vacuum into ?") {
		v.saw = true
		target := args[0].(string)
		bootstrap := false
		for _, event := range v.files.events {
			if event.path == target && event.flags&unix.O_EXCL != 0 {
				bootstrap = true
				if !event.closed {
					v.t.Fatal("VACUUM opened a still-open raw bootstrap descriptor")
				}
			}
		}
		if !bootstrap {
			v.t.Fatal("backup bootstrap ordering was not exercised")
		}
		if e := os.WriteFile(filepath.Join(filepath.Dir(target), ".invented-store-test"), []byte("invented example.org fixture\n"), 0600); e != nil {
			v.t.Fatal(e)
		}
		if r := helperResult(v.t, "life-ex", target); !r.Busy {
			v.t.Fatal("backup target was not exclusively owned")
		}
		if r := sqlProbe(v.t, v.source); !r.Busy {
			v.t.Fatal("backup bootstrap canceled source SQL locks")
		}
	}
	return v.sqlAccess.ExecContext(ctx, query, args...)
}
func TestDescriptorOrderingAndBackup(t *testing.T) {
	ctx := context.Background()
	path := inventedPath(t)
	fs := newTraceFiles()
	life, e := acquireLifecycle(ctx, path, true, fs)
	if e != nil {
		t.Fatal(e)
	}
	if e = life.createMain(); e != nil {
		t.Fatal(e)
	}
	bootstrap := false
	for _, event := range fs.events {
		if event.path == life.entry.path {
			bootstrap = true
			if !event.closed || event.flags&unix.O_EXCL == 0 {
				t.Fatal("main bootstrap still open before SQLite admission")
			}
		}
	}
	if !bootstrap {
		t.Fatal("fresh main creation not exercised")
	}
	if e = maintain(ctx, life, nil); e != nil {
		t.Fatal(e)
	}
	if e = life.release(); e != nil {
		t.Fatal(e)
	}
	fs = newTraceFiles()
	life, e = acquireLifecycle(ctx, path, true, fs)
	if e != nil {
		t.Fatal(e)
	}
	defer life.release()
	db, e := sqliteOpen(ctx, life.entry.path)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	conn, e := db.Conn(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	if _, e = conn.ExecContext(ctx, "insert into messages(graph_id,folder,sender) values('wal-data','inbox','sender@example.org')"); e != nil {
		t.Fatal(e)
	}
	wrapped := &vacuumOrder{sqlAccess: conn, t: t, files: fs, source: path}
	snapshot, digest, e := backupDatabase(ctx, life, wrapped)
	if e != nil {
		t.Fatal(e)
	}
	if !wrapped.saw || digest == "" {
		t.Fatal("VACUUM backup not exercised")
	}
	if r := sqlProbe(t, path); !r.Busy {
		t.Fatalf("backup closing canceled source SQL locks %+v", r)
	}
	if _, e = os.Stat(filepath.Join(filepath.Dir(snapshot), "receipt.json")); e != nil {
		t.Fatal(e)
	}
	for _, event := range fs.events {
		if event.path == life.entry.path {
			t.Fatal("raw existing source descriptor opened")
		}
		if event.flags&unix.O_CLOEXEC == 0 {
			t.Fatalf("raw descriptor can be inherited: %s", event.path)
		}
	}
}

type canceledSQL struct {
	sqlAccess
	cancel context.CancelFunc
	fired  bool
}

func (c *canceledSQL) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	r, e := c.sqlAccess.ExecContext(ctx, query, args...)
	if e == nil && strings.TrimSpace(query) == "drop table messages" {
		c.fired = true
		c.cancel()
	}
	return r, e
}
func TestMigrationMidTransactionCancellation(t *testing.T) {
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
	canceled, cancel := context.WithCancel(ctx)
	defer cancel()
	wrapper := &canceledSQL{sqlAccess: conn, cancel: cancel}
	if e = migrateSchema(canceled, wrapper, ""); !wrapper.fired || !errors.Is(e, context.Canceled) {
		t.Fatalf("mid-transaction cancellation not exercised %v", e)
	}
	after, e := logicalDigest(ctx, conn)
	if e != nil || before != after {
		t.Fatalf("canceled migration changed schema/data %v", e)
	}
	var fk int
	if e = conn.QueryRowContext(ctx, "pragma foreign_keys").Scan(&fk); e != nil || fk != 1 {
		t.Fatalf("canceled migration left FK off %v", e)
	}
}

type errorFiles struct {
	diskFiles
	lockError error
	syncError error
}

func (f errorFiles) flock(fd, how int) error {
	if f.lockError != nil {
		return f.lockError
	}
	return f.diskFiles.flock(fd, how)
}
func (f errorFiles) sync(fd int) error {
	if f.syncError != nil {
		return f.syncError
	}
	return f.diskFiles.sync(fd)
}
func TestLifecycleErrorClassification(t *testing.T) {
	for _, fs := range []errorFiles{{lockError: unix.EIO}, {syncError: unix.ENOSPC}} {
		path := inventedPath(t)
		if l, e := acquireLifecycle(context.Background(), path, true, fs); e == nil {
			l.release()
			t.Fatal("filesystem failure accepted")
		} else if errors.Is(e, ErrLifecycleBusy) || (!errors.Is(e, unix.EIO) && !errors.Is(e, unix.ENOSPC)) {
			t.Fatalf("filesystem failure classified as contention: %v", e)
		}
	}
	path := inventedPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s, e := OpenContext(ctx, path); !errors.Is(e, context.Canceled) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("canceled opener %v", e)
	}
}
