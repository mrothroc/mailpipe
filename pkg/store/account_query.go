package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// HasVerdict checks provenance/time for exactly this account-bound local handle.
func (a *AccountStore) HasVerdict(ctx context.Context, ref mailbox.LocalRef, stage, provenance, at string) (found bool, err error) {
	err = a.transaction(ctx, func(q sqlAccess) error {
		if _, e := a.getMessage(ctx, q, ref); e != nil {
			return e
		}
		var count int
		e := q.QueryRowContext(ctx, "select count(*) from verdicts where n=? and stage=? and provenance=? and at=?", ref.N, stage, provenance, at).Scan(&count)
		found = count > 0
		return e
	})
	return
}

// Pending follows the latest successful-stage semantics of legacy storage.
func (a *AccountStore) Pending(ctx context.Context, f AccountFilter, stage string) ([]StoredMessage, error) {
	return a.messages(ctx, f, "not exists(select 1 from verdicts v where v.n=messages.n and v.stage=? and v.error='')", []any{stage})
}

// LatestVerdicts returns one latest successful verdict per stage/local identity.
func (a *AccountStore) LatestVerdicts(ctx context.Context, f AccountFilter) (out map[mailbox.LocalRef][]Verdict, err error) {
	out = map[mailbox.LocalRef][]Verdict{}
	err = a.transaction(ctx, func(q sqlAccess) error {
		ns, e := a.messageNumbers(ctx, q, f, "", nil)
		if e != nil {
			return e
		}
		for _, n := range ns {
			rows, e := q.QueryContext(ctx, `select v.id,v.n,v.stage,v.labels,v.scores,v.action,v.reason,v.cost_usd,v.final,v.provenance,v.error,v.at from verdicts v join(select stage,max(id) id from verdicts where n=? and error='' group by stage) latest on latest.id=v.id order by v.stage`, n)
			if e != nil {
				return e
			}
			vs, e := scanVerdicts(rows)
			if e != nil {
				return e
			}
			if len(vs) > 0 {
				out[mailbox.LocalRef{AccountID: a.account.ID, N: n}] = vs
			}
		}
		return nil
	})
	return
}

// LatestBySender groups only this account's live latest successful stage rows.
func (a *AccountStore) LatestBySender(ctx context.Context, stage string) (out map[string][]Verdict, err error) {
	out = map[string][]Verdict{}
	err = a.transaction(ctx, func(q sqlAccess) error {
		rows, e := q.QueryContext(ctx, `select v.id,v.n,v.stage,v.labels,v.scores,v.action,v.reason,v.cost_usd,v.final,v.provenance,v.error,v.at,lower(trim(m.sender)) from verdicts v join(select n,max(id) id from verdicts where stage=? and error='' group by n) latest on latest.id=v.id join messages m on m.n=v.n where m.account_id=? and m.provider_id=? and m.identity_valid=1 and m.removed=0 and trim(coalesce(m.sender,''))<>'' and exists(select 1 from memberships ms where ms.account_id=m.account_id and ms.provider_id=m.provider_id and ms.n=m.n) order by v.n`, stage, a.account.ID, a.account.ProviderID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var v Verdict
			var labels, scores, sender string
			if e = rows.Scan(&v.ID, &v.N, &v.Stage, &labels, &scores, &v.Action, &v.Reason, &v.CostUSD, &v.Final, &v.Provenance, &v.Error, &v.At, &sender); e != nil {
				return e
			}
			if e = decodeVerdict(&v, labels, scores); e != nil {
				return e
			}
			out[sender] = append(out[sender], v)
		}
		return rows.Err()
	})
	return
}
func (a *AccountStore) senderMoves(ctx context.Context, noBlock bool) (out map[string]int, err error) {
	out = map[string]int{}
	err = a.transaction(ctx, func(q sqlAccess) error {
		rows, e := q.QueryContext(ctx, `select lower(trim(p.sender)),count(distinct p.n) from plan_items p where p.account_id=? and p.provider_id=? and p.no_block=? and exists(select 1 from actions a where a.plan_id=p.plan_id and a.n=p.n and a.account_id=p.account_id and a.provider_id=p.provider_id and a.status='moved') group by lower(trim(p.sender)) order by lower(trim(p.sender))`, a.account.ID, a.account.ProviderID, noBlock)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var sender string
			var n int
			if e = rows.Scan(&sender, &n); e != nil {
				return e
			}
			out[sender] = n
		}
		return rows.Err()
	})
	return
}

// ExpiredSenders preserves the historical no_block distinction per account.
func (a *AccountStore) ExpiredSenders(ctx context.Context) (map[string]int, error) {
	return a.senderMoves(ctx, true)
}

// MovedSenders excludes no_block attempts from blocking evidence.
func (a *AccountStore) MovedSenders(ctx context.Context) (out []SenderMoves, err error) {
	counts, e := a.senderMoves(ctx, false)
	if e != nil {
		return nil, e
	}
	for _, sender := range sortedKeys(counts) {
		out = append(out, SenderMoves{Sender: sender, Moved: counts[sender]})
	}
	return out, nil
}

// Moved is a historical read. It does not make an unbound/legacy plan executable.
func (a *AccountStore) Moved(ctx context.Context, id int64) (out map[int64]bool, err error) {
	out = map[int64]bool{}
	err = a.transaction(ctx, func(q sqlAccess) error {
		rows, e := q.QueryContext(ctx, "select distinct n from actions where plan_id=? and account_id=? and provider_id=? and status='moved'", id, a.account.ID, a.account.ProviderID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var n int64
			if e = rows.Scan(&n); e != nil {
				return e
			}
			out[n] = true
		}
		return rows.Err()
	})
	return
}

// writeReturnedMetadata updates compatibility projections as well as the full
// provider metadata, so filters never use stale read flags after a logged move.
func (a *AccountStore) writeReturnedMetadata(ctx context.Context, q sqlAccess, ref mailbox.LocalRef, s mailbox.State) error {
	if e := a.local(ref); e != nil {
		return e
	}
	if e := s.Validate(a.account.ID); e != nil {
		return e
	}
	raw, e := jsonText(s.Metadata)
	if e != nil {
		return e
	}
	recipients := make([]string, len(s.Metadata.Recipients))
	for i, r := range s.Metadata.Recipients {
		recipients[i] = r.Address
	}
	rs, e := json.Marshal(lower(recipients))
	if e != nil {
		return e
	}
	received := ""
	if !s.Metadata.Received.IsZero() {
		received = s.Metadata.Received.UTC().Format(receivedLayout)
	}
	result, e := q.ExecContext(ctx, `update messages set locator=?,revision=?,metadata=?,internet_message_id=?,received=?,sender=?,sender_name=?,recipients=?,subject=?,preview=?,has_attachments=?,is_read=? where n=? and account_id=? and provider_id=?`, s.Ref.Locator, s.Revision, raw, s.Metadata.InternetMessageID, received, strings.ToLower(s.Metadata.Sender.Address), s.Metadata.Sender.Name, string(rs), s.Metadata.Subject, s.Metadata.Preview, s.Metadata.HasAttachments, s.Metadata.IsRead, ref.N, a.account.ID, a.account.ProviderID)
	if e != nil {
		return e
	}
	count, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if count != 1 {
		return stale("returned message is not attributed to this account/provider")
	}
	return nil
}
