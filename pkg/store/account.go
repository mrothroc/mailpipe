package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// GraphProvider is the provider key used for verified legacy Graph attribution.
const GraphProvider mailbox.ProviderID = "graph"

// AccountStore is one permanently bound account. It deliberately does not embed
// Store: accountless Graph methods cannot be used on bound storage.
type AccountStore struct {
	store   *Store
	account mailbox.Account
	owner   string
}

// OpenAccount explicitly binds only an entirely empty database, or opens the
// exact existing account/provider binding. Historical stores need BindLegacy.
func OpenAccount(ctx context.Context, path string, account mailbox.Account) (*AccountStore, error) {
	s, e := openStore(ctx, path, &account)
	if e != nil {
		return nil, e
	}
	return &AccountStore{store: s, account: account, owner: randomID()}, nil
}
func randomID() string { return rand.Text() }

// Close drains this handle's SQL operations before releasing its shared lease.
func (a *AccountStore) Close() error { return a.store.Close() }
func (a *AccountStore) transaction(ctx context.Context, work func(sqlAccess) error) error {
	s := a.store
	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.closed {
		return sql.ErrConnDone
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := s.life.validate(); e != nil {
		return e
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	selected, _, bound, e := readBinding(ctx, tx)
	if e != nil {
		return e
	}
	if !bound || selected != a.account {
		return &mailbox.AccountMismatchError{Operation: "account storage", Expected: a.account.ID, Actual: selected.ID}
	}
	if e = work(tx); e != nil {
		return e
	}
	return tx.Commit()
}
func stale(reason string) error {
	return &mailbox.StaleIdentityError{Operation: "storage", Reason: reason}
}
func (a *AccountStore) local(r mailbox.LocalRef) error      { return r.Validate(a.account.ID) }
func (a *AccountStore) scope(r mailbox.CollectionRef) error { return r.Validate(a.account.ID) }

// IMAPLocator is structured occurrence data, separate from the opaque domain
// identity. EpochGeneration is obtained from Collection, never a sequence number.
type IMAPLocator struct {
	CollectionID     mailbox.CollectionID
	MailboxID        string
	UIDValidity, UID uint32
	EpochGeneration  int64
}

// MessageRecord supplies complete metadata plus optional correlation/location
// data. Correlation values never establish identity or deduplicate occurrences.
type MessageRecord struct {
	Message                                   mailbox.Message
	Revision, EmailID, ContentDigest, GraphID string
	IMAP                                      *IMAPLocator
}

// StoredMessage includes history-preserving invalid identities and zero-label
// messages when loaded by local reference. Lists contain live members only.
type StoredMessage struct {
	Local                  mailbox.LocalRef
	State                  mailbox.State
	GraphID                *string
	Body                   *string
	EmailID, ContentDigest string
	IMAP                   *IMAPLocator
	Valid                  bool
}

// CollectionInfo contains an optional current IMAP mailbox identity epoch.
type CollectionInfo struct {
	Collection      mailbox.Collection
	MailboxID       string
	UIDValidity     uint32
	EpochGeneration int64
}

// AccountFilter uses collection identity rather than legacy folder names.
type AccountFilter struct {
	Collection *mailbox.CollectionRef
	Filter     Filter
}

func (a *AccountStore) collection(ctx context.Context, q sqlAccess, ref mailbox.CollectionRef) (CollectionInfo, error) {
	var c CollectionInfo
	c.Collection.Ref = ref
	e := q.QueryRowContext(ctx, `select name,mailbox_id,uidvalidity,epoch_generation from collections where account_id=? and provider_id=? and collection_id=?`, a.account.ID, a.account.ProviderID, ref.CollectionID).Scan(&c.Collection.Name, &c.MailboxID, &c.UIDValidity, &c.EpochGeneration)
	if errors.Is(e, sql.ErrNoRows) {
		return c, stale("collection is not stored")
	}
	return c, e
}
func (a *AccountStore) putCollection(ctx context.Context, q sqlAccess, c mailbox.Collection) error {
	if e := a.scope(c.Ref); e != nil {
		return e
	}
	_, e := q.ExecContext(ctx, `insert into collections(account_id,provider_id,collection_id,name) values(?,?,?,?) on conflict(account_id,provider_id,collection_id) do update set name=case when excluded.name='' then collections.name else excluded.name end`, a.account.ID, a.account.ProviderID, c.Ref.CollectionID, c.Name)
	return e
}

// PutCollection records a display name without changing an identity epoch.
func (a *AccountStore) PutCollection(ctx context.Context, c mailbox.Collection) error {
	return a.transaction(ctx, func(q sqlAccess) error { return a.putCollection(ctx, q, c) })
}

// Collection returns the current epoch generation needed for IMAP identities.
func (a *AccountStore) Collection(ctx context.Context, ref mailbox.CollectionRef) (out CollectionInfo, err error) {
	if err = a.scope(ref); err != nil {
		return
	}
	err = a.transaction(ctx, func(q sqlAccess) error { var e error; out, e = a.collection(ctx, q, ref); return e })
	return
}

func (a *AccountStore) upsertMessage(ctx context.Context, q sqlAccess, c mailbox.Collection, r MessageRecord) (mailbox.LocalRef, error) {
	var local mailbox.LocalRef
	local.AccountID = a.account.ID
	if e := r.Message.Ref.Validate(a.account.ID); e != nil {
		return local, e
	}
	if e := a.scope(c.Ref); e != nil {
		return local, e
	}
	if (a.account.ProviderID == GraphProvider && (r.GraphID == "" || mailbox.MessageID(r.GraphID) != r.Message.Ref.MessageID)) || (a.account.ProviderID != GraphProvider && r.GraphID != "") {
		return local, errors.New("store: Graph compatibility identity is inconsistent with provider")
	}
	if e := a.putCollection(ctx, q, c); e != nil {
		return local, e
	}
	var oldLocator sql.NullString
	var valid bool
	e := q.QueryRowContext(ctx, "select n,locator,identity_valid from messages where account_id=? and provider_id=? and provider_message_id=?", a.account.ID, a.account.ProviderID, r.Message.Ref.MessageID).Scan(&local.N, &oldLocator, &valid)
	existed := e == nil
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return local, e
	}
	if existed && !valid {
		return local, stale("retired occurrence identity cannot be reused")
	}
	var oldCount int
	if existed {
		if e = q.QueryRowContext(ctx, "select count(*) from imap_locators where n=? and valid=1", local.N).Scan(&oldCount); e != nil {
			return local, e
		}
		if oldCount != 0 && r.IMAP == nil {
			return local, stale("structured occurrence locator is required")
		}
	}
	infoForScope, scopeErr := a.collection(ctx, q, c.Ref)
	if scopeErr != nil {
		return local, scopeErr
	}
	if infoForScope.MailboxID != "" && r.IMAP == nil {
		return local, stale("IMAP collection requires a structured occurrence locator")
	}
	if a.account.ProviderID == GraphProvider && r.IMAP != nil {
		return local, errors.New("store: Graph identities cannot carry IMAP locators")
	}
	if r.IMAP != nil {
		l := r.IMAP
		if l.CollectionID != c.Ref.CollectionID || strings.TrimSpace(l.MailboxID) == "" || l.UIDValidity == 0 || l.UID == 0 || l.EpochGeneration <= 0 {
			return local, errors.New("store: complete IMAP occurrence tuple required")
		}
		if existed && oldCount == 0 {
			return local, stale("generic identity cannot acquire an IMAP occurrence")
		}
		if e := a.initializeIMAPEpoch(ctx, q, c.Ref, IMAPEpoch{MailboxID: l.MailboxID, UIDValidity: l.UIDValidity, Generation: l.EpochGeneration}); e != nil {
			return local, e
		}
		if existed && oldCount > 0 {
			var prior IMAPLocator
			if e = q.QueryRowContext(ctx, "select collection_id,mailbox_id,uidvalidity,uid,epoch_generation from imap_locators where n=? and valid=1", local.N).Scan(&prior.CollectionID, &prior.MailboxID, &prior.UIDValidity, &prior.UID, &prior.EpochGeneration); e != nil {
				return local, e
			}
			if prior != *l {
				return local, stale("occurrence identity was reused across locations")
			}
		}
	}
	meta, e := json.Marshal(r.Message.Metadata)
	if e != nil {
		return local, e
	}
	received := ""
	if !r.Message.Metadata.Received.IsZero() {
		received = r.Message.Metadata.Received.UTC().Format(receivedLayout)
	}
	recipients := make([]string, len(r.Message.Metadata.Recipients))
	for i, v := range r.Message.Metadata.Recipients {
		recipients[i] = strings.ToLower(v.Address)
	}
	recipientsJSON, _ := json.Marshal(recipients)
	var graph any
	if r.GraphID != "" {
		graph = r.GraphID
	}
	args := []any{graph, a.account.ID, a.account.ProviderID, r.Message.Ref.MessageID, r.Message.Ref.Locator, r.Revision, r.EmailID, r.ContentDigest, string(meta), r.Message.Metadata.InternetMessageID, received, strings.ToLower(r.Message.Metadata.Sender.Address), r.Message.Metadata.Sender.Name, string(recipientsJSON), r.Message.Metadata.Subject, r.Message.Metadata.Preview, r.Message.Metadata.HasAttachments, r.Message.Metadata.IsRead}
	if existed {
		args = append(args, local.N)
		_, e = q.ExecContext(ctx, `update messages set graph_id=?,account_id=?,provider_id=?,provider_message_id=?,locator=?,revision=?,email_id=?,content_digest=?,metadata=?,internet_message_id=?,received=?,sender=?,sender_name=?,recipients=?,subject=?,preview=?,has_attachments=?,is_read=?,identity_valid=1 where n=?`, args...)
	} else {
		res, err := q.ExecContext(ctx, `insert into messages(graph_id,account_id,provider_id,provider_message_id,locator,revision,email_id,content_digest,metadata,internet_message_id,received,sender,sender_name,recipients,subject,preview,has_attachments,is_read,folder,identity_valid) values(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'',1)`, args...)
		e = err
		if e == nil {
			local.N, e = res.LastInsertId()
		}
	}
	if e != nil {
		return local, e
	}
	if r.IMAP != nil {
		l := r.IMAP
		_, e = q.ExecContext(ctx, `insert into imap_locators(n,account_id,provider_id,collection_id,mailbox_id,uidvalidity,uid,epoch_generation,valid) values(?,?,?,?,?,?,?,?,1) on conflict(n,epoch_generation) do update set valid=1`, local.N, a.account.ID, a.account.ProviderID, l.CollectionID, l.MailboxID, l.UIDValidity, l.UID, l.EpochGeneration)
		if e != nil {
			return local, e
		}
	}
	_, e = q.ExecContext(ctx, "insert or ignore into memberships(account_id,provider_id,n,collection_id) values(?,?,?,?)", a.account.ID, a.account.ProviderID, local.N, c.Ref.CollectionID)
	if e != nil {
		return local, e
	}
	return local, projectMembership(ctx, q, local.N)
}
func projectMembership(ctx context.Context, q sqlAccess, n int64) error {
	_, e := q.ExecContext(ctx, `update messages set removed=not exists(select 1 from memberships where n=?),folder=case when (select count(*) from memberships where n=?)=1 then (select collection_id from memberships where n=?) else '' end where n=?`, n, n, n, n)
	return e
}

// UpsertMessage refreshes one proven identity and adds only this collection's
// membership. Existing unrelated memberships, body and history are retained.
func (a *AccountStore) UpsertMessage(ctx context.Context, c mailbox.Collection, r MessageRecord) (out mailbox.LocalRef, err error) {
	err = a.transaction(ctx, func(q sqlAccess) error { var e error; out, e = a.upsertMessage(ctx, q, c, r); return e })
	return
}
func (a *AccountStore) getMessage(ctx context.Context, q sqlAccess, ref mailbox.LocalRef) (out StoredMessage, err error) {
	if err = a.local(ref); err != nil {
		return
	}
	out.Local = ref
	out.State.Ref.AccountID = a.account.ID
	var graph, body sql.NullString
	var meta string
	err = q.QueryRowContext(ctx, `select provider_message_id,locator,coalesce(revision,''),coalesce(metadata,''),graph_id,body,coalesce(email_id,''),coalesce(content_digest,''),identity_valid from messages where n=? and account_id=? and provider_id=?`, ref.N, a.account.ID, a.account.ProviderID).Scan(&out.State.Ref.MessageID, &out.State.Ref.Locator, &out.State.Revision, &meta, &graph, &body, &out.EmailID, &out.ContentDigest, &out.Valid)
	if errors.Is(err, sql.ErrNoRows) {
		err = stale("local message is not stored")
		return
	}
	if err != nil {
		return
	}
	if err = json.Unmarshal([]byte(meta), &out.State.Metadata); err != nil {
		return
	}
	if graph.Valid {
		out.GraphID = &graph.String
	}
	if body.Valid {
		out.Body = &body.String
	}
	rows, e := q.QueryContext(ctx, "select collection_id from memberships where account_id=? and provider_id=? and n=? order by collection_id", a.account.ID, a.account.ProviderID, ref.N)
	if e != nil {
		err = e
		return
	}
	for rows.Next() {
		var id mailbox.CollectionID
		if e = rows.Scan(&id); e != nil {
			break
		}
		out.State.Memberships = append(out.State.Memberships, mailbox.Membership{Collection: mailbox.CollectionRef{AccountID: a.account.ID, CollectionID: id}})
	}
	err = errors.Join(e, rows.Err())
	rows.Close()
	if err != nil {
		return
	}
	var l IMAPLocator
	e = q.QueryRowContext(ctx, "select collection_id,mailbox_id,uidvalidity,uid,epoch_generation from imap_locators where n=? order by valid desc,epoch_generation desc limit 1", ref.N).Scan(&l.CollectionID, &l.MailboxID, &l.UIDValidity, &l.UID, &l.EpochGeneration)
	if e == nil {
		out.IMAP = &l
	} else if !errors.Is(e, sql.ErrNoRows) {
		err = e
	}
	return
}

// GetMessage loads a stable local handle, including retired identity history.
func (a *AccountStore) GetMessage(ctx context.Context, ref mailbox.LocalRef) (out StoredMessage, err error) {
	err = a.transaction(ctx, func(q sqlAccess) error { var e error; out, e = a.getMessage(ctx, q, ref); return e })
	return
}

// RemoveMembership detaches one collection without removing identity/history.
func (a *AccountStore) RemoveMembership(ctx context.Context, ref mailbox.LocalRef, scope mailbox.CollectionRef) error {
	if e := a.local(ref); e != nil {
		return e
	}
	if e := a.scope(scope); e != nil {
		return e
	}
	return a.transaction(ctx, func(q sqlAccess) error {
		if _, e := a.getMessage(ctx, q, ref); e != nil {
			return e
		}
		_, e := q.ExecContext(ctx, "delete from memberships where account_id=? and provider_id=? and n=? and collection_id=?", a.account.ID, a.account.ProviderID, ref.N, scope.CollectionID)
		if e != nil {
			return e
		}
		return projectMembership(ctx, q, ref.N)
	})
}

// AddMembership attaches an existing identity without manufacturing another N.
func (a *AccountStore) AddMembership(ctx context.Context, ref mailbox.LocalRef, c mailbox.Collection) error {
	return a.transaction(ctx, func(q sqlAccess) error {
		m, e := a.getMessage(ctx, q, ref)
		if e != nil {
			return e
		}
		if !m.Valid {
			return stale("message identity retired")
		}
		if e = a.putCollection(ctx, q, c); e != nil {
			return e
		}
		info, e := a.collection(ctx, q, c.Ref)
		if e != nil {
			return e
		}
		if info.MailboxID != "" && (m.IMAP == nil || m.IMAP.CollectionID != c.Ref.CollectionID) {
			return stale("another IMAP mailbox requires a distinct occurrence")
		}
		_, e = q.ExecContext(ctx, "insert or ignore into memberships values(?,?,?,?)", a.account.ID, a.account.ProviderID, ref.N, c.Ref.CollectionID)
		if e != nil {
			return e
		}
		return projectMembership(ctx, q, ref.N)
	})
}
func (a *AccountStore) accountWhere(f AccountFilter, extra string, args []any) (string, []any, error) {
	if f.Filter.Folder != "" {
		return "", nil, errors.New("store: use collection identity instead of a legacy folder filter")
	}
	w, whereArgs := where(f.Filter)
	w += " and account_id=? and provider_id=? and identity_valid=1 and exists(select 1 from memberships where memberships.n=messages.n)"
	whereArgs = append(whereArgs, a.account.ID, a.account.ProviderID)
	if f.Collection != nil {
		if e := a.scope(*f.Collection); e != nil {
			return "", nil, e
		}
		w += " and exists(select 1 from memberships where memberships.n=messages.n and collection_id=?)"
		whereArgs = append(whereArgs, f.Collection.CollectionID)
	}
	if extra != "" {
		w += " and " + extra
		whereArgs = append(whereArgs, args...)
	}
	return w, whereArgs, nil
}
func (a *AccountStore) messageNumbers(ctx context.Context, q sqlAccess, f AccountFilter, extra string, args []any) ([]int64, error) {
	w, whereArgs, e := a.accountWhere(f, extra, args)
	if e != nil {
		return nil, e
	}
	rows, e := q.QueryContext(ctx, "select n from messages where "+w+" order by received desc,n", whereArgs...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var ns []int64
	for rows.Next() {
		var n int64
		if e = rows.Scan(&n); e != nil {
			return nil, e
		}
		ns = append(ns, n)
	}
	return ns, rows.Err()
}
func (a *AccountStore) messages(ctx context.Context, f AccountFilter, extra string, args []any) (out []StoredMessage, err error) {
	err = a.transaction(ctx, func(q sqlAccess) error {
		ns, e := a.messageNumbers(ctx, q, f, extra, args)
		if e != nil {
			return e
		}
		for _, n := range ns {
			m, e := a.getMessage(ctx, q, mailbox.LocalRef{AccountID: a.account.ID, N: n})
			if e != nil {
				return e
			}
			out = append(out, m)
		}
		return nil
	})
	return
}

// Messages lists live identities once regardless of their membership count.
func (a *AccountStore) Messages(ctx context.Context, f AccountFilter) ([]StoredMessage, error) {
	return a.messages(ctx, f, "", nil)
}

// Count counts distinct live local handles, not membership rows.
func (a *AccountStore) Count(ctx context.Context, f AccountFilter) (out int, err error) {
	err = a.transaction(ctx, func(q sqlAccess) error {
		w, args, e := a.accountWhere(f, "", nil)
		if e != nil {
			return e
		}
		return q.QueryRowContext(ctx, "select count(*) from messages where "+w, args...).Scan(&out)
	})
	return
}

// ByMessageID returns all live copies; a blank header never correlates mail.
func (a *AccountStore) ByMessageID(ctx context.Context, id string) ([]StoredMessage, error) {
	if strings.TrimSpace(id) == "" {
		return nil, a.transaction(ctx, func(sqlAccess) error { return nil })
	}
	return a.messages(ctx, AccountFilter{}, "internet_message_id=?", []any{id})
}

// ByEmailID returns all live occurrences sharing a nonunique correlation ID.
func (a *AccountStore) ByEmailID(ctx context.Context, id string) ([]StoredMessage, error) {
	if strings.TrimSpace(id) == "" {
		return nil, a.transaction(ctx, func(sqlAccess) error { return nil })
	}
	return a.messages(ctx, AccountFilter{}, "email_id=?", []any{id})
}

// SetBody caches content only for a currently valid account-bound identity.
func (a *AccountStore) SetBody(ctx context.Context, ref mailbox.LocalRef, body string) error {
	return a.transaction(ctx, func(q sqlAccess) error {
		m, e := a.getMessage(ctx, q, ref)
		if e != nil {
			return e
		}
		if !m.Valid {
			return stale("message identity retired")
		}
		_, e = q.ExecContext(ctx, "update messages set body=? where n=?", body, ref.N)
		return e
	})
}

// AddVerdict appends provenance to the validated local identity.
func (a *AccountStore) AddVerdict(ctx context.Context, ref mailbox.LocalRef, v Verdict) error {
	return a.AddVerdicts(ctx, ref, []Verdict{v})
}

// AddVerdicts atomically records a batch for exactly one local identity.
func (a *AccountStore) AddVerdicts(ctx context.Context, ref mailbox.LocalRef, vs []Verdict) error {
	return a.transaction(ctx, func(q sqlAccess) error {
		m, e := a.getMessage(ctx, q, ref)
		if e != nil {
			return e
		}
		if !m.Valid {
			return stale("message identity retired")
		}
		for _, v := range vs {
			if v.N != 0 && v.N != ref.N {
				return stale("verdict local identity differs")
			}
			v.N = ref.N
			if e = addVerdict(ctx, q, v); e != nil {
				return e
			}
		}
		return nil
	})
}

// Verdicts reads complete per-message history, even for a retired identity.
func (a *AccountStore) Verdicts(ctx context.Context, ref mailbox.LocalRef) (out []Verdict, err error) {
	err = a.transaction(ctx, func(q sqlAccess) error {
		if _, e := a.getMessage(ctx, q, ref); e != nil {
			return e
		}
		rows, e := q.QueryContext(ctx, `select id,n,stage,labels,scores,action,reason,cost_usd,final,provenance,error,at from verdicts where n=? order by id`, ref.N)
		if e != nil {
			return e
		}
		out, e = scanVerdicts(rows)
		return e
	})
	return
}

// InvalidateIMAPEpoch retires old occurrence mappings and this membership only.
func (a *AccountStore) InvalidateIMAPEpoch(ctx context.Context, scope mailbox.CollectionRef, mailboxID string, validity uint32) error {
	if e := a.scope(scope); e != nil {
		return e
	}
	if strings.TrimSpace(mailboxID) == "" || validity == 0 {
		return errors.New("store: IMAP epoch requires mailbox and UIDVALIDITY")
	}
	return a.transaction(ctx, func(q sqlAccess) error {
		info, e := a.collection(ctx, q, scope)
		if e != nil {
			return e
		}
		if info.MailboxID == "" || info.UIDValidity == 0 {
			return stale("collection has no initialized IMAP epoch")
		}
		if info.MailboxID == mailboxID && info.UIDValidity == validity {
			return nil
		}
		if _, e = q.ExecContext(ctx, "update messages set identity_valid=0 where n in(select n from imap_locators where account_id=? and provider_id=? and collection_id=? and valid=1)", a.account.ID, a.account.ProviderID, scope.CollectionID); e != nil {
			return e
		}
		if _, e = q.ExecContext(ctx, "update imap_locators set valid=0 where account_id=? and provider_id=? and collection_id=?", a.account.ID, a.account.ProviderID, scope.CollectionID); e != nil {
			return e
		}
		if _, e = q.ExecContext(ctx, "delete from memberships where account_id=? and provider_id=? and collection_id=?", a.account.ID, a.account.ProviderID, scope.CollectionID); e != nil {
			return e
		}
		if _, e = q.ExecContext(ctx, "update collections set mailbox_id=?,uidvalidity=?,epoch_generation=epoch_generation+1 where account_id=? and provider_id=? and collection_id=?", mailboxID, validity, a.account.ID, a.account.ProviderID, scope.CollectionID); e != nil {
			return e
		}
		_, e = q.ExecContext(ctx, "delete from provider_sync_state where account_id=? and provider_id=? and collection_id=?", a.account.ID, a.account.ProviderID, scope.CollectionID)
		return e
	})
}

// LegacyPreparation is an opaque, exact inventory challenge. Only the package
// creates it; verification is a trusted adapter seam, not authentication itself.
type LegacyPreparation struct {
	digest, nonce, path string
	Live                []Message
}

// Digest identifies the complete prepared schema and data without exposing it.
func (p LegacyPreparation) Digest() string { return p.digest }

// LegacyVerification must cover every live N and map each legacy folder to its
// authenticated opaque collection. Historical attribution needs owner consent.
type LegacyVerification struct {
	Account                        mailbox.Account
	Nonce                          string
	States                         map[int64]mailbox.State
	Folders                        map[string]mailbox.CollectionRef
	HistoricalAttributionConfirmed bool
}

// LegacyBindingVerifier establishes remote account provenance and owner consent
// outside storage. Implementations must inspect the entire supplied inventory.
type LegacyBindingVerifier interface {
	VerifyLegacy(context.Context, mailbox.Account, LegacyPreparation) (LegacyVerification, error)
}

// Challenge lets the verifier bind its response to the exact preparation.
func (p LegacyPreparation) Challenge() string { return p.nonce }
func maintenanceDB(ctx context.Context, path string) (*lifecycleLease, *sql.DB, error) {
	life, e := acquireLifecycle(ctx, path, true, diskFiles{})
	if e != nil {
		return nil, nil, e
	}
	db, e := sqliteOpen(ctx, life.entry.path)
	if e != nil {
		life.release()
		return nil, nil, e
	}
	current, e := schemaCurrent(ctx, db)
	if e != nil || !current {
		cleanupErr := closeMaintenance(life, db)
		e = errors.Join(e, cleanupErr)
		if e == nil {
			e = errors.New("store: open legacy storage once to migrate before preparing binding")
		}
		return nil, nil, e
	}
	_, e = matchBinding(ctx, db, nil)
	if e != nil {
		return nil, nil, errors.Join(e, closeMaintenance(life, db))
	}
	return life, db, nil
}
func closeMaintenance(l *lifecycleLease, db *sql.DB) error {
	if e := db.Close(); e != nil {
		return l.uncertain(e)
	}
	return l.release()
}

// PrepareLegacyBinding inventories an unbound store under exclusive lifecycle
// ownership. It never infers an account from configuration or message headers.
func PrepareLegacyBinding(ctx context.Context, path string) (out LegacyPreparation, err error) {
	life, db, e := maintenanceDB(ctx, path)
	if e != nil {
		return out, e
	}
	defer func() { err = errors.Join(err, closeMaintenance(life, db)) }()
	out.path = life.entry.path
	out.nonce = randomID()
	out.digest, e = logicalDigest(ctx, db)
	if e != nil {
		return out, e
	}
	rows, e := db.QueryContext(ctx, "select "+inventoryColumns+" from messages where removed=0 order by n")
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		m, e := scan(rows)
		if e != nil {
			return out, e
		}
		out.Live = append(out.Live, m)
	}
	return out, rows.Err()
}

// BindLegacy permanently attributes verified legacy history. All prebinding
// plans stay auditable but lack bound execution preconditions.
func BindLegacy(ctx context.Context, path string, account mailbox.Account, p LegacyPreparation, verifier LegacyBindingVerifier) (out *AccountStore, err error) {
	if e := account.Validate(); e != nil {
		return nil, e
	}
	if account.ProviderID != GraphProvider || verifier == nil || p.digest == "" || p.nonce == "" {
		return nil, errors.New("store: complete Graph legacy verification required")
	}
	life, db, e := maintenanceDB(ctx, path)
	if e != nil {
		return nil, e
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, closeMaintenance(life, db))
		}
	}()
	if life.entry.path != p.path {
		return nil, stale("preparation belongs to another database")
	}
	digest, e := logicalDigest(ctx, db)
	if e != nil {
		return nil, e
	}
	if digest != p.digest {
		return nil, stale("legacy preparation changed")
	}
	// Re-read the live inventory; exported slices supplied to a verifier are never
	// authority for the actual stored data.
	rows, e := db.QueryContext(ctx, "select "+inventoryColumns+" from messages where removed=0 order by n")
	if e != nil {
		return nil, e
	}
	var live []Message
	for rows.Next() {
		m, e := scan(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		live = append(live, m)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	p.Live = make([]Message, len(live))
	for i, m := range live {
		p.Live[i] = m
		p.Live[i].Recipients = append([]string(nil), m.Recipients...)
		if m.Body != nil {
			body := *m.Body
			p.Live[i].Body = &body
		}
	}
	proof, e := verifier.VerifyLegacy(ctx, account, p)
	if e != nil {
		return nil, e
	}
	if proof.Account != account {
		return nil, &mailbox.AccountMismatchError{Operation: "legacy verification", Expected: account.ID, Actual: proof.Account.ID}
	}
	if proof.Nonce != p.nonce || !proof.HistoricalAttributionConfirmed || len(proof.States) != len(live) {
		return nil, errors.New("store: complete verified inventory and historical attribution required")
	}
	for _, m := range live {
		state, ok := proof.States[m.N]
		if !ok {
			return nil, stale("live identity was not verified")
		}
		scope, ok := proof.Folders[m.Folder]
		if !ok || scope.Validate(account.ID) != nil {
			return nil, stale("legacy collection mapping is missing")
		}
		expected := mailbox.State{Ref: mailbox.MessageRef{AccountID: account.ID, MessageID: mailbox.MessageID(m.GraphID), Locator: mailbox.Locator(m.GraphID)}, Metadata: mailbox.Metadata{InternetMessageID: m.InternetMessageID, Sender: mailbox.Address{Address: m.Sender}}, Memberships: []mailbox.Membership{{Collection: scope}}}
		if e = mailbox.CheckPrecondition(account.ID, expected, state); e != nil {
			return nil, e
		}
	}
	conn, e := db.Conn(ctx)
	if e != nil {
		return nil, e
	}
	_, backupDigest, e := backupDatabase(ctx, life, conn)
	if e != nil {
		conn.Close()
		return nil, e
	}
	if backupDigest != p.digest {
		conn.Close()
		return nil, stale("legacy inventory changed during verification")
	}
	a := &AccountStore{account: account}
	e = immediate(ctx, conn, func(q sqlAccess) error {
		digest, e := logicalDigest(ctx, q)
		if e != nil {
			return e
		}
		if digest != p.digest {
			return stale("legacy inventory changed before binding")
		}
		if _, e = q.ExecContext(ctx, "insert into store_binding values(1,?,?,1,?)", account.ID, account.ProviderID, time.Now().UTC().Format(time.RFC3339)); e != nil {
			return e
		}
		// Preserve every original column, including the legacy folder projection.
		rs, e := q.QueryContext(ctx, "select "+inventoryColumns+",removed from messages order by n")
		if e != nil {
			return e
		}
		type legacy struct {
			m       Message
			removed bool
		}
		var all []legacy
		for rs.Next() {
			var m Message
			var recipients string
			var body sql.NullString
			var removed bool
			e = rs.Scan(&m.N, &m.GraphID, &m.InternetMessageID, &m.Folder, &m.Received, &m.Sender, &m.SenderName, &recipients, &m.Subject, &m.Preview, &m.HasAttachments, &m.IsRead, &body, &removed)
			if e != nil {
				break
			}
			if e = json.Unmarshal([]byte(recipients), &m.Recipients); e != nil {
				break
			}
			all = append(all, legacy{m, removed})
		}
		e = errors.Join(e, rs.Err())
		rs.Close()
		if e != nil {
			return e
		}
		for _, row := range all {
			m := row.m
			metadata := legacyMetadata(m)
			valid := m.GraphID != ""
			state := proof.States[m.N]
			if !row.removed {
				metadata = state.Metadata
			}
			raw, e := json.Marshal(metadata)
			if e != nil {
				return e
			}
			_, e = q.ExecContext(ctx, "update messages set account_id=?,provider_id=?,provider_message_id=graph_id,locator=graph_id,revision=?,identity_valid=?,metadata=? where n=?", account.ID, account.ProviderID, state.Revision, valid, string(raw), m.N)
			if e != nil {
				return e
			}
			if !row.removed {
				for _, mem := range state.Memberships {
					if e = a.putCollection(ctx, q, mailbox.Collection{Ref: mem.Collection}); e != nil {
						return e
					}
					if _, e = q.ExecContext(ctx, "insert into memberships values(?,?,?,?)", account.ID, account.ProviderID, m.N, mem.Collection.CollectionID); e != nil {
						return e
					}
				}
			}
		}
		for _, table := range []string{"plans", "plan_items", "actions"} {
			if _, e = q.ExecContext(ctx, "update "+table+" set account_id=?,provider_id=?", account.ID, account.ProviderID); e != nil {
				return e
			}
		}
		return integrity(ctx, q)
	})
	conn.Close()
	if e != nil {
		return nil, e
	}
	if e = closeMaintenance(life, db); e != nil {
		closed = true
		return nil, e
	}
	closed = true
	return OpenAccount(ctx, path, account)
}
func legacyMetadata(m Message) mailbox.Metadata {
	meta := mailbox.Metadata{InternetMessageID: m.InternetMessageID, Sender: mailbox.Address{Address: m.Sender, Name: m.SenderName}, Subject: m.Subject, Preview: m.Preview, HasAttachments: m.HasAttachments, IsRead: m.IsRead}
	meta.Received, _ = time.Parse(receivedLayout, m.Received)
	for _, r := range m.Recipients {
		meta.Recipients = append(meta.Recipients, mailbox.Address{Address: r})
	}
	return meta
}
func canonicalState(s mailbox.State) mailbox.State {
	s.Memberships = append([]mailbox.Membership{}, s.Memberships...)
	sort.Slice(s.Memberships, func(i, j int) bool {
		return s.Memberships[i].Collection.CollectionID < s.Memberships[j].Collection.CollectionID
	})
	return s
}
func jsonText(v any) (string, error) { b, e := json.Marshal(v); return string(b), e }
func (a *AccountStore) epochs(ctx context.Context, q sqlAccess, s mailbox.State) (map[string]int64, error) {
	out := map[string]int64{}
	for _, m := range s.Memberships {
		c, e := a.collection(ctx, q, m.Collection)
		if e != nil {
			return nil, e
		}
		out[string(m.Collection.CollectionID)] = c.EpochGeneration
	}
	return out, nil
}

// Account returns the immutable selected account/provider pair, without secrets.
func (a *AccountStore) Account() mailbox.Account { return a.account }
