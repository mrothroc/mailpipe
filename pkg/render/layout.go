package render

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// Paper and image bounds use physical units so Chrome's viewport cannot change
// the printable area. One inch of vertical headroom accommodates source headers.
const (
	paperWidthIn  = 8.5
	paperHeightIn = 11.0
	pageMarginIn  = 0.5
	printWidthIn  = paperWidthIn - 2*pageMarginIn
	imageHeightIn = paperHeightIn - 2*pageMarginIn - 1
)

var printStyle = fmt.Sprintf(`@media print {
@page { size:%gin %gin; margin:%gin; }
html, body { margin:0 !important; padding:0 !important; }
pre { white-space:pre-wrap; overflow-wrap:anywhere; }
}`, paperWidthIn, paperHeightIn, pageMarginIn)

func addLayoutStyle(n *html.Node, style string) {
	for i := range n.Attr {
		if n.Attr[i].Key == "style" {
			n.Attr[i].Val += ";" + style
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: "style", Val: style})
}

// fitImages operates only on the already sanitized tree. Individual image boxes
// are bounded; containing tables stay fragmentable rather than forming one
// oversized unbreakable unit. It returns whether a subtree has images/text.
func fitImages(n *html.Node) (hasImage, hasText bool) {
	if n.Type == html.TextNode {
		return false, strings.TrimSpace(n.Data) != ""
	}
	if n.Type == html.ElementNode && n.Data == "style" {
		return false, false
	}
	if n.Type == html.ElementNode && n.Data == "img" {
		addLayoutStyle(n, fmt.Sprintf("min-width:0 !important;min-height:0 !important;max-width:min(100%%,%gin) !important;max-height:%gin !important;box-sizing:border-box !important;object-fit:scale-down !important;object-position:center !important;transform:none !important;break-inside:avoid !important;page-break-inside:avoid !important", printWidthIn, imageHeightIn))
		return true, false
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		image, text := fitImages(child)
		hasImage = hasImage || image
		hasText = hasText || text
	}
	if hasImage && !hasText {
		// Adjacent inline replaced elements can form an oversized line box
		// across page breaks. Standalone image runs need independent blocks;
		// images embedded in ordinary text retain their inline presentation.
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.ElementNode && child.Data == "img" {
				addLayoutStyle(child, "display:block !important;")
			}
		}
	}
	if !hasImage || n.Type != html.ElementNode || n.Data == "body" {
		return
	}
	switch n.Data {
	case "div", "p", "figure", "section", "article", "main", "a", "span", "center", "table", "tbody", "thead", "tfoot", "tr", "td", "th":
		style := fmt.Sprintf("min-width:0 !important;max-width:min(100%%,%gin) !important;overflow-wrap:anywhere;", printWidthIn)
		if n.Data == "table" {
			// Preserve a smaller authored table width, but do not let a large
			// HTML width attribute establish an oversized containing block.
			cap := printWidthIn * 96
			for _, a := range n.Attr {
				if a.Key == "width" {
					if v, err := strconv.ParseFloat(a.Val, 64); err == nil && v > 0 && v < cap {
						cap = v
					}
				}
				if a.Key == "style" {
					// Only literal positive px widths are presentation hints;
					// expressions and other CSS remain source presentation.
					for _, declaration := range strings.Split(a.Val, ";") {
						key, value, ok := strings.Cut(declaration, ":")
						if !ok || !strings.EqualFold(strings.TrimSpace(key), "width") {
							continue
						}
						value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(strings.ToLower(value)), "!important"))
						if !strings.HasSuffix(value, "px") {
							continue
						}
						if v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(value, "px")), 64); err == nil && v > 0 && v < cap {
							cap = v
						}
					}
				}
			}
			style += fmt.Sprintf("width:min(100%%,%gpx) !important;table-layout:fixed !important;", cap)
		} else if n.Data == "div" || n.Data == "p" || n.Data == "figure" || n.Data == "td" || n.Data == "th" {
			style += "width:auto !important;"
		}
		if !hasText {
			style += "height:auto !important;min-height:0 !important;max-height:none !important;overflow:visible !important;"
		}
		style += "break-inside:auto !important;page-break-inside:auto !important;"
		addLayoutStyle(n, style)
	}
	return
}
