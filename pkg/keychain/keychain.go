// Package keychain stores secrets in the macOS login Keychain through the
// security tool. Values are written on security's stdin (hex-encoded), never
// as arguments, so a secret cannot be read from the process list.
//
// security -i rejects input lines of about 4 KB, so a value larger than
// chunkBytes is split across items "<account>.1" ... "<account>.N", and the
// item "<account>" then holds a manifest naming N. Rewriting a value replaces
// its chunks one by one, so a reader in another process could otherwise get
// chunks from two different values: WithLock serializes readers and writers
// across processes.
//
// A cancelled context kills the security process. That is reported as an
// interruption (wrapping the context's error), never as a missing or damaged
// item.
package keychain

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrNotFound reports that no item exists for the service and account.
var ErrNotFound = errors.New("keychain item not found")

// ErrDamaged reports a chunked value that cannot be reassembled: a malformed
// manifest or a missing chunk.
var ErrDamaged = errors.New("keychain value damaged")

// itemNotFound is security's exit status for a missing item.
const itemNotFound = 44

const (
	// chunkBytes keeps each stdin line (2 hex chars per byte plus the command)
	// well under security -i's line limit.
	chunkBytes     = 1500
	manifestPrefix = "keychain-chunks/v1:"
)

// safeName limits service and account names to characters that need no
// quoting inside the stdin command line.
var safeName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Runner runs a command with stdin and returns its combined output. It is
// injectable so tests never invoke the real security tool.
type Runner interface {
	Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd.Output()
}

// Keychain reads and writes generic-password items for one service.
type Keychain struct {
	service  string
	runner   Runner
	lockPath string
}

// New returns a Keychain for service; a nil runner uses the real security tool.
func New(service string, runner Runner) *Keychain {
	if runner == nil {
		runner = execRunner{}
	}
	return &Keychain{service: service, runner: runner}
}

// WithLock returns a copy of k that holds an flock on path (created 0600)
// around every operation: readers share it and a writer holds it alone, so no
// reader sees a value while another process is rewriting it.
func (k *Keychain) WithLock(path string) *Keychain {
	c := *k
	c.lockPath = path
	return &c
}

// lockPoll is how often a blocked lock is retried; between tries the
// context is checked, so an interrupted wait ends promptly.
const lockPoll = 10 * time.Millisecond

// lock takes the lock (syscall.LOCK_SH or LOCK_EX) and returns its release.
func (k *Keychain) lock(ctx context.Context, how int) (func(), error) {
	if k.lockPath == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(k.lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("keychain: lock directory: %w", err)
	}
	f, err := os.OpenFile(k.lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("keychain: open lock: %w", err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil // closing the file releases the lock
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("keychain: lock: %w", err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("keychain: waiting for the lock interrupted: %w", ctx.Err())
		case <-time.After(lockPoll):
		}
	}
}

// failure describes a failed security call. A cancelled context is named as
// an interruption so it is never mistaken for a missing or damaged item.
func failure(ctx context.Context, what string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("keychain: %s interrupted: %w", what, err)
	}
	return fmt.Errorf("keychain: %s failed", what)
}

func (k *Keychain) check(account string) error {
	if !safeName.MatchString(k.service) || !safeName.MatchString(account) {
		return fmt.Errorf("keychain: invalid service %q or account %q", k.service, account)
	}
	return nil
}

func missing(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == itemNotFound
}

func chunkAccount(account string, i int) string { return fmt.Sprintf("%s.%d", account, i) }

// get reads one item.
func (k *Keychain) get(ctx context.Context, account string) (string, error) {
	out, err := k.runner.Run(ctx, nil, "security", "find-generic-password", "-s", k.service, "-a", account, "-w")
	if missing(err) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", failure(ctx, fmt.Sprintf("read %s/%s", k.service, account))
	}
	return strings.TrimSuffix(string(out), "\n"), nil // only the newline -w adds
}

// set writes one item.
func (k *Keychain) set(ctx context.Context, account, value string) error {
	line := fmt.Sprintf("add-generic-password -U -s %s -a %s -X %s\n", k.service, account, hex.EncodeToString([]byte(value)))
	if _, err := k.runner.Run(ctx, []byte(line), "security", "-i"); err != nil {
		// The error deliberately omits security's output, which may echo the command.
		return failure(ctx, fmt.Sprintf("write %s/%s", k.service, account))
	}
	return nil
}

// del removes one item; a missing item is not an error.
func (k *Keychain) del(ctx context.Context, account string) error {
	_, err := k.runner.Run(ctx, nil, "security", "delete-generic-password", "-s", k.service, "-a", account)
	if err != nil && !missing(err) {
		return failure(ctx, fmt.Sprintf("delete %s/%s", k.service, account))
	}
	return nil
}

// chunks returns the chunk count recorded for account (0 when unchunked or
// absent). An unreadable item is an error: Set and Delete must not go on
// without knowing which chunks exist, or they would orphan them.
func (k *Keychain) chunks(ctx context.Context, account string) (int, error) {
	v, err := k.get(ctx, account)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !strings.HasPrefix(v, manifestPrefix) {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimPrefix(v, manifestPrefix))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("keychain: %s/%s has a malformed manifest: %w", k.service, account, ErrDamaged)
	}
	return n, nil
}

// Get returns the stored value, or ErrNotFound.
func (k *Keychain) Get(ctx context.Context, account string) (string, error) {
	if err := k.check(account); err != nil {
		return "", err
	}
	unlock, err := k.lock(ctx, syscall.LOCK_SH)
	if err != nil {
		return "", err
	}
	defer unlock()
	v, err := k.get(ctx, account)
	if err != nil || !strings.HasPrefix(v, manifestPrefix) {
		return v, err
	}
	n, err := strconv.Atoi(strings.TrimPrefix(v, manifestPrefix))
	if err != nil || n < 1 {
		return "", fmt.Errorf("keychain: %s/%s has a malformed manifest: %w", k.service, account, ErrDamaged)
	}
	var b strings.Builder
	for i := 1; i <= n; i++ {
		part, err := k.get(ctx, chunkAccount(account, i))
		if err != nil {
			if ctx.Err() != nil {
				return "", err
			}
			return "", fmt.Errorf("keychain: %s/%s is incomplete (chunk %d of %d unreadable): %w", k.service, account, i, n, ErrDamaged)
		}
		b.WriteString(part)
	}
	return b.String(), nil
}

// Set creates or replaces the stored value, chunking it when it is large.
func (k *Keychain) Set(ctx context.Context, account, value string) error {
	if err := k.check(account); err != nil {
		return err
	}
	unlock, err := k.lock(ctx, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlock()
	old, err := k.chunks(ctx, account)
	if err != nil {
		return err
	}
	n := 0
	if len(value) > chunkBytes {
		for start := 0; start < len(value); start += chunkBytes {
			n++
			end := min(start+chunkBytes, len(value))
			if err := k.set(ctx, chunkAccount(account, n), value[start:end]); err != nil {
				return err
			}
		}
		if err := k.set(ctx, account, fmt.Sprintf("%s%d", manifestPrefix, n)); err != nil {
			return err
		}
	} else if err := k.set(ctx, account, value); err != nil {
		return err
	}
	for i := n + 1; i <= old; i++ {
		if err := k.del(ctx, chunkAccount(account, i)); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes the stored value and any chunks; a missing item is not an error.
func (k *Keychain) Delete(ctx context.Context, account string) error {
	if err := k.check(account); err != nil {
		return err
	}
	unlock, err := k.lock(ctx, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlock()
	n, err := k.chunks(ctx, account)
	if err != nil {
		return err
	}
	for i := 1; i <= n; i++ {
		if err := k.del(ctx, chunkAccount(account, i)); err != nil {
			return err
		}
	}
	return k.del(ctx, account)
}
