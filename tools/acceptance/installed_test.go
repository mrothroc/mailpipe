package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstalledSmokeEnvironment(t *testing.T) {
	// A strict test-owned executable records every argv/environment before refusing.
	d := temp(t)
	bin := filepath.Join(d, "mailpipe")
	record := filepath.Join(d, "record")
	script := `#!/bin/bash
set -eu
for name in HOME MAILPIPE_CONFIG MAILPIPE_DB MAILPIPE_OUT_DIR MAILPIPE_STAGES MAILPIPE_TEMPLATES; do
 value="${!name:-}"
 case "$value" in "$SMOKE_ROOT"*) ;; *) exit 71;; esac
done
test -z "${MAILPIPE_CLIENT_ID:-}"
test -z "${OPENROUTER_API_KEY:-}"
printf '%s\n' "$*" >> "$SMOKE_RECORD"
case "$*" in *'auth login'*|*'auth status'*|*sync*|'account capabilities --json') exit 72;; esac
exit 73
`
	script = strings.ReplaceAll(script, "$SMOKE_ROOT", d+"/smoke")
	script = strings.ReplaceAll(script, "$SMOKE_RECORD", record)
	os.WriteFile(bin, []byte(script), 0700)
	c := exec.Command(tool(t), "installed-smoke", "--binary", bin, "--root", filepath.Join(d, "smoke"), "--examples", filepath.Join(d, "examples"))
	c.Env = append(os.Environ(), "SMOKE_ROOT="+filepath.Join(d, "smoke"), "SMOKE_RECORD="+record, "MAILPIPE_CLIENT_ID=canary@example.org", "OPENROUTER_API_KEY=canary@example.org")
	b, e := c.CombinedOutput()
	if e == nil {
		t.Fatal("failure was ignored")
	}
	if bytes.Contains(b, []byte("canary@example.org")) {
		t.Fatal("diagnostic leaked inherited canary")
	}
	// Missing examples must fail without invoking any CLI, then a complete invented example set reaches help.
	if _, e = os.Stat(record); !os.IsNotExist(e) {
		t.Fatal("CLI invoked before example validation")
	}
	os.MkdirAll(filepath.Join(d, "examples/stages"), 0700)
	os.WriteFile(filepath.Join(d, "examples/stages.example.json"), []byte(`{}`), 0600)
	os.WriteFile(filepath.Join(d, "examples/templates.example.json"), []byte(`{}`), 0600)
	os.WriteFile(filepath.Join(d, "examples/stages/newsletter.sh"), []byte("#!/bin/sh\n"), 0700)
	c = exec.Command(tool(t), "installed-smoke", "--binary", bin, "--root", filepath.Join(d, "smoke2"), "--examples", filepath.Join(d, "examples"))
	c.Env = append(os.Environ(), "SMOKE_ROOT="+filepath.Join(d, "smoke2"), "SMOKE_RECORD="+record, "MAILPIPE_CLIENT_ID=canary@example.org", "OPENROUTER_API_KEY=canary@example.org")
	b, e = c.CombinedOutput()
	if e == nil {
		t.Fatal("CLI failure ignored")
	}
	r, _ := os.ReadFile(record)
	if strings.TrimSpace(string(r)) != "--help" {
		t.Fatalf("first CLI lacks isolation or invokes auth: %s %s", r, b)
	}
}

func TestInstalledSmokeRealCLI(t *testing.T) {
	d := temp(t)
	bin := filepath.Join(d, "mailpipe")
	c := exec.Command("go", "build", "-o", bin, "../../cmd/mailpipe")
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatalf("public CLI build: %v %s", e, b)
	}
	sentinel := filepath.Join(d, "sentinel")
	os.Mkdir(sentinel, 0700)
	called := filepath.Join(d, "forbidden-called")
	os.WriteFile(filepath.Join(sentinel, "security"), []byte("#!/bin/sh\nprintf forbidden > '"+called+"'\nexit 91\n"), 0700)
	t.Setenv("PATH", sentinel+":"+os.Getenv("PATH"))
	t.Setenv("MAILPIPE_CLIENT_ID", "canary@example.org")
	t.Setenv("OPENROUTER_API_KEY", "canary@example.org")
	examples, e := filepath.Abs("../../examples")
	if e != nil {
		t.Fatal(e)
	}
	if e := installedSmoke(context.Background(), bin, filepath.Join(d, "smoke"), examples); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(called); !os.IsNotExist(e) {
		t.Fatal("synthetic smoke attempted Keychain")
	}
}
