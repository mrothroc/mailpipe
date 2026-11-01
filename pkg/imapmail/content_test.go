package imapmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/document"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func TestIMAPMIMERead(t *testing.T) {
	s, cfg := localScript(t)
	pdf := []byte("%PDF-1.4 invented record\n%%EOF\n")
	png := []byte("\x89PNG\r\n\x1a\ninvented")
	raw := "From: sender@example.org\r\nSubject: dates\r\nContent-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/html; charset=windows-1252\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" + strings.Repeat("<p>Invented routine text =80.</p>\r\n", 5000) + "<p>First event</p><script>ignore()</script><p>Final deadline 2030-12-31</p>\r\n--m\r\nContent-Type: image/png\r\nContent-ID: <logo@example.org>\r\nContent-Disposition: inline\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString(png) + "\r\n--m\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=record.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString(pdf) + "\r\n--m--\r\n"
	s.messages = []scriptMessage{{1, raw, `\Seen`}}
	c := openClient(t, cfg)
	ref := occurrence(c.Account(), inbox(t, c), 10, 1, 1).Ref
	text, e := c.Text(context.Background(), ref)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(text, "Final deadline 2030-12-31") || !strings.Contains(text, "€") || strings.Contains(text, "ignore()") {
		t.Fatalf("incomplete/unsafe readable text %q", text)
	}
	var out bytes.Buffer
	if e = c.WriteMIME(context.Background(), ref, &out); e != nil || out.String() != raw {
		t.Fatalf("MIME %v %q", e, out.String())
	}
	scope, e := document.NewScope(document.Limits{})
	if e != nil {
		t.Fatal(e)
	}
	source, e := document.OpenSource(context.Background(), c, ref, scope)
	if e != nil {
		t.Fatal(e)
	}
	reader, e := source.Open()
	if e != nil {
		t.Fatal(e)
	}
	doc, e := document.ParseReader(context.Background(), reader, scope)
	e = errors.Join(e, reader.Close())
	if e != nil {
		t.Fatal(e)
	}
	if len(doc.PDFs()) != 1 || len(doc.Inline) != 1 {
		t.Fatal("multipart parts missing")
	}
	for _, part := range []struct {
		blob *document.Blob
		want []byte
	}{{doc.PDFs()[0].Data, pdf}, {doc.Inline["logo@example.org"].Data, png}} {
		r, e := part.blob.Open()
		if e != nil {
			t.Fatal(e)
		}
		got, e := io.ReadAll(r)
		e = errors.Join(e, r.Close())
		if e != nil || !bytes.Equal(got, part.want) {
			t.Fatalf("attachment/inline bytes changed %v", e)
		}
	}
	if e = errors.Join(doc.Close(), source.Close(), scope.Close()); e != nil {
		t.Fatal(e)
	}
	s.mu.Lock()
	chunks := s.sourceFetches
	s.mu.Unlock()
	if chunks < 6 {
		t.Fatalf("fixture did not exercise multi-chunk source: %d", chunks)
	}
	bad := ref
	bad.AccountID = "other-account"
	if _, e = c.Text(context.Background(), bad); !errors.Is(e, mailbox.ErrAccountMismatch) {
		t.Fatalf("account %v", e)
	}
	s.mu.Lock()
	s.validity++
	s.mu.Unlock()
	if _, e = c.Text(context.Background(), ref); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("stale %v", e)
	}
	s.audit(t)
}
func TestIMAPContentResourceBounds(t *testing.T) {
	s, cfg := localScript(t)
	cfg.SourceBytes = 32
	c := openClient(t, cfg)
	ref := occurrence(c.Account(), inbox(t, c), 10, 1, 1).Ref
	text, e := c.Text(context.Background(), ref)
	if !errors.Is(e, document.ErrIncomplete) || text != "" {
		t.Fatalf("oversize %q %v", text, e)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.messages[1].flags != `\Seen` {
		t.Fatal("read flags changed")
	}
}

func TestIMAPMetadataWithoutRepeatedSourceDownloads(t *testing.T) {
	ctx := context.Background()
	s, cfg := localScript(t)
	s.structureReply = `(("TEXT" "PLAIN" NIL NIL NIL "7BIT" 3 1 NIL NIL NIL NIL)("APPLICATION" "PDF" ("NAME" "record.pdf") NIL NIL "BASE64" 12 NIL ("ATTACHMENT" ("FILENAME" "record.pdf")) NIL NIL) "MIXED" ("BOUNDARY" "x") NIL NIL NIL)`
	c := openClient(t, cfg)
	db := openMirror(t, cfg)
	folder := inbox(t, c)
	for range 2 {
		if _, e := c.SyncCollection(ctx, db, folder, ContentMetadataLoader{}, false); e != nil {
			t.Fatal(e)
		}
	}
	ref := occurrence(c.Account(), folder, 10, 1, 1).Ref
	metadata, e := c.Metadata(ctx, ref)
	if e != nil || !metadata.HasAttachments || metadata.Preview != "" {
		t.Fatalf("metadata %+v %v", metadata, e)
	}
	state, e := c.Inspect(ctx, ref)
	if e != nil || state.Ref != ref || len(state.Memberships) != 1 {
		t.Fatalf("inspect %+v %v", state, e)
	}
	s.mu.Lock()
	downloads := s.sourceFetches
	s.structureReply = `("TEXT" "PLAIN" NIL NIL NIL "7BIT" 3 1)`
	s.mu.Unlock()
	if downloads != 0 {
		t.Fatalf("metadata downloaded full source %d times", downloads)
	}
	if _, e = c.Metadata(ctx, ref); e == nil {
		t.Fatal("missing disposition evidence published false")
	}
}

func TestContentMetadataInlineAndAttachmentEvidence(t *testing.T) {
	s, cfg := localScript(t)
	c := openClient(t, cfg)
	ref := occurrence(c.Account(), inbox(t, c), 10, 1, 1).Ref
	for _, test := range []struct {
		structure string
		attached  bool
	}{
		{`("IMAGE" "PNG" ("NAME" "logo.png") "logo@example.org" NIL "BASE64" 16 NIL ("INLINE" ("FILENAME" "logo.png")) NIL NIL)`, false},
		{`("IMAGE" "PNG" ("NAME" "logo.png") "logo@example.org" NIL "BASE64" 16 NIL ("ATTACHMENT" ("FILENAME" "logo.png")) NIL NIL)`, true},
		{`("APPLICATION" "PDF" ("NAME" "record.pdf") NIL NIL "BASE64" 16 NIL ("INLINE" ("FILENAME" "record.pdf")) NIL NIL)`, true},
	} {
		s.mu.Lock()
		s.structureReply = test.structure
		s.mu.Unlock()
		metadata, e := c.Metadata(context.Background(), ref)
		if e != nil || metadata.HasAttachments != test.attached {
			t.Fatalf("attachment=%v want=%v error=%v", metadata.HasAttachments, test.attached, e)
		}
	}
}

func TestContentMetadataRejectsMalformedStructure(t *testing.T) {
	s, cfg := localScript(t)
	c := openClient(t, cfg)
	ref := occurrence(c.Account(), inbox(t, c), 10, 1, 1).Ref
	for _, structure := range []string{
		`("" "PLAIN" NIL NIL NIL "7BIT" 3 1 NIL NIL NIL NIL)`,
		`("TEXT" "PLAIN" NIL NIL NIL "7BIT" 3 NIL NIL NIL NIL NIL)`,
		`("TEXT" "PLAIN" NIL NIL NIL NIL 3 1 NIL NIL NIL NIL)`,
		`("IMAGE" "PNG" NIL (NIL) NIL "BASE64" 16 NIL NIL NIL NIL)`,
		`("MESSAGE" "RFC822" NIL NIL NIL "7BIT" 100 NIL ("TEXT" "PLAIN" NIL NIL NIL "7BIT" 3 1 NIL NIL NIL NIL) 1 NIL NIL NIL NIL)`,
	} {
		s.mu.Lock()
		s.structureReply = structure
		s.mu.Unlock()
		if _, e := c.Metadata(context.Background(), ref); e == nil {
			t.Fatalf("malformed structure published successful metadata: %s", structure)
		}
	}
}
func TestContentMetadataLiteralParametersAndExactNames(t *testing.T) {
	s, cfg := localScript(t)
	c := openClient(t, cfg)
	ref := occurrence(c.Account(), inbox(t, c), 10, 1, 1).Ref
	for _, test := range []struct {
		structure string
		attached  bool
	}{
		{"(\"APPLICATION\" \"PDF\" (\"NAME\" {10}\r\nrecord.pdf) NIL NIL \"BASE64\" 16 NIL (\"ATTACHMENT\" (\"FILENAME\" {10}\r\nrecord.pdf)) NIL NIL)", true},
		{`("TEXT" "PLAIN" ("NAMEPLATE" "custom-value") NIL NIL "7BIT" 3 1 NIL NIL NIL NIL)`, false},
		{`("TEXT" "PLAIN" ("NAME" "") NIL NIL "7BIT" 3 1 NIL NIL NIL NIL)`, false},
	} {
		s.mu.Lock()
		s.structureReply = test.structure
		s.mu.Unlock()
		m, e := c.Metadata(context.Background(), ref)
		if e != nil || m.HasAttachments != test.attached {
			t.Fatalf("attachment=%v want=%v err=%v", m.HasAttachments, test.attached, e)
		}
	}
}
