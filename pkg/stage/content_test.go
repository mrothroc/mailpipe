package stage_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/mrothroc/mailpipe/pkg/document"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/stage"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type contentProbe struct{ calls int }

func (p *contentProbe) Name() string    { return "probe" }
func (p *contentProbe) NeedsBody() bool { return true }
func (p *contentProbe) Evaluate(context.Context, stage.Message) (stage.Verdict, error) {
	p.calls++
	return stage.Verdict{Labels: []string{"delete"}}, nil
}
func TestIncompleteContentProtectsBeforeEvaluation(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		t.Run(map[bool]string{false: "limit", true: "cleanup"}[cleanup], func(t *testing.T) {
			p := &contentProbe{}
			var got stage.Result
			e := document.ErrIncomplete
			if cleanup {
				e = errors.Join(e, document.ErrCleanup)
			}
			_, err := stage.Run(context.Background(), p, []stage.Message{{N: 1}}, func(context.Context, *stage.Message) error { return e }, func(r stage.Result) error { got = r; return nil }, 1)
			if p.calls != 0 || got.Verdict.Action != stage.Keep || len(got.Verdict.Labels) != 1 || got.Verdict.Labels[0] != "review" {
				t.Fatalf("calls=%d result=%+v", p.calls, got)
			}
			if cleanup && !errors.Is(err, stage.ErrFatal) {
				t.Fatalf("cleanup not surfaced: %v", err)
			}
		})
	}
}

type mimeSource struct {
	raw   string
	calls int
}

func (s *mimeSource) Account() mailbox.Account {
	return mailbox.Account{ID: "fixture", ProviderID: "imap"}
}
func (s *mimeSource) WriteMIME(ctx context.Context, _ mailbox.MessageRef, w io.Writer) error {
	s.calls++
	_, e := io.WriteString(w, s.raw)
	return e
}
func TestMIMEFailuresMakeZeroModelCalls(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct{ State string }
		if e := json.NewDecoder(r.Body).Decode(&request); e != nil {
			t.Error(e)
		}
		if !strings.Contains(request.State, "2030-12-31") {
			t.Error("trailing date omitted")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"check": map[string]any{"noul": 0.8}}, "usage": map[string]any{"cost": 0.001}})
	}))
	defer server.Close()
	client := stage.NewJevClient("invented-fixture-token", "fixture-model")
	client.URL = server.URL
	classifier := stage.NewJev("check", client, []stage.Question{{Key: "check", Instructions: "Check complete message", True: "yes", False: "no"}}, 0.7, stage.NewBudget(1))
	for _, test := range []struct {
		name, raw string
		mutate    func(*document.Limits)
		review    bool
	}{
		{"complete", "From: a@example.org\r\nContent-Type: text/html\r\n\r\n<p>Complete deadline 2030-12-31</p>", nil, false},
		{"raw", "From: a@example.org\r\n\r\n" + strings.Repeat("x", 512), func(l *document.Limits) { l.RawBytes = 128 }, true},
		{"decode", "From: a@example.org\r\nContent-Transfer-Encoding: base64\r\n\r\nYWJjZA", nil, true},
		{"quota", "From: a@example.org\r\n\r\ncomplete", func(l *document.Limits) { l.DiskBytes = 16 }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := calls.Load()
			source := &mimeSource{raw: test.raw}
			limits := document.DefaultLimits()
			if test.mutate != nil {
				test.mutate(&limits)
			}
			scope, e := document.NewScope(limits)
			if e != nil {
				t.Fatal(e)
			}
			ref := mailbox.MessageRef{AccountID: "fixture", MessageID: "one", Locator: "one"}
			var result stage.Result
			_, e = stage.Run(context.Background(), classifier, []stage.Message{{N: 1, Sender: "a@example.org"}}, func(ctx context.Context, m *stage.Message) error {
				body, e := document.ReadText(ctx, source, ref, scope)
				m.Body = body
				return e
			}, func(r stage.Result) error { result = r; return nil }, 1)
			if e != nil {
				t.Fatal(e)
			}
			if test.review {
				if calls.Load() != before || result.Verdict.Action != stage.Keep || strings.Join(result.Verdict.Labels, ",") != "review" {
					t.Fatalf("partial decision calls=%d result=%+v", calls.Load()-before, result)
				}
			} else if calls.Load() != before+1 {
				t.Fatal("model observer positive control failed")
			}
			if usage := scope.Usage(); usage.Disk != 0 || usage.Files != 0 || usage.Memory != 0 {
				t.Fatal("resources leaked", usage)
			}
			if e = scope.Close(); e != nil {
				t.Fatal(e)
			}
		})
	}
}
