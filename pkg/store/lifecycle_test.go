package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

type processResult struct {
	Busy  bool
	Mode  string
	Error string
}

// This helper only accepts a caller-created, marked invented test directory.
func TestStoreProcessHelper(t *testing.T) {
	pos := -1
	for i, a := range os.Args {
		if a == "--" {
			pos = i
			break
		}
	}
	if pos < 0 {
		return
	}
	args := os.Args[pos+1:]
	if len(args) != 2 {
		t.Fatal("invalid helper arguments")
	}
	path := args[1]
	b, err := os.ReadFile(filepath.Join(filepath.Dir(path), ".invented-store-test"))
	if err != nil || string(b) != "invented example.org fixture\n" {
		t.Fatal("not a test-owned database")
	}
	result := processResult{}
	switch args[0] {
	case "sql-probe":
		var db *sql.DB
		db, err = sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
		if err == nil {
			defer db.Close()
			err = db.QueryRow("pragma locking_mode=exclusive").Scan(&result.Mode)
			if err == nil {
				err = db.QueryRow("pragma journal_mode=delete").Scan(&result.Mode)
			}
		}
	case "ordinary":
		var db *Store
		db, err = Open(path)
		if err == nil {
			_, err = db.Count(context.Background(), Filter{})
			err = errors.Join(err, db.Close())
			result.Mode = "ordinary"
		}
	case "life-ex":
		var life *lifecycleLease
		life, err = acquireLifecycle(context.Background(), path, true, diskFiles{})
		if err == nil {
			result.Mode = "exclusive"
			err = life.release()
		}
	case "hold-sh", "hold-ex":
		runHoldingHelper(t, path, args[0] == "hold-ex")
		return
	case "quarantine", "fstat-error":
		runQuarantineHelper(t, path, args[0] == "fstat-error")
		result.Mode = "quarantined"
	case "uncertain-close":
		runUncertainCloseHelper(t, path)
		result.Mode = "retained"
	case "crash-migration":
		runCrashMigration(t, path)
		t.Fatal("crash point not reached")
	default:
		t.Fatal("unknown helper operation")
	}
	if err != nil {
		var se *sqlite.Error
		if errors.Is(err, ErrLifecycleBusy) || (errors.As(err, &se) && (se.Code()&255 == 5 || se.Code()&255 == 6)) {
			result.Busy = true
		} else {
			result.Error = err.Error()
		}
	}

	if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func inventedPath(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".invented-store-test"), []byte("invented example.org fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "mail.db")
}
func sqlProbe(t *testing.T, path string) processResult {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreProcessHelper$", "--", "sql-probe", path)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v %s", err, b)
	}
	var r processResult
	if err = json.Unmarshal(b, &r); err != nil {
		t.Fatalf("helper protocol: %v %s", err, b)
	}
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	return r
}

func TestStoreLifecycleLock(t *testing.T) {
	t.Run("overlapping SQLite handles preserve actual locks", func(t *testing.T) {
		path := inventedPath(t)
		a, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		var version string
		if err = a.db.QueryRow("select sqlite_version()").Scan(&version); err != nil {
			t.Fatal(err)
		}
		t.Log("SQLite", version)
		if r := sqlProbe(t, path); !r.Busy {
			t.Fatalf("initial SQL lock not held: %+v", r)
		}
		b, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = b.Close(); err != nil {
			t.Fatal(err)
		}
		if r := sqlProbe(t, path); !r.Busy {
			t.Fatalf("second open/close released first user's SQLite locks: %+v", r)
		}
		alias := filepath.Join(filepath.Dir(path), "alias.db")
		if err = os.Symlink(path, alias); err != nil {
			t.Fatal(err)
		}
		b, err = Open(alias)
		if err != nil {
			t.Fatal(err)
		}
		b.Close()
		if r := sqlProbe(t, path); !r.Busy {
			t.Fatalf("alias close released actual locks: %+v", r)
		}
		if _, err = a.Upsert(context.Background(), "inbox", Record{GraphID: "invented-a", Sender: "sender@example.org"}); err != nil {
			t.Fatal(err)
		}
		if err = a.Close(); err != nil {
			t.Fatal(err)
		}
		if r := sqlProbe(t, path); r.Busy || !strings.EqualFold(r.Mode, "delete") {
			t.Fatalf("positive control after all closes: %+v", r)
		}
	})
	t.Run("lifecycle contention and cancellation", testLifecycleContention)
	t.Run("namespace refusals preserve SQLite locks", testLifecycleRefusals)
	t.Run("kernel crash release", testLifecycleCrashRelease)
	t.Run("isolated descriptor faults", testLifecycleFaults)
	t.Run("manager eviction", testLifecycleEviction)
	t.Run("canonical and filesystem aliases", testLifecycleAliases)
	t.Run("SQL drain before release", testLifecycleDrain)
	t.Run("concurrent initialization", testConcurrentCreation)

}

func helperCommand(mode, path string) *exec.Cmd {
	return exec.Command(os.Args[0], "-test.run=^TestStoreProcessHelper$", "--", mode, path)
}
func helperResult(t *testing.T, mode, path string) processResult {
	t.Helper()
	b, e := helperCommand(mode, path).CombinedOutput()
	if e != nil {
		t.Fatalf("%s helper %v %s", mode, e, b)
	}
	var r processResult
	if e = json.Unmarshal(b, &r); e != nil || r.Error != "" {
		t.Fatalf("%s protocol %+v %v %s", mode, r, e, b)
	}
	return r
}
func testLifecycleContention(t *testing.T) {
	ctx := context.Background()
	path := inventedPath(t)
	a, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	b, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if r := helperResult(t, "ordinary", path); r.Busy || r.Mode != "ordinary" {
		t.Fatalf("third ordinary session refused %+v", r)
	}
	if _, e = acquireLifecycle(ctx, path, true, diskFiles{}); !errors.Is(e, ErrLifecycleBusy) {
		t.Fatalf("local EX admitted: %v", e)
	}
	b.Close()
	if r := helperResult(t, "life-ex", path); !r.Busy {
		t.Fatalf("one close released another SH %+v", r)
	}
	a.Close()
	life, e := acquireLifecycle(ctx, path, true, diskFiles{})
	if e != nil {
		t.Fatal(e)
	}
	canceled, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, e = acquireLifecycle(canceled, path, false, diskFiles{}); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("SH cancellation %v", e)
	}
	life.release()
	a, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	if r := sqlProbe(t, path); !r.Busy {
		t.Fatalf("cancel cleanup released actual SQL locks %+v", r)
	}
}
func testLifecycleRefusals(t *testing.T) {
	path := inventedPath(t)
	a, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	hard := filepath.Join(filepath.Dir(path), "hard.db")
	if e = os.Link(path, hard); e != nil {
		t.Fatal(e)
	}
	if b, e := Open(hard); e == nil {
		b.Close()
		t.Fatal("hard-linked database accepted")
	}
	if _, e = a.Count(context.Background(), Filter{}); e == nil {
		t.Fatal("retained handle ignored changed namespace")
	}
	os.Remove(hard)
	for _, mode := range []string{"symlink", "hardlink", "permissions", "nonempty", "directory"} {
		target := filepath.Join(filepath.Dir(path), "refused-"+mode+".db")
		lock := target + lifecycleSuffix
		switch mode {
		case "symlink":
			e = os.Symlink(path, lock)
		case "hardlink":
			e = os.Link(path, lock)
		case "permissions":
			e = os.WriteFile(lock, nil, 0644)
		case "nonempty":
			e = os.WriteFile(lock, []byte("invented"), 0600)
		case "directory":
			e = os.Mkdir(lock, 0700)
		}
		if e != nil {
			t.Fatal(e)
		}
		if b, e := Open(target); e == nil {
			b.Close()
			t.Fatalf("unsafe %s lock accepted", mode)
		}
		if e = os.Remove(lock); e != nil {
			t.Fatal(e)
		}
		if r := sqlProbe(t, path); !r.Busy {
			t.Fatalf("%s refusal canceled actual SQLite locks %+v", mode, r)
		}
	}
	alias := filepath.Join(filepath.Dir(path), "dangling.db")
	if e = os.Symlink(filepath.Join(filepath.Dir(path), "absent.db"), alias); e != nil {
		t.Fatal(e)
	}
	if b, e := Open(alias); e == nil {
		b.Close()
		t.Fatal("dangling DB symlink accepted")
	}
	sideAlias := filepath.Join(filepath.Dir(path), "sidecar-alias.db")
	if e = os.Symlink(path+lifecycleSuffix, sideAlias); e != nil {
		t.Fatal(e)
	}
	if b, e := Open(sideAlias); e == nil {
		b.Close()
		t.Fatal("sidecar symlink admitted as database")
	}
	info, e := os.Stat(path + lifecycleSuffix)
	if e != nil || info.Size() != 0 {
		t.Fatal("sidecar was changed into a database")
	}
	var integrityResult string
	if e = a.db.QueryRow("pragma integrity_check").Scan(&integrityResult); e != nil || integrityResult != "ok" {
		t.Fatalf("damaged source %v", e)
	}
}
func runHoldingHelper(t *testing.T, path string, exclusive bool) {
	t.Helper()
	life, e := acquireLifecycle(context.Background(), path, exclusive, diskFiles{})
	if e != nil {
		t.Fatal(e)
	}
	defer life.release()
	db, e := sqliteOpen(context.Background(), path)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var n int
	if e = db.QueryRow("select count(*) from messages").Scan(&n); e != nil {
		t.Fatal(e)
	}
	if e = json.NewEncoder(os.Stdout).Encode(processResult{Mode: "held"}); e != nil {
		t.Fatal(e)
	}
	var b [1]byte
	_, _ = os.Stdin.Read(b[:])
}
func testLifecycleCrashRelease(t *testing.T) {
	for _, mode := range []string{"hold-sh", "hold-ex"} {
		t.Run(mode, func(t *testing.T) {
			path := inventedPath(t)
			s, e := Open(path)
			if e != nil {
				t.Fatal(e)
			}
			s.Close()
			before, e := os.Stat(path + lifecycleSuffix)
			if e != nil {
				t.Fatal(e)
			}
			cmd := helperCommand(mode, path)
			in, e := cmd.StdinPipe()
			if e != nil {
				t.Fatal(e)
			}
			out, e := cmd.StdoutPipe()
			if e != nil {
				t.Fatal(e)
			}
			if e = cmd.Start(); e != nil {
				t.Fatal(e)
			}
			defer func() { in.Close(); cmd.Process.Kill(); cmd.Wait() }()
			var r processResult
			if e = json.NewDecoder(out).Decode(&r); e != nil || r.Mode != "held" {
				t.Fatalf("holder startup %v %+v", e, r)
			}
			if mode == "hold-sh" {
				if r = helperResult(t, "life-ex", path); !r.Busy {
					t.Fatal("EX not excluded")
				}
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
				defer cancel()
				if _, e = acquireLifecycle(ctx, path, false, diskFiles{}); !errors.Is(e, context.DeadlineExceeded) {
					t.Fatalf("SH not excluded by EX %v", e)
				}
			}
			if e = cmd.Process.Kill(); e != nil {
				t.Fatal(e)
			}
			cmd.Wait()
			in.Close()
			life, e := acquireLifecycle(context.Background(), path, true, diskFiles{})
			if e != nil {
				t.Fatal(e)
			}
			life.release()
			after, e := os.Stat(path + lifecycleSuffix)
			if e != nil || !os.SameFile(before, after) {
				t.Fatal("crash recovery replaced/unlinked sidecar")
			}
		})
	}
}

type raceFiles struct {
	diskFiles
	source, lockPath string
	candidate        int
	closes           int
	failStat         bool
}

func (f *raceFiles) openat(dir int, name string, flags int, mode uint32) (int, error) {
	if name == filepath.Base(f.lockPath) {
		if !f.failStat {
			if e := os.Remove(f.lockPath); e != nil {
				return -1, e
			}
			if e := os.Link(f.source, f.lockPath); e != nil {
				return -1, e
			}
		}
		fd, e := f.diskFiles.openat(dir, name, flags, mode)
		if e == nil {
			f.candidate = fd
		}
		return fd, e
	}
	return f.diskFiles.openat(dir, name, flags, mode)
}
func (f *raceFiles) fstat(fd int) (fileStat, error) {
	if f.failStat && fd == f.candidate {
		var s unix.Stat_t
		e := unix.Fstat(-1, &s)
		return fileStat{}, e
	}
	return f.diskFiles.fstat(fd)
}
func (f *raceFiles) close(fd int) error {
	if fd == f.candidate {
		f.closes++
	}
	return f.diskFiles.close(fd)
}
func runQuarantineHelper(t *testing.T, path string, failStat bool) {
	a, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	target := filepath.Join(filepath.Dir(path), "candidate.db")
	lock := target + lifecycleSuffix
	if e = os.WriteFile(lock, nil, 0600); e != nil {
		t.Fatal(e)
	}
	fs := &raceFiles{source: path, lockPath: lock, candidate: -1, failStat: failStat}
	if lease, e := acquireLifecycle(context.Background(), target, false, fs); e == nil {
		lease.release()
		t.Fatal("post-open namespace/fstat fault accepted")
	}
	if fs.candidate < 0 || fs.closes != 0 {
		t.Fatalf("uncertain FD closed: fd=%d closes=%d", fs.candidate, fs.closes)
	}
	if e = os.Remove(lock); e != nil {
		t.Fatal(e)
	}
	runtime.GC()
	if r := sqlProbe(t, path); !r.Busy {
		t.Fatalf("quarantine canceled actual SQL locks %+v", r)
	}
	if b, e := Open(path); e == nil {
		b.Close()
		t.Fatal("poisoned process admitted storage")
	}
}

type closeFailureDriver struct{ calls *atomic.Int32 }

func (d closeFailureDriver) Open(name string) (driver.Conn, error) {
	c, e := (&sqlite.Driver{}).Open(name)
	if e != nil {
		return nil, e
	}
	return &closeFailureConn{Conn: c, calls: d.calls}, nil
}

type closeFailureConn struct {
	driver.Conn
	calls *atomic.Int32
}

func (c *closeFailureConn) Close() error {
	c.calls.Add(1)
	return errors.New("invented uncertain driver close")
}
func runUncertainCloseHelper(t *testing.T, path string) {
	a, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.db.Close(); e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int32
	sql.Register("invented-close-failure", closeFailureDriver{calls: &calls})
	db, e := sql.Open("invented-close-failure", sqliteURI(path, "rw", false))
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	a.db = db
	var count int
	if e = db.QueryRow("select count(*) from messages").Scan(&count); e != nil {
		t.Fatal(e)
	}
	if e = a.Close(); e == nil {
		t.Fatal("uncertain SQL close succeeded")
	}
	a.Close()
	if calls.Load() != 1 {
		t.Fatalf("repeated driver Close %d", calls.Load())
	}
	if r := sqlProbe(t, path); !r.Busy {
		t.Fatalf("uncertain SQL close released actual locks %+v", r)
	}
	if r := helperResult(t, "life-ex", path); !r.Busy {
		t.Fatal("uncertain close released lifecycle lease")
	}
	if b, e := Open(path); e == nil {
		b.Close()
		t.Fatal("uncertain close admitted new storage")
	}
}
func testLifecycleFaults(t *testing.T) {
	for _, mode := range []string{"quarantine", "fstat-error", "uncertain-close"} {
		t.Run(mode, func(t *testing.T) {
			r := helperResult(t, mode, inventedPath(t))
			if r.Mode != "quarantined" && r.Mode != "retained" {
				t.Fatalf("fault not exercised %+v", r)
			}
		})
	}
}
func testLifecycleEviction(t *testing.T) {
	lifecycleManager.Lock()
	baseline := len(lifecycleManager.entries)
	lifecycleManager.Unlock()
	for i := 0; i < 8; i++ {
		path := inventedPath(t)
		a, e := Open(path)
		if e != nil {
			t.Fatal(e)
		}
		b, e := Open(path)
		if e != nil {
			t.Fatal(e)
		}
		a.Close()
		b.Close()
		b.Close()
	}
	lifecycleManager.Lock()
	defer lifecycleManager.Unlock()
	if len(lifecycleManager.entries) != baseline {
		t.Fatalf("idle lifecycle map retained entries: %d -> %d", baseline, len(lifecycleManager.entries))
	}
}

type crashSQL struct{ sqlAccess }

func (c crashSQL) ExecContext(ctx context.Context, stmt string, args ...any) (sql.Result, error) {
	r, e := c.sqlAccess.ExecContext(ctx, stmt, args...)
	if e == nil && strings.EqualFold(strings.TrimSpace(stmt), "drop table messages") {
		os.Exit(86)
	}
	return r, e
}
func runCrashMigration(t *testing.T, path string) {
	life, e := acquireLifecycle(context.Background(), path, true, diskFiles{})
	if e != nil {
		t.Fatal(e)
	}
	defer life.release()
	db, e := sqliteOpen(context.Background(), path)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	conn, e := db.Conn(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	if e = migrateSchema(context.Background(), crashSQL{conn}, ""); e != nil {
		t.Fatal(e)
	}
}
func TestMigrationCrashRecovery(t *testing.T) {
	path := frozenLegacy(t)
	ctx := context.Background()
	db := rawFixtureDB(t, path)
	before, e := logicalDigest(ctx, db)
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	e = helperCommand("crash-migration", path).Run()
	var exit *exec.ExitError
	if !errors.As(e, &exit) || exit.ExitCode() != 86 {
		t.Fatalf("crash point not exercised %v", e)
	}
	db = rawFixtureDB(t, path)
	after, e := logicalDigest(ctx, db)
	if e != nil || before != after {
		t.Fatalf("SQLite crash recovery changed history %v", e)
	}
}

func testLifecycleAliases(t *testing.T) {
	root := filepath.Dir(inventedPath(t))
	target := filepath.Join(root, "target")
	if e := os.MkdirAll(filepath.Join(target, "child"), 0700); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(target, "mail.db")
	a, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	link := filepath.Join(root, "link")
	if e = os.Symlink(filepath.Join(target, "child"), link); e != nil {
		t.Fatal(e)
	}
	b, e := Open(link + "/../mail.db")
	if e != nil {
		t.Fatal(e)
	}
	if b.life.entry != a.life.entry {
		b.Close()
		t.Fatal("dot-dot was collapsed before resolving its symlink parent")
	}
	b.Close()
	cwd, e := os.Getwd()
	if e != nil {
		t.Fatal(e)
	}
	relative, e := filepath.Rel(cwd, path)
	if e != nil {
		t.Fatal(e)
	}
	b, e = Open(relative)
	if e != nil {
		t.Fatal(e)
	}
	if b.life.entry != a.life.entry {
		t.Fatal("relative alias split lifecycle identity")
	}
	b.Close()
	for _, pair := range [][2]string{{"Case.db", "case.db"}, {"\u00e9.db", "e\u0301.db"}} {
		first := filepath.Join(root, pair[0])
		second := filepath.Join(root, pair[1])
		left, e := Open(first)
		if e != nil {
			t.Fatal(e)
		}
		leftInfo, e := os.Stat(first)
		if e != nil {
			t.Fatal(e)
		}
		rightInfo, statErr := os.Stat(second)
		same := statErr == nil && os.SameFile(leftInfo, rightInfo)
		right, e := Open(second)
		if e != nil {
			t.Fatal(e)
		}
		if (left.life.entry == right.life.entry) != same {
			t.Fatal("filesystem alias identity does not match lifecycle mapping")
		}
		right.Close()
		left.Close()
	}
	weird := filepath.Join(root, "mail ?#%.db")
	c, e := Open(weird)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.Upsert(context.Background(), "inbox", Record{GraphID: "uri-id", Sender: "sender@example.org"}); e != nil {
		t.Fatal(e)
	}
	c.Close()
	c, e = Open(weird)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if count, e := c.Count(context.Background(), Filter{}); e != nil || count != 1 {
		t.Fatalf("URI filename routed elsewhere %d %v", count, e)
	}
}
func testLifecycleDrain(t *testing.T) {
	path := inventedPath(t)
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	done, e := s.legacy(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	tx, e := s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	var n int
	if e = tx.QueryRow("select count(*) from messages").Scan(&n); e != nil {
		t.Fatal(e)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case e = <-closed:
		t.Fatalf("Close passed an active SQL operation %v", e)
	case <-time.After(20 * time.Millisecond):
	}
	if r := helperResult(t, "life-ex", path); !r.Busy {
		t.Fatal("SQL drain released sidecar early")
	}
	tx.Rollback()
	done()
	if e = <-closed; e != nil {
		t.Fatal(e)
	}
	if r := helperResult(t, "life-ex", path); r.Busy {
		t.Fatal("drained SQL retained lifecycle lock")
	}
}
func testConcurrentCreation(t *testing.T) {
	path := inventedPath(t)
	commands := []*exec.Cmd{helperCommand("ordinary", path), helperCommand("ordinary", path)}
	results := make(chan processResult, 2)
	failures := make(chan error, 2)
	for _, cmd := range commands {
		go func(cmd *exec.Cmd) {
			b, e := cmd.CombinedOutput()
			if e != nil {
				failures <- errors.New(string(b))
				return
			}
			var r processResult
			e = json.Unmarshal(b, &r)
			if e != nil {
				failures <- e
				return
			}
			results <- r
		}(cmd)
	}
	success := 0
	for i := 0; i < 2; i++ {
		select {
		case e := <-failures:
			t.Fatal(e)
		case r := <-results:
			if r.Error != "" {
				t.Fatal(r.Error)
			}
			if !r.Busy {
				success++
			}
		case <-time.After(10 * time.Second):
			t.Fatal("creation did not finish")
		}
	}
	if success == 0 {
		t.Fatal("neither creator initialized storage")
	}
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	for p, want := range map[string]os.FileMode{path: 0600, path + lifecycleSuffix: 0600, filepath.Dir(path): 0700} {
		info, e := os.Stat(p)
		if e != nil || info.Mode().Perm() != want {
			t.Fatalf("creation mode %s %v", p, e)
		}
	}
}
