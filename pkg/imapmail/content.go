package imapmail

import (
	"context"
	"errors"
	"io"

	"github.com/mrothroc/mailpipe/pkg/document"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// ContentMetadataLoader derives complete envelope/flag/attachment metadata from
// bounded authoritative protocol evidence. It never downloads source for preview.
type ContentMetadataLoader struct{}

// StructureAccess is the optional scoped metadata capability. Existing custom
// ReadAccess collaborators remain source compatible.
type StructureAccess interface {
	ReadStructure(context.Context) (ContentStructure, error)
}
type ContentStructure struct {
	HasAttachments bool
	Parts          int
}

func (ContentMetadataLoader) Load(ctx context.Context, o Occurrence, w WireMetadata, a ReadAccess) (mailbox.Metadata, error) {
	access, ok := a.(StructureAccess)
	if !ok {
		return mailbox.Metadata{}, failure("bounded body structure required")
	}
	structure, e := access.ReadStructure(ctx)
	if e != nil {
		return mailbox.Metadata{}, e
	}
	recipients := append([]mailbox.Address(nil), w.Envelope.To...)
	recipients = append(recipients, w.Envelope.Cc...)
	recipients = append(recipients, w.Envelope.Bcc...)
	m := mailbox.Metadata{InternetMessageID: w.Envelope.MessageID, Subject: w.Envelope.Subject, Recipients: recipients, Received: w.InternalDate, HasAttachments: structure.HasAttachments, IsRead: hasFlag(w.Flags, `\Seen`)}
	if len(w.Envelope.From) > 0 {
		m.Sender = w.Envelope.From[0]
	}
	return m, nil
}
func (c *Client) Collections(ctx context.Context) ([]mailbox.Collection, error) {
	d, e := c.Discover(ctx)
	if e != nil {
		return nil, e
	}
	out := make([]mailbox.Collection, len(d.Folders))
	for i, f := range d.Folders {
		out[i] = f.Collection
	}
	return out, nil
}
func (c *Client) WriteMIME(ctx context.Context, ref mailbox.MessageRef, w io.Writer) error {
	return c.ReadSource(ctx, ref, w)
}
func (c *Client) contentScope(ctx context.Context) (*document.Scope, error) {
	if s := document.ScopeFrom(ctx); s != nil {
		return s, nil
	}
	c.mu.Lock()
	s := c.content
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, failure("client closed")
	}
	if s != nil {
		return s, nil
	}
	candidate, e := document.NewScope(c.cfg.ContentLimits)
	if e != nil {
		return nil, e
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.Join(failure("client closed"), candidate.Close())
	}
	if c.content == nil {
		c.content = candidate
		s = candidate
	} else {
		s = c.content
	}
	c.mu.Unlock()
	if s != candidate {
		if e = candidate.Close(); e != nil {
			return nil, e
		}
	}
	return s, nil
}
func (c *Client) Text(ctx context.Context, ref mailbox.MessageRef) (string, error) {
	if _, _, e := c.decode(ref); e != nil {
		return "", e
	}
	s, e := c.contentScope(ctx)
	if e != nil {
		return "", e
	}
	return document.ReadText(ctx, c, ref, s)
}
func (c *Client) Metadata(ctx context.Context, ref mailbox.MessageRef) (mailbox.Metadata, error) {
	state, e := c.Inspect(ctx, ref)
	return state.Metadata, e
}

// withOccurrence uses the existing lease and scoped access; it never nests a
// complete synchronization pass or reenters a public client method.
func (c *Client) withOccurrence(ctx context.Context, ref mailbox.MessageRef, consume func(context.Context, Occurrence, WireMetadata, *scopedRead) error) (err error) {
	o, f, e := c.decode(ref)
	if e != nil {
		return e
	}
	ctx, done, e := c.operation(ctx)
	if e != nil {
		return e
	}
	defer done()
	s, e := c.connect(ctx)
	if e != nil {
		return safe("content connection failed", e)
	}
	defer func() {
		if e := s.close(); e != nil {
			err = safe("content cleanup failed", errors.Join(err, e))
		}
	}()
	selection, e := s.examine(f)
	if e != nil {
		return e
	}
	if selection.validity != o.UIDValidity {
		return safe("content epoch changed", mailbox.ErrStaleIdentity)
	}
	var wire WireMetadata
	seen := false
	e = s.fetch(o.UID, o.UID, nil, 0, func(v fetched) error {
		if seen {
			return failure("duplicate content occurrence")
		}
		seen = true
		wire = v.wire
		return nil
	})
	if e != nil {
		return e
	}
	if !seen {
		return safe("content missing", mailbox.ErrStaleIdentity)
	}
	access := &scopedRead{s: s, ctx: ctx, o: o, wire: wire, limit: c.cfg.SourceBytes, active: true, contentLimits: c.cfg.ContentLimits}
	e = consume(ctx, o, wire, access)
	return errors.Join(e, access.revoke())
}
func (c *Client) Inspect(ctx context.Context, ref mailbox.MessageRef) (state mailbox.State, err error) {
	err = c.withOccurrence(ctx, ref, func(ctx context.Context, o Occurrence, w WireMetadata, a *scopedRead) error {
		metadata, e := (ContentMetadataLoader{}).Load(ctx, o, w, a)
		if e != nil {
			return e
		}
		state = mailbox.State{Ref: o.Ref, Metadata: metadata, Memberships: []mailbox.Membership{{Collection: o.Collection}}}
		return nil
	})
	if err != nil {
		return mailbox.State{}, err
	}
	return state, nil
}

func (a *scopedRead) ReadStructure(ctx context.Context) (out ContentStructure, err error) {
	a.mu.Lock()
	if !a.active {
		a.mu.Unlock()
		return out, failure("source access expired")
	}
	if a.busy {
		a.failure = failure("concurrent source access")
		a.mu.Unlock()
		a.s.cancel()
		return out, failure("concurrent source access")
	}
	a.busy = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.busy = false
		if err != nil {
			a.failure = safe("structure read failed", err)
		}
		a.mu.Unlock()
	}()
	if e := a.ctx.Err(); e != nil {
		return out, e
	}
	if e := ctx.Err(); e != nil {
		return out, e
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { a.s.cancel(); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
	}()
	return a.s.structure(a.o.UID, a.wire.Size, a.contentLimits)
}

var _ document.MIMEReader = (*Client)(nil)
