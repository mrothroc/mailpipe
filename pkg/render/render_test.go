package render_test

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/render"
)

// fakeChrome records its arguments and writes out what a run would print.
type fakeChrome struct {
	args []string
	page string
	out  []byte
	err  error
}

func (f *fakeChrome) Run(_ context.Context, _ string, args ...string) error {
	f.args = args
	for _, a := range args {
		if strings.HasPrefix(a, "file://") {
			// Read the page as a browser would: through the URL, unescaped.
			if u, err := url.Parse(a); err == nil && u.Fragment == "" {
				b, _ := os.ReadFile(u.Path)
				f.page = string(b)
			}
		}
		if p, ok := strings.CutPrefix(a, "--print-to-pdf="); ok && f.out != nil {
			_ = os.WriteFile(p, f.out, 0o600)
		}
	}
	return f.err
}

func flag(args []string, prefix string) string {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return a
		}
	}
	return ""
}

func TestPDFRendersOfflineInAThrowawayProfile(t *testing.T) {
	f := &fakeChrome{out: []byte("%PDF-1.7 fake\n%%EOF\n")}
	c := render.Chrome{Path: "/chrome", Runner: f}
	got, err := c.PDF(context.Background(), "<p>Total $12.00</p>")
	if err != nil || !bytes.Equal(got, f.out) || !strings.Contains(f.page, "<p>Total $12.00</p>") {
		t.Fatalf("PDF = %q, %v; page %q", got, err, f.page)
	}
	for _, want := range []string{"--headless=new", "--proxy-server=127.0.0.1:9", "--proxy-bypass-list=<-loopback>", "--no-pdf-header-footer"} {
		if flag(f.args, want) == "" {
			t.Errorf("missing %s in %v", want, f.args)
		}
	}
	if !strings.Contains(f.page, "script-src 'none'") {
		t.Fatal("renderer did not enforce script blocking")
	}
	profile := strings.TrimPrefix(flag(f.args, "--user-data-dir="), "--user-data-dir=")
	if profile == "" || !strings.HasPrefix(profile, os.TempDir()) && !strings.HasPrefix(profile, filepath.Clean(os.TempDir())) {
		t.Fatalf("profile %q is not a throwaway one", profile)
	}
	if _, err := os.Stat(filepath.Dir(profile)); !os.IsNotExist(err) {
		t.Fatalf("the work directory %s was left behind", filepath.Dir(profile))
	}
}

func TestPDFFailsOnAFailedRunOrAnythingButAPDF(t *testing.T) {
	cases := map[string]*fakeChrome{
		"failed, partial PDF": {out: []byte("%PDF-1.7 cut off"), err: errors.New("exit 1")},
		"not a PDF":           {out: []byte("<html>error</html>")},
		"no output":           {},
		"failed, no output":   {err: errors.New("exit 1")},
	}
	for name, f := range cases {
		if _, err := (render.Chrome{Path: "/chrome", Runner: f}).PDF(context.Background(), "<p>x</p>"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestRealChromeRendersAPDF runs the installed Chrome, when there is one.
func TestRealChromeRendersAPDF(t *testing.T) {
	path := render.FindChrome()
	if path == "" {
		t.Skip("no Chrome installed")
	}
	got, err := render.Chrome{Path: path}.PDF(context.Background(), `<!doctype html><meta charset="utf-8"><p>Receipt — $12.00</p><img src="https://example.invalid/pixel.gif">`)
	if err != nil || !bytes.HasPrefix(got, []byte("%PDF-")) {
		t.Fatalf("PDF %d bytes, %v", len(got), err)
	}
}

// Chrome on macOS writes the PDF and then exits 2 (it cannot create a display
// link when headless): a complete PDF is the result, whatever the exit status.
func TestACompletePDFCountsEvenWhenChromeExitsNonZero(t *testing.T) {
	f := &fakeChrome{out: []byte("%PDF-1.7 whole\n%%EOF\n"), err: errors.New("exit status 2")}
	got, err := render.Chrome{Path: "/chrome", Runner: f}.PDF(context.Background(), "<p>x</p>")
	if err != nil || !bytes.Equal(got, f.out) {
		t.Fatalf("PDF = %q, %v", got, err)
	}
}

// The page's file URL is escaped, so a temporary directory with # or % in its
// name still opens the right file.
func TestThePageURLIsEscaped(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "odd #50%"))
	_ = os.MkdirAll(os.Getenv("TMPDIR"), 0o700)
	f := &fakeChrome{out: []byte("%PDF-1.7 x\n%%EOF\n")}
	if _, err := (render.Chrome{Path: "/chrome", Runner: f}).PDF(context.Background(), "<p>odd</p>"); err != nil || !strings.Contains(f.page, "<p>odd</p>") {
		t.Fatalf("err %v page %q args %v", err, f.page, f.args)
	}
}

// Two renders of a page with in-page links are the same document, apart
// from their creation times.
func TestRealChromeRendersAreRepeatable(t *testing.T) {
	path := render.FindChrome()
	if path == "" {
		t.Skip("no Chrome installed")
	}
	page := `<!doctype html><html><head><base href="about:blank"></head><body><a href="#total">Total</a><p id="total">$12</p></body></html>`
	a, errA := render.Chrome{Path: path}.PDF(context.Background(), page)
	b, errB := render.Chrome{Path: path}.PDF(context.Background(), page)
	times := regexp.MustCompile(`/(?:CreationDate|ModDate) \(D:[^)]*\)`)
	if errA != nil || errB != nil || !bytes.Equal(times.ReplaceAll(a, nil), times.ReplaceAll(b, nil)) {
		t.Fatalf("renders differ (%v, %v)", errA, errB)
	}
	if bytes.Contains(a, []byte("mailpipe-render-")) {
		t.Fatal("the PDF names the temporary page file")
	}
}
