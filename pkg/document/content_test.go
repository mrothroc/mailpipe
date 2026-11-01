package document_test

import (
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/document"
)

func TestCompleteMixedBody(t *testing.T) {
	raw := "From: a@example.org\r\nSubject: dates\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nFirst event\r\n--x\r\nContent-Type: text/plain\r\n\r\nFinal deadline 2030-12-31\r\n--x--\r\n"
	d, err := document.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Text, "First event") || !strings.Contains(d.Text, "Final deadline 2030-12-31") {
		t.Fatalf("incomplete mixed text %q", d.Text)
	}
}

func TestUnknownCharsetRetainedForReview(t *testing.T) {
	_, err := document.Parse([]byte("From: a@example.org\r\nSubject: test\r\nContent-Type: text/plain; charset=x-invented\r\n\r\nbody"))
	if err == nil {
		t.Fatal("unknown charset silently accepted")
	}
}

func TestInlineNestedMessageKeepsHeadersAndDates(t *testing.T) {
	raw := "From: a@example.org\r\nSubject: wrapper\r\nContent-Type: message/rfc822\r\n\r\nFrom: b@example.net\r\nSubject: Important deadline\r\nDate: Tue, 31 Dec 2030 12:00:00 +0000\r\nContent-Type: text/plain\r\n\r\nComplete nested body"
	d, e := document.Parse([]byte(raw))
	if e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"Important deadline", "31 Dec 2030", "Complete nested body"} {
		if !strings.Contains(d.Text, want) {
			t.Fatalf("nested content missing %q: %q", want, d.Text)
		}
	}
}
