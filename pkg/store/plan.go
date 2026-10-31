package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"strings"
	"time"
)

// ByMessageID returns the live messages carrying an RFC 5322 Message-ID (a
// message can have copies in several folders).
func (s *Store) ByMessageID(ctx context.Context, internetMessageID string) ([]Message, error) {
	done, guardErr := s.legacy(ctx)
	if guardErr != nil {
		return nil, guardErr
	}
	defer done()

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
	done, guardErr := s.legacy(ctx)
	if guardErr != nil {
		return nil, guardErr
	}
	defer done()

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
	done, guardErr := s.legacy(ctx)
	if guardErr != nil {
		return 0, guardErr
	}
	defer done()

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
	done, guardErr := s.legacy(ctx)
	if guardErr != nil {
		return Plan{}, nil, guardErr
	}
	defer done()

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
	done, guardErr := s.legacy(ctx)
	if guardErr != nil {
		return nil, guardErr
	}
	defer done()

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
	done, guardErr := s.legacy(ctx)
	if guardErr != nil {
		return nil, guardErr
	}
	defer done()

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

// BoundPlanItem retains both the original audit projection and provider-neutral
// preconditions. Expected is nil for an attributed, non-executable legacy item.
type BoundPlanItem struct {
	Item     PlanItem
	Expected *mailbox.State
	Epochs   map[string]int64
}

// BoundPlan is an account-scoped historical plan, never proof of owner consent.
type BoundPlan struct {
	Plan       Plan
	Account    mailbox.Account
	Generation int64
	Items      []BoundPlanItem
}

// CreateBoundPlan snapshots actual durable state in one transaction. Caller
// identity fields are ignored; only N, action, reasons and NoBlock are inputs.
func (a *AccountStore) CreateBoundPlan(ctx context.Context, scope string, items []PlanItem) (id int64, err error) {
	err = a.transaction(ctx, func(q sqlAccess) error {
		_, generation, _, e := readBinding(ctx, q)
		if e != nil {
			return e
		}
		res, e := q.ExecContext(ctx, "insert into plans(scope,created_at,account_id,provider_id,binding_generation) values(?,?,?,?,?)", scope, time.Now().UTC().Format(time.RFC3339), a.account.ID, a.account.ProviderID, generation)
		if e != nil {
			return e
		}
		id, e = res.LastInsertId()
		if e != nil {
			return e
		}
		for _, item := range items {
			m, e := a.getMessage(ctx, q, mailbox.LocalRef{AccountID: a.account.ID, N: item.N})
			if e != nil {
				return e
			}
			if !m.Valid || len(m.State.Memberships) == 0 {
				return stale("message is not live for planning")
			}
			expected, e := jsonText(canonicalState(m.State))
			if e != nil {
				return e
			}
			epochs, e := a.epochs(ctx, q, m.State)
			if e != nil {
				return e
			}
			epochJSON, e := jsonText(epochs)
			if e != nil {
				return e
			}
			reasons, e := jsonText(nonNil(item.Reasons))
			if e != nil {
				return e
			}
			graph := ""
			if m.GraphID != nil {
				graph = *m.GraphID
			}
			folder := ""
			if len(m.State.Memberships) == 1 {
				folder = string(m.State.Memberships[0].Collection.CollectionID)
			}
			_, e = q.ExecContext(ctx, `insert into plan_items(plan_id,n,graph_id,internet_message_id,sender,action,reasons,planned_folder,no_block,account_id,provider_id,binding_generation,expected_state,expected_epochs) values(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, item.N, graph, m.State.Metadata.InternetMessageID, m.State.Metadata.Sender.Address, item.Action, reasons, folder, item.NoBlock, a.account.ID, a.account.ProviderID, generation, expected, epochJSON)
			if e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		id = 0
	}
	return
}
func (a *AccountStore) planExpected(ctx context.Context, q sqlAccess, id int64, ref mailbox.LocalRef) (expected mailbox.State, epochs map[string]int64, action string, err error) {
	if err = a.local(ref); err != nil {
		return
	}
	var account mailbox.Account
	var generation, storedGeneration int64
	err = q.QueryRowContext(ctx, "select account_id,provider_id,binding_generation from plans where id=?", id).Scan(&account.ID, &account.ProviderID, &storedGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		err = stale("stored plan missing")
		return
	}
	if err != nil {
		return
	}
	_, generation, _, err = readBinding(ctx, q)
	if err != nil {
		return
	}
	if account != a.account {
		err = &mailbox.AccountMismatchError{Operation: "stored plan", Expected: a.account.ID, Actual: account.ID}
		return
	}
	if storedGeneration != generation || storedGeneration == 0 {
		err = stale("legacy or obsolete plan requires fresh review")
		return
	}
	var stateJSON, epochsJSON string
	var itemAccount mailbox.Account
	var itemGeneration int64
	err = q.QueryRowContext(ctx, `select expected_state,expected_epochs,action,account_id,provider_id,binding_generation from plan_items where plan_id=? and n=?`, id, ref.N).Scan(&stateJSON, &epochsJSON, &action, &itemAccount.ID, &itemAccount.ProviderID, &itemGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		err = stale("message is not in stored plan")
		return
	}
	if err != nil {
		return
	}
	if itemAccount != a.account || itemGeneration != generation || stateJSON == "" {
		err = stale("stored plan preconditions missing")
		return
	}
	if err = json.Unmarshal([]byte(stateJSON), &expected); err != nil {
		return
	}
	if err = json.Unmarshal([]byte(epochsJSON), &epochs); err != nil {
		return
	}
	err = expected.Validate(a.account.ID)
	return
}
func (a *AccountStore) reviewed(ctx context.Context, q sqlAccess, id int64, ref mailbox.LocalRef) (out mailbox.ReviewedAction, err error) {
	expected, epochs, _, e := a.planExpected(ctx, q, id, ref)
	if e != nil {
		return out, e
	}
	m, e := a.getMessage(ctx, q, ref)
	if e != nil {
		return out, e
	}
	if !m.Valid {
		return out, stale("planned identity retired")
	}
	currentEpochs, e := a.epochs(ctx, q, m.State)
	if e != nil {
		return out, e
	}
	old, _ := jsonText(epochs)
	current, _ := jsonText(currentEpochs)
	if old != current {
		return out, stale("planned collection epoch changed")
	}
	if e = mailbox.CheckPrecondition(a.account.ID, expected, m.State); e != nil {
		return out, e
	}
	out = mailbox.ReviewedAction{PlanID: id, Local: ref, Expected: expected}
	return out, out.Validate(a.account.ID)
}

// LoadReviewedAction validates durable ownership and preconditions. Its result
// still requires owner authority and live adapter inspection before execution.
func (a *AccountStore) LoadReviewedAction(ctx context.Context, id int64, ref mailbox.LocalRef, current mailbox.Account) (out mailbox.ReviewedAction, err error) {
	if current != a.account {
		return out, &mailbox.AccountMismatchError{Operation: "reviewed storage action", Expected: a.account.ID, Actual: current.ID}
	}
	err = a.transaction(ctx, func(q sqlAccess) error { var e error; out, e = a.reviewed(ctx, q, id, ref); return e })
	return
}

// PlanHistory reads exact historical columns, including attributed legacy plans
// which intentionally have no bound execution preconditions.
func (a *AccountStore) PlanHistory(ctx context.Context, id int64) (out BoundPlan, err error) {
	err = a.transaction(ctx, func(q sqlAccess) error {
		out.Account = a.account
		e := q.QueryRowContext(ctx, `select id,scope,created_at,binding_generation from plans where id=? and account_id=? and provider_id=?`, id, a.account.ID, a.account.ProviderID).Scan(&out.Plan.ID, &out.Plan.Scope, &out.Plan.CreatedAt, &out.Generation)
		if errors.Is(e, sql.ErrNoRows) {
			return stale("stored plan missing")
		}
		if e != nil {
			return e
		}
		rows, e := q.QueryContext(ctx, `select i.n,i.graph_id,i.internet_message_id,i.sender,coalesce(m.subject,''),i.planned_folder,m.folder,m.removed,i.action,i.reasons,i.no_block,i.expected_state,i.expected_epochs from plan_items i join messages m on m.n=i.n where i.plan_id=? and i.account_id=? and i.provider_id=? order by i.n`, id, a.account.ID, a.account.ProviderID)
		if e != nil {
			return e
		}
		defer rows.Close()
		out.Plan.Counts = map[string]int{}
		for rows.Next() {
			var it BoundPlanItem
			var reasons, state, epochs string
			e = rows.Scan(&it.Item.N, &it.Item.GraphID, &it.Item.InternetMessageID, &it.Item.Sender, &it.Item.Subject, &it.Item.PlannedFolder, &it.Item.Folder, &it.Item.Removed, &it.Item.Action, &reasons, &it.Item.NoBlock, &state, &epochs)
			if e != nil {
				return e
			}
			if e = json.Unmarshal([]byte(reasons), &it.Item.Reasons); e != nil {
				return e
			}
			if state != "" {
				it.Expected = &mailbox.State{}
				if e = json.Unmarshal([]byte(state), it.Expected); e != nil {
					return e
				}
				if e = json.Unmarshal([]byte(epochs), &it.Epochs); e != nil {
					return e
				}
			}
			out.Plan.Counts[it.Item.Action]++
			out.Items = append(out.Items, it)
		}
		return rows.Err()
	})
	return
}

// Plans lists this account's history with original IDs and action counts.
func (a *AccountStore) Plans(ctx context.Context) (out []Plan, err error) {
	err = a.transaction(ctx, func(q sqlAccess) error {
		rows, e := q.QueryContext(ctx, `select p.id,p.scope,p.created_at,i.action,count(i.n) from plans p left join plan_items i on i.plan_id=p.id where p.account_id=? and p.provider_id=? group by p.id,i.action order by p.id desc`, a.account.ID, a.account.ProviderID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var p Plan
			var action sql.NullString
			var count int
			if e = rows.Scan(&p.ID, &p.Scope, &p.CreatedAt, &action, &count); e != nil {
				return e
			}
			if len(out) == 0 || out[len(out)-1].ID != p.ID {
				p.Counts = map[string]int{}
				out = append(out, p)
			}
			if action.Valid {
				out[len(out)-1].Counts[action.String] = count
			}
		}
		return rows.Err()
	})
	return
}
