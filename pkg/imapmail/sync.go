package imapmail

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/mailsync"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type protocolAttemptFailure struct{ reason *protocolFault }

func (e *protocolAttemptFailure) Error() string { return e.reason.Error() }

type attemptEvidence struct {
	entered, returned, clean                          bool
	emitError, localError, loaderError, errorReturned error
	marker                                            *protocolAttemptFailure
}

func (e *attemptEvidence) mayRetry(runErr error) bool {
	return !errors.Is(runErr, mailsync.ErrCleanup) && e.entered && e.returned && e.clean && e.marker != nil && e.errorReturned == e.marker && errors.Is(runErr, e.marker) && e.emitError == nil && e.localError == nil && e.loaderError == nil && e.marker.reason.transient
}
func backoff(k int, hint time.Duration, r float64) time.Duration {
	base := time.Second * time.Duration(1<<min(max(k, 0), 3))
	r = max(0, min(r, 1))
	jitter := base/2 + time.Duration(float64(base/2)*r)
	return max(jitter, hint)
}
func (c *Client) wait(ctx context.Context, k int, hint time.Duration) error {
	if hint < 0 || hint > 30*time.Second {
		return failure("invalid retry hint")
	}
	wait := backoff(k, hint, c.random())
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= wait {
		return failure("retry exceeds total deadline")
	}
	return c.clock.Wait(ctx, wait)
}

type checkpoint struct {
	Account        mailbox.AccountID
	Scope          mailbox.CollectionID
	Mailbox        string
	Validity, Next uint32
	Generation     int64
}

func checkpointFor(account mailbox.Account, f Folder, s selected, generation int64) mailbox.Cursor {
	b, _ := json.Marshal(checkpoint{account.ID, f.Collection.Ref.CollectionID, f.MailboxID, s.validity, s.next, generation})
	return mailbox.Cursor{AccountID: account.ID, CollectionID: f.Collection.Ref.CollectionID, Token: "imap-checkpoint-v1:" + base64.RawURLEncoding.EncodeToString(b)}
}
func checkCheckpoint(cp mailbox.Cursor, account mailbox.Account, f Folder, sel selected, generation int64) (checkpoint, error) {
	if cp == (mailbox.Cursor{}) {
		return checkpoint{}, nil
	}
	if e := cp.Validate(f.Collection.Ref); e != nil {
		return checkpoint{}, e
	}
	const prefix = "imap-checkpoint-v1:"
	if !strings.HasPrefix(cp.Token, prefix) || len(cp.Token) > 8192 {
		return checkpoint{}, mailbox.ErrStaleIdentity
	}
	b, e := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(cp.Token, prefix))
	if e != nil {
		return checkpoint{}, mailbox.ErrStaleIdentity
	}
	var old checkpoint
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(&old); e != nil {
		return checkpoint{}, mailbox.ErrStaleIdentity
	}
	var trailing any
	if e = decoder.Decode(&trailing); e != io.EOF {
		return checkpoint{}, mailbox.ErrStaleIdentity
	}
	if old.Account != account.ID || old.Scope != f.Collection.Ref.CollectionID || old.Mailbox != f.MailboxID || old.Validity != sel.validity || old.Generation != generation || old.Next == 0 || old.Next > sel.next {
		return checkpoint{}, mailbox.ErrStaleIdentity
	}
	return old, nil
}

// SyncCollection performs complete protocol attempts through the one RunPass
// publication lifecycle. The required loader owns real content interpretation.
func (c *Client) SyncCollection(ctx context.Context, db *store.AccountStore, folder Folder, loader MetadataLoader, full bool) (SyncResult, error) {
	if missingLoader(loader) {
		return SyncResult{}, failure("metadata loader required")
	}
	if db == nil || db.Account() != c.Account() {
		return SyncResult{}, safe("sync account mismatch", mailbox.ErrAccountMismatch)
	}
	if e := c.validateFolder(folder); e != nil {
		return SyncResult{}, e
	}
	ctx, done, e := c.operation(ctx)
	if e != nil {
		return SyncResult{}, safe("sync unavailable", e)
	}
	defer done()
	var first *selected
	windows := 0
	width := c.cfg.UIDWindow
	for attempt := 0; attempt < c.cfg.MaxAttempts; attempt++ {
		s, e := c.connect(ctx)
		if e != nil {
			var protocol *protocolFault
			if errors.As(e, &protocol) && protocol.transient && attempt+1 < c.cfg.MaxAttempts && ctx.Err() == nil {
				if e = c.wait(ctx, attempt, protocol.hint); e == nil {
					continue
				}
			}
			return SyncResult{}, safe("sync preparation failed", e)
		}
		s.limit = min(s.limit, width)
		// Every session rediscovers exact selectable names. Do not guess folders.
		discovery, e := c.discover(s)
		if e != nil {
			_ = s.close()
			return SyncResult{}, safe("sync discovery failed", e)
		}
		found := false
		for _, f := range discovery.Folders {
			if f.Name == folder.Name && f.Collection.Ref == folder.Collection.Ref {
				found = true
			}
		}
		if !found {
			_ = s.close()
			return SyncResult{}, safe("collection missing; reconcile", mailbox.ErrStaleIdentity)
		}
		selection, e := s.examine(folder)
		if e != nil {
			_ = s.close()
			return SyncResult{}, safe("sync selection failed", e)
		}
		if first == nil {
			copy := selection
			first = &copy
		} else if first.validity != selection.validity {
			_ = s.close()
			return SyncResult{}, safe("reconnect epoch changed; reconcile", mailbox.ErrStaleIdentity)
		}
		if e = db.PutCollection(ctx, folder.Collection); e != nil {
			_ = s.close()
			return SyncResult{}, safe("local collection unavailable", e)
		}
		state, e := db.Collection(ctx, folder.Collection.Ref)
		if e != nil {
			_ = s.close()
			return SyncResult{}, safe("local epoch unavailable", e)
		}
		if state.MailboxID != "" && (state.MailboxID != folder.MailboxID || state.UIDValidity != selection.validity) {
			_ = s.close()
			return SyncResult{}, safe("epoch changed; conditional reconciliation required", mailbox.ErrStaleIdentity)
		}
		cp, e := db.Checkpoint(ctx, folder.Collection.Ref)
		if e != nil {
			_ = s.close()
			return SyncResult{}, safe("checkpoint unavailable", e)
		}
		previous, e := checkCheckpoint(cp, c.Account(), folder, selection, state.EpochGeneration)
		if e != nil {
			_ = s.close()
			return SyncResult{}, safe("checkpoint requires reconciliation", e)
		}
		input := cp
		if full {
			input = mailbox.Cursor{}
		}
		epoch := store.IMAPEpoch{MailboxID: folder.MailboxID, UIDValidity: selection.validity, Generation: state.EpochGeneration}
		spec := mailsync.PassSpec{Account: c.Account(), Collection: folder.Collection, Input: input, Epoch: &epoch}
		proof := &attemptEvidence{}
		result := SyncResult{Collection: folder.Collection.Ref}
		runErr := c.runPass(ctx, db, spec, func(scanCtx context.Context, emit mailsync.EmitPage) (err error) {
			proof.entered = true
			defer func() {
				closeErr := s.close()
				proof.clean = closeErr == nil
				if closeErr != nil {
					err = safe("protocol cleanup failed", closeErr)
				}
				if err != nil && proof.clean && proof.emitError == nil && proof.localError == nil && proof.loaderError == nil && scanCtx.Err() == nil {
					var f *protocolFault
					if errors.As(err, &f) && f.transient {
						proof.marker = &protocolAttemptFailure{f}
						err = proof.marker
					}
				}
				proof.returned = true
				proof.errorReturned = err
			}()
			prior, e := db.ReadIMAPInventory(scanCtx, folder.Collection.Ref, epoch)
			if e != nil {
				proof.localError = e
				return safe("bounded local inventory unavailable; inspect account epoch and limits", e)
			}
			byUID := make(map[uint32]store.IMAPInventoryEntry, len(prior))
			for _, p := range prior {
				byUID[p.IMAP.UID] = p
			}
			mode := mailbox.Snapshot
			if input != (mailbox.Cursor{}) {
				mode = mailbox.Delta
			}
			cursor := input
			ordinal := 0
			nonce := rand.Text()
			emitPage := func(messages []mailbox.Message, records []store.MessageRecord, removed []mailbox.MessageRef, terminal bool) error {
				page := mailbox.SyncPage{Mode: mode, Messages: messages, Removed: removed, Complete: terminal}
				if terminal {
					page.Checkpoint = checkpointFor(c.Account(), folder, selection, epoch.Generation)
				} else {
					ordinal++
					page.Next = mailbox.Cursor{AccountID: c.Account().ID, CollectionID: folder.Collection.Ref.CollectionID, Token: "imap-page-v1:" + nonce + ":" + strconv.Itoa(ordinal)}
				}
				if e := emit(cursor, page, records); e != nil {
					proof.emitError = e
					return e
				}
				cursor = page.Next
				return nil
			}
			baseline := make(map[uint32]WireMetadata)
			retainedBytes := int64(0)
			e = c.sweep(s, selection, &windows, func(v fetched) error {
				if _, ok := baseline[v.uid]; ok {
					return fault("duplicate UID", false)
				}
				// A complete committed inventory already accounted for every
				// UID below its next-assignment high water. A missing prior UID
				// cannot reappear in this epoch without violating UID identity.
				if _, present := byUID[v.uid]; !present && previous.Next != 0 && v.uid < previous.Next {
					return safe("previously absent UID reused in the same epoch; reconcile", mailbox.ErrStaleIdentity)
				}
				if len(baseline) >= c.cfg.MaxMessages {
					return fault("message inventory limit", false)
				}
				charge := int64(256)
				for _, flag := range v.wire.Flags {
					charge += int64(len(flag))
				}
				if charge > (64<<20)-retainedBytes {
					return fault("remote inventory byte limit", false)
				}
				retainedBytes += charge
				// Retain only flags/size for second-sweep comparison, not envelope/content.
				baseline[v.uid] = WireMetadata{Flags: append([]string(nil), v.wire.Flags...), Size: v.wire.Size}
				o := occurrence(c.Account(), folder, selection.validity, v.uid, epoch.Generation)
				if len(o.Ref.MessageID) > store.IMAPInventoryMaxFieldBytes || len(o.Ref.Locator) > store.IMAPInventoryMaxFieldBytes {
					return failure("occurrence identity limit")
				}
				access := &scopedRead{s: s, ctx: scanCtx, o: o, wire: v.wire, limit: c.cfg.SourceBytes, active: true}
				metadata, loadErr := loader.Load(scanCtx, o, v.wire, access)
				accessErr := access.revoke()
				if loadErr != nil || accessErr != nil {
					proof.loaderError = failure("metadata loader or scoped source failed")
					return proof.loaderError
				}
				if scanCtx.Err() != nil {
					return scanCtx.Err()
				}
				message := mailbox.Message{Ref: o.Ref, Metadata: metadata}
				record := store.MessageRecord{Message: message, IMAP: &store.IMAPLocator{CollectionID: folder.Collection.Ref.CollectionID, MailboxID: folder.MailboxID, UIDValidity: selection.validity, UID: v.uid, EpochGeneration: epoch.Generation}}
				if old, ok := byUID[v.uid]; ok {
					if old.Remote != o.Ref {
						return mailbox.ErrStaleIdentity
					}
					result.Updated++
				} else {
					result.Added++
				}
				// A single complete record per page bounds retained metadata and permits
				// source reads only after the enclosing fetch command is fully consumed.
				return emitPage([]mailbox.Message{message}, []store.MessageRecord{record}, nil, false)
			})
			if e != nil {
				return e
			}
			if uint32(len(baseline)) != selection.count {
				return fault("incomplete first UID inventory", true)
			}
			second := make(map[uint32]bool, len(baseline))
			e = c.sweep(s, selection, &windows, func(v fetched) error {
				old, ok := baseline[v.uid]
				if !ok || second[v.uid] || old.Size != v.wire.Size || !sameFlags(old.Flags, v.wire.Flags) {
					return fault("mailbox changed between inventories", true)
				}
				second[v.uid] = true
				return nil
			})
			if e != nil {
				return e
			}
			if len(second) != len(baseline) {
				return fault("incomplete second UID inventory", true)
			}
			final, e := s.examine(folder)
			if e != nil {
				return e
			}
			if final != selection || s.dirty {
				return fault("mailbox changed during complete scan", true)
			}
			removed := make([]mailbox.MessageRef, 0, 100)
			for _, p := range prior {
				if !second[p.IMAP.UID] {
					result.Removed++
					if mode == mailbox.Delta {
						removed = append(removed, p.Remote)
						if len(removed) == 100 {
							if e = emitPage(nil, nil, removed, false); e != nil {
								return e
							}
							removed = make([]mailbox.MessageRef, 0, 100)
						}
					}
				}
			}
			if len(removed) > 0 {
				if e = emitPage(nil, nil, removed, false); e != nil {
					return e
				}
			}
			result.Remote = len(baseline)
			return emitPage(nil, nil, nil, true)
		})
		if !proof.entered {
			_ = s.close()
		}
		if runErr != nil {
			if proof.mayRetry(runErr) && attempt+1 < c.cfg.MaxAttempts && ctx.Err() == nil {
				if proof.marker.reason.window > 0 {
					width = min(width, proof.marker.reason.window)
				}
				if e = c.wait(ctx, attempt, proof.marker.reason.hint); e == nil {
					continue
				}
				return SyncResult{}, safe("retry unavailable", e)
			}
			return SyncResult{}, safe("sync failed; reconcile uncertain publication or cleanup", runErr)
		}
		n, e := db.Count(ctx, store.AccountFilter{Collection: &folder.Collection.Ref})
		if e != nil || n != result.Remote {
			return SyncResult{}, safe("pass committed; local count reporting requires reconciliation", e)
		}
		result.Local = n
		return result, nil
	}
	return SyncResult{}, failure("retry attempts exhausted")
}
func sameFlags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, v := range a {
		if !hasFlag(b, v) {
			return false
		}
	}
	return true
}
func (c *Client) sweep(s *session, sel selected, windows *int, consume func(fetched) error) error {
	// Buffer one bounded UID window, consume the tagged completion, THEN call
	// loader/source/emit. No non-reentrant wire lock is held by collaborators.
	for low := uint64(1); low < uint64(sel.next); low += uint64(s.limit) {
		if *windows >= c.cfg.MaxWindows {
			return fault("UID window budget exhausted", false)
		}
		*windows++
		hi := min(low+uint64(s.limit)-1, uint64(sel.next)-1)
		values := make([]fetched, 0, s.limit)
		e := s.fetch(uint32(low), uint32(hi), nil, 0, func(v fetched) error {
			if len(values) >= s.limit {
				return fault("UID response exceeds range", false)
			}
			values = append(values, v)
			return nil
		})
		if e != nil {
			return e
		}
		for _, v := range values {
			if e = consume(v); e != nil {
				return e
			}
		}
	}
	return nil
}
func (c *Client) validateFolder(f Folder) error {
	if !validText(f.Name, 4096) {
		return failure("invalid folder")
	}
	canonicalFolder := c.folder(f.Name, f.Delimiter)
	if canonicalFolder.Name != f.Name || canonicalFolder.Collection.Ref != f.Collection.Ref || canonicalFolder.MailboxID != f.MailboxID {
		return safe("folder identity mismatch", mailbox.ErrStaleIdentity)
	}
	return nil
}

// ReconcileEpoch requires an explicit previously observed local state, and a
// fresh verified remote observation. It cannot make the remote server atomic.
func (c *Client) ReconcileEpoch(ctx context.Context, db *store.AccountStore, folder Folder, expected store.IMAPEpochState) (store.IMAPEpochState, error) {
	if db == nil || db.Account() != c.Account() {
		return store.IMAPEpochState{}, safe("epoch account mismatch", mailbox.ErrAccountMismatch)
	}
	if e := c.validateFolder(folder); e != nil {
		return store.IMAPEpochState{}, e
	}
	ctx, done, e := c.operation(ctx)
	if e != nil {
		return store.IMAPEpochState{}, safe("epoch unavailable", e)
	}
	defer done()
	s, e := c.connect(ctx)
	if e != nil {
		return store.IMAPEpochState{}, safe("epoch observation failed", e)
	}

	selected, e := s.examine(folder)
	if e != nil {
		_ = s.close()
		return store.IMAPEpochState{}, safe("epoch observation failed", e)
	}
	if e = s.close(); e != nil {
		return store.IMAPEpochState{}, safe("epoch observation cleanup failed", e)
	}
	out, e := db.ReconcileIMAPEpoch(ctx, folder.Collection.Ref, expected, folder.MailboxID, selected.validity)
	if e != nil {
		return store.IMAPEpochState{}, safe("conditional epoch reconciliation refused", e)
	}
	return out, nil
}

func missingLoader(loader MetadataLoader) bool {
	if loader == nil {
		return true
	}
	value := reflect.ValueOf(loader)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	}
	return false
}
