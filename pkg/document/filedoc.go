package document

import (
	"context"
	"errors"
	"html"
	"io"
	"strings"
	"time"
)

// FilePart opens exact decoded attachment/inline bytes without materializing them.
type FilePart struct {
	Filename, ContentType, CID string
	Data                       *Blob
}
type fragment struct {
	data *Blob
	html bool
}
type bodies struct{ read, render []fragment }

// FileDoc owns its decoded spools. Close before accepting any downstream result.
type FileDoc struct {
	MessageID, From, Subject string
	Date                     time.Time
	Attachments              []FilePart
	Inline                   map[string]FilePart
	scope                    *Scope
	files                    []*Blob
	body                     bodies
	release                  func()
	ctx                      context.Context
}

func (d *FileDoc) Close() error {
	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := cleanupContext(parent)
	defer cancel()
	var err error
	for i, f := range d.files {
		if e := ctx.Err(); e != nil {
			err = errors.Join(err, incomplete("message cleanup deadline exceeded", errors.Join(ErrCleanup, e)))
			d.scope.mu.Lock()
			d.scope.poisoned = true
			d.scope.mu.Unlock()
			for _, rest := range d.files[i:] {
				rest.mu.Lock()
				if !rest.removed {
					rest.pending = true
					rest.queue()
				}
				rest.mu.Unlock()
			}
			break
		}
		err = errors.Join(err, f.close(ctx))
	}
	if d.release != nil {
		d.release()
		d.release = nil
	}
	return err
}
func (d *FileDoc) PDFs() []FilePart {
	var a []FilePart
	for _, p := range d.Attachments {
		if p.ContentType == "application/pdf" || strings.HasSuffix(strings.ToLower(p.Filename), ".pdf") {
			a = append(a, p)
		}
	}
	return a
}

func materializedBytes(ctx context.Context, s *Scope, b *Blob, maxBytes int64) (out []byte, release func(), err error) {
	n := b.Size()
	if n > maxBytes {
		return nil, nil, incomplete("materialization limit exceeded", nil)
	}
	release, e := s.reserveMemory(n * 2)
	if e != nil {
		return nil, nil, e
	}
	defer func() {
		if err != nil {
			release()
			release = nil
			out = nil
		}
	}()
	r, e := b.Open()
	if e != nil {
		return nil, release, e
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	out = make([]byte, int(n))
	if n <= s.limits.SmallBytes {
		_, err = io.ReadFull(contextReader{ctx, r}, out)
	} else {
		for offset := 0; offset < len(out); {
			end := min(offset+32768, len(out))
			var count int
			count, err = io.ReadFull(contextReader{ctx, r}, out[offset:end])
			offset += count
			if err != nil {
				break
			}
		}
	}
	return out, release, err
}
func materialize(ctx context.Context, s *Scope, b *Blob, maxBytes int64) ([]byte, error) {
	data, release, e := materializedBytes(ctx, s, b, maxBytes)
	if e != nil {
		return nil, e
	}
	defer release()
	return data, nil
}
func materializeString(ctx context.Context, s *Scope, b *Blob, maxBytes int64) (string, error) {
	data, release, e := materializedBytes(ctx, s, b, maxBytes)
	if e != nil {
		return "", e
	}
	defer release()
	return string(data), nil
}
func (d *FileDoc) Text(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, d.scope.limits.Timeout)
	defer cancel()
	b, e := d.scope.NewBlob(ctx, d.scope.limits.TextBytes, 0)
	if e != nil {
		return "", e
	}
	var err error
	for i, f := range d.body.read {
		if i > 0 {
			_, err = b.Write([]byte("\n\n"))
		}
		if err != nil {
			break
		}
		r, e := f.data.Open()
		if e != nil {
			err = e
			break
		}
		if f.html {
			err = HTMLReadable(ctx, r, b, d.scope.limits)
		} else {
			err = copyChunks(ctx, b, r)
		}
		err = errors.Join(err, r.Close())
		if err != nil {
			break
		}
	}
	if err == nil {
		err = b.Seal()
	}
	var text string
	if err == nil {
		text, err = materializeString(ctx, d.scope, b, d.scope.limits.TextBytes)
	}
	err = errors.Join(err, b.Close())
	if err != nil {
		return "", err
	}
	return text, nil
}
func (d *FileDoc) Materialize(ctx context.Context) (Doc, error) {
	out := Doc{MessageID: d.MessageID, From: d.From, Subject: d.Subject, Date: d.Date, inline: map[string]Part{}}
	var err error
	out.Text, err = d.Text(ctx)
	if err != nil {
		return Doc{}, err
	}
	total := int64(len(out.Text))
	get := func(b *Blob) ([]byte, error) {
		if b.Size() > d.scope.limits.CompatibilityBytes-total {
			return nil, incomplete("compatibility limit exceeded", nil)
		}
		data, e := materialize(ctx, d.scope, b, d.scope.limits.CompatibilityBytes-total)
		total += int64(len(data))
		return data, e
	}
	for _, f := range d.body.render {
		if !f.html && len(d.body.render) == 1 {
			continue
		}
		data, e := get(f.data)
		if e != nil {
			return Doc{}, e
		}
		if f.html {
			out.HTML += string(data)
		} else if len(d.body.render) > 1 {
			out.HTML += "<pre>" + html.EscapeString(string(data)) + "</pre>"
		}
	}
	for _, p := range d.Attachments {
		data, e := get(p.Data)
		if e != nil {
			return Doc{}, e
		}
		out.Attachments = append(out.Attachments, Part{p.Filename, p.ContentType, data})
	}
	for id, p := range d.Inline {
		data, e := get(p.Data)
		if e != nil {
			return Doc{}, e
		}
		out.inline[id] = Part{p.Filename, p.ContentType, data}
	}
	return out, nil
}
