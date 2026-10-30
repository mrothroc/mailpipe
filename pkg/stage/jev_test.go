package stage_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/policy"
	"github.com/mrothroc/mailpipe/pkg/stage"
	"github.com/mrothroc/mailpipe/pkg/store"
)

var question = stage.Question{Key: "political_appeal", Instructions: "Is this a political appeal?",
	True: "asks to donate or act", False: "anything else"}

func jevServer(t *testing.T, h http.HandlerFunc) *stage.JevClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := stage.NewJevClient("sk-or-secret-key", "typesafe/jev-1.14")
	c.URL = srv.URL
	c.Sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func answer(noul any, cost float64) map[string]any {
	return map[string]any{"answers": map[string]any{"political_appeal": map[string]any{"noul": noul}}, "usage": map[string]any{"cost": cost}}
}

func TestJevSendsTheDecisionRequestAndReadsTheScore(t *testing.T) {
	var got map[string]any
	c := jevServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-or-secret-key" || r.Method != http.MethodPost {
			t.Errorf("auth %q method %s", r.Header.Get("Authorization"), r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(answer(0.97, 0.0002))
	})
	budget := stage.NewBudget(1.0)
	j := stage.NewJev("political", c, []stage.Question{question}, 0.5, budget)
	m := msg()
	m.Body = "Please donate"
	v, err := j.Evaluate(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if v.Scores["political_appeal"] != 0.97 || len(v.Labels) != 1 || v.Labels[0] != "political_appeal" || v.CostUSD != 0.0002 || v.Provenance != "jev:typesafe/jev-1.14" {
		t.Fatalf("verdict %+v", v)
	}
	if !j.NeedsBody() || budget.Spent() != 0.0002 {
		t.Fatalf("needsBody %v spent %v", j.NeedsBody(), budget.Spent())
	}
	q := got["questions"].(map[string]any)["political_appeal"].(map[string]any)
	state := got["state"].(string)
	if got["model"] != "typesafe/jev-1.14" || q["type"] != "noul" || q["instructions"] != "Is this a political appeal?" ||
		q["criteria"].(map[string]any)["true"] != "asks to donate or act" ||
		!strings.Contains(state, "From: news@campaign.example.org") || !strings.Contains(state, "Subject: Chip in today") || !strings.Contains(state, "Please donate") {
		t.Fatalf("request %v", got)
	}
}

func TestJevBelowThresholdHasScoreButNoLabel(t *testing.T) {
	c := jevServer(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(answer(0.12, 0.0001)) })
	v, err := stage.NewJev("political", c, []stage.Question{question}, 0.5, stage.NewBudget(1)).Evaluate(context.Background(), msg())
	if err != nil || len(v.Labels) != 0 || v.Scores["political_appeal"] != 0.12 {
		t.Fatalf("verdict %+v err %v", v, err)
	}
}

func TestJevRetriesTransientFailures(t *testing.T) {
	var calls atomic.Int32
	c := jevServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(answer(0.5, 0.0001))
	})
	if _, err := stage.NewJev("political", c, []stage.Question{question}, 0.5, stage.NewBudget(1)).Evaluate(context.Background(), msg()); err != nil || calls.Load() != 3 {
		t.Fatalf("err %v calls %d", err, calls.Load())
	}
}

func TestJevAuthAndBillingFailuresStopTheRun(t *testing.T) {
	for _, status := range []int{401, 402, 403} {
		c := jevServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"key sk-or-secret-key rejected"}}`))
		})
		_, err := stage.NewJev("political", c, []stage.Question{question}, 0.5, stage.NewBudget(1)).Evaluate(context.Background(), msg())
		if !errors.Is(err, stage.ErrFatal) || strings.Contains(err.Error(), "sk-or-secret-key") {
			t.Errorf("status %d: err %v (must be fatal and never echo the key)", status, err)
		}
	}
}

func TestJevMalformedAnswerIsAMessageFailureNotFatal(t *testing.T) {
	for _, noul := range []any{"high", 1.5, nil, true} {
		c := jevServer(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(answer(noul, 0.0001)) })
		budget := stage.NewBudget(1)
		_, err := stage.NewJev("political", c, []stage.Question{question}, 0.5, budget).Evaluate(context.Background(), msg())
		if err == nil || errors.Is(err, stage.ErrFatal) || budget.Spent() != 0.0001 {
			t.Errorf("noul %v: err %v spent %v (cost of a malformed answer is still charged)", noul, err, budget.Spent())
		}
	}
}

func TestJevStopsAtTheBudget(t *testing.T) {
	var calls atomic.Int32
	c := jevServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(answer(0.5, 0.6))
	})
	j := stage.NewJev("political", c, []stage.Question{question}, 0.5, stage.NewBudget(1.0))
	for i := 0; i < 2; i++ {
		if _, err := j.Evaluate(context.Background(), msg()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	_, err := j.Evaluate(context.Background(), msg())
	if !errors.Is(err, stage.ErrFatal) || !errors.Is(err, stage.ErrBudget) || calls.Load() != 2 {
		t.Fatalf("err %v calls %d: the third call must not be made once $1.20 of $1.00 is spent", err, calls.Load())
	}
}

var category = []stage.Question{
	{Key: "promotion", Instructions: "Is this a promotion?", True: "a sale or offer", False: "anything else"},
	{Key: "newsletter", Instructions: "Is this a newsletter?", True: "a recurring digest", False: "anything else"},
}

func TestJevAsksEveryQuestionInOneCall(t *testing.T) {
	var calls atomic.Int32
	var asked []string
	c := jevServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var got struct {
			Questions map[string]any `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		for k := range got.Questions {
			asked = append(asked, k)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{
			"promotion": map[string]any{"noul": 0.91}, "newsletter": map[string]any{"noul": 0.12}}, "usage": map[string]any{"cost": 0.0003}})
	})
	v, err := stage.NewJev("category", c, category, 0.5, stage.NewBudget(1)).Evaluate(context.Background(), msg())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(asked) != 2 {
		t.Fatalf("calls %d, asked %v", calls.Load(), asked)
	}
	if strings.Join(v.Labels, ",") != "promotion" || v.Scores["promotion"] != 0.91 || v.Scores["newsletter"] != 0.12 ||
		v.Reason != "newsletter 0.12, promotion 0.91" || v.CostUSD != 0.0003 {
		t.Fatalf("verdict %+v", v)
	}
}

func TestJevFailsAMessageWhenAnyAnswerIsMissing(t *testing.T) {
	c := jevServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"promotion": map[string]any{"noul": 0.91}},
			"usage": map[string]any{"cost": 0.0003}})
	})
	_, err := stage.NewJev("category", c, category, 0.5, stage.NewBudget(1)).Evaluate(context.Background(), msg())
	if err == nil || !strings.Contains(err.Error(), "gave no answer for newsletter") || errors.Is(err, stage.ErrFatal) {
		t.Fatalf("err %v: a missing answer fails this message only, naming the question", err)
	}
}

func TestJevRetriesAResponseCutOffMidBody(t *testing.T) {
	var calls atomic.Int32
	c := jevServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", "500")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"answers":`)) // the connection closes before the promised 500 bytes
			return
		}
		_ = json.NewEncoder(w).Encode(answer(0.2, 0.0001))
	})
	if _, err := stage.NewJev("political", c, []stage.Question{question}, 0.5, stage.NewBudget(1)).Evaluate(context.Background(), msg()); err != nil || calls.Load() != 2 {
		t.Fatalf("err %v after %d calls: a cut-off response is retried", err, calls.Load())
	}
}

// A question can name today's date, for judgements that change with time
// ("has this event already happened?").
func TestJevFillsInTodayInEveryQuestion(t *testing.T) {
	var got struct {
		Questions map[string]struct {
			Instructions string            `json:"instructions"`
			Criteria     map[string]string `json:"criteria"`
		} `json:"questions"`
	}
	c := jevServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"event_over": map[string]any{"noul": 0.9}}, "usage": map[string]any{"cost": 0.0001}})
	})
	q := stage.Question{Key: "event_over", Instructions: "Today is {today}. Is the event over?",
		True: "it ended before {today}", False: "it is on or after {today}"}
	j := stage.NewJev("event-past", c, []stage.Question{q}, 0.5, stage.NewBudget(1))
	j.Now = func() time.Time { return time.Date(2026, 10, 24, 23, 0, 0, 0, time.FixedZone("PDT", -7*3600)) }
	if _, err := j.Evaluate(context.Background(), msg()); err != nil {
		t.Fatal(err)
	}
	a := got.Questions["event_over"]
	if a.Instructions != "Today is 2026-10-24. Is the event over?" || a.Criteria["true"] != "it ended before 2026-10-24" || a.Criteria["false"] != "it is on or after 2026-10-24" {
		t.Fatalf("question sent: %+v", a)
	}
}

var kind = stage.Choice{Key: "kind", Instructions: "Which kind of email is this?",
	Options: map[string]string{"receipt": "a gift receipt", "none": "something else"}}

func choiceAnswer(choice string, probs map[string]float64, cost float64) map[string]any {
	return map[string]any{"answers": map[string]any{"kind": map[string]any{"type": "choice", "choice": choice, "probabilities": probs}},
		"usage": map[string]any{"cost": cost}}
}

func TestJevChooseSendsAChoiceQuestionAndReadsTheAnswer(t *testing.T) {
	var got map[string]any
	c := jevServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(choiceAnswer("receipt", map[string]float64{"receipt": 0.93, "none": 0.07}, 0.0003))
	})
	budget := stage.NewBudget(1)
	a, err := c.Choose(context.Background(), budget, "the email", kind)
	if err != nil {
		t.Fatal(err)
	}
	if a.Choice != "receipt" || a.Probabilities["receipt"] != 0.93 || a.Cost != 0.0003 || budget.Spent() != 0.0003 {
		t.Fatalf("answer %+v spent %v", a, budget.Spent())
	}
	q := got["questions"].(map[string]any)["kind"].(map[string]any)
	if got["model"] != "typesafe/jev-1.14" || got["state"] != "the email" || q["type"] != "choice" ||
		q["instructions"] != "Which kind of email is this?" || q["criteria"].(map[string]any)["receipt"] != "a gift receipt" {
		t.Fatalf("request %v", got)
	}
}

func TestJevChooseRejectsMalformedAnswers(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"unknown choice":      choiceAnswer("invoice", map[string]float64{"receipt": 0.5, "none": 0.5}, 0),
		"missing probability": choiceAnswer("receipt", map[string]float64{"receipt": 1}, 0),
		"out of range":        choiceAnswer("receipt", map[string]float64{"receipt": 1.5, "none": -0.5}, 0),
		"not a distribution":  choiceAnswer("receipt", map[string]float64{"receipt": 0.9, "none": 0.9}, 0),
		"no answer":           {"answers": map[string]any{}, "usage": map[string]any{"cost": 0}},
	} {
		c := jevServer(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(body) })
		if _, err := c.Choose(context.Background(), stage.NewBudget(1), "s", kind); err == nil || errors.Is(err, stage.ErrFatal) {
			t.Errorf("%s: err %v, want a non-fatal failure", name, err)
		}
	}
}

func TestJevChooseRetriesAndStopsAtTheBudget(t *testing.T) {
	var calls atomic.Int32
	c := jevServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(choiceAnswer("none", map[string]float64{"receipt": 0.1, "none": 0.9}, 0.5))
	})
	budget := stage.NewBudget(0.5)
	if a, err := c.Choose(context.Background(), budget, "s", kind); err != nil || a.Choice != "none" || calls.Load() != 2 {
		t.Fatalf("answer %+v err %v calls %d", a, err, calls.Load())
	}
	if _, err := c.Choose(context.Background(), budget, "s", kind); !errors.Is(err, stage.ErrBudget) || calls.Load() != 2 {
		t.Fatalf("over budget: err %v calls %d", err, calls.Load())
	}
}

func TestConservativeExpiryGatesMixedAndAmbiguousMessages(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		past, future, unclear float64
		wantDelete            bool
	}{
		{"mixed past and future", .96, .78, .04, false},
		{"borderline", .57, .44, .41, false},
		{"uncertain date", .94, .12, .82, false},
		{"conflicting", .98, .91, .03, false},
		{"fully expired", .97, .03, .02, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := jevServer(t, func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					State string `json:"state"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				if !strings.Contains(req.State, "Upcoming registration closes 2031-06-20") {
					t.Error("missing end-of-message deadline")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"past": map[string]any{"noul": tc.past}, "future": map[string]any{"noul": tc.future}, "unclear": map[string]any{"noul": tc.unclear}}, "usage": map[string]any{"cost": 0}})
			})
			qs := []stage.Question{{Key: "past", Threshold: .9}, {Key: "future", Threshold: .2}, {Key: "unclear", Threshold: .2}}
			j := stage.NewJev("schedule", c, qs, .5, stage.NewBudget(1))
			m := msg()
			m.Body = "Past workshop: 2031-05-01. " + strings.Repeat("Newsletter content. ", 3000) + "Upcoming registration closes 2031-06-20"
			v, err := j.Evaluate(context.Background(), m)
			if err != nil {
				t.Fatal(err)
			}
			p := policy.Policy{Rules: []policy.Rule{
				{Stage: "schedule", Label: "past", Action: stage.Delete, Unless: []string{"schedule:future", "schedule:unclear"}, NoBlock: true},
				{Stage: "category", Label: "event", Action: stage.Delete, OlderThan: "30d", Unless: []string{"schedule:future", "schedule:unclear"}, NoBlock: true},
			}}
			d := p.Decide(policy.Message{Received: time.Now().AddDate(-1, 0, 0)}, []store.Verdict{{Stage: v.Stage, Labels: v.Labels}, {Stage: "category", Labels: []string{"event"}}}, time.Now())
			if (d.Action == stage.Delete) != tc.wantDelete {
				t.Fatalf("verdict %+v decision %+v", v, d)
			}
		})
	}
}

func TestOversizedJevInputIsKeptForReviewWithoutModelCall(t *testing.T) {
	var calls atomic.Int32
	c := jevServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(answer(.99, 0))
	})
	c.MaxInputBytes = 1024
	j := stage.NewJev("classification", c, []stage.Question{question}, .5, stage.NewBudget(1))
	m := msg()
	m.Body = strings.Repeat("x", 2048)
	v, err := j.Evaluate(context.Background(), m)
	if err != nil || v.Action != stage.Keep || len(v.Labels) != 1 || v.Labels[0] != "review" || calls.Load() != 0 || strings.Contains(v.Reason, m.Body) {
		t.Fatalf("verdict %+v error %v calls %d", v, err, calls.Load())
	}

	p := policy.Policy{Rules: []policy.Rule{{Senders: []string{m.Sender}, Action: stage.Delete, OlderThan: "7d", Hold: true}}}
	d := p.Decide(policy.Message{Sender: m.Sender, Received: time.Now().AddDate(-1, 0, 0)}, []store.Verdict{{Stage: v.Stage, Labels: v.Labels, Action: v.Action}}, time.Now())
	if d.Action != stage.Keep {
		t.Fatalf("oversized input lost protection against expiry: %+v", d)
	}
}

func TestInputGuardDefaultsAndConfiguredBoundary(t *testing.T) {
	var calls atomic.Int32
	c := jevServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(answer(.99, 0))
	})
	j := stage.NewJev("classifier", c, []stage.Question{question}, .5, stage.NewBudget(1))
	m := msg()
	m.Body = strings.Repeat("x", stage.DefaultMaxInputBytes)
	v, err := j.Evaluate(context.Background(), m)
	if err != nil || v.Action != stage.Keep || calls.Load() != 0 {
		t.Fatalf("oversized default: %+v %v calls %d", v, err, calls.Load())
	}
	m.Body = "Complete message"
	c.MaxInputBytes = len(stage.State(m))
	v, err = j.Evaluate(context.Background(), m)
	if err != nil || len(v.Labels) != 1 || v.Labels[0] != question.Key || calls.Load() != 1 {
		t.Fatalf("exact boundary: %+v %v calls %d", v, err, calls.Load())
	}
}
