package stage_test

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/stage"
)

func msg() stage.Message {
	return stage.Message{N: 7, GraphID: "g7", InternetMessageID: "<g7@example.org>", Folder: "inbox",
		Received: "2026-10-20T10:00:00Z", Sender: "news@campaign.example.org", SenderName: "Campaign",
		Recipients: []string{"alex@example.com"}, Subject: "Chip in today", Preview: "We need you"}
}

func TestVerdictValidation(t *testing.T) {
	ok := []stage.Verdict{
		{},
		{Labels: []string{"known_contact"}, Action: stage.Keep, Final: true},
		{Scores: map[string]float64{"political_appeal": 0.97}, CostUSD: 0.0001},
	}
	for _, v := range ok {
		if err := v.Validate(); err != nil {
			t.Errorf("%+v rejected: %v", v, err)
		}
	}
	bad := map[string]stage.Verdict{
		"unknown action": {Action: "explode"},
		"blank label":    {Labels: []string{" "}},
		"NaN score":      {Scores: map[string]float64{"x": math.NaN()}},
		"infinite score": {Scores: map[string]float64{"x": math.Inf(1)}},
		"negative cost":  {CostUSD: -1},
	}
	for name, v := range bad {
		if err := v.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stage.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExecStageReceivesTheMessageOnStdinAndReturnsItsVerdict(t *testing.T) {
	// The script echoes back the subject it read from stdin as its reason.
	p := script(t, `read line; subj=$(printf '%s' "$line" | sed -n 's/.*"subject":"\([^"]*\)".*/\1/p'); printf '{"labels":["seen"],"reason":"%s","stage":"spoofed"}\n' "$subj"`)
	s := stage.NewExec("probe", []string{p}, 3*time.Second, false)
	v, err := s.Evaluate(context.Background(), msg())
	if err != nil {
		t.Fatal(err)
	}
	if v.Reason != "Chip in today" || len(v.Labels) != 1 || v.Labels[0] != "seen" {
		t.Fatalf("verdict %+v", v)
	}
	if v.Stage != "probe" {
		t.Fatalf("stage name %q: the configured name must win over what the program claims", v.Stage)
	}
}

func TestExecStageUsesArgvNotAShell(t *testing.T) {
	// An argument with shell metacharacters reaches the program verbatim.
	p := script(t, `printf '{"reason":"%s"}\n' "$1"`)
	s := stage.NewExec("probe", []string{p, "$(touch pwned); `id`"}, 3*time.Second, false)
	v, err := s.Evaluate(context.Background(), msg())
	if err != nil || v.Reason != "$(touch pwned); `id`" {
		t.Fatalf("verdict %+v err %v", v, err)
	}
}

func TestExecStageFailuresAreReportedWithStderr(t *testing.T) {
	cases := map[string]struct {
		body    string
		timeout time.Duration
		want    string
	}{
		"nonzero exit": {`echo "model unavailable" >&2; exit 3`, 3 * time.Second, "exited with status 3: model unavailable"},
		"bad json":     {`echo not-json`, 3 * time.Second, "invalid verdict"},
		"bad verdict":  {`echo '{"action":"explode"}'`, 3 * time.Second, "invalid verdict"},
		"two objects":  {`echo '{}{}'`, 3 * time.Second, "invalid verdict"},
		"timeout":      {`sleep 5`, 300 * time.Millisecond, "timed out after"},
	}
	for name, c := range cases {
		s := stage.NewExec("probe", []string{script(t, c.body)}, c.timeout, false)
		_, err := s.Evaluate(context.Background(), msg())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
		if errors.Is(err, stage.ErrFatal) {
			t.Errorf("%s: a single message failure must not stop the run", name)
		}
	}
}

func TestExecStageBodyIsSentOnlyWhenDeclared(t *testing.T) {
	m := msg()
	m.Body = "secret body text"
	p := script(t, `read line; case "$line" in *secret*) echo '{"labels":["got_body"]}';; *) echo '{}';; esac`)
	for needs, want := range map[bool]int{true: 1, false: 0} {
		v, err := stage.NewExec("probe", []string{p}, 3*time.Second, needs).Evaluate(context.Background(), m)
		if err != nil || len(v.Labels) != want {
			t.Errorf("needsBody=%v: verdict %+v err %v", needs, v, err)
		}
	}
}

func TestExampleShellStagePassesTheContract(t *testing.T) {
	s := stage.NewExec("newsletter", []string{"../../examples/stages/newsletter.sh"}, 2*time.Second, false)
	m := msg()
	m.Subject = "Weekly Newsletter: October"
	v, err := s.Evaluate(context.Background(), m)
	if err != nil || len(v.Labels) != 1 || v.Labels[0] != "newsletter" || v.Validate() != nil {
		t.Fatalf("verdict %+v err %v", v, err)
	}
	m.Subject = "Dinner Tuesday?"
	if v, err := s.Evaluate(context.Background(), m); err != nil || len(v.Labels) != 0 {
		t.Fatalf("verdict %+v err %v", v, err)
	}
}
