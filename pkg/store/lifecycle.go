package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// ErrLifecycleBusy means maintenance conflicts with an ordinary session. It is
// distinct from a SQLite transaction's busy error and from filesystem failure.
var ErrLifecycleBusy = errors.New("store lifecycle busy")

// LifecycleBusyError requires the other sessions to finish before maintenance.
type LifecycleBusyError struct{ Operation string }

func (e *LifecycleBusyError) Error() string {
	return "store: " + e.Operation + ": other sessions are active; close them before maintenance"
}
func (e *LifecycleBusyError) Unwrap() error { return ErrLifecycleBusy }

type storageError struct {
	op    string
	cause error
}

func (e *storageError) Error() string {
	return "store: " + e.op + " failed; check private storage or reconcile and restart"
}
func (e *storageError) Unwrap() error { return e.cause }
func storageErr(op string, e error) error {
	if e == nil {
		return nil
	}
	return &storageError{op, e}
}
func unsafeNamespace() error {
	return errors.New("store: unsafe or changed storage namespace; reconcile before reopening")
}

type inode struct{ dev, ino uint64 }
type fileStat struct {
	id    inode
	mode  uint32
	uid   uint32
	links uint64
	size  int64
}

func statValue(s unix.Stat_t) fileStat {
	return fileStat{inode{uint64(s.Dev), uint64(s.Ino)}, uint32(s.Mode), s.Uid, uint64(s.Nlink), s.Size}
}
func (s fileStat) regular() bool { return s.mode&unix.S_IFMT == unix.S_IFREG }

// Narrow filesystem operations permit test-only wrappers without production
// fault callbacks, environment switches, or exported raw descriptors.
type fileSystem interface {
	stat(string, bool) (fileStat, error)
	openat(int, string, int, uint32) (int, error)
	fstat(int) (fileStat, error)
	close(int) error
	sync(int) error
	write(int, []byte) (int, error)
	flock(int, int) error
}
type diskFiles struct{}

func (diskFiles) stat(p string, follow bool) (fileStat, error) {
	var s unix.Stat_t
	var e error
	if follow {
		e = unix.Stat(p, &s)
	} else {
		e = unix.Lstat(p, &s)
	}
	return statValue(s), e
}
func (diskFiles) openat(d int, p string, f int, m uint32) (int, error) {
	return unix.Openat(d, p, f, m)
}
func (diskFiles) fstat(fd int) (fileStat, error) {
	var s unix.Stat_t
	e := unix.Fstat(fd, &s)
	return statValue(s), e
}
func (diskFiles) close(fd int) error                  { return unix.Close(fd) }
func (diskFiles) sync(fd int) error                   { return unix.Fsync(fd) }
func (diskFiles) write(fd int, b []byte) (int, error) { return unix.Write(fd, b) }
func (diskFiles) flock(fd, how int) error             { return unix.Flock(fd, how) }

const lifecycleSuffix = ".mailpipe-lifecycle.lock"

type lifecycleEntry struct {
	path      string
	lock, db  inode
	fd, dir   int
	parent    inode
	refs      int
	exclusive bool
	fs        fileSystem
}
type lifecycleLease struct {
	entry    *lifecycleEntry
	once     sync.Once
	err      error
	retained bool
}

var lifecycleManager = struct {
	sync.Mutex
	entries    map[inode]*lifecycleEntry
	poison     error
	quarantine int
}{entries: make(map[inode]*lifecycleEntry), quarantine: -1}

func canonicalDatabase(path string) (string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return "", unsafeNamespace()
	}
	raw := path
	if !filepath.IsAbs(raw) {
		cwd, e := os.Getwd()
		if e != nil {
			return "", storageErr("database path", e)
		}
		raw = cwd + string(os.PathSeparator) + raw
	}
	var p string
	if _, e := os.Lstat(raw); e == nil {
		resolved, e := filepath.EvalSymlinks(raw)
		if e != nil {
			return "", unsafeNamespace()
		}
		p = resolved
	} else if !errors.Is(e, os.ErrNotExist) {
		return "", storageErr("database metadata", e)
	} else {
		// Do not Clean/Dir the raw path before following existing symlinks: link/..
		// is interpreted relative to the symlink target by the filesystem.
		split := strings.LastIndexByte(raw, byte(os.PathSeparator))
		if split < 0 || split == len(raw)-1 {
			return "", unsafeNamespace()
		}
		parentRaw, base := raw[:split], raw[split+1:]
		if split == 0 {
			parentRaw = string(os.PathSeparator)
		}
		parent, e := filepath.EvalSymlinks(parentRaw)
		if errors.Is(e, os.ErrNotExist) {
			for _, component := range strings.Split(parentRaw, string(os.PathSeparator)) {
				if component == ".." {
					return "", unsafeNamespace()
				}
			}
			if e = os.MkdirAll(parentRaw, 0700); e != nil {
				return "", storageErr("private directory", e)
			}
			parent, e = filepath.EvalSymlinks(parentRaw)
		}
		if e != nil {
			return "", storageErr("directory path", e)
		}
		p = filepath.Join(parent, base)
	}
	p, e := filepath.Abs(p)
	if e != nil {
		return "", storageErr("database path", e)
	}
	// Reserved names are forbidden after symlink resolution too. Only the
	// suffix test folds case; actual database paths are never case-folded.
	base := strings.ToLower(filepath.Base(p))
	for _, suffix := range []string{lifecycleSuffix, "-wal", "-shm", "-journal"} {
		if strings.HasSuffix(base, suffix) {
			return "", unsafeNamespace()
		}
	}
	return p, nil
}
func safeRegular(s fileStat) bool { return s.regular() && s.links == 1 && s.uid == uint32(os.Getuid()) }
func safeLock(s fileStat) bool    { return safeRegular(s) && s.size == 0 && s.mode&0777 == 0600 }

// Only metadata is inspected on the SQLite files. No auxiliary main/WAL/SHM
// descriptor is opened, even on refusal paths.
func namespace(fs fileSystem, path string) (fileStat, bool, error) {
	return inspectNamespace(fs, path, true)
}
func inspectNamespace(fs fileSystem, path string, recoveryGuard bool) (fileStat, bool, error) {
	main, e := fs.stat(path, false)
	exists := e == nil
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return main, false, storageErr("database metadata", e)
	}
	if exists && !safeRegular(main) {
		return main, false, unsafeNamespace()
	}
	siblings := false
	ids := map[inode]bool{}
	if exists {
		ids[main.id] = true
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		s, err := fs.stat(path+suffix, false)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return main, false, storageErr("journal metadata", err)
		}
		siblings = true
		if !safeRegular(s) || ids[s.id] {
			return main, false, unsafeNamespace()
		}
		ids[s.id] = true
	}
	if recoveryGuard && siblings && (!exists || main.size == 0) {
		return main, false, unsafeNamespace()
	}
	return main, exists, nil
}
func validateEntry(e *lifecycleEntry) error {
	p, err := e.fs.stat(filepath.Dir(e.path), false)
	if err != nil {
		return storageErr("directory metadata", err)
	}
	if p.id != e.parent {
		return unsafeNamespace()
	}
	lock, err := e.fs.stat(e.path+lifecycleSuffix, false)
	if err != nil {
		return storageErr("lifecycle metadata", err)
	}
	if lock.id != e.lock || !safeLock(lock) {
		return unsafeNamespace()
	}
	main, exists, err := namespace(e.fs, e.path)
	if err != nil {
		return err
	}
	if e.db != (inode{}) && (!exists || main.id != e.db) {
		return unsafeNamespace()
	}
	if exists && e.db == (inode{}) {
		e.db = main.id
	}
	return nil
}
func acquireLifecycle(ctx context.Context, path string, exclusive bool, fs fileSystem) (*lifecycleLease, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	p, err := canonicalDatabase(path)
	if err != nil {
		return nil, err
	}
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		lease, e := tryLifecycle(p, exclusive, fs)
		if e == nil {
			return lease, nil
		}
		if exclusive || !errors.Is(e, ErrLifecycleBusy) {
			return nil, e
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
func tryLifecycle(path string, exclusive bool, fs fileSystem) (*lifecycleLease, error) {
	lifecycleManager.Lock()
	defer lifecycleManager.Unlock()
	if lifecycleManager.poison != nil {
		return nil, lifecycleManager.poison
	}
	main, exists, err := inspectNamespace(fs, path, false)
	if err != nil {
		return nil, err
	}
	lockPath := path + lifecycleSuffix
	before, err := fs.stat(lockPath, false)
	if err == nil && !safeLock(before) {
		return nil, unsafeNamespace()
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, storageErr("lifecycle metadata", err)
	}
	for _, entry := range lifecycleManager.entries {
		if exists && main.id == entry.lock {
			return nil, unsafeNamespace()
		}
		if exists && main.id == entry.db && (err != nil || before.id != entry.lock) {
			return nil, unsafeNamespace()
		}
	}
	if err == nil {
		if entry := lifecycleManager.entries[before.id]; entry != nil {
			if exclusive || entry.exclusive {
				return nil, &LifecycleBusyError{"maintenance"}
			}
			if e := validateEntry(entry); e != nil {
				return nil, e
			}
			if exists && entry.db != (inode{}) && main.id != entry.db {
				return nil, unsafeNamespace()
			}
			entry.refs++
			return &lifecycleLease{entry: entry}, nil
		}
	}
	parent, err := fs.stat(filepath.Dir(path), false)
	if err != nil {
		return nil, storageErr("directory metadata", err)
	}
	if parent.mode&unix.S_IFMT != unix.S_IFDIR || parent.uid != uint32(os.Getuid()) || parent.mode&0022 != 0 {
		return nil, unsafeNamespace()
	}
	dir, err := fs.openat(unix.AT_FDCWD, filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, storageErr("open private directory", err)
	}
	closeDir := func() { _ = fs.close(dir) }
	openedParent, err := fs.fstat(dir)
	if err != nil || openedParent.id != parent.id {
		closeDir()
		return nil, unsafeNamespace()
	}
	flags := unix.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC
	created := false
	latest, statErr := fs.stat(lockPath, false)
	if statErr == nil {
		if !safeLock(latest) {
			closeDir()
			return nil, unsafeNamespace()
		}
		before = latest
	} else if errors.Is(statErr, os.ErrNotExist) {
		flags |= unix.O_CREAT | unix.O_EXCL
		created = true
	} else {
		closeDir()
		return nil, storageErr("lifecycle metadata", statErr)
	}
	fd, err := fs.openat(dir, filepath.Base(lockPath), flags, 0600)
	if err != nil {
		closeDir()
		if errors.Is(err, os.ErrExist) {
			return nil, &LifecycleBusyError{"initialize lifecycle"}
		}
		return nil, storageErr("open lifecycle", err)
	}
	after, e1 := fs.fstat(fd)
	current, e2 := fs.stat(lockPath, false)
	proven := e1 == nil && e2 == nil && safeLock(after) && safeLock(current) && after.id == current.id
	if !created {
		proven = proven && before.id == after.id
	}
	if exists && after.id == main.id {
		proven = false
	}
	for _, suf := range []string{"-wal", "-shm", "-journal"} {
		s, e := fs.stat(path+suf, false)
		if e == nil && s.id == after.id {
			proven = false
		}
	}
	for _, entry := range lifecycleManager.entries {
		if after.id == entry.db || after.id == entry.lock || (exists && main.id == entry.db) {
			proven = false
		}
	}
	if !proven {
		// Do not attach an os.File finalizer to an unverified descriptor or close
		// it: it may be an active SQLite inode after a namespace race.
		lifecycleManager.quarantine = fd
		lifecycleManager.poison = unsafeNamespace()
		closeDir()
		return nil, lifecycleManager.poison
	}
	cleanup := func() error {
		ce := errors.Join(fs.close(fd), fs.close(dir))
		if ce != nil {
			lifecycleManager.poison = storageErr("lifecycle cleanup; restart required", ce)
		}
		return ce
	}
	if created {
		if err = fs.sync(fd); err == nil {
			err = fs.sync(dir)
		}
		if err != nil {
			return nil, storageErr("persist lifecycle", errors.Join(err, cleanup()))
		}
	}
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	if err = fs.flock(fd, how|unix.LOCK_NB); err != nil {
		if ce := cleanup(); ce != nil {
			return nil, storageErr("lifecycle cleanup", errors.Join(err, ce))
		}
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, &LifecycleBusyError{"maintenance"}
		}
		return nil, storageErr("lock lifecycle", err)
	}
	entry := &lifecycleEntry{path: path, fd: fd, dir: dir, parent: parent.id, lock: after.id, refs: 1, exclusive: exclusive, fs: fs}
	if exists {
		entry.db = main.id
	}
	lifecycleManager.entries[entry.lock] = entry
	if err = validateEntry(entry); err != nil {
		return nil, errors.Join(err, releaseEntry(entry))
	}
	return &lifecycleLease{entry: entry}, nil
}

// Called with the manager mutex held, after all corresponding SQL is closed.
func releaseEntry(e *lifecycleEntry) error {
	e.refs--
	if e.refs != 0 {
		return nil
	}
	err := e.fs.flock(e.fd, unix.LOCK_UN)
	err = errors.Join(err, e.fs.close(e.fd), e.fs.close(e.dir))
	if err != nil {
		lifecycleManager.poison = storageErr("release lifecycle", err)
		return lifecycleManager.poison
	}
	delete(lifecycleManager.entries, e.lock)
	return nil
}
func (l *lifecycleLease) release() error {
	l.once.Do(func() {
		lifecycleManager.Lock()
		defer lifecycleManager.Unlock()
		if l.retained {
			l.err = lifecycleManager.poison
			return
		}
		l.err = releaseEntry(l.entry)
	})
	return l.err
}
func (l *lifecycleLease) uncertain(err error) error {
	lifecycleManager.Lock()
	defer lifecycleManager.Unlock()
	l.retained = true
	lifecycleManager.poison = storageErr("SQL close; restart required", err)
	return lifecycleManager.poison
}
func (l *lifecycleLease) validate() error {
	lifecycleManager.Lock()
	defer lifecycleManager.Unlock()
	return validateEntry(l.entry)
}
func (l *lifecycleLease) createMain() error {
	if !l.entry.exclusive {
		return &LifecycleBusyError{"initialize database"}
	}
	e := l.entry
	_, exists, err := namespace(e.fs, e.path)
	if err != nil || exists {
		return err
	}
	fd, err := e.fs.openat(e.dir, filepath.Base(e.path), unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return storageErr("create database", err)
	}
	// O_EXCL created a new inode: no SQLite connection can yet own it.
	err = e.fs.sync(fd)
	err = errors.Join(err, e.fs.close(fd))
	if err != nil {
		return storageErr("persist new database", err)
	}
	s, err := e.fs.stat(e.path, false)
	if err != nil {
		return storageErr("new database metadata", err)
	}
	if !safeRegular(s) {
		return unsafeNamespace()
	}
	lifecycleManager.Lock()
	e.db = s.id
	lifecycleManager.Unlock()
	return storageErr("persist directory", e.fs.sync(e.dir))
}
