package document

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"io"
	"strings"
)

// Source is complete, identity-bound MIME. It is never published on fetch error.
type Source struct {
	*Blob
	Ref     mailbox.MessageRef
	Account mailbox.Account
	leave   func()
	ctx     context.Context
}

func OpenSource(ctx context.Context, src MIMEReader, ref mailbox.MessageRef, s *Scope) (out *Source, err error) {
	if src == nil || s == nil {
		return nil, incomplete("source and scope required", nil)
	}
	if e := src.Account().Validate(); e != nil {
		return nil, e
	}
	if e := ref.Validate(src.Account().ID); e != nil {
		return nil, e
	}
	out, err = StreamSource(ctx, s, func(w io.Writer) error { return src.WriteMIME(ctx, ref, w) })
	if err == nil {
		out.Ref = ref
		out.Account = src.Account()
	}
	return out, err
}

// StreamSource collects a trusted source callback once into a complete private
// spool. Callback and sticky writer errors both prevent publication.
func StreamSource(ctx context.Context, s *Scope, write func(io.Writer) error) (out *Source, err error) {
	ctx, cancel := context.WithTimeout(ctx, s.limits.Timeout)
	defer cancel()
	leave, e := s.Enter(ctx)
	if e != nil {
		return nil, e
	}
	b, e := s.NewBlob(ctx, s.limits.RawBytes, 0)
	if e != nil {
		leave()
		return nil, e
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, b.Close())
			leave()
			out = nil
		}
	}()
	w := &contextWriter{ctx: ctx, w: b}
	err = write(w)
	err = errors.Join(err, w.err, ctx.Err())
	if err == nil && w.known && b.Size() != w.announced {
		err = incomplete("announced source differs from actual bytes", nil)
	}
	if err == nil {
		err = b.Seal()
	}
	if err != nil {
		return nil, incomplete("source incomplete", err)
	}
	return &Source{Blob: b, leave: leave, ctx: ctx}, nil
}
func (s *Source) Close() error {
	parent := s.ctx
	if parent == nil {
		parent = context.Background()
	}
	e := s.Blob.CloseContext(parent)
	if s.leave != nil {
		s.leave()
		s.leave = nil
	}
	return e
}

type sourceReader struct {
	io.ReadCloser
	scope *Scope
}

func (s *Source) Open() (io.ReadCloser, error) {
	r, e := s.Blob.Open()
	if e != nil {
		return nil, e
	}
	return &sourceReader{r, s.Blob.s}, nil
}

type rawBoundReader struct {
	r      io.Reader
	n, max int64
}

func (r *rawBoundReader) Read(b []byte) (int, error) {
	n, e := r.r.Read(b)
	if int64(n) > r.max-r.n {
		return 0, incomplete("raw byte limit exceeded", nil)
	}
	r.n += int64(n)
	return n, e
}

type contextWriter struct {
	ctx       context.Context
	w         io.Writer
	err       error
	announced int64
	known     bool
}

// AnnounceSourceSize is an optional cooperating transport seam. It never
// substitutes an announcement for actual byte and complete-response checks.
func (w *contextWriter) AnnounceSourceSize(n int64) error {
	if w.err != nil {
		return w.err
	}
	a, ok := w.w.(interface{ AnnounceSourceSize(int64) error })
	if !ok {
		return nil
	}
	w.err = a.AnnounceSourceSize(n)
	if w.err == nil {
		w.announced = n
		w.known = true
	}
	return w.err
}
func (w *contextWriter) Write(p []byte) (int, error) {
	if w.err == nil {
		w.err = w.ctx.Err()
	}
	if w.err != nil {
		return 0, w.err
	}
	n, e := w.w.Write(p)
	if e == nil && n != len(p) {
		e = io.ErrShortWrite
	}
	w.err = e
	return n, e
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}
func copyChunks(ctx context.Context, w io.Writer, r io.Reader) error {
	buf := make([]byte, 32768)
	_, e := io.CopyBuffer(&contextWriter{ctx: ctx, w: w}, contextReader{ctx, r}, buf)
	return e
}

// ReadText validates and cleans the entire MIME before returning complete text.
func ReadText(ctx context.Context, src MIMEReader, ref mailbox.MessageRef, s *Scope) (text string, err error) {
	ctx, cancel := context.WithTimeout(ctx, s.limits.Timeout)
	defer cancel()
	source, e := OpenSource(ctx, src, ref, s)
	if e != nil {
		return "", e
	}
	defer func() {
		err = errors.Join(err, source.Close())
		if err != nil {
			text = ""
		}
	}()
	r, e := source.Open()
	if e != nil {
		return "", e
	}
	d, e := ParseReader(ctx, r, s)
	e = errors.Join(e, r.Close())
	if e != nil {
		if d != nil {
			e = errors.Join(e, d.Close())
		}
		return "", e
	}
	text, err = d.Text(ctx)
	err = errors.Join(err, d.Close())
	return text, err
}

// Header returns the original bounded header block with LF line endings, only
// after Source has proved the entire raw message complete. It reserves display
// materialization capacity independently of source/transport scratch.
func (s *Source) Header(ctx context.Context) (out string, err error) {
	release, e := s.Blob.s.reserveMemory(s.Blob.s.limits.HeaderBytes * 4)
	if e != nil {
		return "", e
	}
	defer release()
	r, e := s.Open()
	if e != nil {
		return "", e
	}
	defer func() {
		err = errors.Join(err, r.Close())
		if err != nil {
			out = ""
		}
	}()
	br := bufio.NewReaderSize(contextReader{ctx, r}, 4096)
	var b strings.Builder
	var count int64
	lineBytes := 0
	for {
		line, e := br.ReadSlice('\n')
		lineBytes += len(line)
		if int64(len(line)) > s.Blob.s.limits.HeaderBytes-count {
			return "", incomplete("header byte limit exceeded", nil)
		}
		count += int64(len(line))
		if e != nil && e != bufio.ErrBufferFull {
			return "", incomplete("incomplete header block", e)
		}
		if e == nil && lineBytes <= 2 && (bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n"))) {
			break
		}
		b.Write(line)
		if e == nil {
			lineBytes = 0
		}
	}

	return strings.ReplaceAll(b.String(), "\r\n", "\n"), nil
}
