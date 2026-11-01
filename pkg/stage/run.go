package stage

import (
	"context"
	"errors"
	"fmt"
	"github.com/mrothroc/mailpipe/pkg/document"
	"sync"
)

// Result is the outcome for one message: a verdict, or the error that
// prevented one. On error, Verdict carries only cost, except Protected results
// whose review/keep verdict must be persisted while the run error is surfaced.
type Result struct {
	Message   Message
	Verdict   Verdict
	Err       error
	Protected bool
}

// Summary counts a run's outcomes.
type Summary struct {
	Evaluated int
	Failed    int
	CostUSD   float64
}

// Run evaluates msgs with s, using workers goroutines. When s needs bodies,
// body fills each message in first. Accepted results go to sink, which is called
// from a single goroutine, so it needs no locking.
//
// A failure on one message (a timeout, a bad answer, a missing body) is
// recorded and the run continues. An error wrapping ErrFatal, a sink error or
// cancellation stops the run: no new message is started, and the error is
// returned along with the summary of accepted outcomes and all reported costs.
// Completed protective keeps are still persisted during the drain; ordinary
// results abandoned after a stop are discarded.
func Run(ctx context.Context, s Stage, msgs []Message, body func(context.Context, *Message) error, sink func(Result) error, workers int) (Summary, error) {
	workers = max(workers, 1)
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan Message)
	results := make(chan Result)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := range jobs {
				results <- evaluate(ctx, s, m, body)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, m := range msgs {
			select {
			case jobs <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	var sum Summary
	var stop error
	for r := range results {
		// Calls already in flight can incur cost even when their normal verdict
		// is abandoned. Protection must still replace any old destructive verdict.
		sum.CostUSD += r.Verdict.CostUSD
		if stop != nil && !r.Protected {
			continue
		}
		if !r.Protected && r.Err != nil && ctx.Err() != nil && errors.Is(r.Err, ctx.Err()) {
			continue // abandoned because the run stopped, not a failure of this message
		}
		if r.Err != nil {
			sum.Failed++
		} else {
			sum.Evaluated++
		}
		if errors.Is(r.Err, ErrFatal) {
			stop = errors.Join(stop, r.Err)
			cancel()
		}
		if err := sink(r); err != nil {
			stop = errors.Join(stop, fmt.Errorf("recording a result failed: %w", err))
			cancel()
		}
	}
	if stop != nil {
		return sum, errors.Join(stop, parent.Err())
	}
	return sum, parent.Err() // nil unless the caller cancelled or timed out
}

func evaluate(ctx context.Context, s Stage, m Message, body func(context.Context, *Message) error) Result {
	if err := ctx.Err(); err != nil {
		return Result{Message: m, Err: err}
	}
	need := s.NeedsBody()
	if c, ok := s.(BodyChooser); ok && need {
		need = c.NeedsBodyOf(m)
	}
	if need && body != nil {
		if err := body(ctx, &m); err != nil {
			return contentFailure(s, m, fmt.Errorf("fetch body: %w", err))
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{Message: m, Err: err}
	}
	v, err := s.Evaluate(ctx, m)
	if err != nil {
		if errors.Is(err, document.ErrIncomplete) {
			r := contentFailure(s, m, err)
			r.Verdict.CostUSD = v.CostUSD
			return r
		}
		return Result{Message: m, Verdict: Verdict{Stage: s.Name(), CostUSD: v.CostUSD}, Err: err}
	}
	if err := v.Validate(); err != nil {
		return Result{Message: m, Err: fmt.Errorf("stage %q returned an invalid verdict: %w", s.Name(), err)}
	}
	v.Stage = s.Name()
	return Result{Message: m, Verdict: v}
}

// contentFailure preserves protection as a successful stored verdict while a
// cleanup failure independently stops the run. Ordinary access failures stay fatal.
func contentFailure(s Stage, m Message, err error) Result {
	if !errors.Is(err, document.ErrIncomplete) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || (errors.Is(err, ErrFatal) && !errors.Is(err, document.ErrCleanup)) {
		return Result{Message: m, Err: err}
	}
	r := Result{Message: m, Protected: true, Verdict: Verdict{Stage: s.Name(), Labels: []string{"review"}, Action: Keep, Reason: "incomplete content; retained for review", Provenance: "content-safety"}}
	if errors.Is(err, document.ErrCleanup) {
		r.Err = errors.Join(ErrFatal, document.ErrCleanup)
	}
	return r
}
