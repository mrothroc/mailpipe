// Package render turns an HTML page into a PDF with headless Chrome. Every
// render sanitises untrusted HTML, enforces a restrictive resource policy and
// uses an offline throwaway browser profile.
package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Timeout bounds one render.
const Timeout = 60 * time.Second

// Runner runs a program; it is injectable so tests never start Chrome.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) error
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// Chrome's helper processes can hold the output pipe open after Chrome
	// exits; don't wait on them for long.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		if msg := lastLine(out.Bytes()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// lastLine returns the last non-empty line of out, which says why a run failed.
func lastLine(out []byte) string {
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	return string(bytes.TrimSpace(lines[len(lines)-1]))
}

// complete reports a whole PDF: its header and, near the end, the end-of-file
// marker that a writer adds last.
func complete(pdf []byte) bool {
	tail := pdf[max(0, len(pdf)-1024):]
	return bytes.HasPrefix(pdf, []byte("%PDF-")) && bytes.Contains(tail, []byte("%%EOF"))
}

// Chrome renders with the Chrome (or Chromium-based browser) binary at Path.
type Chrome struct {
	Path   string
	Runner Runner // nil runs Path
}

// candidates are where macOS keeps Chromium-based browsers.
var candidates = []string{
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
	"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
}

// FindChrome returns the first installed Chromium-based browser, or "".
func FindChrome() string {
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}

// PDF renders page and returns the PDF.
func (c Chrome) PDF(ctx context.Context, page string) ([]byte, error) {
	if c.Path == "" {
		return nil, errors.New("render: no Chrome (install Google Chrome, or give its path)")
	}
	page, err := safeHTML(page)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "mailpipe-render-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in, out := filepath.Join(dir, "page.html"), filepath.Join(dir, "out.pdf")
	if err := os.WriteFile(in, []byte(page), 0o600); err != nil {
		return nil, err
	}
	runner := c.Runner
	if runner == nil {
		runner = execRunner{}
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	err = runner.Run(ctx, c.Path,
		"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-background-networking", "--disable-sync", "--disable-extensions",
		"--user-data-dir="+filepath.Join(dir, "profile"),
		"--proxy-server=127.0.0.1:9", "--proxy-bypass-list=<-loopback>", // offline: nothing reaches the network
		"--no-pdf-header-footer", "--print-to-pdf="+out, (&url.URL{Scheme: "file", Path: in}).String())
	// Chrome on macOS writes the PDF and then exits 2, failing to create a
	// display link when headless: a complete PDF is the result, whatever the
	// exit status says.
	pdf, readErr := os.ReadFile(out)
	if readErr == nil && complete(pdf) {
		return pdf, nil
	}
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	if readErr != nil {
		return nil, fmt.Errorf("render: no PDF written: %w", readErr)
	}
	return nil, errors.New("render: output is not a complete PDF")
}
