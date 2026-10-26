package stage_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/stage"
)

func TestHookScopesSourceAndCleansItAfterEvaluation(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "source-path")
	program := script(t, `input=$(cat); path=$(printf '%s' "$input" | sed -n 's/.*"source_path":"\([^"]*\)".*/\1/p'); test -f "$path" || exit 3; grep -q 'Subject: Invented' "$path" || exit 4; printf '%s' "$path" > "$1"; printf '{"labels":["review"],"cost_usd":0.01}'`)
	calls := 0
	hook := stage.NewHook("custom", []string{program, marker}, time.Second, false, []string{"Sender@Example.org"}, func(context.Context, stage.Message) ([]byte, error) {
		calls++
		return []byte("Subject: Invented\r\n\r\nExample body"), nil
	}, stage.NewBudget(0.01))
	m := stage.Message{Sender: "other@example.net"}
	if v, err := hook.Evaluate(context.Background(), m); err != nil || len(v.Labels) != 0 || calls != 0 {
		t.Fatalf("out of scope: %+v %v calls=%d", v, err, calls)
	}
	m.Sender = "sender@example.org"
	v, err := hook.Evaluate(context.Background(), m)
	if err != nil || strings.Join(v.Labels, ",") != "review" || calls != 1 {
		t.Fatalf("in scope: %+v %v calls=%d", v, err, calls)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(string(b)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source remains: %v", err)
	}
	if _, err := hook.Evaluate(context.Background(), m); !errors.Is(err, stage.ErrBudget) || calls != 1 {
		t.Fatalf("budget: %v calls=%d", err, calls)
	}
}

func TestHookCannotSuggestAnActionOrFinalVerdict(t *testing.T) {
	for _, body := range []string{`printf '{"action":"delete"}'`, `printf '{"final":true}'`} {
		h := stage.NewHook("custom", []string{script(t, body)}, time.Second, false, nil, nil, stage.NewBudget(1))
		if _, err := h.Evaluate(context.Background(), stage.Message{}); err == nil {
			t.Fatal("hook supplied execution policy")
		}
	}
}

func TestHookSourceFailureNeverRunsProgram(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	h := stage.NewHook("custom", []string{script(t, `touch "$1"; printf '{}'`), marker}, time.Second, false, nil, func(context.Context, stage.Message) ([]byte, error) { return nil, errors.New("source unavailable") }, stage.NewBudget(1))
	if _, err := h.Evaluate(context.Background(), stage.Message{}); err == nil {
		t.Fatal("source failure ignored")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("program ran: %v", err)
	}
}

func TestHookWithoutBudgetFailsClosed(t *testing.T) {
	h := stage.NewHook("custom", []string{script(t, `printf '{}'`)}, time.Second, false, nil, nil, nil)
	if _, err := h.Evaluate(context.Background(), stage.Message{}); !errors.Is(err, stage.ErrFatal) {
		t.Fatalf("missing budget: %v", err)
	}
}
