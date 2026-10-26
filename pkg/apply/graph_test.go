package apply_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/apply"
	"github.com/mrothroc/mailpipe/pkg/graph"
)

type fakeGraph struct {
	sent    [][]graph.BatchRequest
	answer  func(graph.BatchRequest) graph.BatchResponse
	getPath string
}

func (g *fakeGraph) Batch(_ context.Context, reqs []graph.BatchRequest) ([]graph.BatchResponse, error) {
	g.sent = append(g.sent, reqs)
	out := make([]graph.BatchResponse, len(reqs))
	for i, r := range reqs {
		out[i] = g.answer(r)
		out[i].ID = r.ID
	}
	return out, nil
}

func (g *fakeGraph) Get(_ context.Context, path string, q url.Values, out any) error {
	g.getPath = path + "?" + q.Encode()
	return json.Unmarshal([]byte(`{"id":"F-IN"}`), out)
}

func TestGraphMailboxLookupReadsIdentityAndFolder(t *testing.T) {
	g := &fakeGraph{answer: func(r graph.BatchRequest) graph.BatchResponse {
		switch {
		case strings.HasPrefix(r.URL, "/me/messages/a%2Fb?"):
			return graph.BatchResponse{Status: 200, Body: json.RawMessage(`{"internetMessageId":"<a@example.org>",
				"from":{"emailAddress":{"address":"news@example.org"}},"sender":{"emailAddress":{"address":"relay@example.net"}},"parentFolderId":"F-IN"}`)}
		case strings.HasPrefix(r.URL, "/me/messages/nofrom?"):
			return graph.BatchResponse{Status: 200, Body: json.RawMessage(`{"internetMessageId":"<c@example.org>",
				"sender":{"emailAddress":{"address":"relay@example.net"}},"parentFolderId":"F-IN"}`)}
		case strings.HasPrefix(r.URL, "/me/messages/gone?"):
			return graph.BatchResponse{Status: 404, Body: json.RawMessage(`{"error":{"code":"ErrorItemNotFound","message":"not found"}}`)}
		}
		return graph.BatchResponse{Status: 503}
	}}
	box := apply.NewGraphMailbox(g)
	got, err := box.Lookup(context.Background(), []string{"a/b", "nofrom", "gone", "busy"})
	if err != nil || len(got) != 4 {
		t.Fatalf("got %+v err %v", got, err)
	}
	if got[0].Remote != (apply.Remote{InternetMessageID: "<a@example.org>", Sender: "news@example.org", ParentFolderID: "F-IN"}) {
		t.Fatalf("lookup %+v: the sender is From, as in the mirror", got[0])
	}
	if got[1].Remote.Sender != "relay@example.net" {
		t.Fatalf("lookup %+v: without From the sender falls back to Sender", got[1])
	}
	if !got[2].Missing || got[2].Err != nil {
		t.Fatalf("404 = %+v, want missing", got[2])
	}
	if got[3].Err == nil || !strings.Contains(got[3].Err.Error(), "503") {
		t.Fatalf("503 = %+v, want an error", got[3])
	}
	q, _ := url.Parse(g.sent[0][0].URL)
	if g.sent[0][0].Method != "GET" || q.Query().Get("$select") != "id,internetMessageId,from,sender,parentFolderId" {
		t.Fatalf("request %+v", g.sent[0][0])
	}
}

func TestGraphMailboxMoveAndFolderID(t *testing.T) {
	g := &fakeGraph{answer: func(r graph.BatchRequest) graph.BatchResponse {
		if r.URL == "/me/messages/bad/move" {
			return graph.BatchResponse{Status: 403, Body: json.RawMessage(`{"error":{"code":"ErrorAccessDenied","message":"denied"}}`)}
		}
		return graph.BatchResponse{Status: 201}
	}}
	box := apply.NewGraphMailbox(g)
	errs, err := box.Move(context.Background(), []string{"ok", "bad"}, "deleteditems")
	if err != nil || len(errs) != 2 || errs[0] != nil || errs[1] == nil || !strings.Contains(errs[1].Error(), "ErrorAccessDenied") {
		t.Fatalf("errs %v err %v", errs, err)
	}
	var apiErr *graph.APIError
	if !errors.As(errs[1], &apiErr) || apiErr.Status != 403 {
		t.Fatalf("move error %#v is not a Graph API error", errs[1])
	}
	r := g.sent[0][0]
	if r.Method != "POST" || r.URL != "/me/messages/ok/move" {
		t.Fatalf("request %+v", r)
	}
	if b, _ := json.Marshal(r.Body); string(b) != `{"destinationId":"deleteditems"}` {
		t.Fatalf("body %s", b)
	}
	id, err := box.FolderID(context.Background(), "inbox")
	if err != nil || id != "F-IN" || g.getPath != "/me/mailFolders/inbox?%24select=id" {
		t.Fatalf("id %q path %q err %v", id, g.getPath, err)
	}
}

func TestGraphMailboxSplitsLargeCallsIntoBatches(t *testing.T) {
	g := &fakeGraph{answer: func(graph.BatchRequest) graph.BatchResponse { return graph.BatchResponse{Status: 201} }}
	ids := make([]string, graph.MaxBatch+3)
	for i := range ids {
		ids[i] = "m" + string(rune('a'+i))
	}
	errs, err := apply.NewGraphMailbox(g).Move(context.Background(), ids, "deleteditems")
	if err != nil || len(errs) != len(ids) || len(g.sent) != 2 || len(g.sent[0]) != graph.MaxBatch {
		t.Fatalf("errs %d batches %d err %v", len(errs), len(g.sent), err)
	}
}
