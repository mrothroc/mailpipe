// Package document reads an email's MIME source into the parts a filing step
// needs: its headers, text and HTML bodies, inline images and attachments,
// and an offline HTML page for rendering it (inline images embedded, remote
// resources removed, so rendering never fetches anything).
package document

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Part is an attachment or an inline image.
type Part struct {
	Filename    string
	ContentType string
	Data        []byte
}

// Doc is a parsed message.
type Doc struct {
	MessageID   string
	From        string
	Subject     string
	Date        time.Time // zero when the header is missing or unreadable
	Text        string    // the first text/plain body
	HTML        string    // the first text/html body
	Attachments []Part
	inline      map[string]Part // by Content-ID, without the angle brackets
}

// maxDepth bounds multipart nesting, against a hostile message.
const maxDepth = 20

var words = mime.WordDecoder{CharsetReader: func(charset string, r io.Reader) (io.Reader, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	s, ok := decodeCharset(b, charset)
	if !ok {
		return nil, fmt.Errorf("unsupported charset %q", charset)
	}
	return strings.NewReader(s), nil
}}

// Parse reads a message's MIME source.
func Parse(raw []byte) (Doc, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Doc{}, fmt.Errorf("read message: %w", err)
	}
	header := func(k string) string {
		v := m.Header.Get(k)
		if d, err := words.DecodeHeader(v); err == nil {
			return d
		}
		return v
	}
	if m.Header.Get("From") == "" && m.Header.Get("Subject") == "" {
		return Doc{}, errors.New("read message: no From or Subject header")
	}
	d := Doc{MessageID: strings.TrimSpace(m.Header.Get("Message-Id")), From: header("From"), Subject: header("Subject"),
		inline: map[string]Part{}}
	if t, err := m.Header.Date(); err == nil {
		d.Date = t
	}
	if err := d.walk(textproto.MIMEHeader(m.Header), m.Body, 0); err != nil {
		return Doc{}, err
	}
	return d, nil
}

func (d *Doc) walk(h textproto.MIMEHeader, body io.Reader, depth int) error {
	if depth > maxDepth {
		return errors.New("read message: multipart nesting too deep")
	}
	ct, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		ct, params = "text/plain", map[string]string{}
	}
	if strings.HasPrefix(ct, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextRawPart()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("read message part: %w", err)
			}
			if err := d.walk(p.Header, p, depth+1); err != nil {
				return err
			}
		}
	}
	data, err := io.ReadAll(decoded(h.Get("Content-Transfer-Encoding"), body))
	if err != nil {
		return fmt.Errorf("read message part: %w", err)
	}
	disposition, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	filename := dparams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	if filename != "" {
		if f, err := words.DecodeHeader(filename); err == nil {
			filename = f
		}
	}
	attached := disposition == "attachment"
	cid := strings.Trim(strings.TrimSpace(h.Get("Content-Id")), "<>")
	switch {
	case ct == "text/plain" && !attached && filename == "" && d.Text == "":
		d.Text, _ = decodeCharset(data, params["charset"])
	case ct == "text/html" && !attached && filename == "" && d.HTML == "":
		d.HTML, _ = decodeCharset(data, params["charset"])
	case cid != "" && strings.HasPrefix(ct, "image/") && !attached:
		d.inline[cid] = Part{Filename: filename, ContentType: ct, Data: data}
	case attached || filename != "":
		d.Attachments = append(d.Attachments, Part{Filename: filename, ContentType: ct, Data: data})
	}
	return nil
}

func decoded(encoding string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, r)
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	}
	return r
}

// cp1252 maps the Windows-1252 bytes 0x80-0x9F that differ from Latin-1.
var cp1252 = map[byte]rune{0x80: '€', 0x82: '‚', 0x83: 'ƒ', 0x84: '„', 0x85: '…', 0x86: '†', 0x87: '‡', 0x88: 'ˆ', 0x89: '‰',
	0x8A: 'Š', 0x8B: '‹', 0x8C: 'Œ', 0x8E: 'Ž', 0x91: '‘', 0x92: '’', 0x93: '“', 0x94: '”', 0x95: '•', 0x96: '–', 0x97: '—',
	0x98: '˜', 0x99: '™', 0x9A: 'š', 0x9B: '›', 0x9C: 'œ', 0x9E: 'ž', 0x9F: 'Ÿ'}

// decodeCharset converts b from charset to UTF-8, reporting whether the
// charset is one it knows; an unknown one is passed through.
func decodeCharset(b []byte, charset string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return string(b), true
	case "iso-8859-1", "latin1", "latin-1", "windows-1252", "cp1252":
		var s strings.Builder
		for _, c := range b {
			if r, ok := cp1252[c]; ok {
				s.WriteRune(r)
			} else {
				s.WriteRune(rune(c))
			}
		}
		return s.String(), true
	}
	return string(b), false
}

// PDFs returns the PDF attachments.
func (d Doc) PDFs() []Part {
	var out []Part
	for _, a := range d.Attachments {
		if a.ContentType == "application/pdf" || strings.HasSuffix(strings.ToLower(a.Filename), ".pdf") {
			out = append(out, a)
		}
	}
	return out
}

var (
	remoteAttr   = regexp.MustCompile(`(?i)(\s(?:src|background|srcset|poster)\s*=\s*)("https?:[^"]*"|'https?:[^']*'|https?:[^\s>]+)`)
	remoteURL    = regexp.MustCompile(`(?i)url\(\s*['"]?https?:[^)]*\)`)
	remoteLink   = regexp.MustCompile(`(?i)<link\b[^>]*href\s*=\s*["']?https?:[^>]*>`)
	remoteImport = regexp.MustCompile(`(?i)@import\s+(?:url\()?\s*["']?https?:[^;]*;?`)
	cidRef       = regexp.MustCompile(`(?i)cid:([^"')\s>]+)`)
	bodyTag      = regexp.MustCompile(`(?i)<body\b[^>]*>`)
	headTag      = regexp.MustCompile(`(?i)<head\b[^>]*>`)
	htmlTag      = regexp.MustCompile(`(?i)<html\b[^>]*>`)
	baseTag      = regexp.MustCompile(`(?i)<base\b[^>]*>`)
)

// OfflineHTML returns a page for rendering the message: a header block (from,
// date, subject), then its HTML body (or its text, escaped), with inline
// images embedded as data URIs and every remote resource removed.
func (d Doc) OfflineHTML() string {
	date := ""
	if !d.Date.IsZero() {
		date = d.Date.Format("2006-01-02 15:04 MST")
	}
	head := fmt.Sprintf(`<div style="font:12px sans-serif;border-bottom:1px solid #999;margin:0 0 12px;padding:0 0 6px">`+
		`<b>From:</b> %s<br><b>Date:</b> %s<br><b>Subject:</b> %s</div>`,
		html.EscapeString(d.From), html.EscapeString(date), html.EscapeString(d.Subject))
	if d.HTML == "" {
		return `<!doctype html><html><head><meta charset="utf-8">` + blankBase + `</head><body>` + head +
			`<pre style="white-space:pre-wrap;font:13px monospace">` + html.EscapeString(d.Text) + `</pre></body></html>`
	}
	h := remoteLink.ReplaceAllString(baseTag.ReplaceAllString(d.HTML, ""), "")
	h = remoteImport.ReplaceAllString(h, "")
	h = remoteURL.ReplaceAllString(h, "none")
	h = remoteAttr.ReplaceAllString(h, `$1""`)
	h = cidRef.ReplaceAllStringFunc(h, func(ref string) string {
		id := ref[len("cid:"):]
		if u, err := url.PathUnescape(id); err == nil {
			id = u // "cid:logo%40shop" names Content-ID <logo@shop>
		}
		p, ok := d.inline[id]
		if !ok {
			return "about:blank"
		}
		return "data:" + p.ContentType + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
	})
	if loc := bodyTag.FindStringIndex(h); loc != nil {
		h = h[:loc[1]] + head + h[loc[1]:]
		if loc := headTag.FindStringIndex(h); loc != nil {
			return h[:loc[1]] + blankBase + h[loc[1]:]
		}
		if loc := htmlTag.FindStringIndex(h); loc != nil {
			return h[:loc[1]] + "<head>" + blankBase + "</head>" + h[loc[1]:]
		}
		return blankBase + h
	}
	return `<!doctype html><html><head><meta charset="utf-8">` + blankBase + `</head><body>` + head + h + `</body></html>`
}

// blankBase pins relative and in-page links to about:blank. Otherwise they
// resolve against the page's temporary file, whose random path then appears
// in the PDF: renders would differ, and a local path would leak.
const blankBase = `<base href="about:blank">`
