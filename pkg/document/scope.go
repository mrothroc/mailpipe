package document

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// Scope owns all temporary content for one long-running operation. Disk bytes
// include reservations and every simultaneous representation, not just raw MIME.
// It never sweeps another scope or adopts abandoned directories.
type Scope struct {
	limits                          Limits
	mu                              sync.Mutex
	closeMu                         sync.Mutex
	root                            *os.Root
	parent                          *os.Root
	name, path                      string
	files                           map[*Blob]bool
	pending                         []*Blob
	disk, memory                    int64
	reservations, active, completed int
	creating                        int
	terminal                        bool
	terminalErr                     error
	slots                           chan struct{}
	changed                         chan struct{}
	closed, poisoned                bool
	remove                          func(context.Context, string) error
}

// Usage is a logical retained-capacity snapshot, not RSS or filesystem blocks.
type Usage struct {
	Disk, Memory                int64
	Files, Reservations, Active int
	Poisoned                    bool
}

func (s *Scope) Usage() Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Usage{s.disk, s.memory, len(s.files), s.reservations, s.active, s.poisoned}
}
func owned(info os.FileInfo, dir bool) bool {
	if info == nil {
		return false
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) {
		return false
	}
	if dir {
		return info.IsDir() && info.Mode().Perm() == 0700
	}
	return info.Mode().IsRegular() && info.Mode().Perm() == 0600 && st.Nlink == 1
}
func randomName() string { return rand.Text() }
func NewScope(l Limits) (*Scope, error) {
	l, e := l.Resolve()
	if e != nil {
		return nil, e
	}
	path, e := filepath.EvalSymlinks(os.TempDir())
	if e != nil {
		return nil, incomplete("temporary root unavailable", e)
	}
	path, e = filepath.Abs(path)
	if e != nil {
		return nil, incomplete("temporary root unavailable", e)
	}
	parent, e := os.OpenRoot(path)
	if e != nil {
		return nil, incomplete("temporary root unavailable", e)
	}
	fail := func(e error) (*Scope, error) { return nil, errors.Join(e, parent.Close()) }
	f, e := parent.Open(".")
	if e != nil {
		return fail(e)
	}
	info, e := f.Stat()
	e = errors.Join(e, f.Close())
	if e != nil {
		return fail(e)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || (st.Uid != 0 && st.Uid != uint32(os.Geteuid())) || (info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0) {
		return fail(incomplete("unsafe temporary parent", nil))
	}
	name := "mailpipe-content-" + randomName()
	if e = parent.Mkdir(name, 0700); e != nil {
		return fail(e)
	}
	root, e := parent.OpenRoot(name)
	if e != nil {
		return fail(errors.Join(e, parent.Remove(name)))
	}
	f, e = root.Open(".")
	if e == nil {
		info, e = f.Stat()
		e = errors.Join(e, f.Close())
	}
	if e != nil || !owned(info, true) {
		return fail(errors.Join(incomplete("unsafe private directory", e), root.Close(), parent.Remove(name)))
	}
	s := &Scope{limits: l, root: root, parent: parent, name: name, path: filepath.Join(path, name), files: map[*Blob]bool{}, slots: make(chan struct{}, l.Pipelines), changed: make(chan struct{}, 1)}
	s.remove = func(ctx context.Context, name string) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		return root.Remove(name)
	}
	return s, nil
}
func (s *Scope) signal() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// Enter admits bounded concurrent parser work. Call leave after all its owned
// resources have been closed. A poisoned scope never starts another pipeline.
func (s *Scope) Enter(ctx context.Context) (func(), error) {
	if e := s.Checkpoint(ctx); e != nil {
		return nil, e
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.mu.Lock()
	if s.closed || s.poisoned {
		s.mu.Unlock()
		<-s.slots
		return nil, incomplete("scope unavailable", ErrCleanup)
	}
	s.active++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.active--
			s.completed++
			s.mu.Unlock()
			<-s.slots
			s.signal()
		})
	}, nil
}
func (s *Scope) reserveMemory(n int64) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 0 || n > s.limits.MemoryBytes-s.memory || s.poisoned || s.closed {
		return nil, incomplete("memory budget exhausted", nil)
	}
	s.memory += n
	var once sync.Once
	return func() { once.Do(func() { s.mu.Lock(); s.memory -= n; s.mu.Unlock() }) }, nil
}

// Blob is a private bounded spool. Seal closes the writer before any reader can
// observe it. Close refuses active readers and retains uncertain disk charges.
type Blob struct {
	mu                          sync.Mutex
	s                           *Scope
	name                        string
	file                        *os.File
	info                        os.FileInfo
	size, charge, reserved, max int64
	sealed, removed, pending    bool
	queued                      bool
	readers                     int
}

func (s *Scope) Limits() Limits { return s.limits }
func (s *Scope) NewBlob(ctx context.Context, maxBytes, announced int64) (*Blob, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if maxBytes <= 0 || maxBytes > 1<<30 || announced < 0 || announced > maxBytes {
		return nil, incomplete("announced size exceeds limit", nil)
	}
	s.mu.Lock()
	if s.closed || s.poisoned || len(s.files)+s.creating >= s.limits.Files || s.reservations >= s.limits.Reservations || announced > s.limits.DiskBytes-s.disk {
		s.mu.Unlock()
		return nil, incomplete("spool quota exhausted", nil)
	}
	s.creating++
	s.reservations++
	s.disk += announced
	s.mu.Unlock()
	name := "part-" + randomName()
	f, e := s.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		s.mu.Lock()
		s.creating--
		s.mu.Unlock()
		s.signal()
		s.mu.Lock()
		s.reservations--
		s.disk -= announced
		s.mu.Unlock()
		return nil, incomplete("spool creation failed", e)
	}
	b := &Blob{s: s, name: name, file: f, max: maxBytes, reserved: announced, charge: announced}
	b.info, e = f.Stat()
	s.mu.Lock()
	s.files[b] = true
	s.creating--
	s.mu.Unlock()
	s.signal()
	if e != nil || !owned(b.info, false) {
		b.pending = true
		b.queue()
		s.mu.Lock()
		s.poisoned = true
		s.mu.Unlock()
		return nil, incomplete("spool ownership failed", errors.Join(ErrCleanup, e))
	}
	return b, nil
}
func (b *Blob) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.s
	s.mu.Lock()
	if b.sealed || b.removed || s.poisoned || s.closed {
		s.mu.Unlock()
		return 0, incomplete("spool unavailable", nil)
	}
	if int64(len(p)) > b.max-b.size {
		s.mu.Unlock()
		return 0, incomplete("content byte limit exceeded", nil)
	}
	extra := max(int64(0), int64(len(p))-b.reserved)
	if extra > s.limits.DiskBytes-s.disk {
		s.mu.Unlock()
		return 0, incomplete("spool quota exhausted", nil)
	}
	s.disk += extra
	b.charge += extra
	b.reserved += extra
	s.mu.Unlock()
	n, e := b.file.Write(p)
	b.size += int64(n)
	b.reserved -= int64(n)
	if e != nil || n != len(p) {
		s.mu.Lock()
		s.poisoned = true
		s.mu.Unlock()
		b.pending = true
		b.queue()
		if e == nil {
			e = io.ErrShortWrite
		}
		return n, incomplete("spool write failed", e)
	}
	return n, nil
}
func (b *Blob) Seal() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.s
	if b.sealed {
		return nil
	}
	if b.removed {
		return incomplete("spool removed", nil)
	}
	if e := b.file.Close(); e != nil {
		s.mu.Lock()
		s.poisoned = true
		s.mu.Unlock()
		b.pending = true
		b.queue()
		return incomplete("spool close failed", errors.Join(ErrCleanup, e))
	}
	b.file = nil
	b.sealed = true
	s.mu.Lock()
	s.disk -= b.reserved
	b.charge -= b.reserved
	b.reserved = 0
	s.reservations--
	s.mu.Unlock()
	return nil
}
func (b *Blob) Size() int64 { b.mu.Lock(); defer b.mu.Unlock(); return b.size }
func (b *Blob) Path() (string, error) {
	r, e := b.Open()
	if e != nil {
		return "", e
	}
	if e = r.Close(); e != nil {
		return "", e
	}
	return filepath.Join(b.s.path, b.name), nil
}

type blobReader struct {
	*os.File
	b    *Blob
	once sync.Once
	err  error
}

func (r *blobReader) Close() error {
	r.once.Do(func() {
		r.err = r.File.Close()
		r.b.mu.Lock()
		if r.err == nil {
			r.b.readers--
		} else {
			r.b.pending = true
			r.b.queue()
		}
		r.b.mu.Unlock()
		if r.err != nil {
			r.b.s.mu.Lock()
			r.b.s.poisoned = true
			r.b.s.mu.Unlock()
		}
		r.b.s.signal()
	})
	return r.err
}
func (b *Blob) Open() (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.sealed || b.removed {
		return nil, incomplete("unsealed source", nil)
	}
	before, e := b.s.root.Lstat(b.name)
	if e != nil || !owned(before, false) || !os.SameFile(b.info, before) {
		return nil, incomplete("source path changed", e)
	}
	f, e := b.s.root.OpenFile(b.name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, incomplete("source reopen failed", e)
	}
	info, e := f.Stat()
	if e != nil || b.info == nil || !owned(info, false) || !os.SameFile(b.info, info) || info.Size() != b.size {
		return nil, incomplete("source identity changed", errors.Join(e, f.Close()))
	}
	b.readers++
	return &blobReader{File: f, b: b}, nil
}
func (b *Blob) Close() error { return b.CloseContext(context.Background()) }
func (b *Blob) CloseContext(parent context.Context) error {
	ctx, cancel := cleanupContext(parent)
	defer cancel()
	return b.close(ctx)
}
func (b *Blob) close(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.s
	if b.removed {
		return nil
	}
	fail := func(e error) error {
		b.pending = true
		b.queue()
		s.mu.Lock()
		s.poisoned = true
		s.mu.Unlock()
		return incomplete("owned cleanup failed", errors.Join(ErrCleanup, e))
	}
	if b.readers != 0 {
		return fail(errors.New("active spool reader"))
	}
	if b.file != nil {
		if e := b.file.Close(); e != nil {
			return fail(e)
		}
		b.file = nil
	}
	info, e := s.root.Lstat(b.name)
	if e != nil || b.info == nil || !owned(info, false) || !os.SameFile(b.info, info) {
		return fail(e)
	}
	if e = s.remove(ctx, b.name); e != nil {
		return fail(e)
	}
	s.mu.Lock()
	s.disk -= b.charge
	if !b.sealed {
		s.reservations--
	}
	b.removed = true
	delete(s.files, b)
	s.mu.Unlock()
	return nil
}

// Checkpoint retries at most 32 completed owned entries. Live readers and live
// documents are never swept. A poisoned scope stays closed to new decisions.
func (s *Scope) Checkpoint(ctx context.Context) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	s.mu.Lock()
	n := min(32, len(s.pending))
	files := append([]*Blob(nil), s.pending[:n]...)
	s.pending = s.pending[n:]
	s.mu.Unlock()
	var err error
	for _, b := range files {
		b.mu.Lock()
		b.queued = false
		can := b.readers == 0
		if !can {
			b.queue()
		}
		b.mu.Unlock()
		if can {
			err = errors.Join(err, b.Close())
		}
	}
	s.mu.Lock()
	poisoned := s.poisoned
	s.mu.Unlock()
	if poisoned {
		err = errors.Join(err, incomplete("cleanup requires reconciliation", ErrCleanup))
	}
	return err
}

// queue is called with the blob lock held; only proven owned entries are retried.
func (b *Blob) queue() {
	if !b.queued {
		b.queued = true
		b.s.mu.Lock()
		b.s.pending = append(b.s.pending, b)
		b.s.mu.Unlock()
	}
}
func (s *Scope) Close() error { return s.CloseContext(context.Background()) }
func (s *Scope) CloseContext(parent context.Context) error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	s.mu.Lock()
	if s.terminal {
		err := s.terminalErr
		s.mu.Unlock()
		return err
	}
	s.closed = true
	s.mu.Unlock()
	ctx, cancel := cleanupContext(parent)
	defer cancel()
	for {
		s.mu.Lock()
		active := s.active + s.creating
		s.mu.Unlock()
		if active == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return incomplete("cleanup work still active", ErrCleanup)
		case <-s.changed:
		}
	}
	s.mu.Lock()
	files := make([]*Blob, 0, len(s.files))
	for b := range s.files {
		files = append(files, b)
	}
	s.mu.Unlock()
	var err error
	for _, b := range files {
		if e := ctx.Err(); e != nil {
			return incomplete("scope cleanup deadline exceeded", errors.Join(ErrCleanup, e))
		}
		err = errors.Join(err, b.close(ctx))
	}
	if err != nil {
		return err
	}
	err = errors.Join(s.root.Close(), s.parent.Remove(s.name), s.parent.Close())
	s.mu.Lock()
	s.terminal = true
	s.terminalErr = err
	s.mu.Unlock()
	return err
}

// AnnounceSourceSize reserves the entire announced raw representation before
// transport writes its first chunk. Actual bytes remain independently checked.
func (b *Blob) AnnounceSourceSize(n int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.s
	if n < 0 || n > b.max || b.size != 0 {
		return incomplete("announced source size invalid", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	extra := max(int64(0), n-b.reserved)
	if extra > s.limits.DiskBytes-s.disk {
		return incomplete("announced source exceeds spool quota", nil)
	}
	s.disk += extra
	b.charge += extra
	b.reserved += extra
	return nil
}
