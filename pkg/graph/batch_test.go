package graph_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/graph"
)

type batchBody struct {
	Requests []struct {
		ID      string            `json:"id"`
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"requests"`
}

func TestBatchSendsSubRequestsWithImmutableIDsAndReturnsThemInOrder(t *testing.T) {
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/$batch" {
			t.Errorf("request %s %s", r.Method, r.URL)
		}
		var in batchBody
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Requests) != 2 {
			t.Fatalf("body %+v err %v", in, err)
		}
		for _, q := range in.Requests {
			if q.Headers["Prefer"] != `IdType="ImmutableId"` {
				t.Errorf("sub-request %s prefer %q", q.ID, q.Headers["Prefer"])
			}
		}
		move := in.Requests[1]
		if move.Method != "POST" || move.URL != "/me/messages/b/move" || move.Headers["Content-Type"] != "application/json" ||
			string(move.Body) != `{"destinationId":"deleteditems"}` {
			t.Errorf("move sub-request %+v body %s", move, move.Body)
		}
		// Graph may answer out of order.
		_, _ = w.Write([]byte(`{"responses":[{"id":"2","status":201,"body":{"id":"b"}},{"id":"1","status":404,"body":{"error":{"code":"ErrorItemNotFound"}}}]}`))
	})
	got, err := c.Batch(context.Background(), []graph.BatchRequest{
		{ID: "1", Method: "GET", URL: "/me/messages/a"},
		{ID: "2", Method: "POST", URL: "/me/messages/b/move", Body: map[string]string{"destinationId": "deleteditems"}},
	})
	if err != nil || len(got) != 2 || got[0].ID != "1" || got[0].Status != 404 || got[1].ID != "2" || got[1].Status != 201 {
		t.Fatalf("got %+v err %v", got, err)
	}
	if !strings.Contains(string(got[1].Body), `"b"`) {
		t.Fatalf("body %s", got[1].Body)
	}
}

// The header name is matched case-insensitively, as HTTP header names are.
func TestBatchRetriesOnlyThrottledSubRequestsHonouringRetryAfter(t *testing.T) {
	var calls atomic.Int32
	var second []string
	c, slept := client(t, func(w http.ResponseWriter, r *http.Request) {
		var in batchBody
		_ = json.NewDecoder(r.Body).Decode(&in)
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"responses":[{"id":"1","status":200,"body":{}},{"id":"2","status":429,"headers":{"retry-after":"5"}},{"id":"3","status":503}]}`))
			return
		}
		for _, q := range in.Requests {
			second = append(second, q.ID)
		}
		_, _ = w.Write([]byte(`{"responses":[{"id":"3","status":200,"body":{}},{"id":"2","status":200,"body":{}}]}`))
	})
	got, err := c.Batch(context.Background(), []graph.BatchRequest{{ID: "1", Method: "GET", URL: "/x"}, {ID: "2", Method: "GET", URL: "/y"}, {ID: "3", Method: "GET", URL: "/z"}})
	if err != nil || len(got) != 3 || got[0].Status != 200 || got[1].Status != 200 || got[2].Status != 200 {
		t.Fatalf("got %+v err %v", got, err)
	}
	if strings.Join(second, ",") != "2,3" {
		t.Fatalf("second round re-sent %v; only the throttled ones should be", second)
	}
	if len(*slept) != 1 || (*slept)[0] != 5*time.Second {
		t.Fatalf("slept %v, want the Retry-After of 5s", *slept)
	}
}

func TestBatchGivesUpReturningTheLastThrottledResponse(t *testing.T) {
	var calls atomic.Int32
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"responses":[{"id":"1","status":429}]}`))
	})
	got, err := c.Batch(context.Background(), []graph.BatchRequest{{ID: "1", Method: "GET", URL: "/x"}})
	if err != nil || len(got) != 1 || got[0].Status != 429 {
		t.Fatalf("got %+v err %v", got, err)
	}
	if int(calls.Load()) != graph.MaxTries {
		t.Fatalf("calls = %d, want %d", calls.Load(), graph.MaxTries)
	}
}

func TestBatchRejectsMissingResponsesAndOversizedOrDuplicateBatches(t *testing.T) {
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"responses":[{"id":"1","status":200}]}`))
	})
	if _, err := c.Batch(context.Background(), []graph.BatchRequest{{ID: "1", URL: "/x"}, {ID: "2", URL: "/y"}}); err == nil ||
		!strings.Contains(err.Error(), `"2"`) {
		t.Fatalf("missing response not reported: %v", err)
	}
	big := make([]graph.BatchRequest, graph.MaxBatch+1)
	for i := range big {
		big[i] = graph.BatchRequest{ID: string(rune('a' + i)), URL: "/x"}
	}
	if _, err := c.Batch(context.Background(), big); err == nil {
		t.Fatal("oversized batch accepted")
	}
	if _, err := c.Batch(context.Background(), []graph.BatchRequest{{ID: "1", URL: "/x"}, {ID: "1", URL: "/y"}}); err == nil {
		t.Fatal("duplicate ids accepted")
	}
}
