package cli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/internal/mailpipe/cli"
	"github.com/mrothroc/mailpipe/pkg/render"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type imageTestTools struct{ chrome, raster, text string }

func imageDependencies(find func() string, look func(string) (string, error), version func(string, string) error) (imageTestTools, error) {
	tools := imageTestTools{chrome: find()}
	if tools.chrome == "" {
		return tools, fmt.Errorf("required headless Chrome missing; install Google Chrome")
	}
	for _, spec := range []struct {
		name, flag, remedy string
		path               *string
	}{
		{"Chrome", "--version", "install an executable Google Chrome", &tools.chrome},
		{"pdftoppm", "-v", "install Poppler (brew install poppler)", &tools.raster},
		{"pdftotext", "-v", "install Poppler (brew install poppler)", &tools.text},
	} {
		if *spec.path == "" {
			p, err := look(spec.name)
			if err != nil {
				return tools, fmt.Errorf("required %s missing; %s: %w", spec.name, spec.remedy, err)
			}
			*spec.path = p
		}
		if err := version(*spec.path, spec.flag); err != nil {
			return tools, fmt.Errorf("required %s unusable; %s: %w", spec.name, spec.remedy, err)
		}
	}
	return tools, nil
}

func requireImageTools(t *testing.T) imageTestTools {
	t.Helper()
	missing := os.Getenv("MAILPIPE_IMAGE_TEST_MISSING") // Only the negative subprocess tests set this.
	tools, err := imageDependencies(func() string {
		if missing == "Chrome" {
			return ""
		}
		return render.FindChrome()
	}, func(name string) (string, error) {
		if missing == name {
			return "", exec.ErrNotFound
		}
		return exec.LookPath(name)
	}, func(path, flag string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, path, flag).CombinedOutput()
		if err == nil && len(bytes.TrimSpace(out)) == 0 {
			return fmt.Errorf("empty version output")
		}
		if err == nil {
			t.Logf("dependency: %s", strings.TrimSpace(string(out)))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

var imagePalettes = [][5]color.RGBA{
	{{230, 20, 30, 255}, {20, 180, 40, 255}, {30, 50, 230, 255}, {240, 130, 10, 255}, {170, 20, 190, 255}},
	{{0, 180, 200, 255}, {230, 30, 160, 255}, {190, 180, 0, 255}, {90, 40, 140, 255}, {30, 100, 130, 255}},
	{{130, 60, 20, 255}, {0, 100, 65, 255}, {100, 120, 230, 255}, {230, 100, 110, 255}, {190, 90, 0, 255}},
}

type imageFixture struct {
	width, height, palette int
	attrs                  string
}

func imageFixturePNG(f imageFixture) []byte {
	im := image.NewRGBA(image.Rect(0, 0, f.width, f.height))
	draw.Draw(im, im.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	p := imagePalettes[f.palette]
	band := max(8, min(f.width, f.height)/20)
	for i, r := range []image.Rectangle{
		image.Rect(band, 0, f.width-band, band), image.Rect(f.width-band, band, f.width, f.height-band),
		image.Rect(band, f.height-band, f.width-band, f.height), image.Rect(0, band, band, f.height-band),
	} {
		draw.Draw(im, r, image.NewUniform(p[i]), image.Point{}, draw.Src)
	}
	// Unequal corner squares make orientation visible during raster inspection.
	for i, pt := range []image.Point{{0, 0}, {f.width - band, 0}, {f.width - band, f.height - band}, {0, f.height - band}} {
		r := image.Rectangle{Min: pt, Max: pt.Add(image.Pt(band, band))}
		draw.Draw(im, r, image.NewUniform(p[i]), image.Point{}, draw.Src)
	}
	radius := min(f.width, f.height) / 6
	for y := -radius; y <= radius; y++ {
		for x := -radius; x <= radius; x++ {
			if x*x+y*y <= radius*radius {
				im.SetRGBA(f.width/2+x, f.height/2+y, p[4])
			}
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func imageFixtureMIME(fixtures []imageFixture, before, after string) string {
	var body, parts strings.Builder
	body.WriteString(before)
	for i, f := range fixtures {
		fmt.Fprintf(&body, `<img alt="synthetic image %d" src="cid:image%d%%40example.org" %s>`, i, i, f.attrs)
		fmt.Fprintf(&parts, "--fixture\r\nContent-Type: image/png\r\nContent-ID: <image%d@example.org>\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n", i, base64.StdEncoding.EncodeToString(imageFixturePNG(f)))
	}
	body.WriteString(after)
	return "From: records@example.org\r\nSubject: Complete image fixture\r\nDate: Tue, 03 Nov 2026 10:00:00 +0000\r\nMIME-Version: 1.0\r\nContent-Type: multipart/related; boundary=fixture\r\n\r\n--fixture\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" + body.String() + "\r\n" + parts.String() + "--fixture--\r\n"
}

func runImageSave(t *testing.T, e *env, chrome string, args ...string) (int, string, string) {
	t.Helper()
	root := cli.NewRootCommand(cli.Deps{
		Auth: func() (cli.Authenticator, error) { return e.auth, nil }, Store: store.Open,
		Graph:  func(cli.Authenticator) cli.Graph { return e.graph },
		Render: render.Chrome{Path: chrome}.PDF,
	})
	var out, stderr bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&stderr)
	root.SetArgs(append([]string{"--db", e.db}, args...))
	err := root.ExecuteContext(context.Background())
	return cli.ExitCode(err), out.String(), stderr.String()
}

type imagePDFWord struct {
	Text string  `xml:",chardata"`
	XMin float64 `xml:"xMin,attr"`
	YMin float64 `xml:"yMin,attr"`
	XMax float64 `xml:"xMax,attr"`
	YMax float64 `xml:"yMax,attr"`
}
type imagePDFPage struct {
	Width  float64        `xml:"width,attr"`
	Height float64        `xml:"height,attr"`
	Words  []imagePDFWord `xml:"word"`
}
type imagePDFMeasurement struct {
	Page, Image    int
	Bounds, Circle image.Rectangle
	Ratio          float64
}

func runImageTool(ctx context.Context, path string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("PDF inspection %s failed: %w: %.500s", filepath.Base(path), err, out)
	}
	return out, nil
}

func inspectImagePDF(t *testing.T, tools imageTestTools, pdf []byte, fixtures []imageFixture, single bool) {
	t.Helper()
	if len(pdf) > 8<<20 {
		t.Fatal("synthetic PDF exceeds 8 MiB inspection bound")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.pdf")
	defer func() {
		if dest := os.Getenv("MAILPIPE_RENDER_EVIDENCE"); dest != "" {
			dest = filepath.Join(dest, strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
			if err := os.MkdirAll(dest, 0o700); err != nil {
				t.Error(err)
				return
			}
			files, _ := filepath.Glob(filepath.Join(dir, "*"))
			for _, file := range files {
				if info, err := os.Stat(file); err == nil && info.Size() <= 8<<20 {
					b, err := os.ReadFile(file)
					if err == nil {
						err = os.WriteFile(filepath.Join(dest, filepath.Base(file)), b, 0o600)
					}
					if err != nil {
						t.Error(err)
					}
				}
			}
		}
	}()
	if err := os.WriteFile(path, pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	text, err := runImageTool(ctx, tools.text, "-bbox", path, "-")
	if err != nil {
		t.Fatal(err)
	}
	dec := xml.NewDecoder(bytes.NewReader(text))
	var pages []imagePDFPage
	for {
		tok, e := dec.Token()
		if e != nil {
			if e != io.EOF {
				t.Fatal(e)
			}
			break
		}
		if start, ok := tok.(xml.StartElement); ok && start.Name.Local == "page" {
			var page imagePDFPage
			if err := dec.DecodeElement(&page, &start); err != nil {
				t.Fatal(err)
			}
			pages = append(pages, page)
		}
	}
	if len(pages) == 0 || len(pages) > 8 {
		t.Fatalf("page count %d, want nonempty <=8 and single=%v", len(pages), single)
	}
	for _, page := range pages {
		if math.Abs(page.Width-612) > 0.1 || math.Abs(page.Height-792) > 0.1 {
			t.Fatalf("PDF geometry %gx%g, want 612x792pt", page.Width, page.Height)
		}
	}
	if _, err = runImageTool(ctx, tools.raster, "-r", "96", "-png", path, filepath.Join(dir, "page")); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "page-*.png"))
	if err != nil || len(files) != len(pages) {
		t.Fatalf("raster pages %d PDF pages %d: %v", len(files), len(pages), err)
	}
	var rasters []image.Image
	for _, file := range files {
		fi, e := os.Stat(file)
		if e != nil || fi.Size() > 8<<20 {
			t.Fatalf("unbounded raster %s: %v", file, e)
		}
		in, e := os.Open(file)
		if e != nil {
			t.Fatal(e)
		}
		cfg, e := png.DecodeConfig(in)
		in.Close()
		if e != nil || cfg.Width != 816 || cfg.Height != 1056 {
			t.Fatalf("raster geometry %+v: %v", cfg, e)
		}
		in, e = os.Open(file)
		if e != nil {
			t.Fatal(e)
		}
		im, e := png.Decode(in)
		in.Close()
		if e != nil {
			t.Fatal(e)
		}
		rasters = append(rasters, im)
	}
	if single && len(pages) != 1 {
		t.Fatalf("page count %d, want one complete image/header page", len(pages))
	}
	measurements, err := measureImagePages(rasters, fixtures)
	if err != nil {
		t.Fatal(err)
	}
	var header strings.Builder
	for _, w := range pages[0].Words {
		header.WriteString(w.Text + " ")
		if w.XMin < 35 || w.XMax > 577 || w.YMin < 35 || w.YMax > 757 {
			t.Fatalf("text outside printable bounds: %+v", w)
		}
	}
	for _, want := range []string{"From:", "records@example.org", "Date:", "2026-11-03", "Subject:", "Complete image fixture"} {
		if !strings.Contains(header.String(), want) {
			t.Errorf("header lacks %q: %s", want, header.String())
		}
	}
	if len(fixtures) == 1 && fixtures[0].width == 120 {
		for _, want := range []string{"Ordinary readable message", "Item", "Total", "End of ordinary message"} {
			if !strings.Contains(header.String(), want) {
				t.Errorf("ordinary layout lost %q", want)
			}
		}
		if absImage(measurements[0].Bounds.Dx()-60) > 2 || absImage(measurements[0].Bounds.Dy()-30) > 2 {
			t.Fatalf("small authored logo expanded: %v", measurements[0].Bounds)
		}
		for _, w := range pages[0].Words {
			if w.Text == "Ordinary" && w.YMax-w.YMin < 10 {
				t.Fatalf("ordinary font shrunk: %+v", w)
			}
		}
	}
	for _, m := range measurements {
		if m.Page == 0 {
			for _, w := range pages[0].Words {
				if w.YMin < 100 && w.YMax*96/72 > float64(m.Bounds.Min.Y)+2 {
					t.Fatalf("header overlaps image: %+v %+v", w, m)
				}
			}
		}
	}
	// Text extraction and actual ink independently establish that headers paint.
	ink := 0
	for y := 48; y < 110; y++ {
		for x := 48; x < 600; x++ {
			r, g, b, _ := rasters[0].At(x, y).RGBA()
			if r < 30000 && g < 30000 && b < 30000 {
				ink++
			}
		}
	}
	if ink < 40 {
		t.Fatal("header has no visible raster ink")
	}
	data, _ := json.MarshalIndent(measurements, "", "  ")
	t.Logf("image measurements: %s", data)
	if dest := os.Getenv("MAILPIPE_RENDER_EVIDENCE"); dest != "" {
		name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
		dest = filepath.Join(dest, name)
		if err := os.MkdirAll(dest, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, file := range append(files, path) {
			b, e := os.ReadFile(file)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(dest, filepath.Base(file)), b, 0o600); e != nil {
				t.Fatal(e)
			}
		}
		if err := os.WriteFile(filepath.Join(dest, "measurements.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func nearImageColor(c color.RGBA, want color.RGBA) bool {
	return absImage(int(c.R)-int(want.R)) < 25 && absImage(int(c.G)-int(want.G)) < 25 && absImage(int(c.B)-int(want.B)) < 25
}
func absImage(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func measureImagePages(pages []image.Image, fixtures []imageFixture) ([]imagePDFMeasurement, error) {
	var out []imagePDFMeasurement
	lastPage := -1
	seenPages := make([]bool, len(pages))
	for index, f := range fixtures {
		found := -1
		var foundBounds, foundCircle image.Rectangle
		for page, im := range pages {
			// Convert once rather than boxing a color through image.Image.At
			// for each palette comparison (particularly expensive under -race).
			rgba, ok := im.(*image.RGBA)
			if !ok {
				rgba = image.NewRGBA(im.Bounds())
				draw.Draw(rgba, rgba.Bounds(), im, im.Bounds().Min, draw.Src)
			}
			var bounds [5]image.Rectangle
			var counts [5]int
			for y := im.Bounds().Min.Y; y < im.Bounds().Max.Y; y++ {
				for x := im.Bounds().Min.X; x < im.Bounds().Max.X; x++ {
					pixel := rgba.RGBAAt(x, y)
					for k, c := range imagePalettes[f.palette] {
						if nearImageColor(pixel, c) {
							r := image.Rect(x, y, x+1, y+1)
							if counts[k] == 0 {
								bounds[k] = r
							} else {
								bounds[k] = bounds[k].Union(r)
							}
							counts[k]++
							break
						}
					}
				}
			}
			present := false
			// Only an edge identifies an image on a page. A resampled corner
			// of another palette can coincide with an interior-circle color.
			for _, n := range counts[:4] {
				present = present || n > 0
			}
			if !present {
				continue
			}
			for k, n := range counts {
				if n < 8 {
					return nil, fmt.Errorf("image %d page %d incomplete marker %d (%d pixels), counts %v bounds %v", index, page, k, n, counts, bounds)
				}
				if k < 4 && float64(n) < 0.9*float64(bounds[k].Dx()*bounds[k].Dy()) {
					return nil, fmt.Errorf("image %d page %d discontinuous edge band %d", index, page, k)
				}
			}
			box := bounds[0].Union(bounds[1]).Union(bounds[2]).Union(bounds[3])
			if box.Min.X < 46 || box.Max.X > 770 || box.Min.Y < 46 || box.Max.Y > 1010 {
				return nil, fmt.Errorf("image %d outside printable bounds: %v", index, box)
			}
			if bounds[0].Min.Y > box.Min.Y+2 || bounds[2].Max.Y < box.Max.Y-2 || bounds[3].Min.X > box.Min.X+2 || bounds[1].Max.X < box.Max.X-2 {
				return nil, fmt.Errorf("image %d edge ordering", index)
			}
			if bounds[0].Dx()*10 < box.Dx()*8 || bounds[2].Dx()*10 < box.Dx()*8 || bounds[1].Dy()*10 < box.Dy()*8 || bounds[3].Dy()*10 < box.Dy()*8 {
				return nil, fmt.Errorf("image %d shortened edge bands", index)
			}
			ratio := float64(f.width) / float64(f.height)
			if math.Abs(float64(box.Dx())-float64(box.Dy())*ratio) > 2+0.02*float64(box.Dx()) {
				return nil, fmt.Errorf("image %d distorted: %v source %dx%d", index, box, f.width, f.height)
			}
			circle := bounds[4]
			if absImage(circle.Dx()-circle.Dy()) > 2+circle.Dx()/50 {
				return nil, fmt.Errorf("image %d distorted interior circle: %v", index, circle)
			}
			if found != -1 {
				return nil, fmt.Errorf("image %d appears on multiple pages", index)
			}
			found = page
			foundBounds = box
			foundCircle = circle
		}
		if found < lastPage || found < 0 {
			return nil, fmt.Errorf("image %d missing or out of order", index)
		}
		if len(out) > 0 && found == lastPage && foundBounds.Min.Y < out[len(out)-1].Bounds.Max.Y-2 {
			return nil, fmt.Errorf("image %d overlaps or precedes prior image", index)
		}
		lastPage = found
		seenPages[found] = true
		out = append(out, imagePDFMeasurement{found, index, foundBounds, foundCircle, float64(foundBounds.Dx()) / float64(foundBounds.Dy())})
	}
	for i, seen := range seenPages {
		if !seen {
			return nil, fmt.Errorf("blank/image-free page %d", i)
		}
	}
	return out, nil
}
