// Package store persists mailbox metadata, account-bound identities, memberships
// and reviewed action history in private SQLite databases. Stable local numbers
// survive migration. Legacy-unbound Graph APIs remain separate from account APIs.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo
)

const schema = `
create table if not exists messages (
    n integer primary key,
    graph_id text,
    account_id text not null default '',
    provider_id text not null default '',
    provider_message_id text, locator text, revision text,
    identity_valid integer not null default 0,
    email_id text, content_digest text, metadata text,
    internet_message_id text,
    folder text not null,
    received text,
    sender text,
    sender_name text,
    recipients text,
    subject text,
    preview text,
    has_attachments integer not null default 0,
    is_read integer not null default 0,
    removed integer not null default 0,
    body text,
    unique(n,account_id,provider_id)
);
create unique index if not exists messages_graph on messages(graph_id) where graph_id is not null;
create unique index if not exists messages_provider on messages(account_id,provider_id,provider_message_id) where provider_message_id is not null;
create index if not exists messages_emailid on messages(email_id);
create index if not exists messages_content_digest on messages(content_digest);
create index if not exists messages_imid on messages(internet_message_id);
create index if not exists messages_sender on messages(sender);
create index if not exists messages_folder on messages(folder, removed, received);
create table if not exists sync_state (folder text primary key, delta_link text, updated text);
create table if not exists verdicts (
    id integer primary key,
    n integer not null references messages(n),
    stage text not null,
    labels text not null default '[]',
    scores text not null default '{}',
    action text not null default '',
    reason text not null default '',
    cost_usd real not null default 0,
    final integer not null default 0,
    provenance text not null default '',
    error text not null default '',
    at text not null
);
create index if not exists verdicts_stage on verdicts(stage, n);
create index if not exists verdicts_n on verdicts(n, id);
create table if not exists plans (
    id integer primary key,
    scope text not null,
    created_at text not null
);
create table if not exists plan_items (
    plan_id integer not null references plans(id),
    n integer not null references messages(n),
    graph_id text not null,
    internet_message_id text not null,
    sender text not null,
    action text not null,
    reasons text not null default '[]',
    planned_folder text not null default '',
    no_block integer not null default 0,
    primary key (plan_id, n)
);
create index if not exists messages_live on messages(removed, sender);
create table if not exists actions (
    id integer primary key,
    plan_id integer not null references plans(id),
    n integer not null references messages(n),
    graph_id text not null,
    action text not null,
    destination text not null,
    status text not null check (status in ('moved', 'skipped', 'failed')),
    detail text not null default '',
    at text not null
);
create index if not exists actions_plan on actions(plan_id, status, n);
`

// Record is a message as reported by the mailbox, before it has a local number.
type Record struct {
	GraphID           string
	InternetMessageID string
	Received          string
	Sender            string
	SenderName        string
	Recipients        []string
	Subject           string
	Preview           string
	HasAttachments    bool
	IsRead            bool
}

// Message is a stored message.
type Message struct {
	N                 int64
	GraphID           string
	InternetMessageID string
	Folder            string
	Received          string
	Sender            string
	SenderName        string
	Recipients        []string
	Subject           string
	Preview           string
	HasAttachments    bool
	IsRead            bool
	Body              *string
}

// Change says whether an upsert brought a message into its folder or refreshed it there.
type Change int

const (
	Added Change = iota
	Updated
)

// Filter narrows Messages and Count.
type Filter struct {
	Folder     string
	SenderLike string    // case-insensitive substring of the sender address
	Since      time.Time // received at or after; zero means no limit
	Unread     bool      // only messages not yet read
	// Labelled ("stage:label") keeps messages whose latest successful
	// verdict from that stage carries that label.
	Labelled string
}

// receivedLayout is how Graph writes receivedDateTime (always UTC), so stored
// times compare correctly as text.
const receivedLayout = "2006-01-02T15:04:05Z"

// Store is an open database.
type Store struct {
	db        *sql.DB
	life      *lifecycleLease
	gate      sync.RWMutex
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

// Open opens a legacy-unbound Graph database. Opening is bounded to five seconds
// while waiting for lifecycle maintenance; ordinary sessions share the sidecar.
// Private directories/files are created 0700/0600. Bound files require OpenAccount.
func Open(path string) (*Store, error) { return OpenContext(context.Background(), path) }

// OpenContext is the cancelable legacy opener. Lifecycle waits are at most five
// seconds; migration work honors ctx without an arbitrary database-size limit.
func OpenContext(ctx context.Context, path string) (*Store, error) { return openStore(ctx, path, nil) }

// Close drains SQL work before releasing lifecycle ownership. It is idempotent.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.gate.Lock()
		defer s.gate.Unlock()
		s.closed = true
		if err := s.db.Close(); err != nil {
			s.closeErr = s.life.uncertain(err)
			return
		}
		s.closeErr = s.life.release()
	})
	return s.closeErr
}
func (s *Store) legacy(ctx context.Context) (func(), error) {
	s.gate.RLock()
	if s.closed {
		s.gate.RUnlock()
		return nil, sql.ErrConnDone
	}
	if err := ctx.Err(); err != nil {
		s.gate.RUnlock()
		return nil, err
	}
	if err := s.life.validate(); err != nil {
		s.gate.RUnlock()
		return nil, err
	}
	var n int
	err := s.db.QueryRowContext(ctx, "select count(*) from store_binding").Scan(&n)
	if err == nil && n != 0 {
		err = errors.New("store: account-aware opener required")
	}
	if err != nil {
		s.gate.RUnlock()
		return nil, err
	}
	return s.gate.RUnlock, nil
}

// Upsert inserts or refreshes a message in folder. It reports Added when the
// message is new to that folder (new, moved in, or previously removed) and
// Updated when it was already there.
func (s *Store) Upsert(ctx context.Context, folder string, r Record) (Change, error) {
	done, guardErr := s.legacy(ctx)
	if guardErr != nil {
		return 0, guardErr
	}
	defer done()

	recipients, err := json.Marshal(lower(r.Recipients))
	if err != nil {
		return 0, err
	}
	var prevFolder string
	var prevRemoved bool
	err = s.db.QueryRowContext(ctx, "select folder, removed from messages where graph_id=?", r.GraphID).Scan(&prevFolder, &prevRemoved)
	existed := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	args := []any{r.InternetMessageID, folder, r.Received, strings.ToLower(r.Sender), r.SenderName, string(recipients),
		r.Subject, r.Preview, r.HasAttachments, r.IsRead, r.GraphID}
	if existed {
		_, err = s.db.ExecContext(ctx, `update messages set internet_message_id=?, folder=?, received=?, sender=?,
			sender_name=?, recipients=?, subject=?, preview=?, has_attachments=?, is_read=?, removed=0 where graph_id=?`, args...)
	} else {
		_, err = s.db.ExecContext(ctx, `insert into messages (internet_message_id, folder, received, sender, sender_name,
			recipients, subject, preview, has_attachments, is_read, graph_id) values (?,?,?,?,?,?,?,?,?,?,?)`, args...)
	}
	if err != nil {
		return 0, err
	}
	if existed && prevFolder == folder && !prevRemoved {
		return Updated, nil
	}
	return Added, nil
}

func lower(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strings.ToLower(v)
	}
	return out
}

// Remove marks a message gone from folder. A message that has since moved to
// another folder is left alone. It reports whether anything changed.
func (s *Store) Remove(ctx context.Context, folder, graphID string) (bool, error) {
	done, guardErr := s.legacy(ctx)
	if guardErr != nil {
		return false, guardErr
	}
	defer done()

	res, err := s.db.ExecContext(ctx, "update messages set removed=1 where graph_id=? and folder=? and removed=0", graphID, folder)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RemoveUnseen marks every live message in folder that is not in seen as
// removed: after a complete pass, Graph's answer is authoritative.
func (s *Store) RemoveUnseen(ctx context.Context, folder string, seen map[string]bool) (int, error) {
	done, guardErr := s.legacy(ctx)
	if guardErr != nil {
		return 0, guardErr
	}
	defer done()

	rows, err := s.db.QueryContext(ctx, "select graph_id from messages where folder=? and removed=0", folder)
	if err != nil {
		return 0, err
	}
	var gone []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, id := range gone {
		if _, err := tx.ExecContext(ctx, "update messages set removed=1 where graph_id=? and folder=? and removed=0", id, folder); err != nil {
			return 0, err
		}
	}
	return len(gone), tx.Commit()
}

func where(f Filter) (string, []any) {
	clauses, args := []string{"removed=0"}, []any{}
	if f.Folder != "" {
		clauses = append(clauses, "folder=?")
		args = append(args, f.Folder)
	}
	if f.SenderLike != "" {
		clauses = append(clauses, "sender like ? escape '\\'")
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(f.SenderLike))
		args = append(args, "%"+esc+"%")
	}
	if f.Unread {
		clauses = append(clauses, "is_read=0")
	}
	if f.Labelled != "" {
		st, label, _ := strings.Cut(f.Labelled, ":")
		st, label = strings.TrimSpace(st), strings.TrimSpace(label)
		if st == "" || label == "" {
			clauses = append(clauses, "0") // a malformed label matches nothing, rather than everything
		} else {
			clauses = append(clauses, `n in (select v.n from verdicts v join (select n, max(id) id from verdicts where stage=? and error='' group by n) l
				on l.id=v.id where exists (select 1 from json_each(v.labels) where value=?))`)
			args = append(args, st, label)
		}
	}
	if !f.Since.IsZero() {
		clauses = append(clauses, "received>=?")
		args = append(args, f.Since.UTC().Format(receivedLayout))
	}
	return strings.Join(clauses, " and "), args
}

// Count returns how many live messages match f.
func (s *Store) Count(ctx context.Context, f Filter) (int, error) {
	done, guardErr := s.legacy(ctx)
	if guardErr != nil {
		return 0, guardErr
	}
	defer done()

	w, args := where(f)
	var n int
	err := s.db.QueryRowContext(ctx, "select count(*) from messages where "+w, args...).Scan(&n)
	return n, err
}

const columns = `n, graph_id, coalesce(internet_message_id,''), folder, coalesce(received,''), coalesce(sender,''),
	coalesce(sender_name,''), coalesce(recipients,'[]'), coalesce(subject,''), coalesce(preview,''),
	has_attachments, is_read, body`

// Inventory reads deliberately exclude cached bodies. Identity verification is
// independent of content, and preparing a binding must not buffer the body cache.
var inventoryColumns = strings.TrimSuffix(columns, ", body") + ", NULL"

type scanner interface{ Scan(...any) error }

func scan(r scanner) (Message, error) {
	var m Message
	var recipients string
	var body sql.NullString
	if err := r.Scan(&m.N, &m.GraphID, &m.InternetMessageID, &m.Folder, &m.Received, &m.Sender, &m.SenderName,
		&recipients, &m.Subject, &m.Preview, &m.HasAttachments, &m.IsRead, &body); err != nil {
		return m, err
	}
	if err := json.Unmarshal([]byte(recipients), &m.Recipients); err != nil {
		return m, err
	}
	if body.Valid {
		m.Body = &body.String
	}
	return m, nil
}

// Messages returns live messages matching f, newest first.
func (s *Store) Messages(ctx context.Context, f Filter) ([]Message, error) {
	done, guardErr := s.legacy(ctx)
	if guardErr != nil {
		return nil, guardErr
	}
	defer done()

	w, args := where(f)
	rows, err := s.db.QueryContext(ctx, "select "+columns+" from messages where "+w+" order by received desc, n asc", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Get returns message n (live or not); ok is false when no such number exists.
func (s *Store) Get(ctx context.Context, n int64) (Message, bool, error) {
	done, guardErr := s.legacy(ctx)
	if guardErr != nil {
		return Message{}, false, guardErr
	}
	defer done()

	m, err := scan(s.db.QueryRowContext(ctx, "select "+columns+" from messages where n=?", n))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, nil
	}
	return m, err == nil, err
}

// SetBody caches a message's text body.
func (s *Store) SetBody(ctx context.Context, n int64, body string) error {
	done, guardErr := s.legacy(ctx)
	if guardErr != nil {
		return guardErr
	}
	defer done()

	_, err := s.db.ExecContext(ctx, "update messages set body=? where n=?", body, n)
	return err
}

// DeltaLink returns the saved delta link for folder ("" when none).
func (s *Store) DeltaLink(ctx context.Context, folder string) (string, error) {
	done, guardErr := s.legacy(ctx)
	if guardErr != nil {
		return "", guardErr
	}
	defer done()

	var link sql.NullString
	err := s.db.QueryRowContext(ctx, "select delta_link from sync_state where folder=?", folder).Scan(&link)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return link.String, err
}

// SetDeltaLink saves the delta link for folder.
func (s *Store) SetDeltaLink(ctx context.Context, folder, link string) error {
	done, guardErr := s.legacy(ctx)
	if guardErr != nil {
		return guardErr
	}
	defer done()

	_, err := s.db.ExecContext(ctx, `insert into sync_state (folder, delta_link, updated) values (?, ?, datetime('now'))
		on conflict(folder) do update set delta_link=excluded.delta_link, updated=excluded.updated`, folder, link)
	return err
}
