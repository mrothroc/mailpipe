package render

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// Browser flags are not a security boundary: sanitise the parsed document and
// put the resource policy before any untrusted markup. Only static email
// presentation is retained. CSS is preserved, but its resource loads are
// constrained by CSP independently of how CSS URLs are spelled or escaped.
const resourcePolicy = "default-src 'none'; script-src 'none'; img-src data:; style-src 'unsafe-inline'; font-src data:; frame-src 'none'; object-src 'none'; connect-src 'none'; form-action 'none'; base-uri about:"

func safeHTML(page string) (string, error) {
	root, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return "", fmt.Errorf("render: parse HTML: %w", err)
	}
	body := &html.Node{Type: html.ElementNode, Data: "body"}
	var copyNode func(*html.Node, *html.Node)
	copyNode = func(n, parent *html.Node) {
		switch n.Type {
		case html.DocumentNode:
			for child := n.FirstChild; child != nil; child = child.NextSibling {
				copyNode(child, parent)
			}
		case html.TextNode:
			parent.AppendChild(&html.Node{Type: html.TextNode, Data: n.Data})
		case html.ElementNode:
			if n.Namespace != "" {
				return
			}
			switch n.Data {
			case "html", "head", "body":
				if n.Data == "body" {
					body.Attr = safeAttributes(n)
				}
				for child := n.FirstChild; child != nil; child = child.NextSibling {
					copyNode(child, parent)
				}
				return
			case "main", "article", "section", "header", "footer", "aside", "figure", "figcaption", "time", "mark", "a", "abbr", "b", "bdi", "bdo", "blockquote", "br", "caption", "center", "cite", "code", "col", "colgroup", "dd", "del", "div", "dl", "dt", "em", "font", "h1", "h2", "h3", "h4", "h5", "h6", "hr", "i", "img", "ins", "kbd", "li", "ol", "p", "pre", "q", "s", "samp", "small", "span", "strike", "strong", "style", "sub", "sup", "table", "tbody", "td", "tfoot", "th", "thead", "tr", "tt", "u", "ul", "var", "wbr":
			default:
				return // scripts, frames, objects, refresh metadata, forms and plugins
			}
			out := &html.Node{Type: html.ElementNode, Data: n.Data, DataAtom: n.DataAtom}
			out.Attr = safeAttributes(n)
			parent.AppendChild(out)
			for child := n.FirstChild; child != nil; child = child.NextSibling {
				copyNode(child, out)
			}
		}
	}
	copyNode(root, body)
	var out bytes.Buffer
	out.WriteString(`<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="` + resourcePolicy + `"><base href="about:blank"></head>`)
	if err := html.Render(&out, body); err != nil {
		return "", err
	}
	out.WriteString("</html>")
	return out.String(), nil
}

func safeAttributes(n *html.Node) []html.Attribute {
	var attrs []html.Attribute
	for _, a := range n.Attr {
		if a.Namespace != "" {
			continue
		}
		switch a.Key {
		case "href":
			if n.Data != "a" {
				continue
			}
			value := strings.TrimSpace(a.Val)
			parsed, err := url.Parse(value)
			if err != nil || !(strings.HasPrefix(value, "#") || parsed.Scheme == "http" || parsed.Scheme == "https" || parsed.Scheme == "mailto") {
				continue
			}
			a.Val = value
		case "src":
			value, ok := rasterDataURI(a.Val)
			if n.Data != "img" || !ok {
				continue
			}
			a.Val = value
		case "id", "title", "alt", "style", "class", "dir", "lang", "align", "valign", "width", "height", "border", "cellpadding", "cellspacing", "colspan", "rowspan", "span", "color", "bgcolor", "face", "size", "start", "type":
		default:
			continue // event handlers, srcset, navigation and unknown attributes
		}
		attrs = append(attrs, a)
	}
	return attrs
}

func rasterDataURI(value string) (string, bool) {
	value = strings.TrimSpace(value)
	header, payload, ok := strings.Cut(value, ",")
	if !ok {
		return "", false
	}
	switch strings.ToLower(header) {
	case "data:image/png;base64", "data:image/jpeg;base64", "data:image/jpg;base64", "data:image/gif;base64", "data:image/webp;base64":
	default:
		return "", false
	}
	payload = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n', '\f':
			return -1
		}
		return r
	}, payload)
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return "", false
	}
	return strings.ToLower(header) + "," + payload, true
}
