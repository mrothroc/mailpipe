package store

import (
	"context"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// BoundVerdict associates one verdict with its account-local identity.
// Repeated identities and verdicts retain their input order.
type BoundVerdict struct {
	Local   mailbox.LocalRef
	Verdict Verdict
}

// AddVerdictsBatch records all verdicts in one account-validated transaction.
// It preserves the validation of AddVerdicts without restricting a batch to one
// message. An empty batch still validates the account and open session.
func (a *AccountStore) AddVerdictsBatch(ctx context.Context, batch []BoundVerdict) error {
	return a.transaction(ctx, func(q sqlAccess) error {
		for _, entry := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			m, err := a.getMessage(ctx, q, entry.Local)
			if err != nil {
				return err
			}
			if !m.Valid {
				return stale("message identity retired")
			}
			if entry.Verdict.N != 0 && entry.Verdict.N != entry.Local.N {
				return stale("verdict local identity differs")
			}
		}
		for _, entry := range batch {
			v := entry.Verdict
			v.N = entry.Local.N
			if err := addVerdict(ctx, q, v); err != nil {
				return err
			}
		}
		return ctx.Err()
	})
}
