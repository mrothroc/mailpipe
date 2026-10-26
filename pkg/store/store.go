// Package store is the local SQLite mirror of mailbox metadata. Each message
// gets a short, stable number N that the CLI and agents use as a handle.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo
)

const schema = `
create table if not exists messages (
    n integer primary key,
    graph_id text unique not null,
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
    body text
);
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

// migrations bring databases created by earlier versions up to the schema
// above; each is idempotent.
func migrate(db *sql.DB) error {
	for _, c := range []struct{ name, ddl string }{
		{"planned_folder", "planned_folder text not null default ''"},
		{"no_block", "no_block integer not null default 0"},
	} {
		var has int
		if err := db.QueryRow("select count(*) from pragma_table_info('plan_items') where name=?", c.name).Scan(&has); err != nil {
			return err
		}
		if has == 0 {
			if _, err := db.Exec("alter table plan_items add column " + c.ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

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
type Store struct{ db *sql.DB }

// Open opens (creating when needed) the database at path. The directory is
// created 0700 and the file 0600: the store holds private mail metadata.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600); err == nil {
		_ = f.Close()
	} else {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Upsert inserts or refreshes a message in folder. It reports Added when the
// message is new to that folder (new, moved in, or previously removed) and
// Updated when it was already there.
func (s *Store) Upsert(ctx context.Context, folder string, r Record) (Change, error) {
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
	w, args := where(f)
	var n int
	err := s.db.QueryRowContext(ctx, "select count(*) from messages where "+w, args...).Scan(&n)
	return n, err
}

const columns = `n, graph_id, coalesce(internet_message_id,''), folder, coalesce(received,''), coalesce(sender,''),
	coalesce(sender_name,''), coalesce(recipients,'[]'), coalesce(subject,''), coalesce(preview,''),
	has_attachments, is_read, body`

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
	m, err := scan(s.db.QueryRowContext(ctx, "select "+columns+" from messages where n=?", n))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, nil
	}
	return m, err == nil, err
}

// SetBody caches a message's text body.
func (s *Store) SetBody(ctx context.Context, n int64, body string) error {
	_, err := s.db.ExecContext(ctx, "update messages set body=? where n=?", body, n)
	return err
}

// DeltaLink returns the saved delta link for folder ("" when none).
func (s *Store) DeltaLink(ctx context.Context, folder string) (string, error) {
	var link sql.NullString
	err := s.db.QueryRowContext(ctx, "select delta_link from sync_state where folder=?", folder).Scan(&link)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return link.String, err
}

// SetDeltaLink saves the delta link for folder.
func (s *Store) SetDeltaLink(ctx context.Context, folder, link string) error {
	_, err := s.db.ExecContext(ctx, `insert into sync_state (folder, delta_link, updated) values (?, ?, datetime('now'))
		on conflict(folder) do update set delta_link=excluded.delta_link, updated=excluded.updated`, folder, link)
	return err
}
