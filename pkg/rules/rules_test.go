package rules_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/rules"
	"github.com/mrothroc/mailpipe/pkg/store"
)

func TestAssessBlocksOnlySendersWithNothingKeptAndNothingProtected(t *testing.T) {
	moved := []store.SenderMoves{
		{Sender: "news@campaign.example.org", Moved: 5},
		{Sender: "alerts@service.example.net", Moved: 40},
		{Sender: "friend@example.org", Moved: 1},
		{Sender: "", Moved: 2},
	}
	kept := map[string]int{"alerts@service.example.net": 3}
	protect := func(s string) string {
		if s == "friend@example.org" {
			return "known_contact"
		}
		return ""
	}
	got := rules.Assess(moved, kept, nil, protect, func(string) bool { return false })
	want := []rules.Candidate{
		{Sender: "news@campaign.example.org", Moved: 5, Blocked: true},
		{Sender: "alerts@service.example.net", Moved: 40, Kept: 3, Reason: "3 kept outside Deleted Items"},
		{Sender: "friend@example.org", Moved: 1, Reason: "known_contact"},
		{Sender: "", Moved: 2, Reason: "no sender address"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if b := rules.Blocked(got); strings.Join(b, ",") != "news@campaign.example.org" {
		t.Fatalf("blocked %v", b)
	}
}

func senders(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("s%03d@example.org", i)
	}
	return out
}

func TestDesiredSortsDedupesAndSplitsSendersIntoNamedRules(t *testing.T) {
	in := append([]string{"S001@Example.org", " s000@example.org "}, senders(rules.PerRule+1)...)
	got := rules.Desired(in)
	if len(got) != 2 || got[0].Name != "mailpipe blocked #1" || got[1].Name != "mailpipe blocked #2" {
		t.Fatalf("rules %+v", got)
	}
	if len(got[0].Senders) != rules.PerRule || got[0].Senders[0] != "s000@example.org" || len(got[1].Senders) != 1 ||
		got[1].Senders[0] != fmt.Sprintf("s%03d@example.org", rules.PerRule) {
		t.Fatalf("senders %d %v / %v", len(got[0].Senders), got[0].Senders[:2], got[1].Senders)
	}
	for _, r := range got {
		if !r.Enabled || !r.Exact {
			t.Fatalf("desired rule %+v must be enabled and exactly mailpipe's shape", r)
		}
	}
	if len(rules.Desired(nil)) != 0 {
		t.Fatal("no senders must mean no rules")
	}
}

func TestKindTellsManagedAndRetiredRulesFromTheOwners(t *testing.T) {
	cases := map[string]rules.Kind{
		"mailpipe blocked #1": rules.Managed, "mailpipe blocked #12": rules.Managed,
		"mailpipe plan 4 #1": rules.Retired, "mailpipe plan 12 #3": rules.Retired,
		"mailpipe blocked #": rules.Owner, "mailpipe blocked #x": rules.Owner, "mailpipe plan 4": rules.Owner,
		"mailpipe plan x #1": rules.Owner, "My rule": rules.Owner,
	}
	for name, want := range cases {
		if got := rules.KindOf(name); got != want {
			t.Errorf("KindOf(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestDiffCreatesUpdatesRetiresAndLeavesTheOwnersRulesAlone(t *testing.T) {
	desired := rules.Desired([]string{"a@example.org", "b@example.org"})
	desired = append(desired, rules.Rule{Name: "mailpipe blocked #2", Senders: []string{"c@example.org"}, Enabled: true, Exact: true})
	server := []rules.Rule{
		{ID: "R1", Name: "mailpipe blocked #1", Sequence: 3, Senders: []string{"a@example.org", "z@example.org"}, Enabled: true, Exact: true},
		{ID: "R9", Name: "mailpipe blocked #9", Sequence: 4, Senders: []string{"y@example.org"}, Enabled: true, Exact: true},
		{ID: "U1", Name: "Family", Sequence: 7, Senders: []string{"a@example.org"}},
		{ID: "P5", Name: "mailpipe plan 5 #1", Sequence: 2, Senders: []string{"x@example.org"}, Enabled: true, Exact: true},
	}
	got := rules.Diff(desired, server)
	type row struct {
		op, name, id   string
		seq            int
		added, removed string
	}
	var rows []row
	for _, c := range got {
		rows = append(rows, row{string(c.Op), c.Rule.Name, c.Rule.ID, c.Rule.Sequence, strings.Join(c.Added, ","), strings.Join(c.Removed, ",")})
	}
	want := []row{
		{"update", "mailpipe blocked #1", "R1", 3, "b@example.org", "z@example.org"},
		{"create", "mailpipe blocked #2", "", 8, "c@example.org", ""},
		{"delete", "mailpipe blocked #9", "R9", 4, "", "y@example.org"},
		{"delete", "mailpipe plan 5 #1", "P5", 2, "", "x@example.org"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("got  %+v\nwant %+v", rows, want)
	}
	if !rules.Pending(got) {
		t.Fatal("changes reported as nothing pending")
	}
}

func TestDiffReportsSameOnlyForAnUntouchedRule(t *testing.T) {
	desired := rules.Desired([]string{"a@example.org"})
	same := rules.Rule{ID: "R1", Name: "mailpipe blocked #1", Senders: []string{"a@example.org"}, Enabled: true, Exact: true}
	if got := rules.Diff(desired, []rules.Rule{same}); len(got) != 1 || got[0].Op != rules.Same || rules.Pending(got) {
		t.Fatalf("got %+v", got)
	}
	disabled := same
	disabled.Enabled = false
	if got := rules.Diff(desired, []rules.Rule{disabled}); len(got) != 1 || got[0].Op != rules.Update || got[0].Rule.ID != "R1" {
		t.Fatalf("a disabled rule must be re-enabled: %+v", got)
	}
	// A rule edited in Outlook may carry conditions a patch would not clear,
	// so it is replaced, keeping its place in the rule order.
	edited := same
	edited.Exact, edited.Sequence = false, 6
	got := rules.Diff(desired, []rules.Rule{edited})
	if len(got) != 1 || got[0].Op != rules.Replace || got[0].Rule.ID != "R1" || got[0].Rule.Sequence != 6 || !got[0].Rule.Exact {
		t.Fatalf("an edited rule must be replaced: %+v", got)
	}
}

func TestDiffDeletesDuplicateNames(t *testing.T) {
	desired := rules.Desired([]string{"a@example.org"})
	r := rules.Rule{Name: "mailpipe blocked #1", Senders: []string{"a@example.org"}, Enabled: true, Exact: true}
	first, second := r, r
	first.ID, second.ID = "R1", "R2"
	got := rules.Diff(desired, []rules.Rule{first, second})
	if len(got) != 2 || got[0].Op != rules.Same || got[0].Rule.ID != "R1" || got[1].Op != rules.Delete || got[1].Rule.ID != "R2" {
		t.Fatalf("got %+v", got)
	}
}

type fakeServer struct {
	log     []string
	failOn  string
	created []rules.Rule
}

func (f *fakeServer) step(s string) error {
	f.log = append(f.log, s)
	if s == f.failOn {
		return errors.New("graph 500")
	}
	return nil
}
func (f *fakeServer) Create(_ context.Context, r rules.Rule) error {
	f.created = append(f.created, r)
	return f.step(fmt.Sprintf("create %s@%d", r.Name, r.Sequence))
}
func (f *fakeServer) Update(_ context.Context, r rules.Rule) error { return f.step("update " + r.ID) }
func (f *fakeServer) Delete(_ context.Context, id string) error    { return f.step("delete " + id) }

func TestExecuteCarriesOutEachChangeAndReplacesByCreatingFirst(t *testing.T) {
	changes := []rules.Change{
		{Op: rules.Same, Rule: rules.Rule{ID: "R0"}},
		{Op: rules.Update, Rule: rules.Rule{ID: "R1"}},
		{Op: rules.Replace, Rule: rules.Rule{ID: "R2", Name: "mailpipe blocked #2", Sequence: 6}},
		{Op: rules.Create, Rule: rules.Rule{Name: "mailpipe blocked #3", Sequence: 9}},
		{Op: rules.Delete, Rule: rules.Rule{ID: "R9"}},
	}
	f := &fakeServer{}
	done, err := rules.Execute(context.Background(), f, changes)
	if err != nil || done != 4 {
		t.Fatalf("done %d err %v", done, err)
	}
	// Creating the replacement first means the senders are never unblocked.
	want := "update R1,create mailpipe blocked #2@6,delete R2,create mailpipe blocked #3@9,delete R9"
	if strings.Join(f.log, ",") != want {
		t.Fatalf("log %v", f.log)
	}
	f = &fakeServer{failOn: "create mailpipe blocked #2@6"}
	done, err = rules.Execute(context.Background(), f, changes)
	if err == nil || !strings.Contains(err.Error(), "mailpipe blocked #2") || done != 1 || strings.Join(f.log, ",") != "update R1,create mailpipe blocked #2@6" {
		t.Fatalf("done %d err %v log %v: a failure stops the run, and the old rule stays", done, err, f.log)
	}
}

// A replacement whose old rule could not be deleted leaves two rules of one
// name; the one in mailpipe's shape is kept, whichever the server lists first.
func TestDiffKeepsTheExactRuleOfADuplicatedName(t *testing.T) {
	desired := rules.Desired([]string{"a@example.org"})
	edited := rules.Rule{ID: "OLD", Name: "mailpipe blocked #1", Senders: []string{"a@example.org"}, Enabled: true}
	exact := rules.Rule{ID: "NEW", Name: "mailpipe blocked #1", Senders: []string{"a@example.org"}, Enabled: true, Exact: true}
	got := rules.Diff(desired, []rules.Rule{edited, exact})
	if len(got) != 2 || got[0].Op != rules.Same || got[0].Rule.ID != "NEW" || got[1].Op != rules.Delete || got[1].Rule.ID != "OLD" {
		t.Fatalf("got %+v", got)
	}
}

// A sender the owner chose to block is blocked even with mail kept, unless
// protected.
func TestAssessBlocksAForcedSenderWhateverIsKept(t *testing.T) {
	moved := []store.SenderMoves{{Sender: "deals@shop.example.com"}, {Sender: "friend@example.org"}}
	kept := map[string]int{"deals@shop.example.com": 4, "friend@example.org": 2}
	protect := func(s string) string {
		if s == "friend@example.org" {
			return "known_contact"
		}
		return ""
	}
	got := rules.Assess(moved, kept, nil, protect, func(string) bool { return true })
	if !got[0].Blocked || got[1].Blocked || got[1].Reason != "known_contact" {
		t.Fatalf("got %+v", got)
	}
}

// Mail deleted only because it expired was still wanted: its sender is not
// blocked, unless the owner chose to block them.
func TestAssessNeverBlocksASenderWithExpiredMail(t *testing.T) {
	moved := []store.SenderMoves{{Sender: "appeal@cause.example.org", Moved: 2}, {Sender: "deals@shop.example.com", Moved: 1}}
	expired := map[string]int{"appeal@cause.example.org": 3, "deals@shop.example.com": 5}
	none := func(string) string { return "" }
	forced := func(s string) bool { return s == "deals@shop.example.com" }
	got := rules.Assess(moved, map[string]int{}, expired, none, forced)
	if got[0].Blocked || got[0].Expired != 3 || got[0].Reason != "3 expired under a shelf life" || !got[1].Blocked {
		t.Fatalf("got %+v", got)
	}
}

func TestDesiredJunkNamesJunkRules(t *testing.T) {
	got := rules.DesiredJunk([]string{"@PUBLISHER.example.org", "@charity.example.net", "@publisher.example.org"})
	if len(got) != 1 || got[0].Name != "mailpipe junk #1" || !got[0].Junk || !got[0].Exact || !got[0].Enabled ||
		strings.Join(got[0].Senders, ",") != "@charity.example.net,@publisher.example.org" {
		t.Fatalf("got %+v", got)
	}
	if rules.KindOf("mailpipe junk #1") != rules.Managed || rules.KindOf("mailpipe junk #") != rules.Owner {
		t.Fatal("junk rule names are not managed")
	}
	if len(rules.DesiredJunk(nil)) != 0 {
		t.Fatal("no junk senders must mean no junk rules")
	}
}
