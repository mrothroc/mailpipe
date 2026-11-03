package document

import (
	"context"
	"encoding/base64"
	"errors"
	nethtml "golang.org/x/net/html"
	"html"
	"io"
	"net/url"
	"strings"
	"text/template"
)

// OfflineHTML materializes only the finite page and inline representations.
func (d *FileDoc) OfflineHTML(ctx context.Context) (page string, err error) {
	ctx, cancel := context.WithTimeout(ctx, d.scope.limits.Timeout)
	defer cancel()
	b, e := d.scope.NewBlob(ctx, d.scope.limits.PageBytes, 0)
	if e != nil {
		return "", e
	}
	defer func() {
		err = errors.Join(err, b.Close())
		if err != nil {
			page = ""
		}
	}()
	write := func(v string) error { _, e := io.WriteString(b, v); return e }
	if e = write(`<!doctype html><html><head><meta charset="utf-8">` + blankBase + `</head><body>` + sourceHeader(d.From, d.Subject, d.Date)); e != nil {
		return "", e
	}
	for _, f := range d.body.render {
		r, e := f.data.Open()
		if e != nil {
			return "", e
		}
		if !f.html {
			e = write(plainTextStart)
			buffer := make([]byte, 32768)
			for e == nil {
				n, re := r.Read(buffer)
				if n > 0 {
					checked := &contextWriter{ctx: ctx, w: b}
					template.HTMLEscape(checked, buffer[:n])
					e = checked.err
				}
				if re != nil {
					if re != io.EOF {
						e = re
					}
					break
				}
			}
			if e == nil {
				e = write("</pre>")
			}
		} else {
			e = d.offlineTokens(ctx, r, b)
		}
		e = errors.Join(e, r.Close())
		if e != nil {
			return "", e
		}
	}
	if e = write("</body></html>"); e != nil {
		return "", e
	}
	if e = b.Seal(); e != nil {
		return "", e
	}
	return materializeString(ctx, d.scope, b, d.scope.limits.PageBytes)
}
func (d *FileDoc) offlineTokens(ctx context.Context, r io.Reader, w io.Writer) error {
	z := nethtml.NewTokenizer(contextReader{ctx, r})
	z.SetMaxBuf(d.scope.limits.HTMLBuffer)
	tokens := 0
	for {
		tt := z.Next()
		tokens++
		if tokens > d.scope.limits.HTMLTokens {
			return incomplete("offline HTML token limit", nil)
		}
		if tt == nethtml.ErrorToken {
			if z.Err() == io.EOF {
				return nil
			}
			return incomplete("offline HTML tokenization failed", z.Err())
		}
		token := z.Token()
		if token.Type == nethtml.StartTagToken || token.Type == nethtml.SelfClosingTagToken {
			if token.Data == "base" || token.Data == "link" {
				continue
			}
			attrs := token.Attr[:0]
			for _, a := range token.Attr {
				switch strings.ToLower(a.Key) {
				case "srcset", "background", "poster":
					continue
				case "src":
					if token.Data != "img" {
						continue
					}
					if strings.HasPrefix(strings.ToLower(a.Val), "cid:") {
						id := a.Val[len("cid:"):]
						if decoded, e := url.PathUnescape(id); e == nil {
							id = decoded
						}
						part, ok := d.Inline[id]
						if !ok {
							continue
						}
						switch part.ContentType {
						case "image/png", "image/jpeg", "image/gif", "image/webp":
						default:
							continue
						}
						// Serialize the element in pieces so base64 never becomes a large string.
						if _, e := io.WriteString(w, `<img src="data:`+part.ContentType+`;base64,`); e != nil {
							return e
						}
						input, e := part.Data.Open()
						if e != nil {
							return e
						}
						enc := base64.NewEncoder(base64.StdEncoding, w)
						e = copyChunks(ctx, enc, input)
						e = errors.Join(e, enc.Close(), input.Close())
						if e != nil {
							return e
						}
						if _, e = io.WriteString(w, `"`); e != nil {
							return e
						}
						// Keep presentation hints without materializing the encoded
						// image. Chrome.PDF remains the sanitization boundary.
						for _, hint := range token.Attr {
							switch hint.Key {
							case "alt", "title", "width", "height", "style", "class":
								if _, e = io.WriteString(w, " "+hint.Key+`="`+html.EscapeString(hint.Val)+`"`); e != nil {
									return e
								}
							}
						}
						if _, e = io.WriteString(w, ">"); e != nil {
							return e
						}
						token.Type = nethtml.CommentToken
						token.Data = "inline image embedded"
						attrs = nil
					}
					continue
				}
				attrs = append(attrs, a)
			}
			token.Attr = attrs
		}
		if _, e := io.WriteString(w, token.String()); e != nil {
			return e
		}
	}
}
