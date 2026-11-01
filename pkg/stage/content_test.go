package stage_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mrothroc/mailpipe/pkg/document"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/stage"
	"github.com/mrothroc/mailpipe/pkg/store"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
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

// drainStage's default evaluator is a canary: incomplete body content must
// never reach evaluation, including when a different worker stops the run.
type drainStage struct {
	body bool
	eval func(context.Context, stage.Message) (stage.Verdict, error)
}

func (s drainStage) Name() string    { return "drain-check" }
func (s drainStage) NeedsBody() bool { return s.body }
func (s drainStage) Evaluate(ctx context.Context, m stage.Message) (stage.Verdict, error) {
	if s.eval == nil {
		panic("incomplete content reached evaluator")
	}
	return s.eval(ctx, m)
}

func drainMirror(t *testing.T) (*store.AccountStore, []mailbox.LocalRef) {
	t.Helper()
	ctx := context.Background()
	account := mailbox.Account{ID: "fixture", ProviderID: "fixture"}
	db, e := store.OpenAccount(ctx, filepath.Join(t.TempDir(), "mirror.db"), account)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := db.Close(); e != nil {
			t.Error(e)
		}
	})
	collection := mailbox.Collection{Ref: mailbox.CollectionRef{AccountID: account.ID, CollectionID: "inbox"}, Name: "Inbox"}
	var refs []mailbox.LocalRef
	for i := 1; i <= 2; i++ {
		id := fmt.Sprintf("message-%d", i)
		ref, e := db.UpsertMessage(ctx, collection, store.MessageRecord{Message: mailbox.Message{Ref: mailbox.MessageRef{AccountID: account.ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator(id)}, Metadata: mailbox.Metadata{InternetMessageID: fmt.Sprintf("<message-%d@example.org>", i), Sender: mailbox.Address{Address: "sender@example.org"}, Subject: "Invented message"}}})
		if e != nil {
			t.Fatal(e)
		}
		if e = db.AddVerdict(ctx, ref, store.Verdict{Stage: "drain-check", Labels: []string{"remove"}, Action: stage.Delete, Reason: "prior complete verdict"}); e != nil {
			t.Fatal(e)
		}
		refs = append(refs, ref)
	}
	return db, refs
}

func TestRunDrainProtectiveKeepsReplaceOldVerdicts(t *testing.T) {
	for _, test := range []struct {
		name                                 string
		firstBefore, firstAfter, secondAfter bool
	}{
		{name: "both persisted"},
		{name: "first write acknowledgment failed", firstAfter: true},
		{name: "both write acknowledgments failed", firstAfter: true, secondAfter: true},
		{name: "first write refused", firstBefore: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, refs := drainMirror(t)
			ctx := context.Background()
			ready, firstSaved := make(chan struct{}), make(chan struct{})
			var entered atomic.Int32
			msgs := []stage.Message{{N: refs[0].N}, {N: refs[1].N}}
			firstErr, secondErr := errors.New("first sink failure"), errors.New("second sink failure")
			body := func(ctx context.Context, m *stage.Message) error {
				if entered.Add(1) == 2 {
					close(ready)
				}
				<-ready
				if m.N == refs[1].N {
					<-firstSaved
					<-ctx.Done()
				}
				return errors.Join(document.ErrIncomplete, document.ErrCleanup)
			}
			var attempts []int64
			sum, runErr := stage.Run(ctx, drainStage{body: true}, msgs, body, func(r stage.Result) error {
				attempts = append(attempts, r.Message.N)
				if r.Message.N == refs[0].N {
					defer close(firstSaved)
				}
				if !r.Protected || r.Verdict.Action != stage.Keep || !errors.Is(r.Err, document.ErrCleanup) {
					t.Errorf("not protective: %+v", r)
				}
				if r.Message.N == refs[0].N && test.firstBefore {
					return firstErr
				}
				// The real sink stores protection as successful policy evidence, just as
				// the CLI does; the run error remains separate from the stored Error.
				ref := mailbox.LocalRef{AccountID: refs[0].AccountID, N: r.Message.N}
				if e := db.AddVerdict(ctx, ref, store.Verdict{Stage: r.Verdict.Stage, Labels: r.Verdict.Labels, Action: r.Verdict.Action, Reason: r.Verdict.Reason, Provenance: r.Verdict.Provenance, CostUSD: r.Verdict.CostUSD}); e != nil {
					return e
				}
				if r.Message.N == refs[0].N && test.firstAfter {
					return firstErr
				}
				if r.Message.N == refs[1].N && test.secondAfter {
					return secondErr
				}
				return nil
			}, 2)
			if !slices.Equal(attempts, []int64{refs[0].N, refs[1].N}) {
				t.Fatalf("lost or duplicate sink effects: %v", attempts)
			}
			if entered.Load() != 2 || sum.Evaluated != 0 || sum.Failed != 2 || sum.CostUSD != 0 {
				t.Fatalf("completed failures=%d summary=%+v", entered.Load(), sum)
			}
			if !errors.Is(runErr, stage.ErrFatal) || !errors.Is(runErr, document.ErrCleanup) {
				t.Fatalf("fatal cleanup lost: %v", runErr)
			}
			if (test.firstBefore || test.firstAfter) && !errors.Is(runErr, firstErr) {
				t.Fatalf("first sink error lost: %v", runErr)
			}
			if test.secondAfter && !errors.Is(runErr, secondErr) {
				t.Fatalf("second sink error lost: %v", runErr)
			}
			latest, e := db.LatestVerdicts(ctx, store.AccountFilter{})
			if e != nil {
				t.Fatal(e)
			}
			for i, ref := range refs {
				history, e := db.Verdicts(ctx, ref)
				if e != nil {
					t.Fatal(e)
				}
				wantCount, wantAction := 2, stage.Keep
				if i == 0 && test.firstBefore {
					wantCount = 1
					wantAction = stage.Delete
				}
				if len(history) != wantCount {
					t.Fatalf("identity %d duplicate/lost writes: %d", ref.N, len(history))
				}
				verdicts := latest[ref]
				if len(verdicts) != 1 || verdicts[0].Action != wantAction || verdicts[0].Error != "" {
					t.Fatalf("identity %d latest policy evidence %+v", ref.N, verdicts)
				}
				if wantAction == stage.Keep && !slices.Equal(verdicts[0].Labels, []string{"review"}) {
					t.Fatalf("missing review: %+v", verdicts[0])
				}
			}
		})
	}
}

func TestRunDrainDiscardsNormalResultsAndCountsReportedCosts(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelParent), func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready, firstSaved := make(chan struct{}), make(chan struct{})
			var entered atomic.Int32
			s := drainStage{eval: func(ctx context.Context, m stage.Message) (stage.Verdict, error) {
				if entered.Add(1) == 3 {
					close(ready)
				}
				<-ready
				if m.N != 1 {
					<-firstSaved
					<-ctx.Done()
				}
				if m.N == 3 {
					return stage.Verdict{Labels: []string{"ordinary"}, CostUSD: .125}, nil
				}
				cost := .25
				if m.N == 2 {
					cost = .5
				}
				return stage.Verdict{CostUSD: cost}, errors.Join(document.ErrIncomplete, document.ErrCleanup)
			}}
			var persisted []int64
			sum, e := stage.Run(parent, s, []stage.Message{{N: 1}, {N: 2}, {N: 3}}, nil, func(r stage.Result) error {
				persisted = append(persisted, r.Message.N)
				if !r.Protected {
					t.Errorf("accepted abandoned normal result %+v", r)
				}
				if r.Message.N == 1 {
					if cancelParent {
						cancel()
					}
					close(firstSaved)
				}
				return nil
			}, 3)
			if !slices.Equal(persisted, []int64{1, 2}) || sum.Evaluated != 0 || sum.Failed != 2 || sum.CostUSD != .875 {
				t.Fatalf("persisted=%v summary=%+v", persisted, sum)
			}
			if !errors.Is(e, stage.ErrFatal) || !errors.Is(e, document.ErrCleanup) || (cancelParent && !errors.Is(e, context.Canceled)) {
				t.Fatalf("failure/cancellation lost: %v", e)
			}
		})
	}
}

func TestRunDrainRetainsOriginalFatalAndAllSinkErrors(t *testing.T) {
	ready, firstSaved := make(chan struct{}), make(chan struct{})
	var entered atomic.Int32
	original, firstSink, secondSink := errors.New("original fatal"), errors.New("first sink"), errors.New("second sink")
	s := drainStage{body: true, eval: func(_ context.Context, m stage.Message) (stage.Verdict, error) {
		if m.N != 1 {
			panic("incomplete content reached evaluator")
		}
		return stage.Verdict{CostUSD: .25}, errors.Join(stage.ErrFatal, original)
	}}
	body := func(ctx context.Context, m *stage.Message) error {
		if entered.Add(1) == 2 {
			close(ready)
		}
		<-ready
		if m.N == 2 {
			<-firstSaved
			<-ctx.Done()
			return errors.Join(document.ErrIncomplete, document.ErrCleanup)
		}
		return nil
	}
	var attempts []int64
	sum, e := stage.Run(context.Background(), s, []stage.Message{{N: 1}, {N: 2}}, body, func(r stage.Result) error {
		attempts = append(attempts, r.Message.N)
		if r.Message.N == 1 {
			close(firstSaved)
			return firstSink
		}
		return secondSink
	}, 2)
	if !slices.Equal(attempts, []int64{1, 2}) || sum.Failed != 2 || sum.Evaluated != 0 || sum.CostUSD != .25 {
		t.Fatalf("attempts=%v summary=%+v", attempts, sum)
	}
	for _, cause := range []error{original, stage.ErrFatal, firstSink, secondSink, document.ErrCleanup} {
		if !errors.Is(e, cause) {
			t.Errorf("lost %v in %v", cause, e)
		}
	}
}

func TestRunDoesNotEvaluateBodyAbandonedAfterFatalStop(t *testing.T) {
	secondStarted := make(chan struct{})
	var evaluated atomic.Int32
	s := drainStage{body: true, eval: func(context.Context, stage.Message) (stage.Verdict, error) {
		evaluated.Add(1)
		return stage.Verdict{Labels: []string{"ordinary"}}, nil
	}}
	body := func(ctx context.Context, m *stage.Message) error {
		if m.N == 1 {
			<-secondStarted
			return errors.Join(document.ErrIncomplete, document.ErrCleanup)
		}
		close(secondStarted)
		<-ctx.Done()
		return nil
	}
	var persisted []int64
	sum, e := stage.Run(context.Background(), s, []stage.Message{{N: 1}, {N: 2}}, body, func(r stage.Result) error { persisted = append(persisted, r.Message.N); return nil }, 2)
	if evaluated.Load() != 0 || !slices.Equal(persisted, []int64{1}) || sum.Failed != 1 || sum.Evaluated != 0 || !errors.Is(e, stage.ErrFatal) {
		t.Fatalf("new evaluation after stop=%d persisted=%v summary=%+v error=%v", evaluated.Load(), persisted, sum, e)
	}
}
