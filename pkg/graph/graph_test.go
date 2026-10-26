package graph_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/graph"
)

func client(t *testing.T, h http.HandlerFunc) (*graph.Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var slept []time.Duration
	c := graph.New(func(context.Context) (string, error) { return "TOKEN", nil })
	c.Base = srv.URL
	c.Sleep = func(d time.Duration) { slept = append(slept, d) }
	return c, &slept
}

func TestGetSendsAuthImmutableIDsAndTextBodies(t *testing.T) {
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/mailFolders/inbox" || r.URL.Query().Get("$select") != "totalItemCount" {
			t.Errorf("request %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer TOKEN" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		p := r.Header.Get("Prefer")
		if !strings.Contains(p, `IdType="ImmutableId"`) || !strings.Contains(p, `outlook.body-content-type="text"`) {
			t.Errorf("prefer %q", p)
		}
		_, _ = w.Write([]byte(`{"totalItemCount": 42}`))
	})
	n, err := c.FolderTotal(context.Background(), "inbox")
	if err != nil || n != 42 {
		t.Fatalf("FolderTotal = %d, %v", n, err)
	}
}

func TestThrottlingHonoursRetryAfterThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	c, slept := client(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			_, _ = w.Write([]byte(`{"ok": true}`))
		}
	})
	var out map[string]bool
	if err := c.Get(context.Background(), "/me", nil, &out); err != nil || !out["ok"] {
		t.Fatalf("Get = %v %v", out, err)
	}
	if len(*slept) != 2 || (*slept)[0] != 7*time.Second {
		t.Fatalf("slept %v", *slept)
	}
}

func TestErrorsCarryStatusCodeAndMessage(t *testing.T) {
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"ErrorAccessDenied","message":"Access is denied."}}`))
	})
	err := c.Get(context.Background(), "/me/mailFolders/inbox", nil, nil)
	var apiErr *graph.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 403 || apiErr.Code != "ErrorAccessDenied" {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(err.Error(), "Access is denied.") {
		t.Fatalf("message %q", err.Error())
	}
}

func TestRetriesGiveUp(t *testing.T) {
	var calls atomic.Int32
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if err := c.Get(context.Background(), "/me", nil, nil); err == nil {
		t.Fatal("no error after retries")
	}
	if int(calls.Load()) != graph.MaxTries {
		t.Fatalf("calls = %d, want %d", calls.Load(), graph.MaxTries)
	}
}

func TestPagesFollowsNextLinksAndExposesDeltaLink(t *testing.T) {
	var base string
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Prefer"), "odata.maxpagesize=") {
			t.Errorf("paging without maxpagesize")
		}
		if r.URL.Query().Get("page") == "2" {
			if r.URL.Query().Get("$select") != "" {
				t.Errorf("query re-sent on a next link")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": "c"}}, "@odata.deltaLink": base + "/delta?token=abc"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}, "@odata.nextLink": base + "/next?page=2"})
	})
	base = c.Base
	var ids []string
	var delta string
	err := c.Pages(context.Background(), "/me/mailFolders/inbox/messages/delta", url.Values{"$select": {"id"}}, func(p graph.Page) error {
		for _, raw := range p.Value {
			var m struct{ ID string }
			_ = json.Unmarshal(raw, &m)
			ids = append(ids, m.ID)
		}
		if p.DeltaLink != "" {
			delta = p.DeltaLink
		}
		return nil
	})
	if err != nil || strings.Join(ids, ",") != "a,b,c" || !strings.HasSuffix(delta, "token=abc") {
		t.Fatalf("ids %v delta %q err %v", ids, delta, err)
	}
}

func TestTokenFailureStopsBeforeAnyRequest(t *testing.T) {
	c := graph.New(func(context.Context) (string, error) { return "", errors.New("not signed in") })
	c.Base = "http://127.0.0.1:1"
	if err := c.Get(context.Background(), "/me", nil, nil); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("err = %v", err)
	}
}

// Raw returns a response body as bytes (a message's MIME source), with the
// same auth, retries and errors as Do.
func TestRawReturnsTheBodyBytes(t *testing.T) {
	var calls atomic.Int32
	mime := "Message-ID: <m1@example.org>\r\nSubject: Receipt\r\n\r\nThanks\r\n"
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != "/me/messages/ID1/$value" || r.Header.Get("Authorization") != "Bearer TOKEN" {
			t.Errorf("request %s auth %q", r.URL, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "message/rfc822")
		_, _ = w.Write([]byte(mime))
	})
	got, err := c.Raw(context.Background(), "/me/messages/ID1/$value")
	if err != nil || string(got) != mime || calls.Load() != 2 {
		t.Fatalf("Raw = %q, %v after %d calls", got, err, calls.Load())
	}
	c, _ = client(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"ErrorItemNotFound","message":"gone"}}`))
	})
	var apiErr *graph.APIError
	if _, err := c.Raw(context.Background(), "/me/messages/X/$value"); !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("err %v", err)
	}
}

// A cancelled context stops retrying at once instead of sleeping through
// every backoff.
func TestCancellationStopsRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	c, slept := client(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		cancel()
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if _, err := c.Raw(ctx, "/me/messages/X/$value"); !errors.Is(err, context.Canceled) || calls.Load() != 1 || len(*slept) > 1 {
		t.Fatalf("err %v after %d calls and %d sleeps", err, calls.Load(), len(*slept))
	}
	if _, err := c.Batch(ctx, []graph.BatchRequest{{ID: "1", Method: "GET", URL: "/me"}}); !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("batch err %v after %d calls", err, calls.Load())
	}
}

// The last failed attempt returns at once: there is nothing left to wait for.
func TestNoSleepAfterTheLastAttempt(t *testing.T) {
	c, slept := client(t, func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close() // a network failure on every attempt
	})
	if _, err := c.Raw(context.Background(), "/me"); err == nil {
		t.Fatal("no error")
	}
	if len(*slept) != graph.MaxTries-1 {
		t.Fatalf("slept %d times for %d attempts", len(*slept), graph.MaxTries)
	}
}
