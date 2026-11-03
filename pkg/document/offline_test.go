package document_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/document"
)

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
