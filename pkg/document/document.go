// Package document reads an email's MIME source into the parts a filing step
// needs: its headers, text and HTML bodies, inline images and attachments,
// and an offline HTML page for rendering it (inline images embedded, remote
// resources removed, so rendering never fetches anything).
package document

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
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

// Parse is the finite compatibility wrapper around the shared streaming parser.
func Parse(raw []byte) (out Doc, err error) {
	s, e := NewScope(Limits{})
	if e != nil {
		return Doc{}, e
	}
	defer func() {
		err = errors.Join(err, s.Close())
		if err != nil {
			out = Doc{}
		}
	}()
	if int64(len(raw)) > s.limits.RawBytes {
		return Doc{}, incomplete("raw byte limit exceeded", nil)
	}
	d, e := ParseReader(context.Background(), bytes.NewReader(raw), s)
	if e != nil {
		return Doc{}, e
	}
	out, err = d.Materialize(context.Background())
	err = errors.Join(err, d.Close())
	return out, err
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
