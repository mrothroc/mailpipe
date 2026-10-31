package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// IMAPEpochState is one transactional local observation, not remote authority.
// SyncPresent distinguishes a missing checkpoint row from an empty token.
type IMAPEpochState struct {
	Account                   mailbox.Account
	Scope                     mailbox.CollectionRef
	BindingGeneration         int64
	MailboxID                 string
	UIDValidity               uint32
	EpochGeneration           int64
	SyncPresent               bool
	SyncGeneration, SyncEpoch int64
	Checkpoint                mailbox.Cursor
}

func (a *AccountStore) imapEpochState(ctx context.Context, q sqlAccess, scope mailbox.CollectionRef) (out IMAPEpochState, err error) {
	out.Account = a.account
	out.Scope = scope
	_, out.BindingGeneration, _, err = readBinding(ctx, q)
	if err != nil {
		return
	}
	info, e := a.collection(ctx, q, scope)
	if e != nil {
		return IMAPEpochState{}, e
	}
	out.MailboxID = info.MailboxID
	out.UIDValidity = info.UIDValidity
	out.EpochGeneration = info.EpochGeneration
	var token string
	err = q.QueryRowContext(ctx, "select generation,identity_epoch,checkpoint from provider_sync_state where account_id=? and provider_id=? and collection_id=?", a.account.ID, a.account.ProviderID, scope.CollectionID).Scan(&out.SyncGeneration, &out.SyncEpoch, &token)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
		return
	}
	if err != nil {
		return
	}
	out.SyncPresent = true
	if out.SyncEpoch != out.EpochGeneration {
		return IMAPEpochState{}, stale("checkpoint epoch changed")
	}
	if token != "" {
		out.Checkpoint = mailbox.Cursor{AccountID: a.account.ID, CollectionID: scope.CollectionID, Token: token}
	}
	return
}

// ReadIMAPEpochState captures binding, epoch and publication in one snapshot.
func (a *AccountStore) ReadIMAPEpochState(ctx context.Context, scope mailbox.CollectionRef) (IMAPEpochState, error) {
	if e := a.scope(scope); e != nil {
		return IMAPEpochState{}, e
	}
	var out IMAPEpochState
	e := a.transaction(ctx, func(q sqlAccess) error { var e error; out, e = a.imapEpochState(ctx, q, scope); return e })
	if e != nil {
		return IMAPEpochState{}, e
	}
	return out, nil
}

// ReconcileIMAPEpoch conditionally retires only the exact observed local state.
// Concurrent stale SQLite snapshots fail their write upgrade without retirement.
func (a *AccountStore) ReconcileIMAPEpoch(ctx context.Context, scope mailbox.CollectionRef, expected IMAPEpochState, mailboxID string, validity uint32) (IMAPEpochState, error) {
	if e := a.scope(scope); e != nil {
		return IMAPEpochState{}, e
	}
	if expected.Account != a.account {
		return IMAPEpochState{}, &mailbox.AccountMismatchError{Operation: "IMAP epoch", Expected: a.account.ID, Actual: expected.Account.ID}
	}
	if expected.Scope != scope || expected.BindingGeneration <= 0 || expected.EpochGeneration <= 0 || expected.UIDValidity == 0 || strings.TrimSpace(expected.MailboxID) == "" || a.account.ProviderID == GraphProvider {
		return IMAPEpochState{}, stale("invalid expected IMAP epoch")
	}
	if strings.TrimSpace(mailboxID) == "" || validity == 0 {
		return IMAPEpochState{}, errors.New("store: IMAP epoch requires mailbox and UIDVALIDITY")
	}
	var out IMAPEpochState
	e := a.transaction(ctx, func(q sqlAccess) error {
		return a.reconcileIMAPEpoch(ctx, q, scope, expected, mailboxID, validity, &out)
	})
	if e != nil {
		return IMAPEpochState{}, e
	}
	return out, nil
}
func (a *AccountStore) updateIMAPEpoch(ctx context.Context, q sqlAccess, scope mailbox.CollectionRef, mailboxID string, validity uint32) error {
	_, e := q.ExecContext(ctx, "update collections set mailbox_id=?,uidvalidity=?,epoch_generation=epoch_generation+1 where account_id=? and provider_id=? and collection_id=?", mailboxID, validity, a.account.ID, a.account.ProviderID, scope.CollectionID)
	return e
}
func (a *AccountStore) retireIMAPEpoch(ctx context.Context, q sqlAccess, scope mailbox.CollectionRef, generation int64) error {
	var e error
	if _, e = q.ExecContext(ctx, "update messages set identity_valid=0 where n in(select n from imap_locators where account_id=? and provider_id=? and collection_id=? and valid=1)", a.account.ID, a.account.ProviderID, scope.CollectionID); e != nil {
		return e
	}
	if _, e = q.ExecContext(ctx, "update imap_locators set valid=0 where account_id=? and provider_id=? and collection_id=?", a.account.ID, a.account.ProviderID, scope.CollectionID); e != nil {
		return e
	}
	if _, e = q.ExecContext(ctx, "delete from memberships where account_id=? and provider_id=? and collection_id=?", a.account.ID, a.account.ProviderID, scope.CollectionID); e != nil {
		return e
	}
	if _, e = q.ExecContext(ctx, "update messages set "+membershipProjection+" where account_id=? and provider_id=? and n in(select n from imap_locators where account_id=? and provider_id=? and collection_id=? and epoch_generation=?)", a.account.ID, a.account.ProviderID, a.account.ID, a.account.ProviderID, scope.CollectionID, generation); e != nil {
		return e
	}

	_, e = q.ExecContext(ctx, "delete from provider_sync_state where account_id=? and provider_id=? and collection_id=?", a.account.ID, a.account.ProviderID, scope.CollectionID)
	return e
}

// reconcileIMAPEpoch is the shared transactional conditional work. Production
// always calls it inside AccountStore.transaction; no transaction escapes.
func (a *AccountStore) reconcileIMAPEpoch(ctx context.Context, q sqlAccess, scope mailbox.CollectionRef, expected IMAPEpochState, mailboxID string, validity uint32, out *IMAPEpochState) error {
	current, e := a.imapEpochState(ctx, q, scope)
	if e != nil {
		return e
	}
	if current != expected {
		return stale("IMAP observation changed")
	}
	if current.MailboxID == mailboxID && current.UIDValidity == validity {
		*out = current
		return nil
	}
	if current.EpochGeneration == math.MaxInt64 {
		return stale("IMAP epoch generation exhausted")
	}
	// FIRST write proves all preconditions while obtaining SQLite writer ownership.
	result, e := q.ExecContext(ctx, `update collections set mailbox_id=?,uidvalidity=?,epoch_generation=epoch_generation+1
where account_id=? and provider_id=? and collection_id=? and mailbox_id=? and uidvalidity=? and epoch_generation=?
and exists(select 1 from store_binding where singleton=1 and account_id=? and provider_id=? and generation=?)
and ((?=0 and not exists(select 1 from provider_sync_state where account_id=? and provider_id=? and collection_id=?))
 or (?=1 and exists(select 1 from provider_sync_state where account_id=? and provider_id=? and collection_id=? and generation=? and identity_epoch=? and checkpoint=?)))`,
		mailboxID, validity, a.account.ID, a.account.ProviderID, scope.CollectionID, expected.MailboxID, expected.UIDValidity, expected.EpochGeneration,
		a.account.ID, a.account.ProviderID, expected.BindingGeneration, expected.SyncPresent, a.account.ID, a.account.ProviderID, scope.CollectionID,
		expected.SyncPresent, a.account.ID, a.account.ProviderID, scope.CollectionID, expected.SyncGeneration, expected.SyncEpoch, expected.Checkpoint.Token)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return stale("IMAP observation changed")
	}
	if e = a.retireIMAPEpoch(ctx, q, scope, current.EpochGeneration); e != nil {
		return e
	}
	*out, e = a.imapEpochState(ctx, q, scope)
	return e
}
