package stage_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/stage"
)

type fakeStage struct {
	body  bool
	fail  map[int64]error
	calls atomic.Int32
	saw   sync.Map // n -> body
}

func (f *fakeStage) Name() string    { return "fake" }
func (f *fakeStage) NeedsBody() bool { return f.body }
func (f *fakeStage) Evaluate(_ context.Context, m stage.Message) (stage.Verdict, error) {
	f.calls.Add(1)
	f.saw.Store(m.N, m.Body)
	if err := f.fail[m.N]; err != nil {
		return stage.Verdict{}, err
	}
	return stage.Verdict{Stage: "fake", Labels: []string{fmt.Sprintf("n%d", m.N)}, CostUSD: 0.01}, nil
}

func messages(n int) []stage.Message {
	out := make([]stage.Message, n)
	for i := range out {
		out[i] = stage.Message{N: int64(i + 1), Sender: "a@example.org"}
	}
	return out
}

func collect(t *testing.T, s stage.Stage, msgs []stage.Message, body func(context.Context, *stage.Message) error, workers int) ([]stage.Result, stage.Summary, error) {
	t.Helper()
	var got []stage.Result
	sum, err := stage.Run(context.Background(), s, msgs, body, func(r stage.Result) error {
		got = append(got, r) // the sink runs on one goroutine; no lock needed
		return nil
	}, workers)
	return got, sum, err
}

func TestRunEvaluatesEveryMessageAndSummarizes(t *testing.T) {
	f := &fakeStage{fail: map[int64]error{3: errors.New("timed out")}}
	got, sum, err := collect(t, f, messages(10), nil, 4)
	if err != nil || len(got) != 10 || sum.Evaluated != 9 || sum.Failed != 1 || fmt.Sprintf("%.2f", sum.CostUSD) != "0.09" {
		t.Fatalf("results %d summary %+v err %v", len(got), sum, err)
	}
	for _, r := range got {
		if (r.Message.N == 3) != (r.Err != nil) {
			t.Fatalf("result %+v", r)
		}
	}
}

func TestRunFetchesBodiesOnlyWhenTheStageNeedsThem(t *testing.T) {
	fetch := func(_ context.Context, m *stage.Message) error {
		if m.N == 2 {
			return errors.New("graph 404")
		}
		m.Body = fmt.Sprintf("body %d", m.N)
		return nil
	}
	f := &fakeStage{body: true}
	got, sum, err := collect(t, f, messages(3), fetch, 1)
	if err != nil || sum.Failed != 1 || f.calls.Load() != 2 || len(got) != 3 {
		t.Fatalf("summary %+v calls %d err %v", sum, f.calls.Load(), err)
	}
	if b, _ := f.saw.Load(int64(3)); b != "body 3" {
		t.Fatalf("stage saw body %q", b)
	}
	g := &fakeStage{}
	calledFetch := false
	if _, _, err := collect(t, g, messages(2), func(context.Context, *stage.Message) error { calledFetch = true; return nil }, 1); err != nil || calledFetch {
		t.Fatalf("body fetched for a stage that does not need it (err %v)", err)
	}
}

func TestRunStopsOnAFatalError(t *testing.T) {
	f := &fakeStage{fail: map[int64]error{2: fmt.Errorf("%w: HTTP 401", stage.ErrFatal)}}
	got, _, err := collect(t, f, messages(50), nil, 1)
	if !errors.Is(err, stage.ErrFatal) {
		t.Fatalf("err = %v", err)
	}
	if f.calls.Load() > 3 || len(got) > 3 {
		t.Fatalf("kept going after a fatal error: %d calls, %d results", f.calls.Load(), len(got))
	}
}

func TestRunStopsWhenTheSinkFails(t *testing.T) {
	f := &fakeStage{}
	_, err := stage.Run(context.Background(), f, messages(50), nil, func(stage.Result) error { return errors.New("disk full") }, 1)
	if err == nil || f.calls.Load() > 3 {
		t.Fatalf("err %v calls %d", err, f.calls.Load())
	}
}

func TestRunHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeStage{}
	recorded := 0
	_, err := stage.Run(ctx, f, messages(50), nil, func(stage.Result) error { recorded++; return nil }, 4)
	if !errors.Is(err, context.Canceled) || f.calls.Load() > 4 {
		t.Fatalf("err %v calls %d", err, f.calls.Load())
	}
	if recorded != 0 {
		t.Fatalf("%d cancelled messages were recorded as failures", recorded)
	}
}

func TestRunRejectsInvalidVerdicts(t *testing.T) {
	bad := &badStage{}
	got, sum, err := collect(t, bad, messages(1), nil, 1)
	if err != nil || sum.Failed != 1 || got[0].Err == nil {
		t.Fatalf("invalid verdict accepted: %+v %v", got, err)
	}
}

type badStage struct{}

func (badStage) Name() string    { return "bad" }
func (badStage) NeedsBody() bool { return false }
func (badStage) Evaluate(context.Context, stage.Message) (stage.Verdict, error) {
	return stage.Verdict{Action: "explode"}, nil
}

// choosyStage needs the bodies of even-numbered messages only.
type choosyStage struct{ fakeStage }

func (c *choosyStage) NeedsBodyOf(m stage.Message) bool { return m.N%2 == 0 }

func TestRunFetchesOnlyTheBodiesAStageChooses(t *testing.T) {
	s := &choosyStage{fakeStage{body: true}}
	var fetched sync.Map
	body := func(_ context.Context, m *stage.Message) error {
		fetched.Store(m.N, true)
		m.Body = "text"
		return nil
	}
	if _, _, err := collect(t, s, messages(4), body, 2); err != nil {
		t.Fatal(err)
	}
	for n := int64(1); n <= 4; n++ {
		_, got := fetched.Load(n)
		if got != (n%2 == 0) {
			t.Errorf("message %d: fetched %v", n, got)
		}
	}
	if s.calls.Load() != 4 {
		t.Errorf("evaluated %d, want every message", s.calls.Load())
	}
}
