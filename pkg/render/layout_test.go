package render_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/render"
)

func TestPDFDefinesPrintableBoundsAndFitsImages(t *testing.T) {
	f := &fakeChrome{out: []byte("%PDF-1.7 fixture\n%%EOF\n")}
	_, err := (render.Chrome{Path: "/chrome", Runner: f}).PDF(context.Background(), `<table width="2400"><tr><td><img width="3000" height="4000" style="max-width:none!important;object-fit:cover!important" src="data:image/png;base64,aW1hZ2U="></td></tr></table>`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"size:8.5in 11in", "margin:0.5in", "max-width:min(100%,7.5in) !important", "max-height:9in !important", "object-fit:scale-down !important", "break-inside:avoid !important", "table-layout:fixed !important"} {
		if !strings.Contains(f.page, want) {
			t.Errorf("missing trusted layout rule %q", want)
		}
	}
	if strings.Index(f.page, "Content-Security-Policy") > strings.Index(f.page, "<style") {
		t.Fatal("print styling precedes CSP")
	}
}

func TestPDFKeepsSmallTableAndInlineLogoPresentation(t *testing.T) {
	f := &fakeChrome{out: []byte("%PDF-1.7 fixture\n%%EOF\n")}
	_, err := (render.Chrome{Path: "/chrome", Runner: f}).PDF(context.Background(), `<table style="width:320px!important"><tr><td>Text <img width="60" height="30" src="data:image/png;base64,aW1hZ2U="> label</td></tr></table>`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.page, "width:min(100%,320px) !important") || !strings.Contains(f.page, `width="60" height="30"`) || strings.Contains(f.page, "display:block") {
		t.Fatalf("small/inline presentation changed: %s", f.page)
	}
}

func TestPDFSeparatesBareImageRunsButKeepsImagesInTextInline(t *testing.T) {
	img := `<img width="60" height="30" src="data:image/png;base64,aW1hZ2U=">`
	for _, tc := range []struct {
		name, page string
		blocks     int
	}{
		{"bare-after-header", `<div>Source header</div>` + img + "\n" + img, 2},
		{"inline-text", `<p>Before ` + img + img + ` after</p>`, 0},
		{"inline-spans", `<p><span>Before</span>` + img + img + `<span>after</span></p>`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeChrome{out: []byte("%PDF-1.7 fixture\n%%EOF\n")}
			if _, err := (render.Chrome{Path: "/chrome", Runner: f}).PDF(context.Background(), tc.page); err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(f.page, "display:block !important"); n != tc.blocks {
				t.Fatalf("standalone blocks %d, want %d: %s", n, tc.blocks, f.page)
			}
		})
	}
}

func TestPDFRejectsSourcePageRulesButPreservesOrdinaryCSS(t *testing.T) {
	f := &fakeChrome{out: []byte("%PDF-1.7 fixture\n%%EOF\n")}
	page := `<style>p {color:blue} @media print { @page receipt:first {size:20in 20in!important;margin:0!important} } @\70 age :left {size:20in 20in!important} p::before {content:"@page stays text"}</style><p>Ordinary text</p>`
	if _, err := (render.Chrome{Path: "/chrome", Runner: f}).PDF(context.Background(), page); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.page, "20in") || !strings.Contains(f.page, "p {color:blue}") || !strings.Contains(f.page, `content:"@page stays text"`) {
		t.Fatalf("source page rule or ordinary CSS handling: %s", f.page)
	}
}
