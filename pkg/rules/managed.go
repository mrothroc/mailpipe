package rules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"

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

// ExecuteReviewed verifies the complete inventory around each mutation. Only the
// exact insertion/removal shifts caused by that mutation advance local evidence;
// an arbitrary fresh server snapshot never replaces reviewed evidence.
func (r *GraphRules) ExecuteReviewed(ctx context.Context, changes []Change) (int, error) {
	expected := maps.Clone(r.original)
	stale := func() error {
		return &mailbox.StaleIdentityError{Operation: "managed rule", Reason: "rule ownership, shape or order changed"}
	}
	readExpected := func() ([]Rule, error) {
		rows, evidence, err := r.readRules(ctx)
		if err != nil {
			return nil, err
		}
		if expected == nil || len(rows) != len(expected) || !maps.Equal(expected, evidence) {
			return nil, stale()
		}
		return rows, nil
	}
	// Verify a single known effect, including every untouched rule and its order.
	verify := func(next map[string]string, write Rule, created bool, updated bool) error {
		rows, evidence, err := r.readRules(ctx)
		if err != nil {
			return err
		}
		if len(rows) != len(evidence) {
			return stale()
		}
		if created {
			var added *Rule
			for i := range rows {
				if _, old := next[rows[i].ID]; !old {
					if added != nil {
						return stale()
					}
					added = &rows[i]
				}
			}
			if added == nil || added.ID == "" || !sameWrittenRule(*added, write) {
				return stale()
			}
			next[added.ID] = evidence[added.ID]
		}
		if updated {
			var have *Rule
			for i := range rows {
				if rows[i].ID == write.ID {
					have = &rows[i]
				}
			}
			if have == nil || !sameWrittenRule(*have, write) || !sameUnpatchedRule(next[write.ID], evidence[write.ID]) {
				return stale()
			}
			next[write.ID] = evidence[write.ID]
		}
		if !maps.Equal(next, evidence) {
			return stale()
		}
		expected = next
		return nil
	}
	done := 0
	for _, change := range changes {
		if change.Op == Same {
			continue
		}
		rows, err := readExpected()
		if err != nil {
			return done, err
		}
		write := change.Rule
		var target *Rule
		for i := range rows {
			if rows[i].ID == write.ID {
				target = &rows[i]
			}
		}
		if change.Op != Create && (target == nil || KindOf(target.Name) == Owner || target.Name != write.Name) {
			return done, stale()
		}
		if KindOf(write.Name) == Owner {
			return done, stale()
		}
		if change.Op == Create {
			write.Sequence = 1
			for _, have := range rows {
				write.Sequence = max(write.Sequence, have.Sequence+1)
			}
		} else {
			write.Sequence = target.Sequence
		}
		next := maps.Clone(expected)
		switch change.Op {
		case Create, Replace:
			for _, have := range rows {
				if have.Sequence >= write.Sequence {
					next[have.ID] = ruleSequence(next[have.ID], have.Sequence+1)
				}
			}
			err = r.Create(ctx, write)
			if err == nil {
				err = verify(next, write, true, false)
			}
			if err == nil && change.Op == Replace {
				// Readback proved the inserted rule and precisely predicted shift. The old
				// identity now has sequence+1; prove it again before removing it.
				var current []Rule
				current, err = readExpected()
				if err == nil {
					next = maps.Clone(expected)
					delete(next, write.ID)
					for _, have := range current {
						if have.Sequence > write.Sequence+1 {
							next[have.ID] = ruleSequence(next[have.ID], have.Sequence-1)
						}
					}
					err = r.Delete(ctx, write.ID)
					if err == nil {
						err = verify(next, Rule{}, false, false)
					}
				}
			}
		case Update:
			err = r.Update(ctx, write)
			if err == nil {
				err = verify(next, write, false, true)
			}
		case Delete:
			delete(next, write.ID)
			for _, have := range rows {
				if have.Sequence > write.Sequence {
					next[have.ID] = ruleSequence(next[have.ID], have.Sequence-1)
				}
			}
			err = r.Delete(ctx, write.ID)
			if err == nil {
				err = verify(next, Rule{}, false, false)
			}
		default:
			err = errors.New("managed rule: invalid reviewed operation")
		}
		if err != nil {
			return done, fmt.Errorf("%s %s: %w", change.Op, change.Rule.Name, err)
		}
		done++
	}
	return done, nil
}

func sameWrittenRule(have, want Rule) bool {
	return have.Name == want.Name && have.Sequence == want.Sequence && have.Enabled && have.Exact && have.Junk == want.Junk && slices.Equal(have.Senders, normalize(want.Senders))
}
func ruleSequence(raw string, sequence int) string {
	var value map[string]any
	_ = json.Unmarshal([]byte(raw), &value)
	value["sequence"] = sequence
	b, _ := json.Marshal(value)
	return string(b)
}
func sameUnpatchedRule(before, after string) bool {
	var a, b map[string]any
	if json.Unmarshal([]byte(before), &a) != nil || json.Unmarshal([]byte(after), &b) != nil {
		return false
	}
	delete(a, "conditions")
	delete(a, "isEnabled")
	delete(b, "conditions")
	delete(b, "isEnabled")
	return reflect.DeepEqual(a, b)
}
