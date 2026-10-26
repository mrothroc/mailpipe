package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ByMessageID returns the live messages carrying an RFC 5322 Message-ID (a
// message can have copies in several folders).
func (s *Store) ByMessageID(ctx context.Context, internetMessageID string) ([]Message, error) {
	if strings.TrimSpace(internetMessageID) == "" {
		return nil, nil // messages without a Message-ID are not copies of one another
	}
	rows, err := s.db.QueryContext(ctx, "select "+columns+" from messages where internet_message_id=? and removed=0 order by n", internetMessageID)
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

// LatestVerdicts returns, for every live message matching f, the latest
// successful verdict of each stage that judged it.
func (s *Store) LatestVerdicts(ctx context.Context, f Filter) (map[int64][]Verdict, error) {
	w, args := where(f)
	rows, err := s.db.QueryContext(ctx, `select v.id, v.n, v.stage, v.labels, v.scores, v.action, v.reason, v.cost_usd, v.final,
		v.provenance, v.error, v.at from verdicts v
		join (select n, stage, max(id) id from verdicts where error='' and n in (select n from messages where `+w+`)
		      group by n, stage) latest on latest.id=v.id
		order by v.n, v.stage`, args...)
	if err != nil {
		return nil, err
	}
	vs, err := scanVerdicts(rows)
	if err != nil {
		return nil, err
	}
	out := map[int64][]Verdict{}
	for _, v := range vs {
		out[v.N] = append(out[v.N], v)
	}
	return out, nil
}

// Plan describes a stored plan.
type Plan struct {
	ID        int64
	Scope     string
	CreatedAt string
	Counts    map[string]int
}

// PlanItem is one planned action. GraphID, InternetMessageID and Sender are
// copied from the message when the plan is created, so an action can check
// that it still targets the message that was reviewed.
type PlanItem struct {
	N                 int64
	GraphID           string
	InternetMessageID string
	Sender            string
	Subject           string
	PlannedFolder     string // the folder the message was in when planned
	Folder            string // the folder it is in now
	Removed           bool   // it has left the mailbox since it was planned
	Action            string
	Reasons           []string
	NoBlock           bool // a deletion that must not put the sender on the blocklist
}

// CreatePlan stores items as a new plan and returns its id.
func (s *Store) CreatePlan(ctx context.Context, scope string, items []PlanItem) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "insert into plans (scope, created_at) values (?, ?)", scope, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, it := range items {
		reasons, err := json.Marshal(nonNil(it.Reasons))
		if err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, `insert into plan_items (plan_id, n, graph_id, internet_message_id, sender, action, reasons, planned_folder, no_block)
			select ?, n, graph_id, coalesce(internet_message_id,''), coalesce(sender,''), ?, ?, folder, ? from messages where n=? and removed=0`,
			id, it.Action, string(reasons), it.NoBlock, it.N)
		if err != nil {
			return 0, err
		}
		if added, err := res.RowsAffected(); err != nil {
			return 0, err
		} else if added != 1 {
			return 0, fmt.Errorf("plan item for message %d: no such message in the mailbox (nothing was stored)", it.N)
		}
	}
	return id, tx.Commit()
}

// Plan returns plan id and its items, ordered by message number.
func (s *Store) Plan(ctx context.Context, id int64) (Plan, []PlanItem, error) {
	var p Plan
	err := s.db.QueryRowContext(ctx, "select id, scope, created_at from plans where id=?", id).Scan(&p.ID, &p.Scope, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil, fmt.Errorf("no plan %d", id)
	}
	if err != nil {
		return p, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `select i.n, i.graph_id, i.internet_message_id, i.sender, coalesce(m.subject,''),
		i.planned_folder, m.folder, m.removed, i.action, i.reasons, i.no_block from plan_items i join messages m on m.n=i.n where i.plan_id=? order by i.n`, id)
	if err != nil {
		return p, nil, err
	}
	defer rows.Close()
	var items []PlanItem
	p.Counts = map[string]int{}
	for rows.Next() {
		var it PlanItem
		var reasons string
		if err := rows.Scan(&it.N, &it.GraphID, &it.InternetMessageID, &it.Sender, &it.Subject, &it.PlannedFolder, &it.Folder, &it.Removed,
			&it.Action, &reasons, &it.NoBlock); err != nil {
			return p, nil, err
		}
		if err := json.Unmarshal([]byte(reasons), &it.Reasons); err != nil {
			return p, nil, err
		}
		p.Counts[it.Action]++
		items = append(items, it)
	}
	return p, items, rows.Err()
}

// Plans lists stored plans, newest first, with their action counts.
func (s *Store) Plans(ctx context.Context) ([]Plan, error) {
	rows, err := s.db.QueryContext(ctx, `select p.id, p.scope, p.created_at, i.action, count(i.n) from plans p
		left join plan_items i on i.plan_id=p.id group by p.id, i.action order by p.id desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		var p Plan
		var action sql.NullString
		var n int
		if err := rows.Scan(&p.ID, &p.Scope, &p.CreatedAt, &action, &n); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != p.ID {
			p.Counts = map[string]int{}
			out = append(out, p)
		}
		if action.Valid {
			out[len(out)-1].Counts[action.String] = n
		}
	}
	return out, rows.Err()
}

// LatestBySender returns the latest successful verdict from stage for each
// live message, grouped by the message's (lower-case) sender, in one query.
// Messages without a sender are left out: they share no sender, so grouping
// them together would link unrelated mail.
func (s *Store) LatestBySender(ctx context.Context, stage string) (map[string][]Verdict, error) {
	rows, err := s.db.QueryContext(ctx, `select v.id, v.n, v.stage, v.labels, v.scores, v.action, v.reason, v.cost_usd, v.final,
		v.provenance, v.error, v.at, lower(trim(m.sender)) from verdicts v
		join (select n, max(id) id from verdicts where stage=? and error=''
		      and n in (select n from messages where removed=0 and trim(coalesce(sender, '')) <> '') group by n) latest on latest.id=v.id
		join messages m on m.n=v.n order by v.n`, stage)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Verdict{}
	for rows.Next() {
		var v Verdict
		var labels, scores, sender string
		if err := rows.Scan(&v.ID, &v.N, &v.Stage, &labels, &scores, &v.Action, &v.Reason, &v.CostUSD, &v.Final,
			&v.Provenance, &v.Error, &v.At, &sender); err != nil {
			return nil, err
		}
		if err := decodeVerdict(&v, labels, scores); err != nil {
			return nil, err
		}
		out[sender] = append(out[sender], v)
	}
	return out, rows.Err()
}
