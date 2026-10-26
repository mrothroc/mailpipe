// Package rules turns what applied plans deleted into one managed blocklist
// of server-side Inbox rules, so future mail from those senders goes straight
// to Deleted Items. A sender is blocked only when none of their mail is kept
// anywhere in the mailbox, and rules mailpipe did not write are never touched.
package rules

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mrothroc/mailpipe/pkg/store"
)

// PerRule is how many senders one rule holds. It keeps each rule readable in
// Outlook's rule editor; the mailbox's rule quota is far larger.
const PerRule = 100

const (
	namePrefix = "mailpipe blocked #"
	// junkPrefix names the rules that move junk senders' mail to Junk.
	junkPrefix = "mailpipe junk #"
	// retiredPrefix named the per-plan rules mailpipe wrote before the
	// blocklist ("mailpipe plan <id> #<k>"); a diff deletes them.
	retiredPrefix = "mailpipe plan "
)

// Rule is an Inbox rule that sends mail from its senders to Deleted Items, or
// for a junk rule matches senders (addresses or @domains) and moves their
// mail to Junk.
type Rule struct {
	ID       string
	Name     string
	Sequence int
	Enabled  bool
	Senders  []string // lower-cased and sorted
	Junk     bool
	// Exact reports that the rule has exactly the shape mailpipe writes: a
	// fromAddresses condition and the delete and stopProcessingRules actions,
	// or for a junk rule a senderContains condition and the moveToFolder
	// (Junk) and stopProcessingRules actions; nothing else.
	Exact bool
}

// RuleName names the blocklist's k-th rule (from 1).
func RuleName(k int) string { return fmt.Sprintf("%s%d", namePrefix, k) }

func isJunk(name string) bool {
	k, ok := strings.CutPrefix(name, junkPrefix)
	return ok && positive(k)
}

// Kind says who a rule belongs to.
type Kind string

const (
	Managed Kind = "managed" // part of the blocklist
	Retired Kind = "retired" // a per-plan rule from before the blocklist
	Owner   Kind = ""        // the owner's own; never touched
)

// KindOf classifies a rule by its name.
func KindOf(name string) Kind {
	if k, ok := strings.CutPrefix(name, namePrefix); (ok && positive(k)) || isJunk(name) {
		return Managed
	}
	if rest, ok := strings.CutPrefix(name, retiredPrefix); ok {
		if plan, k, ok := strings.Cut(rest, " #"); ok && positive(plan) && positive(k) {
			return Retired
		}
	}
	return Owner
}

func positive(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n > 0
}

// Candidate is a sender a plan moved mail from, and whether they are blocked.
type Candidate struct {
	Sender  string
	Moved   int
	Kept    int // messages outside Deleted Items, anywhere in the mailbox
	Expired int // messages deleted only because they expired (no_block)
	Blocked bool
	Reason  string // why a sender is not blocked
}

// Assess decides which senders are blocked. A sender with mail kept, or mail
// that was wanted until it expired, is not blocked. protect returns why a
// sender must never be blocked (the owner, a contact, a keep sender), or "".
// forced reports a sender the owner chose to block, who is blocked whatever
// is kept or expired, unless protected.
func Assess(moved []store.SenderMoves, kept, expired map[string]int, protect func(sender string) string, forced func(sender string) bool) []Candidate {
	out := make([]Candidate, len(moved))
	for i, m := range moved {
		c := Candidate{Sender: m.Sender, Moved: m.Moved, Kept: kept[m.Sender], Expired: expired[m.Sender]}
		switch {
		case strings.TrimSpace(m.Sender) == "":
			c.Reason = "no sender address"
		case protect(m.Sender) != "":
			c.Reason = protect(m.Sender)
		case forced(m.Sender):
			c.Blocked = true
		case c.Kept > 0:
			c.Reason = fmt.Sprintf("%d kept outside Deleted Items", c.Kept)
		case c.Expired > 0:
			c.Reason = fmt.Sprintf("%d expired under a shelf life", c.Expired)
		default:
			c.Blocked = true
		}
		out[i] = c
	}
	return out
}

// Blocked returns the blocked candidates' senders.
func Blocked(cs []Candidate) []string {
	var out []string
	for _, c := range cs {
		if c.Blocked {
			out = append(out, c.Sender)
		}
	}
	return out
}

func normalize(senders []string) []string {
	out := make([]string, 0, len(senders))
	for _, s := range senders {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Desired returns the blocklist's rules for senders: sorted, without
// duplicates, PerRule to a rule.
func Desired(senders []string) []Rule { return split(namePrefix, senders, false) }

// DesiredJunk returns the junk rules for junk senders (addresses or
// @domains), split the same way.
func DesiredJunk(senders []string) []Rule { return split(junkPrefix, senders, true) }

func split(prefix string, senders []string, junk bool) []Rule {
	all := normalize(senders)
	var out []Rule
	for start := 0; start < len(all); start += PerRule {
		out = append(out, Rule{Name: fmt.Sprintf("%s%d", prefix, len(out)+1), Senders: all[start:min(start+PerRule, len(all))],
			Enabled: true, Junk: junk, Exact: true})
	}
	return out
}

// Op is what a diff does to one rule.
type Op string

const (
	Create Op = "create"
	Update Op = "update"
	// Replace writes the rule anew and removes the old one: the old rule was
	// edited outside mailpipe and may carry conditions or actions of its own.
	Replace Op = "replace"
	Delete  Op = "delete"
	Same    Op = "same"
)

// Change is one rule's difference between the server and the plan. Rule is
// the rule to write (for update and replace, with the server rule's ID and
// sequence) or the server's rule (for delete and same).
type Change struct {
	Op      Op
	Rule    Rule
	Added   []string
	Removed []string
}

// Diff compares the blocklist's desired rules with the server's Inbox rules.
// Managed rules are compared; retired per-plan rules are deleted, after every
// other change so their senders stay blocked until the blocklist covers them;
// the owner's rules are left alone. New rules are sequenced after every
// existing one.
func Diff(desired, server []Rule) []Change {
	next := 1
	byName := map[string]Rule{}
	var extra, retired []Rule
	for _, r := range server {
		next = max(next, r.Sequence+1)
		switch KindOf(r.Name) {
		case Retired:
			retired = append(retired, r)
			continue
		case Owner:
			continue
		}
		// Of two rules with one name, the one in mailpipe's shape is kept.
		if first, dup := byName[r.Name]; dup {
			if r.Exact && !first.Exact {
				first, r = r, first
				byName[r.Name] = first
			}
			extra = append(extra, r)
			continue
		}
		byName[r.Name] = r
	}
	var out []Change
	for _, want := range desired {
		have, ok := byName[want.Name]
		if !ok {
			want.Sequence = next
			next++
			out = append(out, Change{Op: Create, Rule: want, Added: want.Senders})
			continue
		}
		delete(byName, want.Name)
		added, removed := difference(want.Senders, have.Senders), difference(have.Senders, want.Senders)
		op := Same
		switch {
		case !have.Exact:
			op = Replace
		case len(added) > 0 || len(removed) > 0 || !have.Enabled:
			op = Update
		}
		if op == Same {
			out = append(out, Change{Op: Same, Rule: have})
			continue
		}
		want.ID, want.Sequence = have.ID, have.Sequence
		out = append(out, Change{Op: op, Rule: want, Added: added, Removed: removed})
	}
	var stale []Rule
	for _, r := range server {
		if have, ok := byName[r.Name]; ok && have.ID == r.ID {
			stale = append(stale, r)
		}
	}
	for _, r := range slices.Concat(stale, extra, retired) {
		out = append(out, Change{Op: Delete, Rule: r, Removed: r.Senders})
	}
	return out
}

// difference returns the members of a that are not in b.
func difference(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}

// Pending reports whether any change is more than "same".
func Pending(cs []Change) bool {
	return slices.ContainsFunc(cs, func(c Change) bool { return c.Op != Same })
}

// Writer is what Execute needs from the server.
type Writer interface {
	Create(ctx context.Context, rule Rule) error
	Update(ctx context.Context, rule Rule) error
	Delete(ctx context.Context, id string) error
}

// Execute carries out a diff in order and returns how many changes it made.
// It stops at the first failure. A replacement is created before the rule it
// replaces is deleted, so its senders are never left unblocked.
func Execute(ctx context.Context, w Writer, changes []Change) (int, error) {
	done := 0
	for _, c := range changes {
		var err error
		switch c.Op {
		case Same:
			continue
		case Create:
			err = w.Create(ctx, c.Rule)
		case Update:
			err = w.Update(ctx, c.Rule)
		case Replace:
			if err = w.Create(ctx, c.Rule); err == nil {
				err = w.Delete(ctx, c.Rule.ID)
			}
		case Delete:
			err = w.Delete(ctx, c.Rule.ID)
		default:
			err = fmt.Errorf("unknown op %q", c.Op)
		}
		if err != nil {
			return done, fmt.Errorf("%s %s: %w", c.Op, c.Rule.Name, err)
		}
		done++
	}
	return done, nil
}
