package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxVerdictBytes = 1 << 20
	maxStderrBytes  = 4 << 10
)

// Exec runs an external program as a stage. The message is written to its
// stdin as one JSON line; it must print one JSON verdict on stdout and exit 0.
// The program is started directly from its argv, never through a shell, so
// message content cannot be interpreted as shell syntax. Its stderr is kept
// for the error when it fails.
type Exec struct {
	name    string
	argv    []string
	timeout time.Duration
	body    bool
}

// NewExec returns an Exec stage running argv with the given timeout.
func NewExec(name string, argv []string, timeout time.Duration, needsBody bool) *Exec {
	return &Exec{name: name, argv: argv, timeout: timeout, body: needsBody}
}

func (e *Exec) Name() string    { return e.name }
func (e *Exec) NeedsBody() bool { return e.body }

// limitedBuffer keeps at most max bytes and records that it overflowed.
type limitedBuffer struct {
	bytes.Buffer
	max      int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); len(p) > room {
		b.overflow = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

func (e *Exec) Evaluate(ctx context.Context, m Message) (Verdict, error) {
	if len(e.argv) == 0 {
		return Verdict{}, fmt.Errorf("%w: stage %q has no command", ErrFatal, e.name)
	}
	if !e.body {
		m.Body = ""
	}
	input, err := json.Marshal(m)
	if err != nil {
		return Verdict{}, err
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.argv[0], e.argv[1:]...)
	cmd.Stdin = bytes.NewReader(append(input, '\n'))
	cmd.Env = append(minimalEnv(), "MAILPIPE_STAGE="+e.name)
	stdout := &limitedBuffer{max: maxVerdictBytes}
	stderr := &limitedBuffer{max: maxStderrBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second

	err = cmd.Run()
	label := filepath.Base(e.argv[0])
	if err := parent.Err(); err != nil {
		return Verdict{}, err // the run was cancelled: an abandoned message, not a stage failure
	}
	if ctx.Err() == context.DeadlineExceeded {
		return Verdict{}, fmt.Errorf("stage %q (%s) timed out after %s", e.name, label, e.timeout)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return Verdict{}, fmt.Errorf("stage %q (%s) exited with status %d: %s", e.name, label, exitErr.ExitCode(), lastLine(stderr.String()))
		}
		return Verdict{}, fmt.Errorf("%w: stage %q could not start %s: %v", ErrFatal, e.name, label, err)
	}
	if stdout.overflow {
		return Verdict{}, fmt.Errorf("stage %q (%s): invalid verdict: output larger than %d bytes", e.name, label, maxVerdictBytes)
	}
	var v Verdict
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return Verdict{}, fmt.Errorf("stage %q (%s): invalid verdict: %v", e.name, label, err)
	}
	if dec.More() {
		return Verdict{}, fmt.Errorf("stage %q (%s): invalid verdict: more than one JSON value", e.name, label)
	}
	if err := v.Validate(); err != nil {
		return Verdict{}, fmt.Errorf("stage %q (%s): invalid verdict: %v", e.name, label, err)
	}
	v.Stage = e.name // the configured name wins over anything the program claims
	if v.Provenance == "" {
		v.Provenance = "exec:" + label
	}
	return v, nil
}

// minimalEnv passes only what a well-behaved program needs, not the caller's
// whole environment (which may hold tokens).
func minimalEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	if s == "" {
		return "(no stderr)"
	}
	return s
}
