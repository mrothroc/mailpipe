package document_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/document"
	"github.com/mrothroc/mailpipe/pkg/render"
	nethtml "golang.org/x/net/html"
)

type offlineSanitizerRunner struct{ page string }

func (r *offlineSanitizerRunner) Run(_ context.Context, _ string, args ...string) error {
	for _, arg := range args {
		if strings.HasPrefix(arg, "file://") {
			u, err := url.Parse(arg)
			if err != nil {
				return err
			}
			page, err := os.ReadFile(u.Path)
			if err != nil {
				return err
			}
			r.page = string(page)
		}
		if path, ok := strings.CutPrefix(arg, "--print-to-pdf="); ok {
			if err := os.WriteFile(path, []byte("%PDF-1.7 synthetic\n%%EOF\n"), 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

func offlineImageAttributes(t *testing.T, page string) map[string]string {
	t.Helper()
	root, err := nethtml.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	var images []*nethtml.Node
	var visit func(*nethtml.Node)
	visit = func(n *nethtml.Node) {
		if n.Type == nethtml.ElementNode && n.Data == "script" {
			t.Error("script survived sanitizer")
		}
		if n.Type == nethtml.ElementNode && n.Data == "img" {
			images = append(images, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(root)
	if len(images) != 1 {
		t.Fatalf("images %d, want one", len(images))
	}
	attrs := map[string]string{}
	for _, a := range images[0].Attr {
		attrs[a.Key] = a.Val
	}
	return attrs
}

func TestOfflineStreamingPreservesMixedCaseImageHints(t *testing.T) {
	var imageData bytes.Buffer
	if err := png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{
		`<IMG SRC="CID:logo%40example.org" WIDTH="60" HEIGHT="30" STYLE="width:60px;--label:&quot;A&amp;B&quot;" ALT="A &amp; &lt;B&gt; &quot;quoted&quot;" TITLE="Title &amp; &lt;value&gt; &quot;quoted&quot;" CLASS="Logo Mark" ONLOAD="unsafe()" DATA-UNSAFE="discard">`,
		`<iMg wIdTh="60" hEiGhT="30" sTyLe="width:60px;--label:&quot;A&amp;B&quot;" aLt="A &amp; &lt;B&gt; &quot;quoted&quot;" tItLe="Title &amp; &lt;value&gt; &quot;quoted&quot;" cLaSs="Logo Mark" sRc="cId:logo%40example.org" oNlOaD="unsafe()" dAtA-uNsAfE="discard">`,
	} {
		t.Run(strings.Fields(tag)[0][1:], func(t *testing.T) {
			raw := "From: records@example.org\r\nSubject: Image hint normalization\r\nMIME-Version: 1.0\r\nContent-Type: multipart/related; boundary=hint\r\n\r\n--hint\r\nContent-Type: text/html\r\n\r\n" + tag + "\r\n--hint\r\nContent-Type: image/png\r\nContent-ID: <logo@example.org>\r\nContent-Transfer-Encoding: base64\r\n\r\n" + b64(imageData.Bytes()) + "\r\n--hint--\r\n"
			s, err := document.NewScope(document.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			d, err := document.ParseReader(context.Background(), strings.NewReader(raw), s)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := d.Close(); err != nil {
					t.Error(err)
				}
			})
			page, err := d.OfflineHTML(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(page, `alt="A &amp; &lt;B&gt; &#34;quoted&#34;"`) {
				t.Fatalf("hint value was not escaped: %s", page)
			}
			check := func(attrs map[string]string, sanitized bool) {
				t.Helper()
				for key, want := range map[string]string{"width": "60", "height": "30", "alt": `A & <B> "quoted"`, "title": `Title & <value> "quoted"`, "class": "Logo Mark"} {
					if attrs[key] != want {
						t.Errorf("%s=%q want %q", key, attrs[key], want)
					}
				}
				style := `width:60px;--label:"A&B"`
				if sanitized {
					if !strings.HasPrefix(attrs["style"], style+";") {
						t.Errorf("sanitized style lost hint: %q", attrs["style"])
					}
				} else if attrs["style"] != style {
					t.Errorf("style=%q want %q", attrs["style"], style)
				}
				if attrs["src"] != "data:image/png;base64,"+b64(imageData.Bytes()) {
					t.Errorf("CID was not embedded exactly: %q", attrs["src"])
				}
				for _, bad := range []string{"onload", "data-unsafe", "srcset"} {
					if _, ok := attrs[bad]; ok {
						t.Errorf("unsafe attribute survived: %s", bad)
					}
				}
			}
			check(offlineImageAttributes(t, page), false)
			runner := &offlineSanitizerRunner{}
			if _, err := (render.Chrome{Path: "/synthetic-chrome", Runner: runner}).PDF(context.Background(), page); err != nil {
				t.Fatal(err)
			}
			check(offlineImageAttributes(t, runner.page), true)
			if !strings.Contains(runner.page, "Content-Security-Policy") || !strings.Contains(runner.page, "img-src data:") {
				t.Error("renderer resource policy missing")
			}
		})
	}
}

func TestOfflineStreamingPreservesImageHintsAndHeaderPresentation(t *testing.T) {
	raw := strings.Replace(receipt, b64([]byte(`<html><head><link rel="stylesheet" href="https://track.example.net/s.css"></head><body style="background:url('https://track.example.net/bg.png')"><img src="cid:logo@shop"><img src="https://track.example.net/pixel.gif" width=1><p>Total $12.00</p></body></html>`)), b64([]byte(`<img src="CID:logo%40shop" alt="Logo &amp; edge" width="60" height="30" style="width:60px" onload="unsafe()">`)), 1)
	s, err := document.NewScope(document.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, err := document.ParseReader(context.Background(), strings.NewReader(raw), s)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	page, err := d.OfflineHTML(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`alt="Logo &amp; edge"`, `width="60"`, `height="30"`, `style="width:60px"`, `font:12px sans-serif`, `From:`, `Subject:`, `Your receipt — order 42`, `data:image/png;base64,`} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q", want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if page, err := d.OfflineHTML(ctx); err == nil || page != "" {
		t.Fatalf("canceled page returned %d bytes: %v", len(page), err)
	}
}

func TestOfflineStreamingPageLimit(t *testing.T) {
	raw := "From: records@example.org\r\nSubject: Bounds\r\nContent-Type: text/plain\r\n\r\n" + strings.Repeat("a", 2048)
	build := func(limit int64) (string, error) {
		limits := document.DefaultLimits()
		limits.PageBytes = limit
		s, err := document.NewScope(limits)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		d, err := document.ParseReader(context.Background(), strings.NewReader(raw), s)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		return d.OfflineHTML(context.Background())
	}
	page, err := build(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if exact, err := build(int64(len(page))); err != nil || exact != page {
		t.Fatalf("exact limit: %v", err)
	}
	if partial, err := build(int64(len(page) - 1)); !errors.Is(err, document.ErrIncomplete) || partial != "" {
		t.Fatalf("plus one: bytes %d err %v", len(partial), err)
	}
}
