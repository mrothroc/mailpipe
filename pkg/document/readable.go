package document

import (
	"context"
	"io"
	"strings"

	"golang.org/x/net/html"
)

// HTMLReadable emits plain text only. It never constructs a browser, resolves a
// URL, executes scripts, or opens any resource named by a token.
func HTMLReadable(ctx context.Context, r io.Reader, w io.Writer, l Limits) error {
	var e error
	l, e = l.Resolve()
	if e != nil {
		return e
	}
	z := html.NewTokenizer(contextReader{ctx, r})
	z.SetMaxBuf(l.HTMLBuffer)
	var stack []string
	depth, tokens := 0, 0
	var written int64
	emit := func(b []byte) error {
		if int64(len(b)) > l.TextBytes-written {
			return incomplete("readable text limit exceeded", nil)
		}
		n, e := w.Write(b)
		written += int64(n)
		if e == nil && n != len(b) {
			e = io.ErrShortWrite
		}
		return e
	}
	suppressed := func(name string) bool {
		switch name {
		case "script", "style", "head", "template", "frame", "iframe", "object", "embed", "svg", "math":
			return true
		}
		return false
	}
	block := func(name string) bool {
		switch name {
		case "br", "p", "div", "li", "tr", "td", "th", "table", "ul", "ol", "pre", "blockquote", "h1", "h2", "h3", "h4", "h5", "h6", "hr":
			return true
		}
		return false
	}
	for {
		if e = ctx.Err(); e != nil {
			return e
		}
		tt := z.Next()
		tokens++
		if tokens > l.HTMLTokens {
			return incomplete("HTML token limit exceeded", nil)
		}
		switch tt {
		case html.ErrorToken:
			if z.Err() == io.EOF {
				return nil
			}
			return incomplete("HTML tokenization failed", z.Err())
		case html.TextToken:
			if len(stack) == 0 {
				if e = emit(z.Text()); e != nil {
					return e
				}
			}
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			tag, _ := z.TagName()
			name := string(tag)
			if tt == html.EndTagToken {
				depth = max(0, depth-1)
				if len(stack) > 0 {
					for i := len(stack) - 1; i >= 0; i-- {
						if stack[i] == name {
							stack = stack[:i]
							break
						}
					}
					continue
				}
			} else {
				if tt == html.StartTagToken && !voidHTML(name) {
					depth++
					if depth > l.HTMLDepth {
						return incomplete("HTML depth limit exceeded", nil)
					}
				}
				if suppressed(name) {
					if tt != html.SelfClosingTagToken && !voidHTML(name) {
						if len(stack) >= l.HTMLDepth {
							return incomplete("HTML suppression depth limit exceeded", nil)
						}
						stack = append(stack, name)
					}
					continue
				}
				if len(stack) > 0 {
					continue
				}
				if name == "img" {
					for {
						key, value, more := z.TagAttr()
						if strings.EqualFold(string(key), "alt") {
							if e = emit(value); e != nil {
								return e
							}
						}
						if !more {
							break
						}
					}
				}
			}
			if len(stack) == 0 && block(name) {
				if e = emit([]byte("\n")); e != nil {
					return e
				}
			}
		}
	}
}
func voidHTML(name string) bool {
	switch name {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr":
		return true
	}
	return false
}
