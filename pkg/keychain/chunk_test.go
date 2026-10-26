package keychain_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/keychain"
)

// fakeKeychain behaves like security(1) over an in-memory keychain, including
// the -i line-length limit that forces large values into chunks.
type fakeKeychain struct {
	mu      sync.Mutex
	items   map[string]string // "service/account" -> value
	maxLine int
	lines   []string
	argv    [][]string
	// beforeWrite, if set, runs (unlocked) before each item is written.
	beforeWrite func(account string)
	// beforeRead, if set, runs (unlocked) before each item is read, with the
	// call's context, as security would be killed when it is cancelled.
	beforeRead func(ctx context.Context, account string)
	// afterRead, if set, runs (unlocked) after each successful read.
	afterRead func(account string)
	// failRead makes reading these accounts fail as security does on a
	// keychain error (not a missing item).
	failRead map[string]bool
}

func newFakeKeychain() *fakeKeychain { return &fakeKeychain{items: map[string]string{}, maxLine: 4000} }

func notFound() error { return exec.Command("sh", "-c", "exit 44").Run() }
func failed() error   { return exec.Command("sh", "-c", "exit 1").Run() }

func flag(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (f *fakeKeychain) Run(ctx context.Context, stdin []byte, _ string, args ...string) ([]byte, error) {
	if args[0] == "-i" && f.beforeWrite != nil {
		f.beforeWrite(flag(strings.Fields(string(stdin)), "-a"))
	}
	if args[0] == "find-generic-password" && f.beforeRead != nil {
		f.beforeRead(ctx, flag(args, "-a"))
	}
	if ctx.Err() != nil {
		return nil, failed() // exec.CommandContext kills the process
	}
	if args[0] == "find-generic-password" && f.afterRead != nil {
		defer f.afterRead(flag(args, "-a")) // runs after the unlock below
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.argv = append(f.argv, args)
	switch args[0] {
	case "-i":
		for _, line := range strings.Split(strings.TrimRight(string(stdin), "\n"), "\n") {
			f.lines = append(f.lines, line)
			if len(line) > f.maxLine {
				return nil, failed()
			}
			fields := strings.Fields(line)
			data, err := hex.DecodeString(flag(fields, "-X"))
			if fields[0] != "add-generic-password" || err != nil {
				return nil, failed()
			}
			f.items[flag(fields, "-s")+"/"+flag(fields, "-a")] = string(data)
		}
		return nil, nil
	case "find-generic-password":
		if f.failRead[flag(args, "-a")] {
			return nil, failed()
		}
		v, ok := f.items[flag(args, "-s")+"/"+flag(args, "-a")]
		if !ok {
			return nil, notFound()
		}
		return []byte(v + "\n"), nil
	case "delete-generic-password":
		key := flag(args, "-s") + "/" + flag(args, "-a")
		if _, ok := f.items[key]; !ok {
			return nil, notFound()
		}
		delete(f.items, key)
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected %v", args)
}

func big(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "{\"token\":%d,\"x\":\"abcdefghij\"}", i)
	}
	return b.String()[:n]
}

func TestLargeValuesRoundTripThroughChunksWithinTheLineLimit(t *testing.T) {
	f := newFakeKeychain()
	k := keychain.New("mailpipe", f)
	ctx := context.Background()
	value := big(9000)
	if err := k.Set(ctx, "token-cache", value); err != nil {
		t.Fatal(err)
	}
	for _, line := range f.lines {
		if len(line) > f.maxLine {
			t.Fatalf("line of %d chars exceeds the limit", len(line))
		}
	}
	if len(f.items) < 4 {
		t.Fatalf("expected the value split across items, got %d items", len(f.items))
	}
	got, err := k.Get(ctx, "token-cache")
	if err != nil || got != value {
		t.Fatalf("round trip failed: err %v, len %d want %d", err, len(got), len(value))
	}
}

func TestShrinkingAValueRemovesLeftoverChunks(t *testing.T) {
	f := newFakeKeychain()
	k := keychain.New("mailpipe", f)
	ctx := context.Background()
	if err := k.Set(ctx, "token-cache", big(9000)); err != nil {
		t.Fatal(err)
	}
	if err := k.Set(ctx, "token-cache", "small"); err != nil {
		t.Fatal(err)
	}
	if len(f.items) != 1 {
		t.Fatalf("items %v, want only the small value", keys(f.items))
	}
	if got, _ := k.Get(ctx, "token-cache"); got != "small" {
		t.Fatalf("Get = %q", got)
	}
}

func TestDeleteRemovesEveryChunk(t *testing.T) {
	f := newFakeKeychain()
	k := keychain.New("mailpipe", f)
	ctx := context.Background()
	_ = k.Set(ctx, "token-cache", big(9000))
	if err := k.Delete(ctx, "token-cache"); err != nil {
		t.Fatal(err)
	}
	if len(f.items) != 0 {
		t.Fatalf("left behind %v", keys(f.items))
	}
}

func TestMissingChunkIsAnErrorNotAShortValue(t *testing.T) {
	f := newFakeKeychain()
	k := keychain.New("mailpipe", f)
	ctx := context.Background()
	_ = k.Set(ctx, "token-cache", big(9000))
	delete(f.items, "mailpipe/token-cache.2")
	if _, err := k.Get(ctx, "token-cache"); err == nil {
		t.Fatal("incomplete value returned without error")
	}
}

func TestLargeValueNeverReachesArgv(t *testing.T) {
	f := newFakeKeychain()
	k := keychain.New("mailpipe", f)
	value := big(9000)
	_ = k.Set(context.Background(), "token-cache", value)
	for _, args := range f.argv {
		for _, a := range args {
			if len(a) > 64 {
				t.Fatalf("long argument on the command line: %d chars", len(a))
			}
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestAnInterruptedReadReportsTheInterruptionNotADamagedValue(t *testing.T) {
	f := newFakeKeychain()
	k := keychain.New("mailpipe", f)
	if err := k.Set(context.Background(), "token-cache", big(9000)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.beforeRead = func(_ context.Context, account string) {
		if account == "token-cache.2" {
			cancel()
		}
	}
	_, err := k.Get(ctx, "token-cache")
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("Get = %v, want an interruption", err)
	}
}

func TestAnInterruptedWriteReportsTheInterruption(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := keychain.New("mailpipe", newFakeKeychain()).Set(ctx, "token-cache", "v")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Set = %v, want an interruption", err)
	}
}

// A reader that starts while another process rewrites a chunked value must
// get one whole value, never chunks from both.
func TestALockedReaderNeverSeesAHalfWrittenValue(t *testing.T) {
	f := newFakeKeychain()
	lock := filepath.Join(t.TempDir(), "keychain.lock")
	writer, reader := keychain.New("mailpipe", f).WithLock(lock), keychain.New("mailpipe", f).WithLock(lock)
	ctx := context.Background()
	old, next := big(9000), strings.Repeat("n", 9000)
	if err := writer.Set(ctx, "token-cache", old); err != nil {
		t.Fatal(err)
	}
	midway, resume := make(chan struct{}), make(chan struct{})
	f.beforeWrite = func(account string) {
		if account == "token-cache.2" {
			close(midway)
			<-resume
		}
	}
	done := make(chan error)
	go func() { done <- writer.Set(ctx, "token-cache", next) }()
	<-midway // chunk 1 is new, the rest are old
	got := make(chan string)
	go func() {
		v, err := reader.Get(ctx, "token-cache")
		if err != nil {
			v = err.Error()
		}
		got <- v
	}()
	select {
	case v := <-got:
		t.Fatalf("reader did not wait for the writer: got %d chars", len(v))
	case <-time.After(100 * time.Millisecond):
	}
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if v := <-got; v != next {
		t.Fatalf("reader got %d chars, not the new value", len(v))
	}
}

func TestWaitingForTheLockStopsWhenInterrupted(t *testing.T) {
	f := newFakeKeychain()
	lock := filepath.Join(t.TempDir(), "keychain.lock")
	writer, reader := keychain.New("mailpipe", f).WithLock(lock), keychain.New("mailpipe", f).WithLock(lock)
	held, release := make(chan struct{}), make(chan struct{})
	f.beforeWrite = func(string) { close(held); <-release }
	go func() { _ = writer.Set(context.Background(), "token-cache", "v") }()
	<-held
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := reader.Get(ctx, "token-cache"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get = %v, want it to give up when its context ends", err)
	}
}

func TestTheLockFileIsPrivate(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "keychain.lock")
	if err := keychain.New("mailpipe", newFakeKeychain()).WithLock(lock).Set(context.Background(), "a", "v"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(lock); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file: %v %v", fi, err)
	}
}

func TestACancellationAfterASuccessfulReadNeverYieldsAnEmptyValue(t *testing.T) {
	f := newFakeKeychain()
	k := keychain.New("mailpipe", f)
	value := big(9000)
	if err := k.Set(context.Background(), "token-cache", value); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.afterRead = func(account string) {
		if account == "token-cache.2" {
			cancel()
		}
	}
	if got, err := k.Get(ctx, "token-cache"); err == nil && got != value {
		t.Fatalf("Get returned %d chars and no error", len(got))
	}
}

func TestAnIncompleteOrMalformedValueIsDamaged(t *testing.T) {
	f := newFakeKeychain()
	k := keychain.New("mailpipe", f)
	ctx := context.Background()
	_ = k.Set(ctx, "token-cache", big(9000))
	delete(f.items, "mailpipe/token-cache.2")
	if _, err := k.Get(ctx, "token-cache"); !errors.Is(err, keychain.ErrDamaged) {
		t.Fatalf("missing chunk: %v, want ErrDamaged", err)
	}
	f.items["mailpipe/token-cache"] = "keychain-chunks/v1:x"
	if _, err := k.Get(ctx, "token-cache"); !errors.Is(err, keychain.ErrDamaged) {
		t.Fatalf("malformed manifest: %v, want ErrDamaged", err)
	}
}

func TestDeleteReadsTheManifestOnce(t *testing.T) {
	f := newFakeKeychain()
	k := keychain.New("mailpipe", f)
	ctx := context.Background()
	_ = k.Set(ctx, "token-cache", big(9000))
	f.argv = nil
	if err := k.Delete(ctx, "token-cache"); err != nil {
		t.Fatal(err)
	}
	reads := 0
	for _, a := range f.argv {
		if a[0] == "find-generic-password" {
			reads++
		}
	}
	if reads != 1 {
		t.Fatalf("Delete read the manifest %d times", reads)
	}
}

func TestTheLockDirectoryIsCreatedPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh")
	if err := keychain.New("mailpipe", newFakeKeychain()).WithLock(filepath.Join(dir, "keychain.lock")).Set(context.Background(), "a", "v"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("lock directory: %v %v", fi, err)
	}
}

func TestValuesEndingInNewlinesRoundTrip(t *testing.T) {
	k := keychain.New("mailpipe", newFakeKeychain())
	ctx := context.Background()
	for _, v := range []string{"line\n\n", big(3000) + "\n"} {
		if err := k.Set(ctx, "a", v); err != nil {
			t.Fatal(err)
		}
		if got, err := k.Get(ctx, "a"); err != nil || got != v {
			t.Fatalf("Get = %q (%d chars), %v; want %d chars", got[max(0, len(got)-5):], len(got), err, len(v))
		}
	}
}

// When the manifest cannot be read, Set and Delete cannot know which chunks
// exist, so they must stop rather than orphan them.
func TestAnUnreadableManifestStopsSetAndDelete(t *testing.T) {
	f := newFakeKeychain()
	k := keychain.New("mailpipe", f)
	ctx := context.Background()
	if err := k.Set(ctx, "token-cache", big(9000)); err != nil {
		t.Fatal(err)
	}
	before := len(f.items)
	f.failRead = map[string]bool{"token-cache": true}
	if err := k.Set(ctx, "token-cache", "small"); err == nil {
		t.Fatal("Set went ahead without knowing the old chunks")
	}
	if err := k.Delete(ctx, "token-cache"); err == nil {
		t.Fatal("Delete went ahead without knowing the old chunks")
	}
	if len(f.items) != before {
		t.Fatalf("items changed: %d -> %d", before, len(f.items))
	}
}
