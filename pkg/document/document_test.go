package document_test

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/document"
)

var pdfBytes = []byte("%PDF-1.4 invented receipt\n%%EOF\n")
var pngBytes = []byte("\x89PNG\r\n\x1a\ninvented")

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// receipt is a typical store receipt: text and HTML alternatives, an inline
// logo, a remote tracking pixel and a PDF attachment.
var receipt = strings.ReplaceAll(`Message-ID: <r1@shop.example.com>
From: "Example Shop" <receipts@shop.example.com>
Date: Mon, 05 Jan 2026 10:00:00 -0800
Subject: =?UTF-8?Q?Your_receipt_=E2=80=94_order_42?=
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="mixed"

--mixed
Content-Type: multipart/related; boundary="related"

--related
Content-Type: multipart/alternative; boundary="alt"

--alt
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

Thanks for your order =E2=80=94 total $12.00

--alt
Content-Type: text/html; charset=utf-8
Content-Transfer-Encoding: base64

`+b64([]byte(`<html><head><link rel="stylesheet" href="https://track.example.net/s.css"></head><body style="background:url('https://track.example.net/bg.png')"><img src="cid:logo@shop"><img src="https://track.example.net/pixel.gif" width=1><p>Total $12.00</p></body></html>`))+`
--alt--

--related
Content-Type: image/png
Content-Transfer-Encoding: base64
Content-ID: <logo@shop>

`+b64(pngBytes)+`
--related--

--mixed
Content-Type: application/pdf; name="Receipt-42.pdf"
Content-Disposition: attachment; filename*=UTF-8''Receipt%E2%80%9442.pdf
Content-Transfer-Encoding: base64

`+b64(pdfBytes)+`
--mixed--
`, "\n", "\r\n")

func TestParseReadsHeadersBodiesAndAttachments(t *testing.T) {
	d, err := document.Parse([]byte(receipt))
	if err != nil {
		t.Fatal(err)
	}
	if d.MessageID != "<r1@shop.example.com>" || d.Subject != "Your receipt — order 42" ||
		d.From != `"Example Shop" <receipts@shop.example.com>` || !d.Date.Equal(time.Date(2026, 1, 5, 18, 0, 0, 0, time.UTC)) {
		t.Fatalf("headers %+v", d)
	}
	if !strings.Contains(d.Text, "Thanks for your order — total $12.00") || !strings.Contains(d.HTML, "<p>Total $12.00</p>") {
		t.Fatalf("text %q html %q", d.Text, d.HTML)
	}
	if len(d.Attachments) != 1 || d.Attachments[0].Filename != "Receipt—42.pdf" || d.Attachments[0].ContentType != "application/pdf" ||
		!bytes.Equal(d.Attachments[0].Data, pdfBytes) {
		t.Fatalf("attachments %+v", d.Attachments)
	}
	if got := d.PDFs(); len(got) != 1 {
		t.Fatalf("PDFs %d", len(got))
	}
}

func TestOfflineHTMLEmbedsInlineImagesAndDropsRemoteResources(t *testing.T) {
	d, _ := document.Parse([]byte(receipt))
	h := d.OfflineHTML()
	if !strings.Contains(h, `src="data:image/png;base64,`+b64(pngBytes)+`"`) {
		t.Fatalf("inline logo not embedded: %s", h)
	}
	if strings.Contains(h, "track.example.net") {
		t.Fatalf("a remote resource survived: %s", h)
	}
	for _, want := range []string{"Example Shop", "2026-01-05", "Your receipt — order 42", "<p>Total $12.00</p>"} {
		if !strings.Contains(h, want) {
			t.Fatalf("offline page lacks %q: %s", want, h)
		}
	}
}

func TestATextOnlyMessageRendersEscaped(t *testing.T) {
	raw := "From: a@example.org\r\nSubject: Note\r\nDate: Tue, 06 Jan 2026 09:00:00 +0000\r\nContent-Type: text/plain; charset=iso-8859-1\r\n\r\nCaf\xe9 <b>not bold</b>\r\n"
	d, err := document.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if d.Text != "Café <b>not bold</b>\r\n" || d.HTML != "" {
		t.Fatalf("text %q html %q", d.Text, d.HTML)
	}
	if h := d.OfflineHTML(); !strings.Contains(h, "Café &lt;b&gt;not bold&lt;/b&gt;") {
		t.Fatalf("offline %s", h)
	}
}

func TestParseRejectsWhatIsNotAMessage(t *testing.T) {
	if _, err := document.Parse([]byte("not a message")); err == nil {
		t.Fatal("garbage parsed")
	}
}

func TestOfflineHTMLFindsAnEscapedContentID(t *testing.T) {
	raw := strings.ReplaceAll(`From: a@example.org
Subject: Logo
MIME-Version: 1.0
Content-Type: multipart/related; boundary="r"

--r
Content-Type: text/html

<img src="cid:logo%40shop">
--r
Content-Type: image/png
Content-Transfer-Encoding: base64
Content-ID: <logo@shop>

`+b64(pngBytes)+`
--r--
`, "\n", "\r\n")
	d, err := document.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if h := d.OfflineHTML(); !strings.Contains(h, "data:image/png;base64,") {
		t.Fatalf("escaped cid not embedded: %s", h)
	}
}

// A named text part is an attachment, not the message's body.
func TestANamedTextPartIsAnAttachment(t *testing.T) {
	raw := strings.ReplaceAll(`From: a@example.org
Subject: Data
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="m"

--m
Content-Type: text/csv; name="data.csv"

a,b
--m
Content-Type: text/plain; name="notes.txt"

attached notes
--m
Content-Type: text/plain

the body
--m--
`, "\n", "\r\n")
	d, err := document.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(d.Text) != "the body" || len(d.Attachments) != 2 || d.Attachments[1].Filename != "notes.txt" {
		t.Fatalf("text %q attachments %+v", d.Text, d.Attachments)
	}
}

// Relative and in-page links must not point at the page's temporary file: a
// base of about:blank keeps renders identical and leaks no local path.
func TestOfflineHTMLPinsLinksToABlankBase(t *testing.T) {
	d, _ := document.Parse([]byte(receipt))
	if h := d.OfflineHTML(); strings.Count(h, `<base href="about:blank">`) != 1 {
		t.Fatalf("no blank base: %s", h)
	}
	text, _ := document.Parse([]byte("From: a@example.org\r\nSubject: t\r\n\r\nhi\r\n"))
	if h := text.OfflineHTML(); strings.Count(h, `<base href="about:blank">`) != 1 {
		t.Fatalf("no blank base on a text page: %s", h)
	}
}

func TestOfflineHTMLReplacesTheEmailsBaseAndAddsAMissingHead(t *testing.T) {
	for name, body := range map[string]string{
		"own base":  `<html><head><base href="https://evil.example.net/"></head><body><a href="x">x</a></body></html>`,
		"no head":   `<html><body><a href="#y">y</a></body></html>`,
		"body only": `<body><a href="#y">y</a></body>`,
	} {
		d, err := document.Parse([]byte("From: a@example.org\r\nSubject: s\r\nContent-Type: text/html\r\n\r\n" + body + "\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		h := d.OfflineHTML()
		if strings.Count(h, "<base ") != 1 || !strings.Contains(h, `<base href="about:blank">`) || strings.Contains(h, "evil.example.net") {
			t.Errorf("%s: %s", name, h)
		}
	}
}
