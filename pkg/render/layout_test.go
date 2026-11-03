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
