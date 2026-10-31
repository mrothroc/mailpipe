package rules_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/rules"
)

type call struct {
	method, path string
	query        url.Values
	body         string
}

// fakeGraph answers Do from a path -> JSON map and Batch from a URL -> response
// map, recording every call.
type fakeGraph struct {
	calls   []call
	answers map[string]string
	batch   map[string]graph.BatchResponse
	doErr   map[string]error
}

func TestReviewedManagedRuleRefusesConcurrentEdit(t *testing.T) {
	const path = "GET /me/mailFolders/inbox/messageRules"
	g := &fakeGraph{answers: map[string]string{path: `{"value":[{"id":"R1","displayName":"mailpipe blocked #1","sequence":1,"isEnabled":true,"conditions":{"fromAddresses":[{"emailAddress":{"address":"a@example.org"}}]},"actions":{"delete":true,"stopProcessingRules":true}}]}`}}
	srv := rules.NewGraphRules(g)
	listed, err := srv.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	changes := rules.Diff(rules.Desired([]string{"b@example.org"}), listed)
	g.answers[path] = strings.Replace(g.answers[path], `"sequence":1`, `"sequence":2`, 1)
	if _, err = srv.ExecuteReviewed(context.Background(), changes); err == nil {
		t.Fatal("concurrently reordered rule overwritten")
	}
	for _, call := range g.calls {
		if call.method != "GET" {
			t.Fatalf("mutation after stale rule: %+v", call)
		}
	}
}

func (g *fakeGraph) Do(_ context.Context, method, path string, q url.Values, _ string, body, out any) error {
	b, _ := json.Marshal(body)
	if body == nil {
		b = nil
	}
	g.calls = append(g.calls, call{method, path, q, string(b)})
	if err := g.doErr[method+" "+path]; err != nil {
		return err
	}
	if a, ok := g.answers[method+" "+path]; ok && out != nil {
		return json.Unmarshal([]byte(a), out)
	}
	return nil
}

func (g *fakeGraph) Batch(_ context.Context, reqs []graph.BatchRequest) ([]graph.BatchResponse, error) {
	out := make([]graph.BatchResponse, len(reqs))
	for i, r := range reqs {
		g.calls = append(g.calls, call{method: "BATCH " + r.Method, path: r.URL})
		resp, ok := g.batch[r.URL]
		if !ok {
			resp = graph.BatchResponse{Status: 404}
		}
		resp.ID = r.ID
		out[i] = resp
	}
	return out, nil
}

func TestListReadsEveryInboxRuleAndRecognisesMailpipesShape(t *testing.T) {
	g := &fakeGraph{answers: map[string]string{
		"GET /me/mailFolders/inbox/messageRules": `{"value":[
			{"id":"R1","displayName":"mailpipe plan 4 #1","sequence":2,"isEnabled":true,
			 "conditions":{"fromAddresses":[{"emailAddress":{"name":"B","address":"B@Example.org"}},{"emailAddress":{"address":"a@example.org"}}]},
			 "actions":{"delete": true ,"stopProcessingRules":
			 true,"markAsRead":false,"moveToFolder":null,"forwardTo":[]}},
			{"id":"R2","displayName":"mailpipe plan 4 #2","sequence":3,"isEnabled":true,
			 "conditions":{"fromAddresses":[{"emailAddress":{"address":"c@example.org"}}],"subjectContains":["sale"]},
			 "actions":{"delete":true,"stopProcessingRules":true}}],
			"@odata.nextLink":"https://graph.example/next"}`,
		"GET https://graph.example/next": `{"value":[
			{"id":"U1","displayName":"Family","sequence":1,"isEnabled":false,"conditions":{"fromAddresses":[{"emailAddress":{"address":"mum@example.org"}}]},
			 "actions":{"moveToFolder":"F-FAM"}},
			{"id":"R3","displayName":"mailpipe plan 4 #3","sequence":4,"isEnabled":true,
			 "conditions":{"fromAddresses":[{"emailAddress":{"address":"d@example.org"}}]},
			 "exceptions":{"senderContains":["vip"]},"actions":{"delete":true,"stopProcessingRules":true}}]}`,
	}}
	got, err := rules.NewGraphRules(g).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []rules.Rule{
		{ID: "R1", Name: "mailpipe plan 4 #1", Sequence: 2, Enabled: true, Senders: []string{"a@example.org", "b@example.org"}, Exact: true},
		{ID: "R2", Name: "mailpipe plan 4 #2", Sequence: 3, Enabled: true, Senders: []string{"c@example.org"}},
		{ID: "U1", Name: "Family", Sequence: 1, Senders: []string{"mum@example.org"}},
		{ID: "R3", Name: "mailpipe plan 4 #3", Sequence: 4, Enabled: true, Senders: []string{"d@example.org"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestCreateUpdateAndDeleteWriteOnlyMailpipesShape(t *testing.T) {
	g := &fakeGraph{}
	r := rules.NewGraphRules(g)
	rule := rules.Rule{ID: "R1", Name: "mailpipe plan 4 #1", Sequence: 5, Senders: []string{"a@example.org"}, Enabled: true, Exact: true}
	ctx := context.Background()
	if err := r.Create(ctx, rule); err != nil {
		t.Fatal(err)
	}
	if err := r.Update(ctx, rule); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, "R1"); err != nil {
		t.Fatal(err)
	}
	want := []call{
		{"POST", "/me/mailFolders/inbox/messageRules", nil, `{"actions":{"delete":true,"stopProcessingRules":true},` +
			`"conditions":{"fromAddresses":[{"emailAddress":{"address":"a@example.org"}}]},"displayName":"mailpipe plan 4 #1","isEnabled":true,"sequence":5}`},
		{"PATCH", "/me/mailFolders/inbox/messageRules/R1", nil, `{"conditions":{"fromAddresses":[{"emailAddress":{"address":"a@example.org"}}]},"isEnabled":true}`},
		{"DELETE", "/me/mailFolders/inbox/messageRules/R1", nil, ""},
	}
	if !reflect.DeepEqual(g.calls, want) {
		t.Fatalf("calls\n%+v\nwant\n%+v", g.calls, want)
	}
	if err := r.Update(ctx, rules.Rule{Name: "x"}); err == nil {
		t.Fatal("an update without an id was sent")
	}
}

func filterURL(sender string) string {
	q := url.Values{"$filter": {"from/emailAddress/address eq '" + strings.ReplaceAll(sender, "'", "''") + "'"},
		"$select": {"parentFolderId"}, "$top": {"1000"}}
	return "/me/messages?" + q.Encode()
}

func TestKeptCountsEachSendersMailOutsideDeletedItemsAcrossTheMailbox(t *testing.T) {
	g := &fakeGraph{
		answers: map[string]string{
			"GET /me/mailFolders/deleteditems": `{"id":"F-DEL"}`,
			"GET https://graph.example/more":   `{"value":[{"parentFolderId":"F-ARCHIVE"},{"parentFolderId":"F-DEL"}]}`,
		},
		batch: map[string]graph.BatchResponse{
			filterURL("news@campaign.example.org"): {Status: 200, Body: json.RawMessage(`{"value":[{"parentFolderId":"F-DEL"},{"parentFolderId":"F-DEL"}]}`)},
			filterURL("o'neil@example.org"): {Status: 200, Body: json.RawMessage(`{"value":[{"parentFolderId":"F-DEL"},{"parentFolderId":"F-IN"}],
				"@odata.nextLink":"https://graph.example/more"}`)},
		},
	}
	got, err := rules.NewGraphRules(g).Kept(context.Background(), []string{"news@campaign.example.org", "o'neil@example.org"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]int{"news@campaign.example.org": 0, "o'neil@example.org": 2}) {
		t.Fatalf("kept %v", got)
	}
}

func TestKeptFailsRatherThanGuessWhenASearchFails(t *testing.T) {
	g := &fakeGraph{answers: map[string]string{"GET /me/mailFolders/deleteditems": `{"id":"F-DEL"}`},
		batch: map[string]graph.BatchResponse{filterURL("a@example.org"): {Status: 400, Body: json.RawMessage(`{"error":{"code":"BadRequest","message":"bad filter"}}`)}}}
	_, err := rules.NewGraphRules(g).Kept(context.Background(), []string{"a@example.org"})
	if err == nil || !strings.Contains(err.Error(), "a@example.org") {
		t.Fatalf("err %v: an unanswered search must stop the assessment, naming the sender", err)
	}
	g = &fakeGraph{doErr: map[string]error{"GET /me/mailFolders/deleteditems": errors.New("offline")}}
	if _, err := rules.NewGraphRules(g).Kept(context.Background(), []string{"a@example.org"}); err == nil {
		t.Fatal("a failed folder lookup was ignored")
	}
}

func TestKeptWithNoSendersAsksNothingAndDeleteNeedsAnID(t *testing.T) {
	g := &fakeGraph{}
	r := rules.NewGraphRules(g)
	if got, err := r.Kept(context.Background(), nil); err != nil || len(got) != 0 || len(g.calls) != 0 {
		t.Fatalf("got %v err %v calls %v", got, err, g.calls)
	}
	if err := r.Delete(context.Background(), " "); err == nil || len(g.calls) != 0 {
		t.Fatalf("a delete without an id was sent: %v %v", err, g.calls)
	}
}

func TestJunkRulesMatchSendersAndMoveToJunk(t *testing.T) {
	g := &fakeGraph{answers: map[string]string{
		"GET /me/mailFolders/junkemail": `{"id":"F-JUNK"}`,
		"GET /me/mailFolders/inbox/messageRules": `{"value":[
			{"id":"J1","displayName":"mailpipe junk #1","sequence":2,"isEnabled":true,
			 "conditions":{"senderContains":["@publisher.example.org","@Charity.example.net"]},"actions":{"moveToFolder":"F-JUNK","stopProcessingRules":true}},
			{"id":"J2","displayName":"mailpipe junk #2","sequence":3,"isEnabled":true,
			 "conditions":{"senderContains":["@x.org"]},"actions":{"moveToFolder":"F-OTHER","stopProcessingRules":true}}]}`,
	}}
	r := rules.NewGraphRules(g)
	ctx := context.Background()
	got, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []rules.Rule{
		{ID: "J1", Name: "mailpipe junk #1", Sequence: 2, Enabled: true, Senders: []string{"@charity.example.net", "@publisher.example.org"}, Junk: true, Exact: true},
		{ID: "J2", Name: "mailpipe junk #2", Sequence: 3, Enabled: true, Senders: []string{"@x.org"}, Junk: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	g.calls = nil
	rule := rules.Rule{ID: "J1", Name: "mailpipe junk #1", Sequence: 4, Senders: []string{"@publisher.example.org"}, Enabled: true, Junk: true, Exact: true}
	if err := r.Create(ctx, rule); err != nil {
		t.Fatal(err)
	}
	if err := r.Update(ctx, rule); err != nil {
		t.Fatal(err)
	}
	wantCalls := []call{
		{"POST", "/me/mailFolders/inbox/messageRules", nil, `{"actions":{"moveToFolder":"F-JUNK","stopProcessingRules":true},` +
			`"conditions":{"senderContains":["@publisher.example.org"]},"displayName":"mailpipe junk #1","isEnabled":true,"sequence":4}`},
		{"PATCH", "/me/mailFolders/inbox/messageRules/J1", nil, `{"conditions":{"senderContains":["@publisher.example.org"]},"isEnabled":true}`},
	}
	if !reflect.DeepEqual(g.calls, wantCalls) {
		t.Fatalf("calls\n%+v\nwant\n%+v (the Junk folder id is looked up once)", g.calls, wantCalls)
	}
}

// orderedGraph models server insertion/removal resequencing, not static replies.
type orderedGraph struct {
	fakeGraph
	rows      []map[string]any
	mutations []string
	after     func(string, []map[string]any)
}

func (g *orderedGraph) Do(ctx context.Context, method, path string, q url.Values, prefer string, body, out any) error {
	if path == "/me/mailFolders/junkemail" {
		return json.Unmarshal([]byte(`{"id":"JUNK"}`), out)
	}
	if method == "GET" {
		b, _ := json.Marshal(map[string]any{"value": g.rows})
		return json.Unmarshal(b, out)
	}
	g.mutations = append(g.mutations, method)
	b, _ := json.Marshal(body)
	var in map[string]any
	_ = json.Unmarshal(b, &in)
	id := strings.TrimPrefix(path, "/me/mailFolders/inbox/messageRules/")
	switch method {
	case "POST":
		seq := in["sequence"].(float64)
		for _, r := range g.rows {
			if r["sequence"].(float64) >= seq {
				r["sequence"] = r["sequence"].(float64) + 1
			}
		}
		in["id"] = "new" + strconv.Itoa(len(g.mutations))
		g.rows = append(g.rows, in)
	case "DELETE":
		for i, r := range g.rows {
			if r["id"] == id {
				seq := r["sequence"].(float64)
				g.rows = append(g.rows[:i], g.rows[i+1:]...)
				for _, r := range g.rows {
					if r["sequence"].(float64) > seq {
						r["sequence"] = r["sequence"].(float64) - 1
					}
				}
				break
			}
		}
	case "PATCH":
		for _, r := range g.rows {
			if r["id"] == id {
				for k, v := range in {
					r[k] = v
				}
			}
		}
	}
	if g.after != nil {
		g.after(method, g.rows)
	}
	return nil
}
func orderedFixture(t *testing.T) *orderedGraph {
	t.Helper()
	g := &orderedGraph{}
	err := json.Unmarshal([]byte(`[
 {"id":"O","displayName":"Owner","sequence":1,"isEnabled":true,"conditions":{},"actions":{"markAsRead":true}},
 {"id":"A","displayName":"mailpipe blocked #1","sequence":2,"isEnabled":true,"conditions":{"fromAddresses":[{"emailAddress":{"address":"old@example.org"}}],"subjectContains":["invented"]},"actions":{"delete":true,"stopProcessingRules":true}},
 {"id":"B","displayName":"mailpipe blocked #2","sequence":3,"isEnabled":true,"conditions":{"fromAddresses":[{"emailAddress":{"address":"b@example.org"}}]},"actions":{"delete":true,"stopProcessingRules":true}},
 {"id":"C","displayName":"mailpipe plan 1 #1","sequence":4,"isEnabled":true,"conditions":{},"actions":{"delete":true}}]`), &g.rows)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func TestReviewedManagedOwnSequenceEffects(t *testing.T) {
	for _, mode := range []string{"replace", "deletes", "mixed", "diff"} {
		t.Run(mode, func(t *testing.T) {
			g := orderedFixture(t)
			s := rules.NewGraphRules(g)
			listed, err := s.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var changes []rules.Change
			switch mode {
			case "replace":
				changes = []rules.Change{{Op: rules.Replace, Rule: rules.Rule{ID: "A", Name: listed[1].Name, Sequence: 2, Senders: []string{"a@example.org"}, Enabled: true, Exact: true}}}
			case "deletes":
				changes = []rules.Change{{Op: rules.Delete, Rule: listed[1]}, {Op: rules.Delete, Rule: listed[2]}, {Op: rules.Delete, Rule: listed[3]}}
			case "diff":
				changes = rules.Diff([]rules.Rule{
					{Name: listed[1].Name, Senders: []string{"a@example.org"}, Enabled: true, Exact: true},
					{Name: listed[2].Name, Senders: []string{"updated@example.org"}, Enabled: true, Exact: true},
					{Name: "mailpipe blocked #3", Senders: []string{"d@example.org"}, Enabled: true, Exact: true},
				}, listed)
			case "mixed":
				changes = []rules.Change{{Op: rules.Delete, Rule: listed[1]}, {Op: rules.Update, Rule: rules.Rule{ID: "B", Name: listed[2].Name, Sequence: 3, Senders: []string{"updated@example.org"}, Enabled: true, Exact: true}}, {Op: rules.Replace, Rule: rules.Rule{ID: "C", Name: listed[3].Name, Sequence: 4, Senders: []string{"c@example.org"}, Enabled: true, Exact: true}}, {Op: rules.Create, Rule: rules.Rule{Name: "mailpipe blocked #3", Sequence: 5, Senders: []string{"d@example.org"}, Enabled: true, Exact: true}}}
			}
			n, err := s.ExecuteReviewed(context.Background(), changes)
			if err != nil || n != len(changes) {
				t.Fatalf("done=%d mutations=%v err=%v", n, g.mutations, err)
			}
			after, err := s.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range after {
				if r.ID == "O" && (r.Name != "Owner" || r.Sequence != 1) {
					t.Fatalf("owner changed %+v", r)
				}
			}
			if mode == "replace" && (len(g.mutations) != 2 || g.mutations[0] != "POST" || g.mutations[1] != "DELETE") {
				t.Fatal(g.mutations)
			}
		})
	}
}
func TestReviewedManagedRejectsExternalEffectsDuringOwnShift(t *testing.T) {
	for _, field := range []string{"ownership", "shape", "order", "owner-shape", "new-rule"} {
		t.Run(field, func(t *testing.T) {
			g := orderedFixture(t)
			s := rules.NewGraphRules(g)
			listed, err := s.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			g.after = func(method string, rows []map[string]any) {
				if method != "POST" {
					return
				}
				switch field {
				case "ownership":
					rows[1]["displayName"] = "Owner changed"
				case "shape":
					rows[1]["extraUnknown"] = true
				case "order":
					rows[1]["sequence"], rows[2]["sequence"] = rows[2]["sequence"], rows[1]["sequence"]
				case "owner-shape":
					rows[0]["actions"] = map[string]any{"markAsRead": false}
				case "new-rule":
					rows[len(rows)-1]["actions"] = map[string]any{"forwardTo": []string{"external@example.org"}}
				}
			}
			_, err = s.ExecuteReviewed(context.Background(), []rules.Change{{Op: rules.Replace, Rule: rules.Rule{ID: "A", Name: listed[1].Name, Sequence: 2, Senders: []string{"a@example.org"}, Enabled: true, Exact: true}}})
			if err == nil {
				t.Fatal("external edit adopted")
			}
			if len(g.mutations) != 1 {
				t.Fatalf("stale delete/replay %v", g.mutations)
			}
		})
	}
}
