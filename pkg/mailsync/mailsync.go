// Package mailsync coordinates account-bound sync passes. Protocol adapters own
// traversal and completeness evidence; storage owns durable publication checks.
package mailsync

import (
	"context"
	"errors"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
)

var DefaultFolders = []string{"inbox", "deleteditems", "sentitems"}

type Result struct {
	Folder                                 string
	Added, Updated, Removed, Local, Remote int
}

// CollectionSource pairs pages and exact supplements without requiring content
// methods. Pull adapters use SyncCollection; protocol scanners can use RunPass.
type CollectionSource interface {
	Account() mailbox.Account
	ReadPage(context.Context, mailbox.SyncRequest) (mailbox.SyncPage, []store.MessageRecord, error)
	Total(context.Context, mailbox.CollectionRef) (int, error)
}

func SyncCollection(ctx context.Context, s *store.AccountStore, src CollectionSource, c mailbox.Collection, full bool) (Result, error) {
	result := Result{Folder: c.Name}
	if s == nil || src == nil {
		return result, errors.New("sync: store and source required")
	}
	if s.Account() != src.Account() {
		return result, &mailbox.AccountMismatchError{Operation: "sync", Expected: s.Account().ID, Actual: src.Account().ID}
	}
	if err := s.PutCollection(ctx, c); err != nil {
		return result, err
	}
	input := mailbox.Cursor{}
	var err error
	if !full {
		input, err = s.Checkpoint(ctx, c.Ref)
		if err != nil {
			return result, err
		}
	}
	old, err := s.Messages(ctx, store.AccountFilter{Collection: &c.Ref})
	if err != nil {
		return result, err
	}
	present := map[mailbox.MessageID]bool{}
	for _, m := range old {
		present[m.State.Ref.MessageID] = true
	}
	seen := map[mailbox.MessageID]bool{}
	visited := map[mailbox.Cursor]bool{}
	err = RunPass(ctx, s, PassSpec{Account: src.Account(), Collection: c, Input: input}, func(ctx context.Context, emit EmitPage) error {
		cursor := input
		for {
			if visited[cursor] {
				return errors.New("sync: continuation cycle")
			}
			visited[cursor] = true
			page, records, e := src.ReadPage(ctx, mailbox.SyncRequest{Collection: c.Ref, Cursor: cursor})
			if e != nil {
				return e
			}
			if e = emit(cursor, page, records); e != nil {
				return e
			}
			for _, m := range page.Messages {
				if present[m.Ref.MessageID] {
					result.Updated++
				} else {
					result.Added++
				}
				present[m.Ref.MessageID] = true
				seen[m.Ref.MessageID] = true
			}
			for _, ref := range page.Removed {
				if present[ref.MessageID] {
					result.Removed++
					delete(present, ref.MessageID)
				}
			}
			if page.Complete {
				if page.Mode == mailbox.Snapshot {
					for id := range present {
						if !seen[id] {
							result.Removed++
						}
					}
				}
				return nil
			}
			cursor = page.Next
		}
	})
	if err != nil {
		return Result{Folder: c.Name}, err
	}
	result.Local, err = s.Count(ctx, store.AccountFilter{Collection: &c.Ref})
	if err != nil {
		return result, err
	}
	result.Remote, err = src.Total(ctx, c.Ref)
	return result, err
}
