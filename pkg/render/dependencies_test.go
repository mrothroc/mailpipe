package render_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/render"
)

// Legacy standalone browser tests remain optional. The release gate makes
// every unavailable/unusable dependency fatal before any optional skip path.
func browserDependencies(t *testing.T, text bool) (string, string) {
	t.Helper()
	chrome := render.FindChrome()
	fail := func(err error) {
		t.Helper()
		if os.Getenv("MAILPIPE_REQUIRE_RENDER_TESTS") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	if chrome == "" {
		fail(fmt.Errorf("required headless Chrome missing; install Google Chrome"))
	}
	check := func(path, flag, remedy string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, path, flag).CombinedOutput()
		if err != nil || len(strings.TrimSpace(string(out))) == 0 {
			fail(fmt.Errorf("required %s unusable; %s: %v", path, remedy, err))
		}
	}
	check(chrome, "--version", "install executable Google Chrome")
	textPath := ""
	if text || os.Getenv("MAILPIPE_REQUIRE_RENDER_TESTS") == "1" {
		for _, name := range []string{"pdftotext", "pdftoppm"} {
			path, err := exec.LookPath(name)
			if err != nil {
				fail(fmt.Errorf("required %s missing; install Poppler (brew install poppler): %w", name, err))
			}
			check(path, "-v", "install Poppler (brew install poppler)")
			if name == "pdftotext" {
				textPath = path
			}
		}
	}
	return chrome, textPath
}
