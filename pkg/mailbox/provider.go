package mailbox

import (
	"context"
	"errors"
	"io"
	"strings"
)

// Provider is read-only mailbox access. All methods must validate account-bound
// input before transport, honor context, and return real errors without default
// success. Reads must not change mail, including its read flags. A missing or
// invalidated message/cursor returns StaleIdentityError, never a zero success.
// Inspect reads authoritative state, not a cached mirror. Text returns complete
// readable content or error, never silently truncated text.
//
// WriteMIME streams complete source, does not close the writer, and propagates
// writer, context and limit errors. On error callers discard partial output.
// Adapters should use at most 32 KiB copy scratch rather than buffering whole
// messages. This target does not bound caller storage or library internals:
// adapters must separately document finite transport/parser/total limits. Check
// context between chunks/parts and use cancelable transport reads or deadlines.
// Arbitrary blocking writers cannot be interrupted by checking context; callers
// needing bounded cancellation must supply a context-aware or bounded writer.
type Provider interface {
	Account() Account
	Collections(context.Context) ([]Collection, error)
	Sync(context.Context, SyncRequest) (SyncPage, error)
	Metadata(context.Context, MessageRef) (Metadata, error)
	Text(context.Context, MessageRef) (string, error)
	WriteMIME(context.Context, MessageRef, io.Writer) error
	Inspect(context.Context, MessageRef) (State, error)
}

// Cursor is an account/collection-bound opaque continuation or checkpoint.
// The all-zero value requests a fresh snapshot. Tokens contain no credentials.
type Cursor struct {
	AccountID    AccountID
	CollectionID CollectionID
	Token        string
}

// Validate accepts only a zero cursor or a complete binding to the scope.
func (c Cursor) Validate(scope CollectionRef) error {
	if err := scope.Validate(scope.AccountID); err != nil {
		return err
	}
	if c == (Cursor{}) {
		return nil
	}
	if err := boundAccount("sync cursor", scope.AccountID, c.AccountID); err != nil {
		return err
	}
	if c.CollectionID != scope.CollectionID {
		return &StaleIdentityError{Operation: "sync cursor", Reason: "collection changed"}
	}
	if strings.TrimSpace(c.Token) == "" {
		return errors.New("sync cursor: token is required")
	}
	return nil
}

// SyncRequest selects a collection and an opaque cursor. Expired cursors must
// return a stale error, not silently start a snapshot and infer deletions.
type SyncRequest struct {
	Collection CollectionRef
	Cursor     Cursor
}

// Validate must run before using a cursor on a transport.
func (r SyncRequest) Validate(account AccountID) error {
	if err := r.Collection.Validate(account); err != nil {
		return err
	}
	return r.Cursor.Validate(r.Collection)
}

// SyncMode distinguishes a full snapshot from explicit incremental changes.
type SyncMode string

const (
	Snapshot SyncMode = "snapshot"
	Delta    SyncMode = "delta"
)

// SyncPage contains changes only for the requested collection. Removed detaches
// memberships, never deletes account-wide messages. Upserts are idempotent by
// account/message/collection. Mode must remain constant throughout a pass.
//
// Only a successful complete snapshot permits absence-based membership removal.
// Interrupted passes permit neither absence removal nor checkpoint commits.
// Consumers stage the pass and persist its terminal checkpoint only after all
// effects are durable. A zero terminal checkpoint means no incremental token is
// available: start a new snapshot next time. Next is for intermediate pages only.
type SyncPage struct {
	Mode             SyncMode
	Messages         []Message
	Removed          []MessageRef
	Next, Checkpoint Cursor
	Complete         bool
}

// Validate checks one page's shape and scope. Cross-page mode consistency,
// durable staging and replay belong to the future synchronizer, not this type.
func (p SyncPage) Validate(scope CollectionRef) error {
	if err := scope.Validate(scope.AccountID); err != nil {
		return err
	}
	if p.Mode != Snapshot && p.Mode != Delta {
		return errors.New("sync page: snapshot or delta mode is required")
	}
	if err := p.Next.Validate(scope); err != nil {
		return err
	}
	if err := p.Checkpoint.Validate(scope); err != nil {
		return err
	}
	if p.Complete {
		if p.Next != (Cursor{}) {
			return errors.New("sync page: terminal page cannot have a next cursor")
		}
	} else if p.Next == (Cursor{}) || p.Checkpoint != (Cursor{}) {
		return errors.New("sync page: intermediate page requires next cursor and no checkpoint")
	}
	if p.Mode == Snapshot && len(p.Removed) > 0 {
		return errors.New("sync page: snapshot cannot contain explicit removals")
	}
	for _, m := range p.Messages {
		if err := m.Ref.Validate(scope.AccountID); err != nil {
			return err
		}
	}
	for _, r := range p.Removed {
		if err := r.Validate(scope.AccountID); err != nil {
			return err
		}
	}
	return nil
}
