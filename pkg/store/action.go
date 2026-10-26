package store

import (
	"context"
	"time"
)

// Action statuses. Only a move is done; a skipped or failed item stays
// pending, so a later apply re-checks it.
const (
	ActionMoved   = "moved"
	ActionSkipped = "skipped"
	ActionFailed  = "failed"
)

// Action is one attempt to carry out a plan item.
type Action struct {
	ID          int64
	PlanID      int64
	N           int64
	GraphID     string
	Sender      string // from the message, when listed
	Subject     string // from the message, when listed
	Action      string
	Destination string
	Status      string
	Detail      string
	At          string
}

// LogActions appends attempts to the action log in one transaction. A moved
// message's local copy moves to the destination folder in the same
// transaction, so listings are current before the next sync (which then finds
// it already there: message ids are immutable).
func (s *Store) LogActions(ctx context.Context, actions []Action) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, a := range actions {
		if _, err := tx.ExecContext(ctx, `insert into actions (plan_id, n, graph_id, action, destination, status, detail, at)
			values (?, ?, ?, ?, ?, ?, ?, ?)`, a.PlanID, a.N, a.GraphID, a.Action, a.Destination, a.Status, a.Detail, now); err != nil {
			return err
		}
		if a.Status == ActionMoved {
			if _, err := tx.ExecContext(ctx, "update messages set folder=? where n=?", a.Destination, a.N); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// Moved returns the messages a plan has already moved.
func (s *Store) Moved(ctx context.Context, planID int64) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, "select distinct n from actions where plan_id=? and status=?", planID, ActionMoved)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// Actions returns a plan's action log in the order it was written. Rows are
// kept even if their message row is gone, so the audit trail is complete.
func (s *Store) Actions(ctx context.Context, planID int64) ([]Action, error) {
	rows, err := s.db.QueryContext(ctx, `select a.id, a.plan_id, a.n, a.graph_id, coalesce(m.sender,''), coalesce(m.subject,''),
		a.action, a.destination, a.status, a.detail, a.at from actions a left join messages m on m.n=a.n where a.plan_id=? order by a.id`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		var a Action
		if err := rows.Scan(&a.ID, &a.PlanID, &a.N, &a.GraphID, &a.Sender, &a.Subject, &a.Action, &a.Destination, &a.Status, &a.Detail, &a.At); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SenderMoves counts the messages a plan has moved from one sender.
type SenderMoves struct {
	Sender string
	Moved  int
}

// ExpiredSenders returns, per sender (trimmed and lower-cased), how many
// distinct messages were moved by no_block deletions: mail that was wanted
// but expired, which must not count against its sender.
func (s *Store) ExpiredSenders(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `select lower(trim(p.sender)) s, count(distinct p.n) from plan_items p
		where p.no_block=1 and exists (select 1 from actions a where a.plan_id=p.plan_id and a.n=p.n and a.status=?)
		group by s`, ActionMoved)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var sender string
		var n int
		if err := rows.Scan(&sender, &n); err != nil {
			return nil, err
		}
		out[sender] = n
	}
	return out, rows.Err()
}

// MovedSenders returns, per sender (trimmed and lower-cased), how many
// distinct messages any plan has moved, ordered by sender. Deletions planned
// as no_block (mail that merely expired) do not count.
func (s *Store) MovedSenders(ctx context.Context) ([]SenderMoves, error) {
	rows, err := s.db.QueryContext(ctx, `select lower(trim(p.sender)) s, count(distinct p.n) from plan_items p
		where p.no_block=0 and exists (select 1 from actions a where a.plan_id=p.plan_id and a.n=p.n and a.status=?)
		group by s order by s`, ActionMoved)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SenderMoves
	for rows.Next() {
		var m SenderMoves
		if err := rows.Scan(&m.Sender, &m.Moved); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
