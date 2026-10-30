package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// IMAPEpoch initializes a collection's first observed mailbox epoch. Subsequent
// changes require explicit InvalidateIMAPEpoch, never a cursor fallback.
type IMAPEpoch struct {
	MailboxID   string
	UIDValidity uint32
	Generation  int64
}

// SyncPass is an opaque capability owned by the creating account-aware handle.
// Reopening another session does not confer the ability to abort its pass.
type SyncPass struct {
	id, owner       string
	account         mailbox.Account
	scope           mailbox.CollectionRef
	actionWatermark int64
}

func (a *AccountStore) passValid(p SyncPass) error {
	if p.account != a.account {
		return &mailbox.AccountMismatchError{Operation: "sync pass", Expected: a.account.ID, Actual: p.account.ID}
	}
	if p.id == "" || p.owner != a.owner {
		return stale("sync pass belongs to another session")
	}
	return a.scope(p.scope)
}

type passState struct {
	input, next, mode, checkpoint string
	generation, epoch             int64
	complete                      bool
}

func (a *AccountStore) passState(ctx context.Context, q sqlAccess, p SyncPass) (s passState, err error) {
	if err = a.passValid(p); err != nil {
		return
	}
	err = q.QueryRowContext(ctx, `select input,next,mode,checkpoint,generation,epoch,complete from sync_passes where id=? and owner=? and account_id=? and provider_id=? and collection_id=?`, p.id, p.owner, a.account.ID, a.account.ProviderID, p.scope.CollectionID).Scan(&s.input, &s.next, &s.mode, &s.checkpoint, &s.generation, &s.epoch, &s.complete)
	if errors.Is(err, sql.ErrNoRows) {
		err = stale("sync pass is no longer staged")
	}
	return
}
func (a *AccountStore) checkpoint(ctx context.Context, q sqlAccess, scope mailbox.CollectionRef) (token string, generation, epoch int64, err error) {
	info, e := a.collection(ctx, q, scope)
	if e != nil {
		err = e
		return
	}
	epoch = info.EpochGeneration
	var storedEpoch int64
	err = q.QueryRowContext(ctx, `select checkpoint,generation,identity_epoch from provider_sync_state where account_id=? and provider_id=? and collection_id=?`, a.account.ID, a.account.ProviderID, scope.CollectionID).Scan(&token, &generation, &storedEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
		return
	}
	if err == nil && storedEpoch != epoch {
		err = stale("checkpoint epoch changed")
	}
	return
}

// Checkpoint returns only a durable, scope-bound terminal checkpoint.
func (a *AccountStore) Checkpoint(ctx context.Context, scope mailbox.CollectionRef) (out mailbox.Cursor, err error) {
	if err = a.scope(scope); err != nil {
		return
	}
	err = a.transaction(ctx, func(q sqlAccess) error {
		token, _, _, e := a.checkpoint(ctx, q, scope)
		if e == nil && token != "" {
			out = mailbox.Cursor{AccountID: a.account.ID, CollectionID: scope.CollectionID, Token: token}
		}
		return e
	})
	return
}

// initializeIMAPEpoch never reinterprets existing generic evidence as an IMAP
// occurrence. Initial binding is permitted only before any scope has been used.
func (a *AccountStore) initializeIMAPEpoch(ctx context.Context, q sqlAccess, scope mailbox.CollectionRef, epoch IMAPEpoch) error {
	info, e := a.collection(ctx, q, scope)
	if e != nil {
		return e
	}
	if strings.TrimSpace(epoch.MailboxID) == "" || epoch.UIDValidity == 0 || epoch.Generation != info.EpochGeneration {
		return stale("complete current IMAP epoch required")
	}
	if info.MailboxID != "" {
		if info.MailboxID != epoch.MailboxID || info.UIDValidity != epoch.UIDValidity {
			return stale("IMAP epoch changed; explicit invalidation required")
		}
		return nil
	}
	var used bool
	if e = q.QueryRowContext(ctx, `select exists(select 1 from memberships where account_id=?1 and provider_id=?2 and collection_id=?3) or exists(select 1 from provider_sync_state where account_id=?1 and provider_id=?2 and collection_id=?3) or exists(select 1 from sync_passes where account_id=?1 and provider_id=?2 and collection_id=?3)`, a.account.ID, a.account.ProviderID, scope.CollectionID).Scan(&used); e != nil {
		return e
	}
	if used {
		return stale("used generic collection cannot acquire an IMAP epoch")
	}
	_, e = q.ExecContext(ctx, "update collections set mailbox_id=?,uidvalidity=? where account_id=? and provider_id=? and collection_id=?", epoch.MailboxID, epoch.UIDValidity, a.account.ID, a.account.ProviderID, scope.CollectionID)
	return e
}

// actionWatermark uses the append-only local audit sequence, never a provider
// revision token. Capture all audits so commit only searches the new rowid range.
func (a *AccountStore) actionWatermark(ctx context.Context, q sqlAccess) (id int64, err error) {
	err = q.QueryRowContext(ctx, "select coalesce(max(id),0) from actions where account_id=? and provider_id=?", a.account.ID, a.account.ProviderID).Scan(&id)
	return
}

// Account-wide invalidation is conservative: an action can change global
// metadata or create edges in a different snapshot scope. Failures/skips do not
// change provider state and therefore do not invalidate the pass.
func (a *AccountStore) successfulActionAfter(ctx context.Context, q sqlAccess, watermark int64) (found bool, err error) {
	err = q.QueryRowContext(ctx, "select exists(select 1 from actions where id>? and account_id=? and provider_id=? and status=?)", watermark, a.account.ID, a.account.ProviderID, ActionMoved).Scan(&found)
	return
}

// BeginSync stages a new pass without changing visible mail or checkpoint.
func (a *AccountStore) BeginSync(ctx context.Context, c mailbox.Collection, input mailbox.Cursor, epoch *IMAPEpoch) (out SyncPass, err error) {
	if err = a.scope(c.Ref); err != nil {
		return
	}
	if err = input.Validate(c.Ref); err != nil {
		return
	}
	err = a.transaction(ctx, func(q sqlAccess) error {
		if e := a.putCollection(ctx, q, c); e != nil {
			return e
		}
		if epoch != nil {
			if a.account.ProviderID == GraphProvider {
				return errors.New("store: Graph collections cannot carry IMAP epochs")
			}
			if e := a.initializeIMAPEpoch(ctx, q, c.Ref, *epoch); e != nil {
				return e
			}
		}
		token, generation, currentEpoch, e := a.checkpoint(ctx, q, c.Ref)
		if e != nil {
			return e
		}
		if input != (mailbox.Cursor{}) && input.Token != token {
			return stale("checkpoint does not match durable state")
		}
		watermark, e := a.actionWatermark(ctx, q)
		if e != nil {
			return e
		}
		out = SyncPass{id: randomID(), owner: a.owner, account: a.account, scope: c.Ref, actionWatermark: watermark}
		_, e = q.ExecContext(ctx, `insert into sync_passes(id,owner,account_id,provider_id,collection_id,input,generation,epoch) values(?,?,?,?,?,?,?,?)`, out.id, out.owner, a.account.ID, a.account.ProviderID, c.Ref.CollectionID, input.Token, generation, currentEpoch)
		return e
	})
	return
}

// StagePage validates the exact continuation chain and persists effects only as
// staging. Supplements may add revision/correlation/IMAP data but cannot replace
// a page message's complete metadata or identity.
func (a *AccountStore) StagePage(ctx context.Context, p SyncPass, input mailbox.Cursor, page mailbox.SyncPage, supplements []MessageRecord) error {
	if e := a.passValid(p); e != nil {
		return e
	}
	if e := input.Validate(p.scope); e != nil {
		return e
	}
	if e := page.Validate(p.scope); e != nil {
		return e
	}
	return a.transaction(ctx, func(q sqlAccess) error {
		s, e := a.passState(ctx, q, p)
		if e != nil {
			return e
		}
		if s.complete {
			return stale("sync pass is already terminal")
		}
		_, generation, epoch, e := a.checkpoint(ctx, q, p.scope)
		if e != nil {
			return e
		}
		if generation != s.generation || epoch != s.epoch {
			return stale("staged pass generation changed")
		}
		expected := s.next
		if s.mode == "" {
			expected = s.input
		}
		if input.Token != expected {
			return stale("sync continuation chain changed")
		}
		if (s.mode != "" && s.mode != string(page.Mode)) || (s.mode == "" && s.input == "" && page.Mode != mailbox.Snapshot) || (s.mode == "" && s.input != "" && page.Mode != mailbox.Delta) {
			return errors.New("store: sync mode is inconsistent with pass")
		}
		byID := map[mailbox.MessageID]MessageRecord{}
		for _, r := range supplements {
			if _, ok := byID[r.Message.Ref.MessageID]; ok {
				return errors.New("store: duplicate sync supplement")
			}
			byID[r.Message.Ref.MessageID] = r
		}
		records := make([]MessageRecord, 0, len(page.Messages))
		seen := map[mailbox.MessageID]bool{}
		for _, m := range page.Messages {
			if seen[m.Ref.MessageID] {
				return errors.New("store: duplicate page identity")
			}
			seen[m.Ref.MessageID] = true
			r, ok := byID[m.Ref.MessageID]
			if !ok {
				r = MessageRecord{Message: m}
			} else {
				left, _ := jsonText(m)
				right, _ := jsonText(r.Message)
				if left != right {
					return stale("supplement changes page identity or metadata")
				}
				delete(byID, m.Ref.MessageID)
			}
			if r.IMAP != nil && (r.IMAP.CollectionID != p.scope.CollectionID || r.IMAP.EpochGeneration != s.epoch || r.IMAP.UID == 0 || r.IMAP.UIDValidity == 0 || r.IMAP.MailboxID == "") {
				return stale("staged IMAP locator is invalid")
			}
			records = append(records, r)
		}
		if len(byID) != 0 {
			return errors.New("store: supplement has no matching page message")
		}
		raw, e := jsonText(page)
		if e != nil {
			return e
		}
		extra, e := jsonText(records)
		if e != nil {
			return e
		}
		_, e = q.ExecContext(ctx, `insert into sync_pages(pass_id,ordinal,page,supplements) select ?,coalesce(max(ordinal),0)+1,?,? from sync_pages where pass_id=?`, p.id, raw, extra, p.id)
		if e != nil {
			return e
		}
		for _, m := range page.Messages {
			if _, e = q.ExecContext(ctx, "insert or ignore into sync_seen(pass_id,message_id) values(?,?)", p.id, m.Ref.MessageID); e != nil {
				return e
			}
		}
		_, e = q.ExecContext(ctx, "update sync_passes set mode=?,next=?,complete=?,checkpoint=? where id=?", page.Mode, page.Next.Token, page.Complete, page.Checkpoint.Token, p.id)
		return e
	})
}
func (a *AccountStore) removeRemoteMembership(ctx context.Context, q sqlAccess, scope mailbox.CollectionRef, remote mailbox.MessageRef) error {
	if e := remote.Validate(a.account.ID); e != nil {
		return e
	}
	var n int64
	var locator string
	var valid bool
	e := q.QueryRowContext(ctx, "select n,locator,identity_valid from messages where account_id=? and provider_id=? and provider_message_id=?", a.account.ID, a.account.ProviderID, remote.MessageID).Scan(&n, &locator, &valid)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	if !valid || mailbox.Locator(locator) != remote.Locator {
		return stale("removed occurrence identity changed")
	}
	_, e = q.ExecContext(ctx, "delete from memberships where account_id=? and provider_id=? and n=? and collection_id=?", a.account.ID, a.account.ProviderID, n, scope.CollectionID)
	if e != nil {
		return e
	}
	return projectMembership(ctx, q, n)
}

// CommitSync publishes all pages, scope-limited absence and terminal checkpoint
// atomically. Failure leaves old visible state and checkpoint authoritative.
func (a *AccountStore) CommitSync(ctx context.Context, p SyncPass) error {
	if e := a.passValid(p); e != nil {
		return e
	}
	return a.transaction(ctx, func(q sqlAccess) error {
		s, e := a.passState(ctx, q, p)
		if e != nil {
			return e
		}
		if !s.complete || s.next != "" || s.mode == "" {
			return errors.New("store: sync pass has no complete terminal page")
		}
		changed, e := a.successfulActionAfter(ctx, q, p.actionWatermark)
		if e != nil {
			return e
		}
		if changed {
			return stale("successful action changed the mirror after sync began; reconcile with a fresh pass")
		}
		_, generation, epoch, e := a.checkpoint(ctx, q, p.scope)
		if e != nil {
			return e
		}
		if generation != s.generation || epoch != s.epoch {
			return stale("sync checkpoint generation changed")
		}
		info, e := a.collection(ctx, q, p.scope)
		if e != nil {
			return e
		}
		var ordinal int64
		for {
			var pageJSON, extraJSON string
			var next int64
			e = q.QueryRowContext(ctx, "select ordinal,page,supplements from sync_pages where pass_id=? and ordinal>? order by ordinal limit 1", p.id, ordinal).Scan(&next, &pageJSON, &extraJSON)
			if errors.Is(e, sql.ErrNoRows) {
				break
			}
			if e != nil {
				return e
			}
			ordinal = next
			var page mailbox.SyncPage
			var records []MessageRecord
			if e = json.Unmarshal([]byte(pageJSON), &page); e != nil {
				return e
			}
			if e = json.Unmarshal([]byte(extraJSON), &records); e != nil {
				return e
			}
			if e := page.Validate(p.scope); e != nil {
				return e
			}
			for _, r := range records {
				if _, e = a.upsertMessage(ctx, q, info.Collection, r); e != nil {
					return e
				}
			}
			for _, remote := range page.Removed {
				if e = a.removeRemoteMembership(ctx, q, p.scope, remote); e != nil {
					return e
				}
			}
		}
		if s.mode == string(mailbox.Snapshot) {
			var last int64
			for {
				var n int64
				e = q.QueryRowContext(ctx, `select m.n from messages m join memberships ms on ms.n=m.n where ms.account_id=? and ms.provider_id=? and ms.collection_id=? and m.n>? and not exists(select 1 from sync_seen s where s.pass_id=? and s.message_id=m.provider_message_id) order by m.n limit 1`, a.account.ID, a.account.ProviderID, p.scope.CollectionID, last, p.id).Scan(&n)
				if errors.Is(e, sql.ErrNoRows) {
					break
				}
				if e != nil {
					return e
				}
				last = n
				if _, e = q.ExecContext(ctx, "delete from memberships where account_id=? and provider_id=? and collection_id=? and n=?", a.account.ID, a.account.ProviderID, p.scope.CollectionID, n); e != nil {
					return e
				}
				if e = projectMembership(ctx, q, n); e != nil {
					return e
				}
			}
		}
		_, e = q.ExecContext(ctx, `insert into provider_sync_state(account_id,provider_id,collection_id,checkpoint,generation,identity_epoch) values(?,?,?,?,?,?) on conflict(account_id,provider_id,collection_id) do update set checkpoint=excluded.checkpoint,generation=excluded.generation,identity_epoch=excluded.identity_epoch`, a.account.ID, a.account.ProviderID, p.scope.CollectionID, s.checkpoint, s.generation+1, s.epoch)
		if e != nil {
			return e
		}
		_, e = q.ExecContext(ctx, "delete from sync_passes where id=? and owner=?", p.id, p.owner)
		return e
	})
}

// AbortSync removes only the pass capability owned by this session.
func (a *AccountStore) AbortSync(ctx context.Context, p SyncPass) error {
	if e := a.passValid(p); e != nil {
		return e
	}
	return a.transaction(ctx, func(q sqlAccess) error {
		if _, e := a.passState(ctx, q, p); e != nil {
			return e
		}
		_, e := q.ExecContext(ctx, "delete from sync_passes where id=? and owner=?", p.id, p.owner)
		return e
	})
}

// CleanupSyncPasses discards abandoned staging only under exclusive lifecycle
// ownership. Ordinary opens never infer that another session's pass is abandoned.
func CleanupSyncPasses(ctx context.Context, path string, account mailbox.Account) (count int64, err error) {
	if err = account.Validate(); err != nil {
		return
	}
	life, e := acquireLifecycle(ctx, path, true, diskFiles{})
	if e != nil {
		err = e
		return
	}
	db, e := sqliteOpen(ctx, life.entry.path)
	if e != nil {
		life.release()
		err = e
		return
	}
	defer func() { err = errors.Join(err, closeMaintenance(life, db)) }()
	matched, e := matchBinding(ctx, db, &account)
	if e != nil {
		err = e
		return
	}
	if !matched {
		err = errors.New("store: cleanup requires a bound account")
		return
	}
	conn, e := db.Conn(ctx)
	if e != nil {
		err = e
		return
	}
	defer conn.Close()
	err = immediate(ctx, conn, func(q sqlAccess) error {
		res, e := q.ExecContext(ctx, "delete from sync_passes where account_id=? and provider_id=?", account.ID, account.ProviderID)
		if e == nil {
			count, e = res.RowsAffected()
		}
		return e
	})
	return
}
