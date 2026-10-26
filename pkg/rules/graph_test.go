package rules_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
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
