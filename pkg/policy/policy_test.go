package policy_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/policy"
	"github.com/mrothroc/mailpipe/pkg/store"
)

var p = policy.Policy{
	KeepSenders: []string{"Newsletter@Example.org"},
	Rules: []policy.Rule{
		{Stage: "political", Label: "political_appeal", Action: "delete"},
		{Stage: "newsletter", Label: "newsletter", Action: "digest"},
	},
}

// decide is Decide for a message with no received time, judged now.
func decide(p policy.Policy, sender string, vs []store.Verdict) (string, []string) {
	d := p.Decide(policy.Message{Sender: sender}, vs, now)
	return d.Action, d.Reasons
}

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func v(stage string, labels []string, action string, final bool) store.Verdict {
	return store.Verdict{Stage: stage, Labels: labels, Action: action, Final: final}
}

func TestDecide(t *testing.T) {
	cases := map[string]struct {
		sender   string
		verdicts []store.Verdict
		action   string
		reason   string
	}{
		"rule match":                {"a@campaign.example.org", []store.Verdict{v("political", []string{"political_appeal"}, "", false)}, "delete", "political:political_appeal"},
		"explicit verdict":          {"a@x.example", []store.Verdict{v("legacy-custom", []string{"discard_candidate"}, "delete", false)}, "delete", "legacy-custom:action=delete"},
		"no label no rule":          {"a@x.example", []store.Verdict{v("political", nil, "", false)}, "", ""},
		"final keep wins":           {"friend@example.org", []store.Verdict{v("known", []string{"known_contact"}, "keep", true), v("political", []string{"political_appeal"}, "", false)}, "keep", "known:action=keep"},
		"keep sender wins":          {"newsletter@example.org", []store.Verdict{v("political", []string{"political_appeal"}, "", false)}, "keep", "policy:keep_sender"},
		"delete beats digest":       {"a@x.example", []store.Verdict{v("newsletter", []string{"newsletter"}, "", false), v("political", []string{"political_appeal"}, "", false)}, "delete", "political:political_appeal"},
		"quarantine beats delete":   {"a@x.example", []store.Verdict{v("political", []string{"political_appeal"}, "", false), v("other", nil, "quarantine", false)}, "quarantine", "other:action=quarantine"},
		"non-final keep still wins": {"a@x.example", []store.Verdict{v("other", nil, "keep", false), v("political", []string{"political_appeal"}, "", false)}, "keep", "other:action=keep"},
	}
	for name, c := range cases {
		action, reasons := decide(p, c.sender, c.verdicts)
		if action != c.action || (c.reason != "" && !contains(reasons, c.reason)) || (c.reason == "" && len(reasons) != 0) {
			t.Errorf("%s: Decide = %q %v, want %q with %q", name, action, reasons, c.action, c.reason)
		}
	}
}

func TestDecideListsEveryReasonForTheChosenAction(t *testing.T) {
	action, reasons := decide(p, "a@x.example", []store.Verdict{
		v("political", []string{"political_appeal"}, "", false),
		v("imported-classifier", []string{"flagged"}, "delete", false),
	})
	if action != "delete" || strings.Join(reasons, ",") != "imported-classifier:action=delete,political:political_appeal" {
		t.Fatalf("Decide = %q %v", action, reasons)
	}
}

func TestValidate(t *testing.T) {
	bad := []policy.Policy{
		{Rules: []policy.Rule{{Stage: "", Label: "x", Action: "delete"}}},
		{Rules: []policy.Rule{{Stage: "s", Label: "x", Action: "explode"}}},
	}
	for _, b := range bad {
		if err := b.Validate(); err == nil {
			t.Errorf("%+v accepted", b)
		}
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// An action outside the known set (from a buggy or hostile stage) is ignored:
// it must neither outrank nor block real actions.
func TestUnknownVerdictActionsAreIgnored(t *testing.T) {
	action, reasons := decide(p, "a@x.example", []store.Verdict{
		v("rogue", nil, "explode", false),
		v("political", []string{"political_appeal"}, "", false),
	})
	if action != "delete" || len(reasons) != 1 || reasons[0] != "political:political_appeal" {
		t.Fatalf("Decide = %q %v", action, reasons)
	}
	if action, _ := decide(p, "a@x.example", []store.Verdict{v("rogue", nil, "explode", false)}); action != "" {
		t.Fatalf("unknown action produced %q", action)
	}
}

// A rule with an unknown action (should Validate be bypassed) never outranks a
// real action.
func TestUnknownRuleActionsAreIgnored(t *testing.T) {
	q := policy.Policy{Rules: []policy.Rule{
		{Stage: "political", Label: "political_appeal", Action: "explode"},
		{Stage: "political", Label: "political_appeal", Action: "delete"},
	}}
	action, reasons := decide(q, "a@x.example", []store.Verdict{v("political", []string{"political_appeal"}, "", false)})
	if action != "delete" || len(reasons) != 1 {
		t.Fatalf("Decide = %q %v", action, reasons)
	}
}

func TestBlankKeepSendersAreRejected(t *testing.T) {
	if err := (policy.Policy{KeepSenders: []string{"  "}}).Validate(); err == nil {
		t.Fatal("blank keep sender accepted")
	}
}

func TestFailedVerdictsNeverDecide(t *testing.T) {
	failed := v("political", []string{"political_appeal"}, "delete", false)
	failed.Error = "timed out"
	if action, _ := decide(p, "a@x.example", []store.Verdict{failed}); action != "" {
		t.Fatalf("a failed verdict decided %q", action)
	}
}

func TestKeepsMatchesKeepSendersIgnoringCaseAndSpace(t *testing.T) {
	if !p.Keeps(" newsletter@example.ORG ") || p.Keeps("other@example.org") {
		t.Fatal("Keeps disagrees with keep_senders")
	}
}

func TestBlankNeverBlockEntriesAreRejected(t *testing.T) {
	if err := (policy.Policy{NeverBlock: []string{" "}}).Validate(); err == nil {
		t.Fatal("blank never_block entry accepted")
	}
}

func TestNeverBlockProtectsFromRulesButNotFromDeletes(t *testing.T) {
	q := policy.Policy{NeverBlock: []string{"No-Reply@Platform.example.com"},
		Rules: []policy.Rule{{Stage: "political", Label: "political_appeal", Action: "delete"}}}
	if !q.NeverBlocks(" no-reply@platform.example.COM ") || q.NeverBlocks("other@example.org") {
		t.Fatal("NeverBlocks disagrees with never_block")
	}
	action, _ := decide(q, "no-reply@platform.example.com", []store.Verdict{v("political", []string{"political_appeal"}, "", false)})
	if action != "delete" {
		t.Fatalf("a never_block sender's message decided %q, want delete", action)
	}
}

func TestABlankSenderIsNeverListed(t *testing.T) {
	q := policy.Policy{KeepSenders: []string{""}, NeverBlock: []string{" "}} // unvalidated
	if q.Keeps("  ") || q.NeverBlocks("") {
		t.Fatal("a blank sender matched a blank entry")
	}
}

func TestValidateNamesTheFirstListWithABlankEntry(t *testing.T) {
	q := policy.Policy{KeepSenders: []string{" "}, NeverBlock: []string{" "}}
	for range 20 {
		if err := q.Validate(); err == nil || !strings.Contains(err.Error(), "keep_senders") {
			t.Fatalf("Validate = %v, want keep_senders named first", err)
		}
	}
}

func at(daysAgo int) policy.Message {
	return policy.Message{Sender: "deals@shop.example.com", Received: now.AddDate(0, 0, -daysAgo)}
}

var shelf = policy.Policy{Rules: []policy.Rule{
	{Stage: "category", Label: "promotion", Action: "delete", OlderThan: "7d", NoBlock: true},
	{Stage: "category", Label: "event", Action: "delete", OlderThan: "30d", NoBlock: true, Unless: []string{"event-past:event_upcoming", "category:transactional"}},
	{Stage: "event-past", Label: "event_over", Action: "delete", NoBlock: true},
	{Stage: "political", Label: "political_appeal", Action: "delete"},
}}

func TestOlderThanAppliesARuleOnlyOnceTheMessageIsThatOld(t *testing.T) {
	promo := []store.Verdict{v("category", []string{"promotion"}, "", false)}
	if d := shelf.Decide(at(6), promo, now); d.Action != "" {
		t.Fatalf("6 days old: %+v", d)
	}
	if d := shelf.Decide(at(7), promo, now); d.Action != "delete" || d.Block || strings.Join(d.Reasons, ",") != "category:promotion>7d" {
		t.Fatalf("7 days old: %+v", d)
	}
	if d := shelf.Decide(policy.Message{Sender: "x@example.org"}, promo, now); d.Action != "" {
		t.Fatalf("an unknown age must never satisfy older_than: %+v", d)
	}
}

func TestUnlessSkipsARuleWhenTheOtherLabelIsPresent(t *testing.T) {
	event := v("category", []string{"event"}, "", false)
	if d := shelf.Decide(at(40), []store.Verdict{event, v("event-past", []string{"event_upcoming"}, "", false)}, now); d.Action != "" {
		t.Fatalf("an upcoming event was decided %+v", d)
	}
	if d := shelf.Decide(at(40), []store.Verdict{event}, now); d.Action != "delete" {
		t.Fatalf("an undated old event: %+v", d)
	}
	receipt := v("category", []string{"event", "transactional"}, "", false)
	if d := shelf.Decide(at(40), []store.Verdict{receipt}, now); d.Action != "" {
		t.Fatalf("any one unless label must skip the rule: %+v", d)
	}
	if d := shelf.Decide(at(2), []store.Verdict{event, v("event-past", []string{"event_over"}, "", false)}, now); d.Action != "delete" {
		t.Fatalf("a past event, however recent: %+v", d)
	}
}

func TestADeletionBlocksUnlessEveryReasonIsNoBlock(t *testing.T) {
	both := []store.Verdict{v("category", []string{"promotion"}, "", false), v("political", []string{"political_appeal"}, "", false)}
	if d := shelf.Decide(at(10), both, now); d.Action != "delete" || !d.Block {
		t.Fatalf("a political reason must still block: %+v", d)
	}
	legacy := []store.Verdict{v("imported-classifier", nil, "delete", false)}
	if d := shelf.Decide(at(10), legacy, now); !d.Block {
		t.Fatalf("a verdict's own delete blocks, as before: %+v", d)
	}
}

func TestBlockSendersDeleteEverythingAndBlock(t *testing.T) {
	q := policy.Policy{BlockSenders: []string{"Deals@Shop.example.com"}}
	if d := q.Decide(at(0), nil, now); d.Action != "delete" || !d.Block || strings.Join(d.Reasons, ",") != "policy:block_sender" {
		t.Fatalf("block sender: %+v", d)
	}
	if !q.Blocks(" deals@shop.EXAMPLE.com ") || q.Blocks("other@example.org") {
		t.Fatal("Blocks disagrees with block_senders")
	}
	contact := []store.Verdict{v("known", []string{"known_contact"}, "keep", true)}
	if d := q.Decide(at(0), contact, now); d.Action != "keep" {
		t.Fatalf("a contact on the block list must be kept: %+v", d)
	}
}

func TestValidateChecksTheNewRuleFields(t *testing.T) {
	for _, r := range []policy.Rule{
		{Stage: "s", Label: "l", Action: "delete", OlderThan: "soon"},
		{Stage: "s", Label: "l", Action: "delete", OlderThan: "-3d"},
		{Stage: "s", Label: "l", Action: "delete", Unless: []string{"nocolon"}},
		{Stage: "s", Label: "l", Action: "delete", Unless: []string{"category:x", ":label"}},
	} {
		if err := (policy.Policy{Rules: []policy.Rule{r}}).Validate(); err == nil {
			t.Errorf("rule %+v accepted", r)
		}
	}
	if err := (policy.Policy{BlockSenders: []string{" "}}).Validate(); err == nil {
		t.Error("blank block sender accepted")
	}
	if err := shelf.Validate(); err != nil {
		t.Errorf("valid rules rejected: %v", err)
	}
}

func TestParseAgeReadsDaysAndDurations(t *testing.T) {
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "36h": 36 * time.Hour, " 1d ": 24 * time.Hour} {
		if got, err := policy.ParseAge(in); err != nil || got != want {
			t.Errorf("ParseAge(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0d", "-1h", "week", "7"} {
		if _, err := policy.ParseAge(bad); err == nil {
			t.Errorf("ParseAge(%q) accepted", bad)
		}
	}
}

func TestUnlessIgnoresSpacingAroundStageAndLabel(t *testing.T) {
	q := policy.Policy{Rules: []policy.Rule{{Stage: "category", Label: "news", Action: "delete", Unless: []string{" category : transactional "}}}}
	vs := []store.Verdict{v("category", []string{"news", "transactional"}, "", false)}
	if d := q.Decide(at(40), vs, now); d.Action != "" {
		t.Fatalf("a spaced unless entry did not apply: %+v", d)
	}
}

func TestJunkSendersMatchAddressesAndWholeDomains(t *testing.T) {
	q := policy.Policy{JunkSenders: []string{"@PUBLISHER.example.org", " donate@reference.example.com "}}
	for sender, want := range map[string]bool{
		"editor@publisher.example.org": true, "ExecutiveDirector@publisher.example.org": true, "donate@reference.example.com": true,
		"hello@reference.example.com": false, "x@notpublisher.example.org": false, "publisher.example.org@example.com": false, "": false,
	} {
		if got := q.Junks(sender); got != want {
			t.Errorf("Junks(%q) = %v, want %v", sender, got, want)
		}
	}
	for _, bad := range []string{"publisher.example.org", "@", " ", "a@b@c"} {
		if err := (policy.Policy{JunkSenders: []string{bad}}).Validate(); err == nil {
			t.Errorf("junk sender %q accepted", bad)
		}
	}
	if err := q.Validate(); err != nil {
		t.Errorf("valid junk senders rejected: %v", err)
	}
}

// A rule can name senders instead of a stage label: an expiry for one
// sender's mail, whatever its category.
func TestASenderRuleExpiresThatSendersMail(t *testing.T) {
	q := policy.Policy{Rules: []policy.Rule{{Senders: []string{"@payroll.example.org", "no-reply@payroll.example.com"}, Action: "delete", OlderThan: "30d", NoBlock: true}}}
	old := policy.Message{Sender: "its-payday@Payroll.example.org", Received: now.AddDate(0, 0, -31)}
	if d := q.Decide(old, nil, now); d.Action != "delete" || d.Block || strings.Join(d.Reasons, ",") != "senders:@payroll.example.org>30d" {
		t.Fatalf("old: %+v", d)
	}
	recent := policy.Message{Sender: "its-payday@payroll.example.org", Received: now.AddDate(0, 0, -3)}
	if d := q.Decide(recent, nil, now); d.Action != "" {
		t.Fatalf("recent: %+v", d)
	}
	other := policy.Message{Sender: "no-reply@hello.payroll.example.com", Received: now.AddDate(0, 0, -90)}
	if d := q.Decide(other, nil, now); d.Action != "" {
		t.Fatalf("an unlisted address: %+v", d)
	}
	contact := []store.Verdict{v("known", []string{"known_contact"}, "keep", true)}
	if d := q.Decide(old, contact, now); d.Action != "keep" {
		t.Fatalf("a keep still wins: %+v", d)
	}
}

func TestARuleNamesALabelOrSendersNotBoth(t *testing.T) {
	for _, r := range []policy.Rule{
		{Action: "delete"}, // neither
		{Stage: "s", Label: "l", Senders: []string{"@x.example"}, Action: "delete"}, // both
		{Senders: []string{"x.example"}, Action: "delete"},                          // not an address or @domain
	} {
		if err := (policy.Policy{Rules: []policy.Rule{r}}).Validate(); err == nil {
			t.Errorf("rule %+v accepted", r)
		}
	}
}

func TestSenderRulesCoverAddressesUnlessAndBadSenders(t *testing.T) {
	q := policy.Policy{Rules: []policy.Rule{{Senders: []string{"@payroll.example.org", "no-reply@payroll.example.com"}, Action: "delete", NoBlock: true,
		Unless: []string{"category:transactional"}}}}
	if d := q.Decide(policy.Message{Sender: "No-Reply@payroll.example.com"}, nil, now); d.Action != "delete" || d.Reasons[0] != "senders:no-reply@payroll.example.com" {
		t.Fatalf("exact address: %+v", d)
	}
	tx := []store.Verdict{v("category", []string{"transactional"}, "", false)}
	if d := q.Decide(policy.Message{Sender: "x@payroll.example.org"}, tx, now); d.Action != "" {
		t.Fatalf("unless must skip a sender rule: %+v", d)
	}
	if d := q.Decide(policy.Message{Sender: "@payroll.example.org"}, nil, now); d.Action != "" {
		t.Fatalf("a bare @domain sender matched: %+v", d)
	}
	if (policy.Policy{JunkSenders: []string{"@publisher.example.org"}}).Junks("@publisher.example.org") {
		t.Fatal("a bare @domain sender is junk")
	}
	// A verdict with no stage must not make a sender rule match as a stage rule.
	if d := q.Decide(policy.Message{Sender: "x@example.org"}, []store.Verdict{{Labels: []string{""}}}, now); d.Action != "" {
		t.Fatalf("a stageless verdict matched a sender rule: %+v", d)
	}
	for _, bad := range []string{"@ payroll.example.com", "user @payroll.example.com", "us er@payroll.example.com"} {
		if err := (policy.Policy{JunkSenders: []string{bad}}).Validate(); err == nil {
			t.Errorf("entry %q with inner space accepted", bad)
		}
	}
}

// A hold rule keeps a sender's mail until it is old enough, whatever other
// rules say, then deletes it: an appeal from a friend lasts its seven days.
func TestAHoldRuleKeepsMailUntilItAges(t *testing.T) {
	q := policy.Policy{NeverBlock: []string{"friend@example.org"}, Rules: []policy.Rule{
		{Stage: "political", Label: "political_appeal", Action: "delete"},
		{Senders: []string{"friend@example.org"}, Action: "delete", OlderThan: "7d", Hold: true, NoBlock: true},
	}}
	appeal := []store.Verdict{v("political", []string{"political_appeal"}, "", false)}
	recent := policy.Message{Sender: "Friend@example.org", Received: now.AddDate(0, 0, -2)}
	if d := q.Decide(recent, appeal, now); d.Action != "keep" || strings.Join(d.Reasons, ",") != "senders:friend@example.org<7d" {
		t.Fatalf("recent: %+v", d)
	}
	old := policy.Message{Sender: "friend@example.org", Received: now.AddDate(0, 0, -8)}
	// never_block, checked when the blocklist is built, keeps the sender off it.
	if d := q.Decide(old, appeal, now); d.Action != "delete" || !q.NeverBlocks(old.Sender) {
		t.Fatalf("old: %+v", d)
	}
	// Mail of unknown age is held: it never qualifies for an age rule.
	if d := q.Decide(policy.Message{Sender: "friend@example.org"}, appeal, now); d.Action != "keep" {
		t.Fatalf("unknown age: %+v", d)
	}
}

func TestHoldNeedsSendersAndAnAge(t *testing.T) {
	for name, r := range map[string]policy.Rule{
		"no age":     {Senders: []string{"a@example.org"}, Action: "delete", Hold: true},
		"label rule": {Stage: "s", Label: "l", Action: "delete", OlderThan: "7d", Hold: true},
	} {
		if err := (policy.Policy{Rules: []policy.Rule{r}}).Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A hold rule sets the sender's lifetime both ways: once its mail is old
// enough, an older keep rule does not save it. The allowlist (the owner, a
// contact) still does.
func TestAnAgedHoldRuleOutranksKeepRules(t *testing.T) {
	q := policy.Policy{Rules: []policy.Rule{
		{Stage: "legacy", Label: "kept", Action: "keep"},
		{Senders: []string{"friend@example.org"}, Action: "delete", OlderThan: "7d", Hold: true, NoBlock: true},
	}}
	old := policy.Message{Sender: "friend@example.org", Received: now.AddDate(0, 0, -30)}
	kept := []store.Verdict{v("legacy", []string{"kept"}, "", false)}
	if d := q.Decide(old, kept, now); d.Action != "delete" || d.Block || strings.Join(d.Reasons, ",") != "senders:friend@example.org>7d" {
		t.Fatalf("keep rule: %+v", d)
	}
	contact := append(kept, v("known", []string{"known_contact"}, "keep", true))
	if d := q.Decide(old, contact, now); d.Action != "keep" {
		t.Fatalf("contact: %+v", d)
	}
}

// A private classifier can opt one sender into reviewed deletion while all
// missing, failed or uncertain classifications retain the default protection.
func TestPrivateClassifierLabelsCanLiftOnlyTheirScopedDefaultKeep(t *testing.T) {
	q := policy.Policy{Rules: []policy.Rule{
		{Senders: []string{"alerts@service.example.net"}, Action: "keep", Unless: []string{"custom:discard_candidate"}},
		{Stage: "custom", Label: "discard_candidate", Action: "delete", NoBlock: true},
	}}
	m := policy.Message{Sender: "alerts@service.example.net"}
	for name, vs := range map[string][]store.Verdict{
		"missing":   nil,
		"uncertain": {{Stage: "custom", Labels: []string{"review"}}},
		"failed":    {{Stage: "custom", Labels: []string{"discard_candidate"}, Error: "evidence unavailable"}},
	} {
		if d := q.Decide(m, vs, now); d.Action != "keep" {
			t.Errorf("%s: %+v", name, d)
		}
	}
	d := q.Decide(m, []store.Verdict{{Stage: "custom", Labels: []string{"discard_candidate"}}}, now)
	if d.Action != "delete" || d.Block {
		t.Fatalf("confirmed: %+v", d)
	}
	if d := q.Decide(policy.Message{Sender: "other@example.org"}, nil, now); d.Action != "" {
		t.Fatalf("unrelated sender: %+v", d)
	}
}
