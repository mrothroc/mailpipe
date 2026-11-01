package cli_test

import (
	"errors"
	"fmt"
	"github.com/mrothroc/mailpipe/pkg/document"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateHookGetsOnlyScopedMIMEAndLeavesNoSourceFile(t *testing.T) {
	e := newEnv(t)
	if code, _, errOut := e.run("sync"); code != 0 {
		t.Fatal(errOut)
	}
	e.graph.mime = map[string]string{"g1": "From: sender@example.org\r\nSubject: Invented\r\n\r\nExample"}
	dir := t.TempDir()
	marker := filepath.Join(dir, "path")
	program := filepath.Join(dir, "classifier.sh")
	script := `#!/bin/sh
input=$(cat)
path=$(printf '%s' "$input" | sed -n 's/.*"source_path":"\([^"]*\)".*/\1/p')
test -f "$path" || exit 3
printf '%s' "$path" > "$1"
printf '{"labels":["review"]}'
`
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "stages.json")
	// g1's sender is part of the generic fake mailbox fixture.
	config := fmt.Sprintf(`{"stages":[{"name":"private-check","type":"hook","command":[%q,%q],"senders":["info@campaign.example.org"],"needs_source":true}]}`, program, marker)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAILPIPE_STAGES", configPath)
	code, out, errOut := e.run("run", "private-check")
	if code != 0 || !strings.Contains(out, "review") || e.graph.rawCalls.Load() != 1 {
		t.Fatalf("code %d out %s err %s calls %d", code, out, errOut, e.graph.rawCalls.Load())
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(string(b)); !os.IsNotExist(err) {
		t.Fatalf("source remains: %v", err)
	}
}

func TestContentProtectionPersistsDespiteCleanupRunError(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		t.Run(fmt.Sprint(cleanup), func(t *testing.T) {
			e := newEnv(t)
			if code, _, stderr := e.run("sync"); code != 0 {
				t.Fatal(stderr)
			}
			marker := filepath.Join(t.TempDir(), "ran")
			program := filepath.Join(t.TempDir(), "hook.sh")
			if err := os.WriteFile(program, []byte("#!/bin/sh\ntouch \"$1\"\nprintf '{\"labels\":[\"unsafe\"]}'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			config := fmt.Sprintf(`{"stages":[{"name":"content-check","type":"hook","command":[%q,%q],"senders":["info@campaign.example.org"],"needs_source":true}]}`, program, marker)
			path := filepath.Join(t.TempDir(), "stages.json")
			if err := os.WriteFile(path, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MAILPIPE_STAGES", path)
			failure := document.ErrIncomplete
			if cleanup {
				failure = errors.Join(failure, document.ErrCleanup)
			}
			e.graph.rawErr = map[string]error{"g1": failure}
			code, out, stderr := e.run("run", "content-check")
			if cleanup && code == 0 {
				t.Fatal("cleanup run reported success", stderr)
			}
			if !cleanup && code != 0 {
				t.Fatal("protective review failed", stderr)
			}
			if !strings.Contains(out, "review") || !strings.Contains(out, "keep") {
				t.Fatal("protection not reported", out, stderr)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("hook ran on incomplete source", err)
			}
			n := ""
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "\treview\t") {
					n = strings.Split(line, "\t")[0]
					break
				}
			}
			if n == "" {
				t.Fatal("review row missing", out)
			}
			code, verdicts, stderr := e.run("verdicts", n)
			if code != 0 || !strings.Contains(verdicts, "review") || !strings.Contains(verdicts, "keep") {
				t.Fatal("protective verdict not persisted", verdicts, stderr)
			}
		})
	}
}
