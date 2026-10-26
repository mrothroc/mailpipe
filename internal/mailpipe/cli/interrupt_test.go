package cli_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mrothroc/agentio"
)

func TestAnInterruptionIsReportedAsOneWithARerunHint(t *testing.T) {
	e := newEnv(t)
	e.auth.err = fmt.Errorf("reading the token cache: keychain: read interrupted: %w", context.Canceled)
	code, _, errOut := e.run("auth", "status")
	if code != agentio.ExitRuntime {
		t.Fatalf("code %d, want %d; err %q", code, agentio.ExitRuntime, errOut)
	}
	if !strings.Contains(errOut, "interrupted") || !strings.Contains(errOut, "Try: rerun the same command to continue") ||
		strings.Contains(errOut, "Try: mailpipe auth login") {
		t.Fatalf("err %q", errOut)
	}
}

func TestAnInterruptedLoginIsNotAStaleSignIn(t *testing.T) {
	e := newEnv(t)
	e.auth.err = fmt.Errorf("sign-in failed: %w", context.Canceled)
	code, _, errOut := e.run("auth", "login")
	if code != agentio.ExitRuntime || !strings.Contains(errOut, "Try: rerun the same command to continue") {
		t.Fatalf("code %d err %q", code, errOut)
	}
}
