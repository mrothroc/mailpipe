package render_test

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/render"
)

func TestPDFBlocksActiveMarkupAndLocalResourceReferences(t *testing.T) {
	f := &fakeChrome{out: []byte("%PDF-1.7 fixture\n%%EOF\n")}
	page := `<!-- <head> --><html><head><base href="file:///tmp/"><meta http-equiv="refresh" content="0;url=file:///tmp/fixture.html"><style>p {color:blue; background:url(file:///tmp/fixture.png)}</style></head><body onload="alert(1)"><p>Message content</p><script>document.write('unsafe')</script><iframe src="file:///tmp/fixture.html"></iframe><object data="file:///tmp/fixture.html"></object><img src="file:///tmp/fixture.png"><svg><script>alert(2)</script></svg><a href="java&#115;cript:alert(3)">link</a></body></html>`
	if _, err := (render.Chrome{Path: "/chrome", Runner: f}).PDF(context.Background(), page); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"<script", "<iframe", "<object", "<svg", "onload=", "http-equiv=\"refresh\"", "href=\"javascript:", "src=\"file:"} {
		if strings.Contains(f.page, bad) {
			t.Errorf("active markup survived: %s", bad)
		}
	}
	if !strings.Contains(f.page, "Content-Security-Policy") || !strings.Contains(f.page, "default-src 'none'") || !strings.Contains(f.page, "img-src data:") || !strings.Contains(f.page, "<p>Message content</p>") {
		t.Fatal("safe content or resource policy missing")
	}
	if strings.Count(f.page, `<base href="about:blank">`) != 1 {
		t.Fatal("trusted base missing or duplicated")
	}
}

func TestPDFPreservesStaticEmailPresentation(t *testing.T) {
	f := &fakeChrome{out: []byte("%PDF-1.7 fixture\n%%EOF\n")}
	page := `<html><head><style>p {color:blue}</style></head><body style="font-family:serif" bgcolor="#eeeeee" onload="alert(1)"><main><article><header>Header</header><section><figure><figcaption>Caption</figcaption></figure><aside>Note</aside><time>Today</time><mark>Highlighted</mark></section><footer>Footer</footer></article></main><table><tr><td colspan="2"><p style="font-weight:bold">Receipt &amp; notes</p><img alt="logo" src="data:image/png;base64,aW1hZ2U="><img src=" data:image/jpg;base64,aW1h Z2U= "><a href="https://example.com/receipt">Receipt</a><a href="mailto:billing@example.com">Contact</a></td></tr></table></body></html>`
	if _, err := (render.Chrome{Path: "/chrome", Runner: f}).PDF(context.Background(), page); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<style>p {color:blue}</style>`, `<body style="font-family:serif" bgcolor="#eeeeee">`, `<main><article><header>Header</header><section>`, `<figcaption>Caption</figcaption>`, `<time>Today</time>`, `<mark>Highlighted</mark>`, `<footer>Footer</footer>`, `src="data:image/jpg;base64,aW1hZ2U="`, `<table>`, `colspan="2"`, `style="font-weight:bold"`, `Receipt &amp; notes`, `src="data:image/png;base64,aW1hZ2U="`, `href="https://example.com/receipt"`, `href="mailto:billing@example.com"`} {
		if !strings.Contains(f.page, want) {
			t.Errorf("static presentation lost: %s", want)
		}
	}
}

// This optional integration test checks actual browser behaviour rather than
// trusting the presence of browser command-line flags. All content is invented.
func TestRealChromeCannotExecuteMailScriptsOrIncludeLocalFiles(t *testing.T) {
	chrome := render.FindChrome()
	if chrome == "" {
		t.Skip("no Chrome installed")
	}
	textTool, err := exec.LookPath("pdftotext")
	if err != nil {
		t.Skip("optional pdftotext integration helper not installed")
	}
	dir := t.TempDir()
	local := filepath.Join(dir, "fixture.html")
	const localMarker = "CANARY_LOCAL"
	const scriptMarker = "CANARY_SCRIPT"
	if err := os.WriteFile(local, []byte("<p>"+localMarker+"</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileURL := (&url.URL{Scheme: "file", Path: local}).String()
	page := `<html><head></head><body><p>Message content</p><script>document.write('` + scriptMarker + `')</script><iframe width="600" height="150" src="` + fileURL + `"></iframe><object width="600" height="150" data="` + fileURL + `"></object></body></html>`
	pdf, err := (render.Chrome{Path: chrome}).PDF(context.Background(), page)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "result.pdf")
	if err := os.WriteFile(path, pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	text, err := exec.Command(textTool, path, "-").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "Message content") {
		t.Fatal("message content was lost")
	}
	for _, marker := range []string{localMarker, scriptMarker} {
		if strings.Contains(string(text), marker) {
			t.Fatalf("unsafe browser behaviour included synthetic marker %s", marker)
		}
	}
}
