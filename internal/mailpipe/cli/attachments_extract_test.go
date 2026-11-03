package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/document"
)

func TestAttachmentNamesRejectUntrustedPaths(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "a/b", `a\b`, `C:\escape`, ".", "..", "x\x00y", "x\x1by", "x\u202ey", strings.Repeat("x", 241)} {
		if _, err := attachmentName(document.InspectablePart{Path: "1.2", Filename: name, ContentType: "image/png"}); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	for _, tc := range []struct{ name, ct, want string }{{"", "image/png", "part-1-2.png"}, {"", "unknown/type", "part-1-2.bin"}, {"record.pdf", "application/pdf", "part-1-2-record.pdf"}} {
		got, err := attachmentName(document.InspectablePart{Path: "1.2", Filename: tc.name, ContentType: tc.ct})
		if err != nil || got != tc.want {
			t.Fatalf("%q %v", got, err)
		}
	}
}

type attachmentFaultFile struct {
	attachmentFile
	fault string
}

func (f attachmentFaultFile) Write(p []byte) (int, error) {
	if f.fault == "short" {
		return 0, nil
	}
	return f.attachmentFile.Write(p)
}
func (f attachmentFaultFile) Sync() error {
	if f.fault == "sync" {
		return errors.New("injected sync")
	}
	return f.attachmentFile.Sync()
}
func (f attachmentFaultFile) Close() error {
	e := f.attachmentFile.Close()
	if f.fault == "close" {
		return errors.Join(e, errors.New("injected close"))
	}
	return e
}
func (f attachmentFaultFile) Stat() (os.FileInfo, error) {
	if f.fault == "stat" {
		return nil, errors.New("injected stat")
	}
	return f.attachmentFile.Stat()
}

func privateAttachmentRoot(t *testing.T) *attachmentRoot {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	r, e := openAttachmentRoot(dir)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := r.Close(); e != nil {
			t.Error(e)
		}
	})
	return r
}

func TestAttachmentStagingFaultsNeverPublish(t *testing.T) {
	for _, fault := range []string{"open", "short", "sync", "close", "stat", "readback-open", "readback-digest", "size", "closed-blob", "quota", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			s, rows := extractionTestRows(t)
			r := privateAttachmentRoot(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			open := r.stageOpen
			r.stageOpen = func(name string) (attachmentFile, error) {
				if fault == "open" {
					return nil, errors.New("injected open")
				}
				f, e := open(name)
				if e != nil {
					return nil, e
				}
				if fault == "cancel" {
					cancel()
				}
				return attachmentFaultFile{f, fault}, nil
			}
			if fault == "readback-open" {
				r.stageRead = func(string) (io.ReadCloser, error) { return nil, errors.New("injected reopen") }
			}
			if fault == "readback-digest" {
				rows[0].SHA256 = strings.Repeat("0", 64)
			}
			if fault == "size" {
				rows[0].DecodedBytes--
			}
			if fault == "closed-blob" {
				if e := rows[0].data.Close(); e != nil {
					t.Fatal(e)
				}
			}
			limits := document.DefaultLimits()
			if fault == "quota" {
				limits.DecodedBytes = 21
			}
			err := extractAttachments(ctx, r, rows, true, limits, s.Close)
			if err == nil {
				t.Fatal("injected failure succeeded")
			}
			entries, e := os.ReadDir(r.path)
			if e != nil || len(entries) != 0 {
				t.Fatalf("failure leaked/published %v %v", entries, e)
			}
		})
	}
}

func TestAttachmentPinnedRootSurvivesPathReplacement(t *testing.T) {
	s, rows := extractionTestRows(t)
	r := privateAttachmentRoot(t)
	original := r.path + "-moved"
	if e := os.Rename(r.path, original); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := os.Rename(original, r.path); e != nil {
			t.Error(e)
		}
	})
	outside := t.TempDir()
	if e := os.Symlink(outside, r.path); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := os.Remove(r.path); e != nil {
			t.Error(e)
		}
	})
	if e := extractAttachments(context.Background(), r, rows, true, document.DefaultLimits(), s.Close); e != nil {
		t.Fatal(e)
	}
	entries, e := os.ReadDir(outside)
	if e != nil || len(entries) != 0 {
		t.Fatal("pinned root escaped", entries, e)
	}
	for _, row := range rows {
		b, e := os.ReadFile(filepath.Join(original, row.OutputName))
		if e != nil || string(b) != "exact\x00bytes" {
			t.Fatal("missing pinned output", e)
		}
	}
}

func TestAttachmentLateCancellationAndCleanupFault(t *testing.T) {
	for _, fault := range []string{"before-link", "between-links", "unlink", "directory-sync", "link-io"} {
		t.Run(fault, func(t *testing.T) {
			s, rows := extractionTestRows(t)
			r := privateAttachmentRoot(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			link := r.link
			r.link = func(a, b string) error {
				if fault == "link-io" {
					return errors.New("injected link")
				}
				e := link(a, b)
				if fault == "between-links" {
					cancel()
				}
				return e
			}
			remove := r.remove
			if fault == "unlink" {
				r.remove = func(string) error { return errors.New("injected unlink") }
			}
			if fault == "directory-sync" {
				r.sync = func() error { return errors.New("injected directory sync") }
			}
			err := extractAttachments(ctx, r, rows, true, document.DefaultLimits(), func() error {
				e := s.Close()
				if fault == "before-link" {
					cancel()
				}
				return e
			})
			if err == nil {
				t.Fatal("false success")
			}
			code, _ := attachmentError(err)
			if (fault == "unlink" || fault == "directory-sync") && code != "cleanup" {
				t.Fatal(code, err)
			}
			if fault == "between-links" && (rows[0].Status != "published" || rows[1].Status != "failed") {
				t.Fatalf("cancellation results: %+v %+v", rows[0], rows[1])
			}
			entries, e := os.ReadDir(r.path)
			if e != nil {
				t.Fatal(e)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".mailpipe-") {
					if fault != "unlink" {
						t.Fatal("stage leak")
					}
					if e := remove(entry.Name()); e != nil {
						t.Fatal(e)
					}
				} else if b, e := os.ReadFile(filepath.Join(r.path, entry.Name())); e != nil || string(b) != "exact\x00bytes" {
					t.Fatal("partial output", e)
				}
			}
		})
	}
}

type attachmentBrokenReader struct{}

func (attachmentBrokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type attachmentShortWriter struct{}

func (attachmentShortWriter) Write([]byte) (int, error) { return 0, nil }

type attachmentCloseReader struct{ io.Reader }

func (attachmentCloseReader) Close() error { return errors.New("injected close") }

func TestAttachmentCopyAndDigestFailures(t *testing.T) {
	if _, e := attachmentCopy(context.Background(), io.Discard, attachmentBrokenReader{}, 10); !errors.Is(e, io.ErrUnexpectedEOF) {
		t.Fatal(e)
	}
	if _, e := attachmentCopy(context.Background(), attachmentShortWriter{}, strings.NewReader("x"), 10); !errors.Is(e, io.ErrShortWrite) {
		t.Fatal(e)
	}
	if _, _, e := attachmentDigest(context.Background(), func() (io.ReadCloser, error) { return nil, os.ErrNotExist }, 10); !errors.Is(e, os.ErrNotExist) {
		t.Fatal(e)
	}
	if h, _, e := attachmentDigest(context.Background(), func() (io.ReadCloser, error) { return attachmentCloseReader{strings.NewReader("x")}, nil }, 10); e == nil || h != "" {
		t.Fatal(h, e)
	}
}

func TestAttachmentFinalIdentityGuardPreservesStaleFailure(t *testing.T) {
	s, rows := extractionTestRows(t)
	r := privateAttachmentRoot(t)
	err := extractAttachments(context.Background(), r, rows, true, document.DefaultLimits(), func() error {
		return errors.Join(attachmentFail("stale_identity", mailbox.ErrStaleIdentity), s.Close())
	})
	code, coded := attachmentError(err)
	if code != "stale_identity" || agentio.ExitCode(coded) != 3 {
		t.Fatalf("final identity guard: %s %v", code, err)
	}
	entries, e := os.ReadDir(r.path)
	if e != nil || len(entries) != 0 {
		t.Fatalf("stale identity published/leaked: %v %v", entries, e)
	}
}

func TestAttachmentFailureRowsPreserveUntouchedSelections(t *testing.T) {
	rows := attachmentFailureRows(1, []*attachmentRow{{Status: "not_saved"}, {Status: "failed", ErrorCode: "collision"}}, "collision")
	if rows[0].Status != "not_saved" || rows[1].Status != "failed" || rows[1].ErrorCode != "collision" {
		t.Fatalf("misreported untouched selection: %+v %+v", rows[0], rows[1])
	}
	rows = attachmentFailureRows(1, []*attachmentRow{{Status: "saved", SavedPath: "/selected/complete.bin"}, {Status: "not_saved"}}, "cleanup")
	if rows[0].Status != "published" || rows[0].ErrorCode != "cleanup" || rows[1].Status != "not_saved" {
		t.Fatal(rows)
	}
	rows = attachmentFailureRows(1, []*attachmentRow{{Status: "not_saved"}}, "cleanup")
	if rows[0].Status != "failed" || rows[0].ErrorCode != "cleanup" {
		t.Fatal(rows)
	}
}

func TestAttachmentSelectorBindsEveryIdentityComponent(t *testing.T) {
	a := mailbox.Account{ID: "account", ProviderID: "graph"}
	ref := mailbox.MessageRef{AccountID: a.ID, MessageID: "message", Locator: "locator"}
	want := attachmentSelector(a, 1, ref, "raw", "1.2", "decoded")
	for _, change := range []string{"provider", "account", "n", "message", "locator", "raw", "path", "decoded"} {
		b, r, n, raw, path, decoded := a, ref, int64(1), "raw", "1.2", "decoded"
		switch change {
		case "provider":
			b.ProviderID = "other"
		case "account":
			b.ID = "other"
			r.AccountID = b.ID
		case "n":
			n++
		case "message":
			r.MessageID = "other"
		case "locator":
			r.Locator = "other"
		case "raw":
			raw = "other"
		case "path":
			path = "1.3"
		case "decoded":
			decoded = "other"
		}
		if got := attachmentSelector(b, n, r, raw, path, decoded); got == want {
			t.Fatal("selector ignored", change)
		}
	}
	for _, tc := range []struct {
		err  error
		code string
		exit int
	}{{attachmentFail("usage", nil), "usage", 2}, {attachmentFail("no_data", nil), "no_data", 4}, {attachmentFail("stale_identity", nil), "stale_identity", 3}, {document.ErrCleanup, "cleanup", 1}, {document.ErrIncomplete, "incomplete_content", 1}, {context.Canceled, "cancelled", 1}, {errors.New("private canary"), "io_error", 1}} {
		code, e := attachmentError(tc.err)
		if code != tc.code || agentio.ExitCode(e) != tc.exit || strings.Contains(e.Error(), "private canary") {
			t.Fatal(code, e)
		}
	}
}

func TestAttachmentRootAndCopyBoundaries(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if r, err := openAttachmentRoot(dir); err == nil {
		r.Close()
		t.Fatal("accepted broad directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if r, err := openAttachmentRoot(link); err == nil {
		r.Close()
		t.Fatal("accepted root symlink")
	}
	if r, err := openAttachmentRoot(filepath.Join(dir, "missing")); err == nil {
		r.Close()
		t.Fatal("created missing root")
	}
	for _, size := range []int64{0, 3, 4} {
		n, err := attachmentCopy(context.Background(), io.Discard, strings.NewReader("abc"), size)
		if size < 3 && err == nil || size >= 3 && (err != nil || n != 3) {
			t.Fatalf("size %d n %d err %v", size, n, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := attachmentCopy(ctx, io.Discard, strings.NewReader("abc"), 3); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func extractionTestRows(t *testing.T) (*document.Scope, []*attachmentRow) {
	t.Helper()
	s, err := document.NewScope(document.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var rows []*attachmentRow
	for _, name := range []string{"first.bin", "second.bin"} {
		b, err := s.NewBlob(context.Background(), 32, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = b.Write([]byte("exact\x00bytes")); err != nil {
			t.Fatal(err)
		}
		if err = b.Seal(); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256([]byte("exact\x00bytes"))
		rows = append(rows, &attachmentRow{OutputName: name, Extractable: true, DecodedBytes: 11, SHA256: hex.EncodeToString(h[:]), data: b})
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, rows
}

func TestAttachmentPublicationWaitsForCleanupAndReportsLateCollision(t *testing.T) {
	for _, failCleanup := range []bool{true, false} {
		t.Run(strings.ToLower(map[bool]string{true: "cleanup", false: "collision"}[failCleanup]), func(t *testing.T) {
			s, rows := extractionTestRows(t)
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			r, err := openAttachmentRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			original := r.link
			calls := 0
			r.link = func(a, b string) error {
				calls++
				if calls == 2 {
					return os.ErrExist
				}
				return original(a, b)
			}
			err = extractAttachments(context.Background(), r, rows, true, document.DefaultLimits(), func() error {
				if failCleanup {
					return errors.New("cleanup failed")
				}
				return s.Close()
			})
			if err == nil {
				t.Fatal("false success")
			}
			entries, e := os.ReadDir(r.path)
			if e != nil {
				t.Fatal(e)
			}
			if failCleanup && (len(entries) != 0 || calls != 0) {
				t.Fatal("published before cleanup")
			}
			if !failCleanup && (len(entries) != 1 || rows[0].Status != "published" || rows[1].Status != "failed") {
				t.Fatalf("late collision: %v %+v", entries, rows)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".mailpipe-") {
					t.Fatal("stage leaked")
				}
			}
		})
	}
}

func TestAttachmentPublicationRejectsStageSymlinkSubstitution(t *testing.T) {
	s, rows := extractionTestRows(t)
	rows = rows[:1]
	r := privateAttachmentRoot(t)
	const sentinel = "unrelated synthetic bytes"
	if err := os.WriteFile(filepath.Join(r.path, "sentinel.bin"), []byte(sentinel), 0600); err != nil {
		t.Fatal(err)
	}
	link := r.link
	var replacedStage string
	r.link = func(a, b string) error {
		replacedStage = a
		if err := r.root.Remove(a); err != nil {
			return err
		}
		if err := r.root.Symlink("sentinel.bin", a); err != nil {
			return err
		}
		return link(a, b)
	}
	err := extractAttachments(context.Background(), r, rows, true, document.DefaultLimits(), s.Close)
	if err == nil || rows[0].Status == "saved" || rows[0].Status == "published" || rows[0].SavedPath != "" {
		t.Fatalf("substitution claimed verified publication: err=%v row=%+v", err, rows[0])
	}
	for _, name := range []string{replacedStage, rows[0].OutputName} {
		info, e := r.root.Lstat(name)
		if e != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("unexpected replacement removed or misreported: %s %v %v", name, info, e)
		}
	}
	b, e := os.ReadFile(filepath.Join(r.path, "sentinel.bin"))
	if e != nil || string(b) != sentinel {
		t.Fatal("unrelated sentinel changed", e)
	}
	code, _ := attachmentError(err)
	formatted := attachmentFailureRows(1, rows, code)
	if formatted[0].Status != "failed" || formatted[0].SavedPath != "" || !strings.Contains(formatted[0].Detail, "destination could not be verified") {
		t.Fatalf("formatter claimed success: %+v", formatted[0])
	}
}

func TestAttachmentFailedStageStatNeverAuthorizesReplacementCleanup(t *testing.T) {
	s, rows := extractionTestRows(t)
	r := privateAttachmentRoot(t)
	if e := os.WriteFile(filepath.Join(r.path, "sentinel.bin"), []byte("unrelated sentinel"), 0600); e != nil {
		t.Fatal(e)
	}
	open := r.stageOpen
	var replacement string
	r.stageOpen = func(name string) (attachmentFile, error) {
		f, e := open(name)
		if e != nil {
			return nil, e
		}
		replacement = name
		if e = r.root.Remove(name); e != nil {
			f.Close()
			return nil, e
		}
		if e = r.root.Symlink("sentinel.bin", name); e != nil {
			f.Close()
			return nil, e
		}
		return attachmentFaultFile{f, "stat"}, nil
	}
	err := extractAttachments(context.Background(), r, rows, true, document.DefaultLimits(), s.Close)
	if err == nil || rows[0].Status == "saved" || rows[0].SavedPath != "" {
		t.Fatal("failed Stat claimed publication", err, rows[0])
	}
	info, e := r.root.Lstat(replacement)
	if e != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("unrelated replacement removed", e)
	}
	data, e := os.ReadFile(filepath.Join(r.path, "sentinel.bin"))
	if e != nil || string(data) != "unrelated sentinel" {
		t.Fatal("sentinel changed", e)
	}
	code, _ := attachmentError(err)
	if code != "cleanup" {
		t.Fatal("uncertain cleanup not surfaced", code, err)
	}
}

func TestAttachmentPublicationArtifactIntegrityFaults(t *testing.T) {
	for _, fault := range []string{"stage-regular-swap", "stage-bytes", "stage-bytes-preserved-time", "stage-mode", "destination-symlink", "destination-regular-swap", "destination-bytes-preserved-time", "destination-mode", "missing-link", "extra-alias", "earlier-output-change", "cleanup-output-change", "sync-output-change", "link-error-after-effect"} {
		t.Run(fault, func(t *testing.T) {
			s, rows := extractionTestRows(t)
			r := privateAttachmentRoot(t)
			if fault != "earlier-output-change" {
				rows = rows[:1]
			}
			if e := os.WriteFile(filepath.Join(r.path, "sentinel.bin"), []byte("unrelated sentinel"), 0600); e != nil {
				t.Fatal(e)
			}
			identities := make(map[string]os.FileInfo)
			link := r.link
			r.link = func(a, b string) error {
				f, e := r.root.Open(a)
				if e != nil {
					return e
				}
				t.Cleanup(func() { f.Close() })
				info, e := f.Stat()
				if e != nil {
					return e
				}
				identities[b] = info
				switch fault {
				case "stage-regular-swap":
					if e = r.root.Remove(a); e != nil {
						return e
					}
					if e = os.WriteFile(filepath.Join(r.path, a), []byte("exact\x00bytes"), 0600); e != nil {
						return e
					}
				case "stage-bytes":
					if e = os.WriteFile(filepath.Join(r.path, a), []byte("wrong\x00bytes"), 0600); e != nil {
						return e
					}
				case "stage-bytes-preserved-time":
					if e = os.WriteFile(filepath.Join(r.path, a), []byte("wrong\x00bytes"), 0600); e != nil {
						return e
					}
					if e = os.Chtimes(filepath.Join(r.path, a), info.ModTime(), info.ModTime()); e != nil {
						return e
					}
				case "stage-mode":
					if e = os.Chmod(filepath.Join(r.path, a), 0644); e != nil {
						return e
					}
				case "missing-link":
					return nil
				case "extra-alias":
					if e = r.root.Link(a, "unexpected-alias.bin"); e != nil {
						return e
					}
				case "earlier-output-change":
					if b == rows[1].OutputName {
						if e = os.WriteFile(filepath.Join(r.path, rows[0].OutputName), []byte("wrong\x00bytes"), 0600); e != nil {
							return e
						}
					}
				}
				if e = link(a, b); e != nil {
					return e
				}
				switch fault {
				case "destination-symlink":
					if e = r.root.Remove(b); e != nil {
						return e
					}
					return r.root.Symlink("sentinel.bin", b)
				case "destination-regular-swap":
					if e = r.root.Remove(b); e != nil {
						return e
					}
					return os.WriteFile(filepath.Join(r.path, b), []byte("exact\x00bytes"), 0600)
				case "destination-mode":
					return os.Chmod(filepath.Join(r.path, b), 0644)
				case "destination-bytes-preserved-time":
					if e = os.WriteFile(filepath.Join(r.path, b), []byte("wrong\x00bytes"), 0600); e != nil {
						return e
					}
					return os.Chtimes(filepath.Join(r.path, b), info.ModTime(), info.ModTime())
				case "link-error-after-effect":
					return errors.New("link acknowledgement uncertain")
				}
				return nil
			}
			remove := r.remove
			if fault == "cleanup-output-change" {
				r.remove = func(name string) error {
					e := remove(name)
					if e != nil {
						return e
					}
					return os.WriteFile(filepath.Join(r.path, rows[0].OutputName), []byte("wrong\x00bytes"), 0600)
				}
			}
			sync := r.sync
			if fault == "sync-output-change" {
				r.sync = func() error {
					if e := os.WriteFile(filepath.Join(r.path, rows[0].OutputName), []byte("wrong\x00bytes"), 0600); e != nil {
						return e
					}
					return sync()
				}
			}
			err := extractAttachments(context.Background(), r, rows, true, document.DefaultLimits(), s.Close)
			if err == nil {
				t.Fatal("publication integrity fault succeeded")
			}
			for _, row := range rows {
				if row.Status == "saved" {
					t.Fatalf("fault reported saved: %+v", row)
				}
				if row.Status == "published" || row.SavedPath != "" {
					info, e := r.root.Lstat(row.OutputName)
					if e != nil || !attachmentOwned(info, false) || !os.SameFile(identities[row.OutputName], info) {
						t.Fatalf("unverified identity/mode reported published: %+v %v", row, e)
					}
					data, e := os.ReadFile(filepath.Join(r.path, row.OutputName))
					h := sha256.Sum256(data)
					if e != nil || hex.EncodeToString(h[:]) != row.SHA256 || int64(len(data)) != row.DecodedBytes {
						t.Fatalf("unverified bytes reported published: %+v %v", row, e)
					}
				}
			}
			if fault == "link-error-after-effect" && rows[0].Status != "published" {
				t.Fatalf("complete uncertain outcome not reported: %+v", rows[0])
			}
			if fault == "earlier-output-change" && (rows[0].SavedPath != "" || rows[1].Status != "published") {
				t.Fatalf("partial integrity result: %+v %+v", rows[0], rows[1])
			}
			b, e := os.ReadFile(filepath.Join(r.path, "sentinel.bin"))
			if e != nil || string(b) != "unrelated sentinel" {
				t.Fatal("sentinel changed", e)
			}
		})
	}
}
