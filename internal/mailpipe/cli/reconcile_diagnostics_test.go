package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/store"
)

func diagnosticFixture() *graphmail.ReconciliationSenderAddressDiagnostic {
	return &graphmail.ReconciliationSenderAddressDiagnostic{AccountID: "graph:invented", N: 9007199254740993, ImmutableID: "opaque-invented", EnumeratedSenderAddress: "Sender@Example.org", InspectedSenderAddress: "different@example.org"}
}
func privateDiagnostic(t *testing.T) (*senderDiagnosticOutput, string) {
	t.Helper()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(dir, "report.json")
	d, e := openSenderDiagnostic(path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.close() })
	return d, path
}
func TestSenderDiagnosticEncoding(t *testing.T) {
	for _, value := range []string{"", "K@example.org", "\x1b\nhttps://host.example.org/private?token=secret", "<script>\u2028&"} {
		d := diagnosticFixture()
		d.EnumeratedSenderAddress = value
		raw, e := encodeSenderDiagnostic(d)
		if e != nil {
			t.Fatal(e)
		}
		var decoded senderDiagnosticDocument
		if e = json.Unmarshal(raw, &decoded); e != nil {
			t.Fatal(e)
		}
		if decoded.Enumerated != value || decoded.N != d.N || decoded.ReconciliationComplete || !decoded.EnumerationComplete || decoded.Format != 1 {
			t.Fatal("lossy raw diagnostic")
		}
		var fields map[string]any
		json.Unmarshal(raw, &fields)
		if len(fields) != 12 {
			t.Fatalf("unexpected exported fields: %d", len(fields))
		}
	}
	for _, kind := range []string{"invalid", "raw-limit", "escaped-limit", "number"} {
		d := diagnosticFixture()
		switch kind {
		case "invalid":
			d.InspectedSenderAddress = "\xff"
		case "raw-limit":
			d.InspectedSenderAddress = strings.Repeat("a", senderDiagnosticLimit+1)
		case "escaped-limit":
			d.InspectedSenderAddress = strings.Repeat("\x00", senderDiagnosticLimit/2)
		case "number":
			d.N = 0
		}
		if raw, e := encodeSenderDiagnostic(d); e == nil || raw != nil {
			t.Fatal("invalid/oversized payload accepted")
		}
	}
	if _, e := encodeSenderDiagnostic(nil); e == nil {
		t.Fatal("nil diagnostic accepted")
	}
}
func TestSenderDiagnosticPathBoundaries(t *testing.T) {
	for _, kind := range []string{"relative", "empty", "missing-parent", "public-parent", "file", "symlink", "dangling", "fifo", "hardlink", "directory-symlink", "checkout-directory", "checkout-file", "checkout-symlink", "context", "context-alias", "reserved-name"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			os.Chmod(dir, 0700)
			path := filepath.Join(dir, "new.json")
			var contexts []string
			switch kind {
			case "relative":
				path = "relative.json"
			case "empty":
				path = ""
			case "missing-parent":
				path = filepath.Join(dir, "missing", "new.json")
			case "public-parent":
				os.Chmod(dir, 0755)
			case "file":
				os.WriteFile(path, []byte("untouched"), 0600)
			case "symlink":
				target := filepath.Join(dir, "target")
				os.WriteFile(target, []byte("untouched"), 0600)
				os.Symlink(target, path)
			case "dangling":
				os.Symlink(filepath.Join(dir, "absent"), path)
			case "fifo":
				if e := syscall.Mkfifo(path, 0600); e != nil {
					t.Fatal(e)
				}
			case "hardlink":
				target := filepath.Join(dir, "target")
				os.WriteFile(target, []byte("untouched"), 0600)
				if e := os.Link(target, path); e != nil {
					t.Fatal(e)
				}
			case "directory-symlink":
				alias := filepath.Join(t.TempDir(), "alias")
				os.Symlink(dir, alias)
				path = filepath.Join(alias, "new.json")
			case "checkout-directory":
				os.Mkdir(filepath.Join(dir, ".git"), 0700)
			case "checkout-file":
				os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: elsewhere"), 0600)
			case "checkout-symlink":
				os.Symlink("missing", filepath.Join(dir, ".git"))
			case "context":
				contexts = []string{filepath.Join(dir, "missing.db")}
			case "context-alias":
				alias := filepath.Join(t.TempDir(), "alias")
				os.Symlink(dir, alias)
				contexts = []string{filepath.Join(alias, "missing.db-wal")}
			case "reserved-name":
				path = filepath.Join(dir, ".git")
			}
			d, e := openSenderDiagnostic(path, contexts...)
			if d != nil {
				d.close()
			}
			if e == nil {
				t.Fatal("unsafe destination accepted")
			}
		})
	}
}

type diagnosticFaultFile struct {
	attachmentFile
	kind   string
	fault  error
	cancel context.CancelFunc
}

func (f *diagnosticFaultFile) Stat() (os.FileInfo, error) {
	if f.kind == "stat" {
		return nil, f.fault
	}
	return f.attachmentFile.Stat()
}
func (f *diagnosticFaultFile) Write(p []byte) (int, error) {
	switch f.kind {
	case "write":
		return 0, f.fault
	case "short":
		return len(p) - 1, nil
	case "cancel":
		f.cancel()
	}
	return f.attachmentFile.Write(p)
}
func (f *diagnosticFaultFile) Sync() error {
	if f.kind == "sync" {
		return f.fault
	}
	return f.attachmentFile.Sync()
}
func (f *diagnosticFaultFile) Close() error {
	e := f.attachmentFile.Close()
	if f.kind == "close" {
		return errors.Join(e, f.fault)
	}
	return e
}

func TestSenderDiagnosticWriterFaults(t *testing.T) {
	NewRootCommand(Deps{})
	for _, kind := range []string{"success", "open", "stat", "pin", "write", "short", "sync", "close", "read", "read-differs", "directory-sync", "canceled", "cancel", "remove", "replacement", "rename", "permissions", "late-pin-close", "root-close"} {
		t.Run(kind, func(t *testing.T) {
			d, path := privateDiagnostic(t)
			fault := errors.New("writer-secret\x1b\nhttps://host.example.org/private")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			original := d.root.stageOpen
			d.root.stageOpen = func(name string) (attachmentFile, error) {
				if kind == "open" {
					return nil, fault
				}
				f, e := original(name)
				if e != nil {
					return nil, e
				}
				if kind == "pin" {
					os.Rename(path, path+".held")
					os.WriteFile(path, []byte("replacement"), 0600)
				}
				return &diagnosticFaultFile{attachmentFile: f, kind: kind, fault: fault, cancel: cancel}, nil
			}
			originalRead := d.readBack
			switch kind {
			case "read":
				d.readBack = func(*os.File) ([]byte, error) { return nil, fault }
			case "read-differs":
				d.readBack = func(*os.File) ([]byte, error) { return []byte("different"), nil }
			case "directory-sync":
				d.root.sync = func() error { return fault }
			case "canceled":
				cancel()
			case "remove":
				d.readBack = func(*os.File) ([]byte, error) { return nil, fault }
				d.root.remove = func(string) error { return fault }
			case "replacement":
				d.readBack = func(f *os.File) ([]byte, error) {
					raw, e := originalRead(f)
					os.Rename(path, path+".held")
					os.WriteFile(path, []byte("replacement"), 0600)
					return raw, e
				}
			case "rename":
				d.readBack = func(f *os.File) ([]byte, error) {
					raw, e := originalRead(f)
					os.Rename(filepath.Dir(path), filepath.Dir(path)+"-renamed")
					t.Cleanup(func() { os.RemoveAll(filepath.Dir(path) + "-renamed") })
					return raw, e
				}
			case "permissions":
				d.readBack = func(f *os.File) ([]byte, error) {
					raw, e := originalRead(f)
					os.Chmod(filepath.Dir(path), 0755)
					return raw, e
				}
			case "late-pin-close":
				d.readBack = func(f *os.File) ([]byte, error) { raw, e := originalRead(f); return raw, errors.Join(e, f.Close()) }
			case "root-close":
				close := d.closeRoot
				d.closeRoot = func() error { return errors.Join(close(), fault) }
			}
			cause := &graphmail.ReconciliationError{Reason: "incomplete primary-mailbox evidence; phase=consistency; category=metadata-changed", Cause: io.ErrUnexpectedEOF, SenderAddressDiagnostic: diagnosticFixture()}
			var out, stderr bytes.Buffer
			a := agentio.New("account reconcile", agentio.WithStdout(&out), agentio.WithStderr(&stderr))
			result := d.failure(ctx, a, cause)
			a.Fail(result)
			if agentio.ExitCode(result) != 1 || out.Len() != 0 || !errors.Is(result, io.ErrUnexpectedEOF) {
				t.Fatal("writer altered failure identity/authority")
			}
			text := stderr.String()
			for _, value := range []string{"writer-secret", "host.example.org", "Sender@Example.org", "opaque-invented", path, "\x1b"} {
				if strings.Contains(text, value) {
					t.Fatal("private data leaked")
				}
			}
			saved := strings.Contains(text, "saved privately")
			if saved != (kind == "success") {
				t.Fatalf("wrong saved status for %s: %s", kind, text)
			}
			raw, e := os.ReadFile(path)
			if kind == "success" {
				info, _ := os.Lstat(path)
				if e != nil || !attachmentOwned(info, false) || !json.Valid(raw) {
					t.Fatal("unsafe completed report")
				}
			}
			if kind == "replacement" || kind == "pin" {
				if string(raw) != "replacement" {
					t.Fatal("unknown replacement deleted")
				}
			}
			if kind == "write" || kind == "short" || kind == "close" || kind == "read" || kind == "read-differs" || kind == "sync" || kind == "cancel" {
				if !errors.Is(e, os.ErrNotExist) {
					t.Fatal("partial owned report remained")
				}
			}
			if len(text) > 1024 {
				t.Fatal("unbounded normal error")
			}
		})
	}
}
func TestSenderDiagnosticNoEligiblePayload(t *testing.T) {
	NewRootCommand(Deps{})
	d, path := privateDiagnostic(t)
	var out, stderr bytes.Buffer
	a := agentio.New("account reconcile", agentio.WithStdout(&out), agentio.WithStderr(&stderr))
	cause := &graphmail.ReconciliationError{Structural: true, Reason: "identity conflict", Conflict: &graphmail.ReconciliationConflict{Originals: []graphmail.ReconciliationConflictOriginal{{N: 1}}}}
	result := d.failure(context.Background(), a, cause)
	if agentio.ExitCode(result) != 3 || out.Len() != 0 || !strings.Contains(stderr.String(), "not produced") {
		t.Fatal("opt-in used legacy stdout diagnostic")
	}
	if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("unrelated failure created report")
	}
}

func TestSenderDiagnosticFlagPreflightHasNoCollaboratorEffects(t *testing.T) {
	for _, kind := range []string{"execute", "execute-false", "confirm-account", "confirm-history", "confirm-reconciliation", "relative", "database-parent", "config-parent"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			os.Chmod(dir, 0700)
			contextDir := t.TempDir()
			os.Chmod(contextDir, 0700)
			t.Setenv("MAILPIPE_CONFIG", filepath.Join(contextDir, "missing-config.json"))
			path := filepath.Join(dir, "new.json")
			db := filepath.Join(contextDir, "missing.db")
			args := []string{"account", "reconcile", "--sender-address-diagnostic-out", path}
			switch kind {
			case "execute":
				args = append(args, "--execute")
			case "execute-false":
				args = append(args, "--execute=false")
			case "confirm-account", "confirm-history", "confirm-reconciliation":
				args = append(args, "--"+kind+"=")
			case "relative":
				args[3] = "relative.json"
			case "database-parent":
				db = filepath.Join(dir, "missing.db")
			case "config-parent":
				t.Setenv("MAILPIPE_CONFIG", filepath.Join(dir, "missing-config.json"))
			}
			calls := 0
			root := NewRootCommand(Deps{Auth: func() (Authenticator, error) { calls++; return nil, errors.New("unexpected auth") }, Store: func(string) (*store.Store, error) { calls++; return nil, errors.New("unexpected store") }})
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.SetArgs(append([]string{"--db", db}, args...))
			err := root.ExecuteContext(context.Background())
			if ExitCode(err) != 2 || calls != 0 || out.Len() != 0 {
				t.Fatalf("preflight had effects: exit=%d calls=%d", ExitCode(err), calls)
			}
			entries, e := os.ReadDir(dir)
			if e != nil || len(entries) != 0 {
				t.Fatal("preflight wrote output", e)
			}
			if strings.Contains(stderr.String(), dir) {
				t.Fatal("path leaked")
			}
		})
	}
}

func TestSenderDiagnosticOutputErrorsPreserveFailureClass(t *testing.T) {
	NewRootCommand(Deps{})
	for _, structural := range []bool{false, true} {
		d, path := privateDiagnostic(t)
		fault := errors.New("private-output-secret\n\x1b")
		a := agentio.New("account reconcile", agentio.WithStderr(reconciliationFailWriter{err: fault}))
		cause := &graphmail.ReconciliationError{Structural: structural, Reason: "blocked evidence", Cause: context.Canceled}
		result := d.failure(context.Background(), a, cause)
		want := 1
		if structural {
			want = 3
		}
		if agentio.ExitCode(result) != want || !errors.Is(result, fault) || !errors.Is(result, context.Canceled) {
			t.Fatal("output fault changed origin class/identity")
		}
		if strings.Contains(result.Error(), "secret") {
			t.Fatal("raw output fault rendered")
		}
		if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("no-payload output fault wrote report")
		}
	}
	d, _ := privateDiagnostic(t)
	a := agentio.New("account reconcile", agentio.WithStderr(reconciliationFailWriter{}))
	if result := d.failure(context.Background(), a, &graphmail.ReconciliationError{Reason: "incomplete evidence"}); !errors.Is(result, io.ErrShortWrite) {
		t.Fatal("short status write lost")
	}
}

func TestSenderDiagnosticExclusiveCreationAfterPreflight(t *testing.T) {
	d, path := privateDiagnostic(t)
	if e := os.WriteFile(path, []byte("created-after-preflight"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := d.write(context.Background(), []byte("private observations")); !errors.Is(e, os.ErrExist) {
		t.Fatal("late collision not refused", e)
	}
	raw, e := os.ReadFile(path)
	if e != nil || string(raw) != "created-after-preflight" {
		t.Fatal("late collision overwritten")
	}
}

func TestSenderDiagnosticClosurePreservesFailureClass(t *testing.T) {
	for _, cause := range []error{nil, context.Canceled, agentio.NewUsageError("private-original", ""), &graphmail.ReconciliationError{Structural: true, Reason: "identity conflict"}} {
		fault := errors.New("private-close-secret")
		result := senderDiagnosticClosureFailure(cause, fault)
		want := 1
		if cause != nil {
			want = agentio.ExitCode(classify(reconciliationError(cause)))
		}
		if agentio.ExitCode(result) != want || !errors.Is(result, fault) || (cause != nil && !errors.Is(result, cause)) {
			t.Fatal("resource failure changed class/identity")
		}
		if strings.Contains(result.Error(), "private-") {
			t.Fatal("raw resource error rendered")
		}
	}
}
