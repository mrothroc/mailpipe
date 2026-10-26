package cli_test

import (
	"fmt"
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
