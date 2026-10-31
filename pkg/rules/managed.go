package rules

import (
	"context"
	"errors"
	"fmt"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// ManagedService preserves the fuller reviewed managed-rule contract, including
// junk predicates, ordering and replacement. It is not NativeRuleCapability.
type ManagedService interface {
	List(context.Context) ([]Rule, error)
	Kept(context.Context, []string) (map[string]int, error)
	ExecuteReviewed(context.Context, []Change) (int, error)
}

type managedProvider interface{ ManagedRules() ManagedService }

func ManagedFor(p mailbox.Provider) (ManagedService, error) {
	if p, ok := p.(managedProvider); ok {
		return p.ManagedRules(), nil
	}
	return nil, &mailbox.UnsupportedError{Operation: "managed junk and block rules"}
}

// ExecuteReviewed rechecks complete original evidence before every existing-rule
// mutation. Replacements are still created before the old rule is removed.
func (r *GraphRules) ExecuteReviewed(ctx context.Context, changes []Change) (int, error) {
	done := 0
	check := func(change Change) error {
		expected := r.original[change.Rule.ID]
		if expected == "" {
			return errors.New("managed rule: original reviewed evidence required")
		}
		current, evidence, err := r.readRules(ctx)
		if err != nil {
			return err
		}
		for _, have := range current {
			if have.ID == change.Rule.ID {
				if KindOf(have.Name) == Owner || have.Name != change.Rule.Name || evidence[have.ID] != expected {
					return &mailbox.StaleIdentityError{Operation: "managed rule", Reason: "rule ownership, shape or order changed"}
				}
				return nil
			}
		}
		return &mailbox.StaleIdentityError{Operation: "managed rule", Reason: "reviewed rule is missing"}
	}
	for _, change := range changes {
		var err error
		switch change.Op {
		case Same:
			continue
		case Create:
			current, _, e := r.readRules(ctx)
			if e != nil {
				return done, e
			}
			for _, have := range current {
				if have.Sequence >= change.Rule.Sequence {
					return done, &mailbox.StaleIdentityError{Operation: "managed rule", Reason: "rule ordering changed"}
				}
			}
			err = r.Create(ctx, change.Rule)
		case Update:
			if err = check(change); err == nil {
				err = r.Update(ctx, change.Rule)
			}
		case Delete:
			if err = check(change); err == nil {
				err = r.Delete(ctx, change.Rule.ID)
			}
		case Replace:
			if err = check(change); err == nil {
				err = r.Create(ctx, change.Rule)
			}
			if err == nil {
				err = check(change)
			}
			if err == nil {
				err = r.Delete(ctx, change.Rule.ID)
			}
		default:
			return done, errors.New("managed rule: invalid reviewed operation")
		}
		if err != nil {
			return done, fmt.Errorf("%s %s: %w", change.Op, change.Rule.Name, err)
		}
		done++
	}
	return done, nil
}
