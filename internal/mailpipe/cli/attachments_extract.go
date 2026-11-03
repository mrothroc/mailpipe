package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mrothroc/mailpipe/pkg/document"
)

func attachmentName(p document.InspectablePart) (string, error) {
	for _, c := range p.Path {
		if c != '.' && (c < '0' || c > '9') {
			return "", attachmentFail("unsafe_path", nil)
		}
	}
	if p.Path == "" {
		return "", attachmentFail("unsafe_path", nil)
	}
	prefix := "part-" + strings.ReplaceAll(p.Path, ".", "-")
	name := p.Filename
	if name == "" {
		ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp", "application/pdf": ".pdf", "message/rfc822": ".eml"}[p.ContentType]
		if ext == "" {
			ext = ".bin"
		}
		return prefix + ext, nil
	}
	if !utf8.ValidString(name) || strings.TrimSpace(name) == "" || name == "." || name == ".." || filepath.IsAbs(name) || strings.ContainsAny(name, "/\\:") {
		return "", attachmentFail("unsafe_path", nil)
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == 0x061c || r == 0x200e || r == 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 {
			return "", attachmentFail("unsafe_path", nil)
		}
	}
	name = prefix + "-" + name
	if len(name) > 240 || !filepath.IsLocal(name) {
		return "", attachmentFail("unsafe_path", nil)
	}
	return name, nil
}

type attachmentFile interface {
	io.ReadWriteCloser
	Fd() uintptr
	Stat() (os.FileInfo, error)
	Sync() error
}

// Per-operation functions permit fault tests without shared mutable hooks or
// production bypass flags. Every operation remains relative to root.
type attachmentRoot struct {
	root      *os.Root
	path      string
	link      func(string, string) error
	remove    func(string) error
	stageOpen func(string) (attachmentFile, error)
	stageRead func(string) (io.ReadCloser, error)
	sync      func() error
}

type attachmentArtifact struct {
	name      string
	info      os.FileInfo
	pin       *os.File
	attempted bool
	linkErr   error
}

func attachmentOwned(info os.FileInfo, directory bool) bool {
	if info == nil || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) {
		return false
	}
	if directory {
		return info.IsDir() && info.Mode().Perm() == 0700
	}
	return info.Mode().IsRegular() && info.Mode().Perm() == 0600 && st.Nlink == 1
}

func openAttachmentRoot(path string) (*attachmentRoot, error) {
	absolute, e := filepath.Abs(path)
	if e != nil {
		return nil, attachmentFail("unsafe_path", e)
	}
	info, e := os.Lstat(absolute)
	if e != nil || !attachmentOwned(info, true) {
		return nil, attachmentFail("unsafe_path", e)
	}
	canonical, e := filepath.EvalSymlinks(absolute)
	if e != nil {
		return nil, attachmentFail("unsafe_path", e)
	}
	root, e := os.OpenRoot(canonical)
	if e != nil {
		return nil, attachmentFail("unsafe_path", e)
	}
	f, e := root.Open(".")
	var pinned os.FileInfo
	if e == nil {
		pinned, e = f.Stat()
		e = errors.Join(e, f.Close())
	}
	if e != nil || !attachmentOwned(pinned, true) || !os.SameFile(info, pinned) {
		return nil, attachmentFail("unsafe_path", errors.Join(e, root.Close()))
	}
	r := &attachmentRoot{root: root, path: absolute, link: root.Link, remove: root.Remove}
	r.stageOpen = func(name string) (attachmentFile, error) {
		return root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	}
	r.stageRead = func(name string) (io.ReadCloser, error) { return root.Open(name) }
	r.sync = func() (err error) {
		f, e := root.Open(".")
		if e != nil {
			return e
		}
		return errors.Join(f.Sync(), f.Close())
	}
	return r, nil
}
func (r *attachmentRoot) Close() error { return r.root.Close() }

func attachmentCopy(ctx context.Context, w io.Writer, r io.Reader, max int64) (int64, error) {
	buffer := make([]byte, 32768)
	var total int64
	for {
		if e := ctx.Err(); e != nil {
			return total, e
		}
		n, re := r.Read(buffer)
		if int64(n) > max-total {
			return total, attachmentFail("incomplete_content", document.ErrIncomplete)
		}
		if n > 0 {
			written, e := w.Write(buffer[:n])
			total += int64(written)
			if e != nil {
				return total, e
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if re != nil {
			if re == io.EOF {
				return total, nil
			}
			return total, re
		}
	}
}

func stageAttachment(ctx context.Context, r *attachmentRoot, name string, row *attachmentRow, max int64, artifact *attachmentArtifact) (result error) {
	f, e := r.stageOpen(name)
	if e != nil {
		return e
	}
	artifact.name = name
	closed := false
	defer func() {
		if !closed {
			result = errors.Join(result, f.Close())
		}
	}()
	info, e := f.Stat()
	if e != nil || !attachmentOwned(info, false) {
		// A failed high-level Stat still fails staging. Independently inspect
		// the created descriptor solely to authorize cleanup, never publication.
		var st syscall.Stat_t
		if syscall.Fstat(int(f.Fd()), &st) == nil {
			if current, pathErr := r.root.Lstat(name); pathErr == nil && attachmentOwned(current, false) {
				if named, ok := current.Sys().(*syscall.Stat_t); ok && named.Dev == st.Dev && named.Ino == st.Ino && named.Uid == st.Uid {
					var cleanupProofErr error
					artifact.pin, artifact.info, cleanupProofErr = pinAttachmentStage(r, name, current)
					e = errors.Join(e, cleanupProofErr)
				}
			}
		}
		return attachmentFail("unsafe_path", e)
	}
	artifact.info = info
	artifact.pin, artifact.info, e = pinAttachmentStage(r, name, info)
	if e != nil {
		return e
	}
	input, e := row.data.Open()
	if e != nil {
		return e
	}
	n, e := attachmentCopy(ctx, f, input, max)
	e = errors.Join(e, input.Close())
	if e == nil && n != row.DecodedBytes {
		e = attachmentFail("incomplete_content", nil)
	}
	if e == nil {
		e = f.Sync()
	}
	if e == nil {
		// Retain a real descriptor through publication so removing the stage
		// pathname cannot free/reuse the inode we verified.
		var pinned os.FileInfo
		pinned, e = artifact.pin.Stat()
		if e == nil && (!attachmentOwned(pinned, false) || !os.SameFile(info, pinned)) {
			e = attachmentFail("unsafe_path", nil)
		}
		if e == nil {
			artifact.info = pinned
		}
	}
	e = errors.Join(e, f.Close())
	closed = true
	if e != nil {
		return e
	}
	// Check the inode again before reopening; rooted containment alone permits
	// contained symlinks, whereas stage files must never be symlinks.
	current, e := r.root.Lstat(name)
	if e != nil || !attachmentOwned(current, false) || !os.SameFile(info, current) {
		return attachmentFail("unsafe_path", e)
	}
	h, size, e := attachmentDigest(ctx, func() (io.ReadCloser, error) {
		reader, err := r.stageRead(name)
		if err != nil {
			return nil, err
		}
		stat, ok := reader.(interface{ Stat() (os.FileInfo, error) })
		if !ok {
			return nil, errors.Join(attachmentFail("unsafe_path", nil), reader.Close())
		}
		opened, err := stat.Stat()
		if err != nil || !attachmentOwned(opened, false) || !os.SameFile(info, opened) {
			return nil, errors.Join(attachmentFail("unsafe_path", err), reader.Close())
		}
		return reader, nil
	}, max)
	if e != nil {
		return e
	}
	if h != row.SHA256 || size != row.DecodedBytes {
		return attachmentFail("incomplete_content", nil)
	}
	pinned, e := artifact.pin.Stat()
	current, pathErr := r.root.Lstat(name)
	if e != nil || pathErr != nil || !attachmentArtifactMatches(pinned, artifact.info, row, 1) || !attachmentArtifactMatches(current, artifact.info, row, 1) {
		return attachmentFail("unsafe_path", errors.Join(e, pathErr))
	}
	return nil
}

func pinAttachmentStage(r *attachmentRoot, name string, expected os.FileInfo) (*os.File, os.FileInfo, error) {
	f, e := r.root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, nil, e
	}
	info, e := f.Stat()
	if e != nil || !attachmentOwned(info, false) || !os.SameFile(info, expected) {
		return nil, nil, errors.Join(attachmentFail("unsafe_path", e), f.Close())
	}
	return f, info, nil
}

func attachmentArtifactMatches(info, expected os.FileInfo, row *attachmentRow, maxLinks uint64) bool {
	if info == nil || expected == nil || !os.SameFile(info, expected) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Size() != row.DecodedBytes || !info.ModTime().Equal(expected.ModTime()) {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid()) && st.Nlink >= 1 && uint64(st.Nlink) <= maxLinks
}

// Follow the filing writers' final-link readback through a pinned directory.
// Extraction additionally requires the staged inode, 0600 mode and exact digest;
// its content scope has already closed before publication.
func verifyAttachmentArtifact(ctx context.Context, r *attachmentRoot, name string, row *attachmentRow, expected os.FileInfo, maxLinks uint64, maxBytes int64) (result error) {
	before, e := r.root.Lstat(name)
	if e != nil {
		return e
	}
	if !attachmentArtifactMatches(before, expected, row, maxLinks) {
		return attachmentFail("unsafe_path", nil)
	}
	f, e := r.root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return e
	}
	defer func() { result = errors.Join(result, f.Close()) }()
	opened, e := f.Stat()
	if e != nil || !attachmentArtifactMatches(opened, expected, row, maxLinks) {
		return attachmentFail("unsafe_path", e)
	}
	h, size, e := attachmentDigest(ctx, func() (io.ReadCloser, error) {
		// The helper owns only this reader wrapper; f stays pinned for the
		// post-read descriptor/path observations below.
		return io.NopCloser(f), nil
	}, maxBytes)
	if e != nil {
		return e
	}
	if h != row.SHA256 || size != row.DecodedBytes {
		return attachmentFail("incomplete_content", nil)
	}
	after, e := f.Stat()
	current, pathErr := r.root.Lstat(name)
	if e != nil || pathErr != nil || !attachmentArtifactMatches(after, expected, row, maxLinks) || !attachmentArtifactMatches(current, expected, row, maxLinks) {
		return attachmentFail("unsafe_path", errors.Join(e, pathErr))
	}
	return nil
}

func extractAttachments(ctx context.Context, r *attachmentRoot, rows []*attachmentRow, execute bool, l document.Limits, closeContent func() error) (result error) {
	for _, row := range rows {
		row.Status = "not_saved"
	}
	fail := func(row *attachmentRow, code string, e error) error {
		row.Status, row.ErrorCode = "failed", code
		return attachmentFail(code, e)
	}
	var total int64
	for _, row := range rows {
		if !row.Extractable || row.OutputName == "" || !filepath.IsLocal(row.OutputName) || filepath.Base(row.OutputName) != row.OutputName {
			return fail(row, "unsafe_path", nil)
		}
		row.PlannedPath = filepath.Join(r.path, row.OutputName)
		if row.DecodedBytes < 0 || row.DecodedBytes > l.PartBytes || row.DecodedBytes > l.DecodedBytes-total {
			return fail(row, "incomplete_content", nil)
		}
		total += row.DecodedBytes
		if _, e := r.root.Lstat(row.OutputName); e == nil {
			return fail(row, "collision", nil)
		} else if !os.IsNotExist(e) {
			return fail(row, "unsafe_path", e)
		}
	}
	if !execute {
		if e := closeContent(); e != nil {
			return e
		}
		for _, row := range rows {
			row.Status = "ready"
		}
		return ctx.Err()
	}
	artifacts := make([]attachmentArtifact, len(rows))
	cleanup := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var err error
		for i := range artifacts {
			a := &artifacts[i]
			if a.name == "" {
				continue
			}
			if e := cleanupCtx.Err(); e != nil {
				return errors.Join(err, e)
			}
			current, e := r.root.Lstat(a.name)
			if os.IsNotExist(e) {
				a.name = ""
				continue
			}
			if e != nil || a.info == nil || !os.SameFile(current, a.info) {
				// An unknown replacement is not ours to remove, even if it occupies
				// the unpredictable name we originally created.
				err = errors.Join(err, attachmentFail("unsafe_path", e))
				continue
			}
			if e = r.remove(a.name); e != nil {
				err = errors.Join(err, e)
			} else {
				a.name = ""
			}
		}
		return err
	}
	defer func() {
		if e := cleanup(); e != nil {
			result = attachmentFail("cleanup", errors.Join(result, e))
		}
		attempted := false
		for i := range artifacts {
			attempted = attempted || artifacts[i].attempted
		}
		if attempted {
			if e := r.sync(); e != nil {
				result = attachmentFail("cleanup", errors.Join(result, e))
			}
		}
		// Reconcile every attempted destination after all link, cleanup and sync
		// operations, including earlier outputs on a late batch failure. Cancellation
		// cannot make us label an unaudited destination as a complete publication.
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		for i := range artifacts {
			a, row := &artifacts[i], rows[i]
			if !a.attempted {
				continue
			}
			e := verifyAttachmentArtifact(auditCtx, r, row.OutputName, row, a.info, 1, l.PartBytes)
			if e == nil {
				row.Status, row.SavedPath = "published", row.PlannedPath
				row.artifactUnverified = false
			} else if os.IsNotExist(e) && a.linkErr != nil {
				// A failed link with no destination has no artifact to claim.
				row.SavedPath = ""
			} else {
				code, _ := attachmentError(e)
				row.Status, row.SavedPath, row.ErrorCode, row.Detail = "failed", "", code, "destination could not be verified; inspect planned_path"
				row.artifactUnverified = true
				result = errors.Join(result, e)
			}
		}
		for i := range artifacts {
			if artifacts[i].pin != nil {
				if e := artifacts[i].pin.Close(); e != nil {
					result = attachmentFail("cleanup", errors.Join(result, e))
				}
			}
		}
		if result == nil {
			for _, row := range rows {
				row.Status = "saved"
			}
		}
	}()
	for i, row := range rows {
		name := ".mailpipe-attachment-" + rand.Text()
		if e := stageAttachment(ctx, r, name, row, l.PartBytes, &artifacts[i]); e != nil {
			code, _ := attachmentError(e)
			return fail(row, code, e)
		}
	}
	if e := closeContent(); e != nil {
		return e
	}
	for i, row := range rows {
		if e := ctx.Err(); e != nil {
			return fail(row, "cancelled", e)
		}
		a := &artifacts[i]
		a.attempted = true
		a.linkErr = r.link(a.name, row.OutputName)
		if a.linkErr != nil {
			code := "io_error"
			if os.IsExist(a.linkErr) {
				code = "collision"
			}
			return fail(row, code, a.linkErr)
		}
		// This is reconciliation of an already attempted local write. Finish
		// its bounded readback even if cancellation arrived during Link; the
		// next iteration still refuses any further publication on cancellation.
		verifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		e := verifyAttachmentArtifact(verifyCtx, r, row.OutputName, row, a.info, 2, l.PartBytes)
		cancel()
		if e != nil {
			code, _ := attachmentError(e)
			row.Status, row.ErrorCode, row.Detail = "failed", code, "destination could not be verified; inspect planned_path"
			row.artifactUnverified = true
			return e
		}
	}
	return nil
}
