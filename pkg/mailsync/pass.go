package mailsync

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
)

// PassSpec binds one scanner attempt to a durable account and collection.
// Epoch initializes an observed IMAP epoch; it never authorizes epoch retirement.
type PassSpec struct {
	Account    mailbox.Account
	Collection mailbox.Collection
	Input      mailbox.Cursor
	Epoch      *store.IMAPEpoch
}

// EmitPage stages a complete page with its exact persistence supplements.
// Scanners call it serially, during Scan only, and do not retain it.
type EmitPage func(input mailbox.Cursor, page mailbox.SyncPage, records []store.MessageRecord) error

// Scan owns protocol traversal, completeness evidence and protocol resources.
// It closes/joins those resources before returning, preserving the operation
// context for publication. Returning nil alone does not establish completion.
type Scan func(context.Context, EmitPage) error

// ErrCleanup means owned staging could not be discarded. Reconcile before any
// retry; original context, scanner and storage errors remain joined to it.
var ErrCleanup = errors.New("sync staging cleanup failed; reconcile before retry")

// RunPass owns exactly one staged publication lifecycle. Store remains the
// authority for page chains, modes, epochs, generations and action watermarks.
// This function never retries, resets epochs, closes the caller's store or
// serializes other sessions. A failure after uncertain publication is an error.
func RunPass(ctx context.Context, s *store.AccountStore, spec PassSpec, scan Scan) (err error) {
	if s == nil || scan == nil {
		return errors.New("sync pass: store and scanner are required")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = spec.Account.Validate(); err != nil {
		return err
	}
	if spec.Account != s.Account() {
		return &mailbox.AccountMismatchError{Operation: "sync pass", Expected: s.Account().ID, Actual: spec.Account.ID}
	}
	if err = spec.Collection.Ref.Validate(spec.Account.ID); err != nil {
		return err
	}
	if err = spec.Input.Validate(spec.Collection.Ref); err != nil {
		return err
	}
	if spec.Epoch != nil {
		copy := *spec.Epoch
		spec.Epoch = &copy
	}
	pass, err := s.BeginSync(ctx, spec.Collection, spec.Input, spec.Epoch)
	if pass != (store.SyncPass{}) {
		defer func() {
			if err != nil {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				if e := s.AbortSync(cleanup, pass); e != nil {
					err = errors.Join(err, ErrCleanup, e)
				}
			}
		}()
	}
	if err != nil {
		return err
	}
	var mu sync.Mutex // only protects this callback's lifetime, never other passes
	active, terminal := true, false
	var stagedErr error
	emit := func(input mailbox.Cursor, page mailbox.SyncPage, records []store.MessageRecord) error {
		mu.Lock()
		defer mu.Unlock()
		if !active {
			return errors.New("sync pass: scanner has returned")
		}
		if stagedErr != nil {
			return stagedErr
		}
		if terminal {
			stagedErr = errors.New("sync pass: page after terminal evidence")
			return stagedErr
		}
		stagedErr = s.StagePage(ctx, pass, input, page, records)
		if stagedErr == nil {
			terminal = page.Complete
		}
		return stagedErr
	}
	scanErr := scan(ctx, emit)
	mu.Lock()
	active = false
	err = errors.Join(scanErr, stagedErr, ctx.Err())
	if err == nil && !terminal {
		err = errors.New("sync pass: scanner supplied no terminal evidence")
	}
	mu.Unlock()
	if err != nil {
		return err
	}
	return s.CommitSync(ctx, pass)
}
