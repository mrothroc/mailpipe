package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/store"
)

func TestMailControlsAreInertInTerminalAndRoundTripInJSON(t *testing.T) {
	e := newEnv(t)
	const hostile = "Receipt\x1b]52;c;Zml4dHVyZQ==\a\x1b]8;;https://example.net\x1b\\link\x1b]8;;\x1b\\\u009b31m\u061c\u200e\u200f\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069\rhidden"
	s, err := store.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Upsert(context.Background(), "inbox", store.Record{GraphID: "fixture", Sender: "sender@example.org", SenderName: hostile, Subject: hostile, InternetMessageID: "<fixture@example.org>"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetBody(context.Background(), 1, "Hello, 世界 🌍\n"+hostile+"\nThanks\n"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	e.graph.mime = map[string]string{"fixture": "Subject: " + hostile + "\r\nX-Example: fixture\r\n\r\nbody"}
	for _, args := range [][]string{{"ls"}, {"digest"}, {"show", "1"}, {"show", "1", "--headers"}} {
		code, out, stderr := e.run(args...)
		if code != 0 {
			t.Fatalf("%v: %d %q", args, code, stderr)
		}
		for _, r := range out + stderr {
			if (r < 32 && r != '\n' && r != '\t') || (r >= 127 && r <= 159) || strings.ContainsRune("\u061c\u200e\u200f\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069", r) {
				t.Fatalf("%v emitted terminal control %U", args, r)
			}
		}
		if len(args) == 2 && args[0] == "show" && !strings.Contains(out, "Hello, 世界 🌍\n") {
			t.Fatal("unicode or body layout lost")
		}
	}
	code, out, stderr := e.run("ls", "--json")
	var row struct {
		Subject string `json:"subject"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &row) != nil || row.Subject != hostile {
		t.Fatalf("JSON changed: %q %q", out, stderr)
	}
}

func TestSaveCannotEscapeThroughSymlinkedDirectories(t *testing.T) {
	for _, nested := range []string{"", "/new/subfolder"} {
		t.Run(nested, func(t *testing.T) {
			e, root := savable(t)
			outside := t.TempDir()
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
				t.Fatal(err)
			}
			m := manifest(t, e, "{g1}\tlinked"+nested+"\treceipt.pdf")
			code, out, stderr := e.run("save", "--manifest", m, "--root", root, "--execute")
			if code == 0 || !strings.Contains(out, "\tfailed\t") {
				t.Fatalf("escape accepted: %d %q %q", code, out, stderr)
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("outside root modified: %v %v", entries, err)
			}
		})
	}
}

func TestSaveAllowsRelativeSymlinkInsideRoot(t *testing.T) {
	e, root := savable(t)
	if err := os.MkdirAll(filepath.Join(root, "records"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("records", filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	m := manifest(t, e, "{g1}\tlinked\treceipt.pdf")
	code, out, stderr := e.run("save", "--manifest", m, "--root", root, "--execute")
	if code != 0 {
		t.Fatalf("safe symlink rejected: %q %q", out, stderr)
	}
	if b, err := os.ReadFile(filepath.Join(root, "records/receipt.pdf")); err != nil || string(b) != attachedPDF {
		t.Fatalf("file: %q %v", b, err)
	}
}
