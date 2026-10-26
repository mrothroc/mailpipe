package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Verdict is one stage's recorded judgement of a message, or the error that
// prevented one (Error non-empty).
type Verdict struct {
	ID         int64
	N          int64
	Stage      string
	Labels     []string
	Scores     map[string]float64
	Action     string
	Reason     string
	CostUSD    float64
	Final      bool
	Provenance string
	Error      string
	At         string
}

// execer is satisfied by *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// AddVerdict records v; At defaults to now (UTC, RFC 3339).
func (s *Store) AddVerdict(ctx context.Context, v Verdict) error { return addVerdict(ctx, s.db, v) }

// AddVerdicts records vs in one transaction: all of them, or none.
func (s *Store) AddVerdicts(ctx context.Context, vs []Verdict) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, v := range vs {
		if err := addVerdict(ctx, tx, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func addVerdict(ctx context.Context, db execer, v Verdict) error {
	if v.At == "" {
		v.At = time.Now().UTC().Format(time.RFC3339)
	}
	labels, err := json.Marshal(nonNil(v.Labels))
	if err != nil {
		return err
	}
	scores, err := json.Marshal(nonNilMap(v.Scores))
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `insert into verdicts (n, stage, labels, scores, action, reason, cost_usd, final,
		provenance, error, at) values (?,?,?,?,?,?,?,?,?,?,?)`,
		v.N, v.Stage, string(labels), string(scores), v.Action, v.Reason, v.CostUSD, v.Final, v.Provenance, v.Error, v.At)
	return err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilMap(m map[string]float64) map[string]float64 {
	if m == nil {
		return map[string]float64{}
	}
	return m
}

// HasVerdict reports whether message n already has a verdict from stage with
// this provenance and time (so an import can be re-run safely).
func (s *Store) HasVerdict(ctx context.Context, n int64, stage, provenance, at string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, "select count(*) from verdicts where n=? and stage=? and provenance=? and at=?", n, stage, provenance, at).Scan(&found)
	return found > 0, err
}

// Verdicts returns message n's verdict history, oldest first.
func (s *Store) Verdicts(ctx context.Context, n int64) ([]Verdict, error) {
	rows, err := s.db.QueryContext(ctx, `select id, n, stage, labels, scores, action, reason, cost_usd, final,
		provenance, error, at from verdicts where n=? order by id`, n)
	if err != nil {
		return nil, err
	}
	return scanVerdicts(rows)
}

// Pending returns live messages matching f that have no successful verdict
// from stage yet (a recorded failure does not count, so it is retried),
// newest first.
func (s *Store) Pending(ctx context.Context, f Filter, stage string) ([]Message, error) {
	w, args := where(f)
	args = append(args, stage)
	rows, err := s.db.QueryContext(ctx, "select "+columns+" from messages where "+w+
		" and n not in (select n from verdicts where stage=? and error='') order by received desc, n asc", args...)
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

// SentRecipients returns every distinct, lower-cased recipient of synced Sent
// Items, sorted.
func (s *Store) SentRecipients(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `select distinct lower(value) from messages, json_each(messages.recipients)
		where folder='sentitems' and removed=0 order by 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// scanVerdicts reads every row of a verdicts query (the full column list, in
// table order) and closes rows.
func scanVerdicts(rows *sql.Rows) ([]Verdict, error) {
	defer rows.Close()
	var out []Verdict
	for rows.Next() {
		var v Verdict
		var labels, scores string
		if err := rows.Scan(&v.ID, &v.N, &v.Stage, &labels, &scores, &v.Action, &v.Reason, &v.CostUSD, &v.Final,
			&v.Provenance, &v.Error, &v.At); err != nil {
			return nil, err
		}
		if err := decodeVerdict(&v, labels, scores); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func decodeVerdict(v *Verdict, labels, scores string) error {
	if err := json.Unmarshal([]byte(labels), &v.Labels); err != nil {
		return err
	}
	return json.Unmarshal([]byte(scores), &v.Scores)
}
