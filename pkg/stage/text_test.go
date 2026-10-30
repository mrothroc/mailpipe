package stage_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mrothroc/mailpipe/pkg/stage"
)

func graphStyleBody(prose string) string {
	var b strings.Builder
	b.WriteString("\r\n\r\n[https://cdn.example.org/assets/" + strings.Repeat("a1b2c3", 20) + "/logo.jpg]\r\n")
	for i := 0; i < 60; i++ {
		b.WriteString("[https://track.example.org/click?id=" + strings.Repeat("x", 120) + "]\r\n\r\n")
	}
	b.WriteString(prose)
	b.WriteString(" Read more<https://news.example.org/story/" + strings.Repeat("z", 100) + ">\r\n")
	return b.String()
}

// Graph's text rendering spells every image and link out as a URL. The model
// must read the prose, not thousands of characters of tracking URLs.
func TestStateDropsURLNoise(t *testing.T) {
	prose := "Chip in $5 today to defend democracy before midnight."
	m := msg()
	m.Body = graphStyleBody(prose)
	state := stage.State(m)
	if !strings.Contains(state, prose) {
		t.Fatalf("the appeal was cut off; state is %d chars:\n%.300s", len(state), state)
	}
	if strings.Contains(state, "https://") {
		t.Fatalf("URL noise left in the state:\n%.300s", state)
	}
	if !strings.Contains(state, "Read more") {
		t.Fatalf("link text was dropped with its URL:\n%s", state)
	}
}

func TestStateKeepsOrdinaryTextAndCollapsesBlankRuns(t *testing.T) {
	m := msg()
	m.Body = "Hi Alex,\r\n\r\n\r\n\r\nDinner Tuesday?   See you at 7.\r\n"
	state := stage.State(m)
	if !strings.HasSuffix(state, "Hi Alex,\n\nDinner Tuesday? See you at 7.") {
		t.Fatalf("state = %q", state)
	}
}

func TestStatePreservesLongMessagesAndTheirFinalDeadline(t *testing.T) {
	m := msg()
	m.Body = strings.Repeat("Reference material. ", 6000) + "Final deadline: 2031-06-20."
	if !strings.HasSuffix(stage.State(m), "Final deadline: 2031-06-20.") {
		t.Fatal("the model must receive the deadline at the end of a long message")
	}
}

func TestStateNeverSplitsACharacter(t *testing.T) {
	m := msg()
	m.Body = strings.Repeat("é", 4000) // 8000 bytes
	if s := stage.State(m); !utf8.ValidString(s) {
		t.Fatal("truncation split a multi-byte character")
	}
}

func TestWithoutQuotedCutsAtTheFirstReplyMarker(t *testing.T) {
	cases := map[string]struct{ body, kept string }{
		"outlook": {"Got it.\n\n________________________________\nFrom: owner@example.com\nSent: 2026-10-24T00:54:52Z\nTo: a@example.org\n\nolder\n",
			"Got it.\n"},
		"outlook without a separator": {"Thanks\nFrom: owner@example.com\nDate: Fri, 23 Oct 2026\n\nolder\n", "Thanks\n"},
		"on wrote":                    {"Yes.\n\nOn Oct 23, 2026, at 5:14 PM, A <a@example.org> wrote:\n> older\n", "Yes.\n"},
		"on wrote over two lines":     {"Yes.\nOn Sat, Oct 24, 2026 at 1:14 AM A Sender <\na@example.org> wrote:\n> older\n", "Yes.\n"},
		"original message":            {"Fine\n-----Original Message-----\nFrom: a@example.org\n", "Fine\n"},
		"trailing quote block":        {"Agreed\n\n> one\n>\n> two\n", "Agreed\n"},
	}
	for name, c := range cases {
		kept, removed := stage.WithoutQuoted(c.body)
		if kept != c.kept || removed == 0 {
			t.Fatalf("%s: kept %q (removed %d), want %q", name, kept, removed, c.kept)
		}
	}
}

func TestWithoutQuotedLeavesOrdinaryTextAlone(t *testing.T) {
	for _, body := range []string{
		"From: the desk of the editor\nwe write weekly\n", // a From: line without Sent: or Date:
		"> a quote\nmy reply below it\n",                  // an interleaved quote is part of the reply
		"no quotes at all",
	} {
		if kept, removed := stage.WithoutQuoted(body); kept != body || removed != 0 {
			t.Fatalf("%q: kept %q, removed %d", body, kept, removed)
		}
	}
}
