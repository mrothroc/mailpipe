package imapmail

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// scopedRead delegates the already-held operation lease. It never reenters the
// public Client gate or holds a lock across a loader/writer/network call.
type scopedRead struct {
	mu           sync.Mutex
	s            *session
	ctx          context.Context
	o            Occurrence
	wire         WireMetadata
	limit        int64
	active, busy bool
	failure      error
}

func (a *scopedRead) revoke() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.active = false
	if a.busy && a.failure == nil {
		a.failure = failure("loader retained active read")
	}
	return a.failure
}
func (a *scopedRead) ReadSource(ctx context.Context, w io.Writer) (err error) {
	a.mu.Lock()
	if !a.active {
		a.mu.Unlock()
		return failure("source access expired")
	}
	if a.busy {
		a.failure = failure("concurrent source access")
		a.mu.Unlock()
		a.s.cancel()
		return failure("concurrent source access")
	}
	a.busy = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.busy = false
		if err != nil {
			a.failure = safe("source read failed", err)
		}
		a.mu.Unlock()
	}()
	if w == nil {
		return failure("source writer required")
	}
	if e := a.ctx.Err(); e != nil {
		return e
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	// A shorter collaborator context can interrupt transport; Background cannot
	// extend the original Load/session context. No worker is abandoned on timeout.
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { a.s.cancel(); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
	}()
	size := a.wire.Size
	if int64(size) > a.limit {
		return failure("complete source exceeds byte limit")
	}
	offset := uint32(0)
	for {
		if e := a.ctx.Err(); e != nil {
			return e
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		length := min(uint32(32768), size-offset)
		if length == 0 {
			length = 1
		}
		seen := false
		e := a.s.fetch(a.o.UID, a.o.UID, &offset, length, func(f fetched) error {
			if seen || f.uid != a.o.UID || f.wire.Size != size || f.offset != offset {
				return failure("source identity size or offset changed")
			}
			seen = true
			expected := min(length, size-offset)
			if uint32(len(f.body)) != expected {
				return failure("incomplete source chunk")
			}
			n, e := w.Write(f.body)
			if e != nil {
				return safe("source writer failed", e)
			}
			if n != len(f.body) {
				return io.ErrShortWrite
			}
			return nil
		})
		if e != nil {
			return e
		}
		if !seen {
			return safe("source occurrence missing", mailbox.ErrStaleIdentity)
		}
		if offset+length >= size {
			return nil
		}
		offset += length
	}
}

// ReadSource fetches complete source without marking mail read. Callers discard
// partial output on error and own their writer; arbitrary blocked writers must
// cooperate with context. This does not implement a full content Provider.
func (c *Client) ReadSource(ctx context.Context, ref mailbox.MessageRef, w io.Writer) (err error) {
	ctx, done, e := c.operation(ctx)
	if e != nil {
		return safe("source unavailable", e)
	}
	defer done()
	o, f, e := c.decode(ref)
	if e != nil {
		return safe("invalid source identity", e)
	}
	s, e := c.connect(ctx)
	if e != nil {
		return safe("source connection failed", e)
	}
	defer func() {
		if e := s.close(); e != nil {
			err = safe("source cleanup failed", errors.Join(err, e))
		}
	}()
	selection, e := s.examine(f)
	if e != nil {
		return safe("source selection failed", e)
	}
	if selection.validity != o.UIDValidity {
		return safe("source epoch changed", mailbox.ErrStaleIdentity)
	}
	var wire WireMetadata
	seen := false
	e = s.fetch(o.UID, o.UID, nil, 0, func(v fetched) error {
		if seen {
			return failure("duplicate source occurrence")
		}
		seen = true
		wire = v.wire
		return nil
	})
	if e != nil {
		return safe("source metadata unavailable", e)
	}
	if !seen {
		return safe("source occurrence no longer present", mailbox.ErrStaleIdentity)
	}
	access := &scopedRead{s: s, ctx: ctx, o: o, wire: wire, limit: c.cfg.SourceBytes, active: true}
	e = access.ReadSource(ctx, w)
	e = errors.Join(e, access.revoke())
	if e != nil {
		return safe("complete source unavailable", e)
	}
	return nil
}
