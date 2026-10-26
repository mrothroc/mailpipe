package stage_test

// Regression tests for stage error handling and address normalization.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mrothroc/mailpipe/pkg/stage"
)

// Decisions failures carry their HTTP status, so callers can tell a bad key
// from a bad request.
func TestDecisionsErrorsCarryTheirStatus(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 404} {
		c := jevServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
		_, err := stage.NewJev("political", c, []stage.Question{question}, 0.5, stage.NewBudget(1)).Evaluate(context.Background(), msg())
		var de *stage.DecisionsError
		if !errors.As(err, &de) || de.Status != status || !errors.Is(err, stage.ErrFatal) {
			t.Errorf("status %d: err %v", status, err)
		}
	}
}

// Cancelling the run while an exec stage is running is an abandoned message,
// not a stage failure.
func TestExecCancellationIsReportedAsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := stage.NewExec("probe", []string{script(t, "sleep 5")}, 10*time.Second, false)
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	_, err := s.Evaluate(ctx, msg())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// Error text from the Decisions API is cut on a character boundary.
func TestDecisionsErrorTextStaysValidUTF8(t *testing.T) {
	c := jevServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("a" + strings.Repeat("é", 400))) // odd offset, so byte 300 falls inside a character
	})
	_, err := stage.NewJev("political", c, []stage.Question{question}, 0.5, stage.NewBudget(1)).Evaluate(context.Background(), msg())
	if err == nil || !utf8.ValidString(err.Error()) {
		t.Fatalf("err %q is not valid UTF-8", err)
	}
}

func TestAllowlistAndHoneypotTrimAddresses(t *testing.T) {
	a := stage.NewAllowlist("known", nil, nil, []string{"friend@example.org"}, nil)
	m := msg()
	m.Sender = "  Friend@Example.org "
	if v, _ := a.Evaluate(context.Background(), m); len(v.Labels) != 1 {
		t.Fatalf("padded contact not matched: %+v", v)
	}
	h := stage.NewHoneypot("honeypot", []string{"trap@example.com"}, nil)
	m.Recipients = []string{" trap@example.com "}
	if v, _ := h.Evaluate(context.Background(), m); len(v.Labels) != 1 {
		t.Fatalf("padded honeypot not matched: %+v", v)
	}
}

// A failed call that was still charged is counted in the run's cost.
func TestRunCountsTheCostOfFailedMessages(t *testing.T) {
	c := jevServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"political_appeal":{"noul":"bad"}},"usage":{"cost":0.25}}`))
	})
	j := stage.NewJev("political", c, []stage.Question{question}, 0.5, stage.NewBudget(10))
	var recorded []stage.Result
	sum, err := stage.Run(context.Background(), j, messages(2), nil, func(r stage.Result) error { recorded = append(recorded, r); return nil }, 1)
	if err != nil || sum.Failed != 2 || sum.CostUSD != 0.5 {
		t.Fatalf("summary %+v err %v, want 2 failures costing $0.50", sum, err)
	}
	if recorded[0].Verdict.CostUSD != 0.25 {
		t.Fatalf("failed result lost its cost: %+v", recorded[0])
	}
}

// Cancelling during a retry backoff returns promptly.
func TestJevBackoffHonoursCancellation(t *testing.T) {
	c := jevServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	c.Sleep = nil // the real, context-aware backoff
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := stage.NewJev("political", c, []stage.Question{question}, 0.5, stage.NewBudget(1)).Evaluate(ctx, msg())
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("err %v after %s", err, time.Since(start))
	}
}
