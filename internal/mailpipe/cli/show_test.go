package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

const threaded = "Reply.\n\nOn Oct 23, 2026, at 5:14 PM, A <a@example.org> wrote:\n> old\n> older\n"

func TestShowNoQuotesCutsTheReplyHistory(t *testing.T) {
	e := newEnv(t)
	e.graph.body = threaded
	e.run("sync")
	_, ls, _ := e.run("ls", "--from", "campaign")
	n := firstN(t, ls)
	code, out, errOut := e.run("show", n, "--no-quotes")
	if code != 0 || !strings.HasSuffix(out, "\n\nReply.\n[quoted history removed: 3 lines. Try: mailpipe show "+n+"]\n") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	_, out, _ = e.run("show", n)
	if !strings.HasSuffix(out, threaded) {
		t.Fatalf("without --no-quotes the history must stay: %q", out)
	}
	_, out, _ = e.run("show", n, "--no-quotes", "--json")
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil || m["body"] != "Reply.\n" || m["quoted_lines_removed"] != 3.0 {
		t.Fatalf("json %q (%v)", out, err)
	}
}

func TestShowReportsOneRowAndDescribesItsOwnFlags(t *testing.T) {
	e := newEnv(t)
	e.run("sync")
	_, ls, _ := e.run("ls")
	if _, _, errOut := e.run("show", firstN(t, ls)); !strings.Contains(errOut, "| rows=1 |") {
		t.Fatalf("footer %q", errOut)
	}
	_, help, _ := e.run("show", "--help")
	for _, want := range []string{"emit the message as one JSON object, body included", "maximum body lines on stdout"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help lacks %q: %s", want, help)
		}
	}
	if strings.Contains(help, "per row") || strings.Contains(help, "data rows") {
		t.Fatalf("show's help still describes rows: %s", help)
	}
}
