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

func stageAttachment(ctx context.Context, r *attachmentRoot, name string, row *attachmentRow, max int64) (result error) {
	f, e := r.stageOpen(name)
	if e != nil {
		return e
	}
	closed := false
	defer func() {
		if !closed {
			result = errors.Join(result, f.Close())
		}
	}()
	info, e := f.Stat()
	if e != nil || !attachmentOwned(info, false) {
		return attachmentFail("unsafe_path", e)
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
	stages := make([]string, len(rows))
	cleanup := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var err error
		for i, name := range stages {
			if name != "" {
				if e := cleanupCtx.Err(); e != nil {
					return errors.Join(err, e)
				}
				if e := r.remove(name); e != nil {
					err = errors.Join(err, e)
				} else {
					stages[i] = ""
				}
			}
		}
		return err
	}
	defer func() {
		if e := cleanup(); e != nil {
			result = attachmentFail("cleanup", errors.Join(result, e))
		}
	}()
	for i, row := range rows {
		name := ".mailpipe-attachment-" + rand.Text()
		// A failed exclusive create must not cause cleanup to remove an entry
		// we did not create. Register only after stageOpen succeeds.
		open := r.stageOpen
		r.stageOpen = func(name string) (attachmentFile, error) {
			f, e := open(name)
			if e == nil {
				stages[i] = name
			}
			return f, e
		}
		e := stageAttachment(ctx, r, name, row, l.PartBytes)
		r.stageOpen = open
		if e != nil {
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
		if e := r.link(stages[i], row.OutputName); e != nil {
			code := "io_error"
			if os.IsExist(e) {
				code = "collision"
			}
			return fail(row, code, e)
		}
		row.Status, row.SavedPath = "published", row.PlannedPath
	}
	if e := cleanup(); e != nil {
		return attachmentFail("cleanup", e)
	}
	if e := r.sync(); e != nil {
		return attachmentFail("cleanup", e)
	}
	for _, row := range rows {
		row.Status = "saved"
	}
	return nil
}
