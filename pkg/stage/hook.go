package stage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	budget  *Budget
	mu      sync.Mutex
}

// NewHook creates a classifier. An empty sender list matches all messages; a
// nil source callback disables MIME access. Calls are serialised so each child
// receives the remaining budget after the previous child's reported cost.
func NewHook(name string, argv []string, timeout time.Duration, needsBody bool, senders []string, source func(context.Context, Message) ([]byte, error), budget *Budget) *Hook {
	return &Hook{exec: NewExec(name, argv, timeout, needsBody), senders: lowerSet(senders), source: source, budget: budget}
}

func (h *Hook) Name() string    { return h.exec.Name() }
func (h *Hook) NeedsBody() bool { return h.exec.NeedsBody() }
func (h *Hook) matches(m Message) bool {
	return len(h.senders) == 0 || h.senders[strings.ToLower(strings.TrimSpace(m.Sender))]
}
func (h *Hook) NeedsBodyOf(m Message) bool { return h.matches(m) }

func (h *Hook) Evaluate(ctx context.Context, m Message) (Verdict, error) {
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
	m.MaxCostUSD = h.budget.max - h.budget.Spent()
	m.SourcePath = ""
	if h.source != nil {
		raw, err := h.source(ctx, m)
		if err != nil {
			return Verdict{}, fmt.Errorf("hook source: %w", err)
		}
		dir, err := os.MkdirTemp("", "mailpipe-hook-")
		if err != nil {
			return Verdict{}, err
		}
		defer os.RemoveAll(dir)
		m.SourcePath = filepath.Join(dir, "message.eml")
		if err := os.WriteFile(m.SourcePath, raw, 0o600); err != nil {
			return Verdict{}, err
		}
	}
	v, err := h.exec.Evaluate(ctx, m)
	if err != nil {
		return v, err
	}
	h.budget.charge(v.CostUSD)
	if v.Action != "" || v.Final {
		return Verdict{CostUSD: v.CostUSD}, fmt.Errorf("hook %q must return labels and scores, not action or final", h.Name())
	}
	return v, nil
}
