// Package policy turns stage verdicts into actions. Stages only judge; the
// policy decides, so a stage (or an agent behind one) can never act on mail by
// itself.
package policy

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mrothroc/mailpipe/pkg/stage"
	"github.com/mrothroc/mailpipe/pkg/store"
)

// Rule maps a stage's label, or a message's sender, to an action.
type Rule struct {
	Stage string `json:"stage,omitempty"`
	Label string `json:"label,omitempty"`
	// Senders (addresses, or @domain for every address there) match by
	// sender instead of by a stage label: say, an expiry for one sender.
	Senders []string `json:"senders,omitempty"`
	Action  string   `json:"action"`
	// OlderThan ("7d", "36h") applies the rule only to messages received at
	// least that long ago; a message of unknown age never qualifies.
	OlderThan string `json:"older_than,omitempty"`
	// Unless ("stage:label" entries) skips the rule for a message carrying
	// any of those labels.
	Unless []string `json:"unless,omitempty"`
	// NoBlock marks a deletion that must not put its sender on the
	// blocklist: mail that expires, from senders whose new mail is wanted.
	NoBlock bool `json:"no_block,omitempty"`
	// Hold (a senders rule with older_than) sets the sender's lifetime: its
	// mail is kept until it is that old and then the rule applies, whatever
	// other rules say. A stage that keeps (the allowlist) still wins.
	Hold bool `json:"hold,omitempty"`
}

// Message is what the policy needs to know about a message besides its
// verdicts.
type Message struct {
	Sender   string
	Received time.Time // zero when unknown
}

// Decision is the policy's action for a message, every reason for it, and
// whether a deletion should also block the sender.
type Decision struct {
	Action  string
	Reasons []string
	Block   bool
}

// ParseAge reads a positive age: whole days ("7d") or a Go duration ("36h").
func ParseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil {
			d = time.Duration(n) * 24 * time.Hour
		}
	} else if v, err := time.ParseDuration(s); err == nil {
		d = v
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q is not a positive age (want days like 7d or a duration like 36h)", s)
	}
	return d, nil
}

// Policy is the owner's decision rules.
type Policy struct {
	// KeepSenders are never acted on, whatever any stage says.
	KeepSenders []string `json:"keep_senders"`
	// JunkSenders (addresses, or "@domain" for every address there) have their
	// new mail moved to Junk by a server rule, and are never blocked.
	JunkSenders []string `json:"junk_senders,omitempty"`
	// BlockSenders' mail is all deleted, whatever its age, and they are
	// blocked; a keep (a contact, a keep sender) still wins.
	BlockSenders []string `json:"block_senders,omitempty"`
	// NeverBlock senders' messages may still be deleted one by one, but a
	// sender rule never blocks them: a platform such as a newsletter host
	// sends some mail worth keeping.
	NeverBlock []string `json:"never_block"`
	Rules      []Rule   `json:"rules"`
}

// rank orders actions from most to least protective: a keep always wins, and
// a reversible quarantine beats a delete.
var rank = map[string]int{stage.Keep: 0, stage.Quarantine: 1, stage.Delete: 2, stage.Digest: 3}

// Validate checks every rule.
func (p Policy) Validate() error {
	for _, l := range []struct {
		name    string
		senders []string
	}{{"keep_senders", p.KeepSenders}, {"block_senders", p.BlockSenders}, {"junk_senders", p.JunkSenders}, {"never_block", p.NeverBlock}} {
		for _, k := range l.senders {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("policy %s must not contain blank entries", l.name)
			}
		}
	}
	for _, j := range p.JunkSenders {
		if !addressOrDomain(j) {
			return fmt.Errorf("policy junk_senders entry %q must be an address or @domain", j)
		}
	}
	for i, r := range p.Rules {
		labelled := strings.TrimSpace(r.Stage) != "" || strings.TrimSpace(r.Label) != ""
		switch {
		case len(r.Senders) > 0 && labelled:
			return fmt.Errorf("policy rule %d names both a stage label and senders; use one", i+1)
		case len(r.Senders) > 0:
			for _, e := range r.Senders {
				if !addressOrDomain(e) {
					return fmt.Errorf("policy rule %d: senders entry %q must be an address or @domain", i+1, e)
				}
			}
		case strings.TrimSpace(r.Stage) == "" || strings.TrimSpace(r.Label) == "":
			return fmt.Errorf("policy rule %d needs a stage and a label, or senders", i+1)
		}
		if _, ok := rank[r.Action]; !ok {
			return fmt.Errorf("policy rule %d: unknown action %q (want keep, quarantine, delete or digest)", i+1, r.Action)
		}
		if r.Hold && (len(r.Senders) == 0 || r.OlderThan == "") {
			return fmt.Errorf("policy rule %d: hold needs senders and older_than", i+1)
		}
		if r.OlderThan != "" {
			if _, err := ParseAge(r.OlderThan); err != nil {
				return fmt.Errorf("policy rule %d: older_than %v", i+1, err)
			}
		}
		for _, u := range r.Unless {
			if st, l, ok := strings.Cut(u, ":"); !ok || strings.TrimSpace(st) == "" || strings.TrimSpace(l) == "" {
				return fmt.Errorf("policy rule %d: unless %q must be stage:label", i+1, u)
			}
		}
	}
	return nil
}

// Decide returns the action for m given the latest verdict of each stage,
// every reason supporting it, and whether a deletion blocks the sender: it
// does unless every reason for it is a no_block rule. An empty action means
// nothing applies.
func (p Policy) Decide(m Message, verdicts []store.Verdict, now time.Time) Decision {
	if p.Keeps(m.Sender) {
		return Decision{Action: stage.Keep, Reasons: []string{"policy:keep_sender"}}
	}
	type reason struct {
		text  string
		block bool
	}
	reasons := map[string][]reason{}
	if p.Blocks(m.Sender) {
		reasons[stage.Delete] = append(reasons[stage.Delete], reason{"policy:block_sender", true})
	}
	labelled := map[string]bool{}
	for _, v := range verdicts {
		if v.Error == "" {
			for _, l := range v.Labels {
				labelled[v.Stage+":"+l] = true
			}
		}
	}
	// aged reports whether r's older_than allows it for m, and the reason
	// suffix that says so.
	aged := func(r Rule) (string, bool) {
		if r.OlderThan == "" {
			return "", true
		}
		age, err := ParseAge(r.OlderThan)
		if err != nil || m.Received.IsZero() || now.Sub(m.Received) < age {
			return "", false
		}
		return ">" + strings.TrimSpace(r.OlderThan), true
	}
	skipped := func(r Rule) bool {
		return slices.ContainsFunc(r.Unless, func(u string) bool { return labelled[stageLabel(u)] })
	}
	for _, r := range p.Rules {
		if !r.Hold || skipped(r) {
			continue
		}
		entry, ok := matchSender(r.Senders, m.Sender)
		if !ok {
			continue
		}
		text, aged := aged(r)
		if !aged {
			return Decision{Action: stage.Keep, Reasons: []string{"senders:" + entry + "<" + strings.TrimSpace(r.OlderThan)}}
		}
		// Old enough: the rule outranks every other rule, but not a stage
		// that keeps (the allowlist: the owner, a contact).
		if !slices.ContainsFunc(verdicts, func(v store.Verdict) bool { return v.Error == "" && v.Action == stage.Keep }) {
			return Decision{Action: r.Action, Reasons: []string{"senders:" + entry + text}, Block: r.Action == stage.Delete && !r.NoBlock}
		}
	}
	for _, r := range p.Rules {
		if _, known := rank[r.Action]; !known || len(r.Senders) == 0 || skipped(r) {
			continue
		}
		if entry, ok := matchSender(r.Senders, m.Sender); ok {
			if text, ok := aged(r); ok {
				reasons[r.Action] = append(reasons[r.Action], reason{"senders:" + entry + text, !r.NoBlock})
			}
		}
	}
	for _, v := range verdicts {
		if v.Error != "" {
			continue // a failed evaluation is not a judgement
		}
		if _, known := rank[v.Action]; known {
			reasons[v.Action] = append(reasons[v.Action], reason{v.Stage + ":action=" + v.Action, true})
		}
		for _, r := range p.Rules {
			if _, known := rank[r.Action]; !known || len(r.Senders) > 0 || r.Stage != v.Stage {
				continue // an unknown action can neither outrank nor block a real one
			}
			if skipped(r) {
				continue
			}
			text, ok := aged(r)
			if !ok {
				continue
			}
			for _, l := range v.Labels {
				if l == r.Label {
					reasons[r.Action] = append(reasons[r.Action], reason{v.Stage + ":" + l + text, !r.NoBlock})
				}
			}
		}
	}
	best := ""
	for action := range reasons {
		if best == "" || rank[action] < rank[best] {
			best = action
		}
	}
	if best == "" {
		return Decision{}
	}
	d := Decision{Action: best}
	for _, r := range reasons[best] {
		d.Reasons = append(d.Reasons, r.text)
		d.Block = d.Block || (best == stage.Delete && r.block)
	}
	sort.Strings(d.Reasons)
	return d
}

// Keeps reports whether sender is a keep sender, ignoring case and spacing.
func (p Policy) Keeps(sender string) bool { return listed(p.KeepSenders, sender) }

// stageLabel normalizes a "stage:label" entry, which config may space out.
func stageLabel(s string) string {
	st, l, _ := strings.Cut(s, ":")
	return strings.TrimSpace(st) + ":" + strings.TrimSpace(l)
}

// Junks reports whether sender's mail goes to Junk: an exact address entry,
// or an "@domain" entry for an address at that domain.
func (p Policy) Junks(sender string) bool {
	_, ok := matchSender(p.JunkSenders, sender)
	return ok
}

// matchSender returns the first entry of list (an address, or @domain for an
// address at exactly that domain) that sender matches, ignoring case and
// spacing.
func matchSender(list []string, sender string) (string, bool) {
	sender = strings.ToLower(strings.TrimSpace(sender))
	if sender == "" {
		return "", false
	}
	for _, e := range list {
		e = strings.ToLower(strings.TrimSpace(e))
		if strings.HasPrefix(e, "@") {
			// A domain matches an address there, never a bare "@domain".
			if strings.HasSuffix(sender, e) && len(sender) > len(e) {
				return e, true
			}
		} else if e == sender {
			return e, true
		}
	}
	return "", false
}

// addressOrDomain reports an address or "@domain" entry: one @ with
// something after it.
func addressOrDomain(s string) bool {
	s = strings.TrimSpace(s)
	if strings.ContainsAny(s, " \t\r\n") {
		return false // could never match an address
	}
	_, domain, ok := strings.Cut(s, "@")
	return ok && domain != "" && !strings.Contains(domain, "@")
}

// Blocks reports whether sender is a block sender, ignoring case and spacing.
func (p Policy) Blocks(sender string) bool { return listed(p.BlockSenders, sender) }

// NeverBlocks reports whether a sender rule must never block sender, ignoring
// case and spacing.
func (p Policy) NeverBlocks(sender string) bool { return listed(p.NeverBlock, sender) }

func listed(list []string, sender string) bool {
	sender = strings.ToLower(strings.TrimSpace(sender))
	if sender == "" {
		return false
	}
	for _, k := range list {
		if strings.ToLower(strings.TrimSpace(k)) == sender {
			return true
		}
	}
	return false
}
