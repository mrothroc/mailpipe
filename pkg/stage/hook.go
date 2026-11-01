package stage

import (
	"context"
	"errors"
	"fmt"
	"github.com/mrothroc/mailpipe/pkg/document"
	"io"
	"strings"
	"sync"
	"time"
)

// Hook is an explicitly configured, trusted personal classifier. It uses the
// exec protocol, with optional MIME source and an exact sender scope. Hooks
// supply labels and scores, never actions or final keep decisions. They are
// normal user processes, not sandboxed plugins.
type Hook struct {
	exec    *Exec
	senders map[string]bool
	source  func(context.Context, Message) ([]byte, error)
	stream  func(context.Context, Message, io.Writer) error
	budget  *Budget
	mu      sync.Mutex
}

// NewHook creates a classifier. An empty sender list matches all messages; a
// nil source callback disables MIME access. Calls are serialised so each child
// receives the remaining budget after the previous child's reported cost.
func NewHook(name string, argv []string, timeout time.Duration, needsBody bool, senders []string, source func(context.Context, Message) ([]byte, error), budget *Budget) *Hook {
	return &Hook{exec: NewExec(name, argv, timeout, needsBody), senders: lowerSet(senders), source: source, budget: budget}
}

// NewStreamingHook preserves the hook JSON contract while avoiding whole-source
// byte slices. All hooks in a command share the Scope carried by their context.
func NewStreamingHook(name string, argv []string, timeout time.Duration, needsBody bool, senders []string, source func(context.Context, Message, io.Writer) error, budget *Budget) *Hook {
	h := NewHook(name, argv, timeout, needsBody, senders, nil, budget)
	h.stream = source
	return h
}

func (h *Hook) Name() string    { return h.exec.Name() }
func (h *Hook) NeedsBody() bool { return h.exec.NeedsBody() }
func (h *Hook) matches(m Message) bool {
	return len(h.senders) == 0 || h.senders[strings.ToLower(strings.TrimSpace(m.Sender))]
}
func (h *Hook) NeedsBodyOf(m Message) bool { return h.matches(m) }

func (h *Hook) Evaluate(ctx context.Context, m Message) (v Verdict, err error) {
	if !h.matches(m) {
		return Verdict{Stage: h.Name(), Provenance: "hook:" + h.Name()}, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Verdict{}, err
	}
	if h.budget == nil {
		return Verdict{}, fmt.Errorf("%w: hook %q requires a cost budget", ErrFatal, h.Name())
	}
	if err := h.budget.check(); err != nil {
		return Verdict{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, h.exec.timeout)
	defer cancel()
	m.MaxCostUSD = h.budget.max - h.budget.Spent()
	m.SourcePath = ""

	if h.source != nil || h.stream != nil {
		scope := document.ScopeFrom(ctx)
		own := scope == nil
		if own {
			scope, err = document.NewScope(document.Limits{})
			if err != nil {
				return Verdict{}, err
			}
		}
		var source *document.Source
		defer func() {
			var cleanup error
			if source != nil {
				cleanup = source.Close()
			}
			if own {
				cleanup = errors.Join(cleanup, scope.Close())
			}
			if cleanup != nil {
				err = errors.Join(err, cleanup)
				v = Verdict{CostUSD: v.CostUSD}
			}
		}()
		source, err = document.StreamSource(ctx, scope, func(w io.Writer) error {
			if h.stream != nil {
				return h.stream(ctx, m, w)
			}
			raw, e := h.source(ctx, m)
			if e != nil {
				return e
			}
			if int64(len(raw)) > scope.Limits().RawBytes {
				return document.ErrIncomplete
			}
			_, e = w.Write(raw)
			return e
		})
		if err != nil {
			return Verdict{}, fmt.Errorf("hook source: %w", err)
		}
		m.SourcePath, err = source.Path()
		if err != nil {
			return Verdict{}, err
		}
	}
	v, err = h.exec.Evaluate(ctx, m)
	if err != nil {
		return v, err
	}
	h.budget.charge(v.CostUSD)
	if v.Action != "" || v.Final {
		return Verdict{CostUSD: v.CostUSD}, fmt.Errorf("hook %q must return labels and scores, not action or final", h.Name())
	}
	return v, nil
}
