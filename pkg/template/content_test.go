package template_test

import (
	"bytes"
	"context"
	"github.com/mrothroc/mailpipe/pkg/document"
	"github.com/mrothroc/mailpipe/pkg/stage"
	"strings"
	"testing"
)

func TestHTMLReadableFingerprintAndTrailingDate(t *testing.T) {
	set := mustParse(t, `{"templates":[{"id":"record","senders":["records@example.org"],"describe":"Example record","subject":"Record","anchors":["Complete record","2030-12-31"]}]}`)
	var text bytes.Buffer
	if e := document.HTMLReadable(context.Background(), strings.NewReader(`<p>Complete record</p><script>discard()</script><div>2030-12-31</div>`), &text, document.Limits{}); e != nil {
		t.Fatal(e)
	}
	msg := stage.Message{Sender: "records@example.org", Subject: "Record", Received: "2030-01-01T00:00:00Z", Body: text.String()}
	result := set.For(msg.Sender)[0].Match(msg, pacific)
	if !result.OK {
		t.Fatalf("shared readable fingerprint failed %+v", result)
	}
	if !strings.Contains(stage.State(msg), "2030-12-31") || strings.Contains(stage.State(msg), "discard()") {
		t.Fatal("model and fingerprint content diverged")
	}
}
