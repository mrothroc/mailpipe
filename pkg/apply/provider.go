package apply

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type reviewedBatch interface {
	TrashBatch(context.Context, []mailbox.ReviewedAction) ([]mailbox.State, []error, error)
}
type inspectionBatch interface {
	InspectBatch(context.Context, []mailbox.MessageRef) ([]mailbox.State, []error, error)
}

// RunBound loads execution authority exclusively from an account-bound stored
// plan. Execute remains explicit owner authority supplied by the CLI caller.
// Optional domain batch methods retain adapter chunking without exposing wire.
func RunBound(ctx context.Context, s *store.AccountStore, p mailbox.Provider, id int64, opts Options, emit func(Outcome) error) (Summary, error) {
	var sum Summary
	if emit == nil || opts.MaxItems < 0 {
		return sum, errors.New("apply: outcome callback and nonnegative item limit required")
	}
	if s == nil {
		return sum, ErrStalePlan
	}
	if p == nil {
		return sum, errors.New("apply: provider required")
	}
	if s.Account() != p.Account() {
		return sum, &mailbox.AccountMismatchError{Operation: "apply", Expected: s.Account().ID, Actual: p.Account().ID}
	}
	if opts.Action != "delete" || opts.Destination != "deleteditems" {
		return sum, &mailbox.UnsupportedError{Operation: "reviewed action"}
	}
	if _, ok := p.(mailbox.TrashCapability); !ok {
		return sum, &mailbox.UnsupportedError{Operation: "recoverable trash"}
	}
	plan, err := s.PlanHistory(ctx, id)
	if err != nil {
		return sum, err
	}
	done, err := s.Moved(ctx, id)
	if err != nil {
		return sum, err
	}
	var entries []store.BoundPlanItem
	for _, entry := range plan.Items {
		if entry.Item.Action != opts.Action {
			continue
		}
		if done[entry.Item.N] {
			sum.AlreadyMoved++
			continue
		}
		if entry.Expected == nil || entry.Item.PlannedFolder == "" {
			return sum, ErrStalePlan
		}
		entries = append(entries, entry)
	}
	if opts.MaxItems > 0 && len(entries) > opts.MaxItems {
		entries = entries[:opts.MaxItems]
	}
	outcomes := make([]Outcome, len(entries))
	requests := make([]mailbox.ReviewedAction, len(entries))
	var inspectIndices []int
	var refs []mailbox.MessageRef
	for i, entry := range entries {
		outcomes[i] = Outcome{Item: entry.Item, Status: store.ActionSkipped}
		r, e := s.LoadReviewedAction(ctx, id, mailbox.LocalRef{AccountID: p.Account().ID, N: entry.Item.N}, p.Account())
		if e != nil {
			if !errors.Is(e, mailbox.ErrStaleIdentity) {
				return sum, e
			}
			outcomes[i].Detail = "stored message changed; review a fresh plan"
			continue
		}
		requests[i] = r
		inspectIndices = append(inspectIndices, i)
		refs = append(refs, r.Expected.Ref)
	}
	states := make([]mailbox.State, len(refs))
	inspectionErrors := make([]error, len(refs))
	if len(refs) > 0 {
		if batch, ok := p.(inspectionBatch); ok {
			states, inspectionErrors, err = batch.InspectBatch(ctx, refs)
		} else {
			for i, ref := range refs {
				states[i], inspectionErrors[i] = p.Inspect(ctx, ref)
			}
		}
		if err != nil {
			return sum, fmt.Errorf("check messages: %w", err)
		}
		if len(states) != len(refs) || len(inspectionErrors) != len(refs) {
			return sum, errors.New("apply: incomplete inspection results")
		}
	}
	var ready []int
	var mutations []mailbox.ReviewedAction
	for j, i := range inspectIndices {
		e := inspectionErrors[j]
		if e != nil {
			if errors.Is(e, mailbox.ErrStaleIdentity) {
				outcomes[i].Detail = "no longer in the mailbox"
			} else {
				outcomes[i].Status = store.ActionFailed
				outcomes[i].Detail = "check: " + e.Error()
			}
			continue
		}
		if e = mailbox.CheckPrecondition(p.Account().ID, requests[i].Expected, states[j]); e != nil {
			var stale *mailbox.StaleIdentityError
			if errors.As(e, &stale) {
				switch stale.Reason {
				case "message header identity changed":
					outcomes[i].Detail = "Message-ID changed"
				case "sender changed":
					outcomes[i].Detail = "sender changed"
				case "collection memberships changed":
					outcomes[i].Detail = "collection memberships changed"
				default:
					outcomes[i].Detail = "message identity or revision changed"
				}
			} else {
				outcomes[i].Status = store.ActionFailed
				outcomes[i].Detail = e.Error()
			}
			continue
		}
		outcomes[i].Status = WouldMove
		ready = append(ready, i)
		mutations = append(mutations, requests[i])
	}
	var returned []mailbox.State
	var mutationErrors []error
	var runErr error
	if opts.Execute && len(mutations) > 0 {
		if batch, ok := p.(reviewedBatch); ok {
			returned, mutationErrors, runErr = batch.TrashBatch(ctx, mutations)
		} else {
			returned = make([]mailbox.State, len(mutations))
			mutationErrors = make([]error, len(mutations))
			for j, r := range mutations {
				returned[j], mutationErrors[j] = mailbox.ApplyReviewed(ctx, p, mailbox.Trash, r)
				if mutationErrors[j] != nil {
					runErr = mutationErrors[j]
					break
				}
			}
		}
		if len(returned) != len(mutations) || len(mutationErrors) != len(mutations) {
			runErr = errors.Join(runErr, errors.New("apply: uncertain incomplete mutation results; reconcile before retry"))
		}
		for j, i := range ready {
			outcomes[i].Status = store.ActionFailed
			if j < len(returned) && j < len(mutationErrors) && mutationErrors[j] == nil && returned[j].Ref.MessageID != "" {
				result := returned[j]
				proofErr := result.Validate(p.Account().ID)
				if proofErr == nil && (result.Ref.MessageID != mutations[j].Expected.Ref.MessageID || result.Metadata.InternetMessageID != mutations[j].Expected.Metadata.InternetMessageID || !strings.EqualFold(strings.TrimSpace(result.Metadata.Sender.Address), strings.TrimSpace(mutations[j].Expected.Metadata.Sender.Address))) {
					proofErr = errors.New("apply: returned identity requires reconciliation")
				}
				if proofErr != nil {
					mutationErrors[j] = proofErr
					runErr = errors.Join(runErr, proofErr)
				} else {
					outcomes[i].Status = store.ActionMoved
					continue
				}
			}
			if j < len(mutationErrors) && mutationErrors[j] != nil {
				outcomes[i].Detail = "move: " + mutationErrors[j].Error()
			} else {
				outcomes[i].Detail = "move: uncertain outcome; reconcile before retry"
			}
		}
	}
	if opts.Execute && len(outcomes) > 0 {
		attempts := make([]store.BoundAction, len(outcomes))
		resultByIndex := map[int]*mailbox.State{}
		for j, i := range ready {
			if outcomes[i].Status == store.ActionMoved {
				resultByIndex[i] = &returned[j]
			}
		}
		for i, o := range outcomes {
			attempts[i] = store.BoundAction{PlanID: id, Local: mailbox.LocalRef{AccountID: p.Account().ID, N: o.Item.N}, Expected: *entries[i].Expected, Result: resultByIndex[i], Action: opts.Action, Destination: opts.Destination, Status: o.Status, Detail: o.Detail}
		}
		if e := s.RecordBoundActions(ctx, attempts); e != nil {
			runErr = errors.Join(runErr, fmt.Errorf("write the action log; reconcile remote outcomes before retry: %w", e))
		}
	}
	for _, o := range outcomes {
		sum.add(o.Status)
		if e := emit(o); e != nil {
			runErr = errors.Join(runErr, e)
		}
	}
	return sum, runErr
}
