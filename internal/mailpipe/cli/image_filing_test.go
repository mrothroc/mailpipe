package cli_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/document"
	"github.com/mrothroc/mailpipe/pkg/render"
)

func TestCompleteImageFiling(t *testing.T) {
	tools := requireImageTools(t)
	for _, tc := range []struct {
		name          string
		images        []imageFixture
		before, after string
	}{
		{"portrait", []imageFixture{{600, 3600, 0, ""}}, "", ""},
		{"landscape", []imageFixture{{3600, 600, 0, ""}}, "", ""},
		{"oversized", []imageFixture{{2400, 3200, 0, `width="2400" height="3200"`}}, "", ""},
		{"conflicting", []imageFixture{{600, 3600, 0, `width="3000" height="3000" style="min-width:3000px!important;min-height:3000px!important;max-width:none!important;max-height:none!important;object-fit:cover!important"`}}, "", ""},
		{"table", []imageFixture{{600, 3600, 0, ""}}, `<table width="2400" style="width:2400px"><tr><td><div style="width:2400px;height:4000px;overflow:hidden">`, `</div></td></tr></table>`},
		{"multiple", []imageFixture{{600, 3600, 0, ""}, {3600, 600, 1, ""}, {2400, 3200, 2, ""}}, `<div>`, `</div>`},
		{"multiple-bare", []imageFixture{{600, 3600, 0, ""}, {3600, 600, 1, ""}, {2400, 3200, 2, ""}}, "", ""},
		{"page-style", []imageFixture{{600, 3600, 0, ""}}, `<style>@page { size:20in 20in !important; margin:0 !important; }</style>`, ""},
		{"page-style-specific", []imageFixture{{600, 3600, 0, ""}}, `<style>@media print { @page receipt:first { size:20in 20in !important; margin:0 !important; } @page :first { size:20in 20in !important; margin:0 !important; } body { page:receipt; } }</style>`, ""},
		{"page-style-recovery", []imageFixture{{600, 3600, 0, ""}}, "<style>p{content:\"broken\n} @page :first{size:20in 20in!important;margin:0!important}</style>", ""},
		{"page-style-url-recovery", []imageFixture{{600, 3600, 0, ""}}, `<style>p{background:url(bad"token)}@page :first{size:20in 20in!important;margin:0!important}</style>`, ""},
		{"ordinary", []imageFixture{{120, 60, 0, `width="60" height="30"`}}, `<p style="font:16px serif;color:blue">Ordinary readable message</p><table width="500"><tr><td>Item</td><td>Total</td></tr></table>`, `<p>End of ordinary message</p>`},
		{"inline-text", []imageFixture{{120, 60, 0, `width="60" height="30"`}}, `<p style="font:16px serif;color:blue">Ordinary readable message</p><table width="500"><tr><td>Item</td><td>Total</td></tr></table><p>Inline <span>before</span> `, ` <span>after</span> image.</p><p>End of ordinary message</p>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := imageFixtureMIME(tc.images, tc.before, tc.after)
			t.Run("compatibility", func(t *testing.T) {
				doc, err := document.Parse([]byte(raw))
				if err != nil {
					t.Fatal(err)
				}
				pdf, err := (render.Chrome{Path: tools.chrome}).PDF(context.Background(), doc.OfflineHTML())
				if err != nil {
					t.Fatal(err)
				}
				inspectImagePDF(t, tools, pdf, tc.images, len(tc.images) == 1)
			})
			t.Run("filed", func(t *testing.T) {
				e, root := savable(t)
				e.graph.mime["g2"] = raw
				m := manifest(t, e, "{g2}\tSynthetic\tcomplete.pdf")
				calls := e.graph.rawCalls.Load()
				code, out, errOut := runImageSave(t, e, tools.chrome, "save", "--manifest", m, "--root", root)
				if code != 0 || !strings.Contains(out, "would_save") || e.graph.rawCalls.Load() != calls {
					t.Fatalf("dry-run %d %s %s", code, out, errOut)
				}
				if _, err := os.Stat(root); !os.IsNotExist(err) {
					t.Fatal("dry run created output root")
				}
				code, out, errOut = runImageSave(t, e, tools.chrome, "save", "--manifest", m, "--root", root, "--execute")
				if code != 0 || !strings.Contains(out, "\tsaved\t") {
					t.Fatalf("save %d %s %s", code, out, errOut)
				}
				path := filepath.Join(root, "Synthetic", "complete.pdf")
				pdf, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				fi, err := os.Stat(path)
				if err != nil || fi.Mode().Perm() != 0o600 {
					t.Fatalf("private PDF: %v %v", fi, err)
				}
				_, verdicts, _ := e.run("verdicts", firstN(t, out))
				if !strings.Contains(verdicts, "\tsaved\t") {
					t.Fatalf("missing saved verdict: %s", verdicts)
				}
				inspectImagePDF(t, tools, pdf, tc.images, len(tc.images) == 1)
			})
		})
	}
}

func TestImageFilingDependencyFailures(t *testing.T) {
	for _, missing := range []string{"Chrome", "pdftoppm", "pdftotext"} {
		t.Run(missing, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestCompleteImageFiling$", "-test.v")
			cmd.Env = append(os.Environ(), "MAILPIPE_REQUIRE_RENDER_TESTS=1", "MAILPIPE_IMAGE_TEST_MISSING="+missing)
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "required ") || !strings.Contains(string(out), missing) || !strings.Contains(string(out), "install") || strings.Contains(string(out), "--- SKIP") {
				t.Fatalf("required entry point did not fail actionably: %v %s", err, out)
			}
		})
	}
	for _, name := range []string{"Chrome", "pdftoppm", "pdftotext"} {
		t.Run("broken-"+name, func(t *testing.T) {
			_, err := imageDependencies(func() string { return "Chrome" }, func(s string) (string, error) { return s, nil }, func(path, _ string) error {
				if path == name {
					return errors.New("executable failed or timed out")
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "install") {
				t.Fatalf("broken dependency: %v", err)
			}
		})
	}
	_, err := imageDependencies(func() string { return "Chrome" }, func(s string) (string, error) { return s, nil }, func(string, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runImageTool(context.Background(), "/nonexistent-example-pdftoppm"); err == nil || !strings.Contains(err.Error(), "inspection") {
		t.Fatalf("conversion failure was hidden: %v", err)
	}
	t.Run("nonexecutable-Chrome", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "chrome")
		if err := os.WriteFile(path, []byte("synthetic nonexecutable dependency"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := imageDependencies(func() string { return path }, exec.LookPath, func(path, flag string) error { return exec.CommandContext(context.Background(), path, flag).Run() })
		if err == nil || !strings.Contains(err.Error(), "Chrome unusable") || !strings.Contains(err.Error(), "install") {
			t.Fatalf("nonexecutable Chrome accepted: %v", err)
		}
	})
	t.Run("canceled-preflight", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := imageDependencies(func() string { return os.Args[0] }, exec.LookPath, func(path, flag string) error { return exec.CommandContext(ctx, path, flag).Run() })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled dependency probe: %v", err)
		}
	})
}

func TestImageFilingMarkerOracleRejectsCorruption(t *testing.T) {
	f := imageFixture{300, 600, 0, ""}
	data := imageFixturePNG(f)
	source, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	page := func(stretch bool) *image.RGBA {
		im := image.NewRGBA(image.Rect(0, 0, 816, 1056))
		draw.Draw(im, im.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
		width := 300
		if stretch {
			width = 400
		}
		for y := 0; y < 600; y++ {
			for x := 0; x < width; x++ {
				im.Set(x+100, y+150, source.At(x*300/width, y))
			}
		}
		return im
	}
	if _, err := measureImagePages([]image.Image{page(false)}, []imageFixture{f}); err != nil {
		t.Fatal("positive control", err)
	}
	for _, kind := range []string{"crop", "missing-edge", "edge-gap", "distort", "blank-page"} {
		t.Run(kind, func(t *testing.T) {
			im := page(kind == "distort")
			pages := []image.Image{im}
			switch kind {
			case "crop":
				draw.Draw(im, image.Rect(100, 150, 120, 750), image.NewUniform(color.White), image.Point{}, draw.Src)
			case "missing-edge":
				draw.Draw(im, image.Rect(100, 730, 400, 750), image.NewUniform(color.White), image.Point{}, draw.Src)
			case "edge-gap":
				draw.Draw(im, image.Rect(180, 150, 320, 170), image.NewUniform(color.White), image.Point{}, draw.Src)
			case "blank-page":
				blank := image.NewRGBA(im.Bounds())
				draw.Draw(blank, blank.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
				pages = append([]image.Image{blank}, im)
			}
			if _, err := measureImagePages(pages, []imageFixture{f}); err == nil {
				t.Fatal("corrupt fixture accepted")
			}
		})
	}
}
