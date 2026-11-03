package document

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"image"
	"image/png"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestPartsInspectionRetainsUnnamedImagesAndExactBytes(t *testing.T) {
	payload := []byte{0, 1, 2, 255, '\r', '\n'}
	raw := "From: sender@example.org\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nBody\r\n--x\r\nContent-Type: image/png\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString(payload) + "\r\n--x--\r\n"
	s, err := NewScope(Limits{})
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParsePartsReader(context.Background(), strings.NewReader(raw), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Parts) != 1 || d.Parts[0].Path != "1.2" || d.Parts[0].Kind != "inline_image" {
		t.Fatalf("parts: %+v", d.Parts)
	}
	r, err := d.Parts[0].Data.Open()
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(b) != sha256.Sum256(payload) {
		t.Fatal("decoded bytes changed")
	}
	if err = errors.Join(r.Close(), d.Close(), s.Close()); err != nil {
		t.Fatal(err)
	}
	legacy, err := Parse([]byte(raw))
	if err != nil || len(legacy.Attachments) != 0 || len(legacy.inline) != 0 || !strings.Contains(legacy.Text, "Body") {
		t.Fatalf("legacy changed: %+v %v", legacy, err)
	}
}

func partsMIME(parts ...string) string {
	return "From: sender@example.org\r\nSubject: Synthetic\r\nContent-Type: multipart/mixed; boundary=outer\r\n\r\n--outer\r\n" + strings.Join(parts, "\r\n--outer\r\n") + "\r\n--outer--\r\n"
}

func TestPartsImageKindsFollowLegacyAttachmentPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, headers, kind string
		attachments, inline int
	}{
		{"named-no-cid", "Content-Disposition: inline; filename=picture.png\r\n", "attachment", 1, 0},
		{"unnamed-no-cid", "", "inline_image", 0, 0},
		{"named-cid", "Content-Disposition: inline; filename=picture.png\r\nContent-ID: <picture@example.org>\r\n", "inline_image", 0, 1},
		{"explicit-attachment", "Content-Disposition: attachment; filename=picture.png\r\nContent-ID: <picture@example.org>\r\n", "attachment", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "From: sender@example.org\r\nContent-Type: image/png\r\n" + tc.headers + "\r\nimage-bytes"
			s := testScope(t, Limits{})
			d, e := ParsePartsReader(context.Background(), strings.NewReader(raw), s)
			if e != nil {
				t.Fatal(e)
			}
			defer d.Close()
			if len(d.Attachments) != tc.attachments || len(d.Inline) != tc.inline {
				t.Fatalf("legacy classification changed: %d attachments %d inline", len(d.Attachments), len(d.Inline))
			}
			if len(d.Parts) != 1 || d.Parts[0].Kind != tc.kind {
				t.Fatalf("inventory kind %+v; want %s", d.Parts, tc.kind)
			}
			legacy, e := Parse([]byte(raw))
			if e != nil {
				t.Fatal(e)
			}
			if len(legacy.Attachments) != tc.attachments || len(legacy.inline) != tc.inline {
				t.Fatal("compatibility classification changed")
			}
		})
	}
}

func TestPartsMixedNestedInventoryAndCompatibility(t *testing.T) {
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	img := "Content-Type: image/png\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString(imageBytes.Bytes())
	attached := "Content-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=same.bin\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n=00=FFexact=0D=0A"
	nested := "Content-Type: message/rfc822\r\n\r\nFrom: nested@example.net\r\n" + img
	opaque := "Content-Type: message/rfc822\r\nContent-Disposition: attachment\r\n\r\nFrom: nested@example.net\r\n" + img
	container := "Content-Type: multipart/mixed; boundary=inner\r\nContent-Disposition: attachment; filename=container.bin\r\n\r\n--inner\r\n" + img + "\r\n--inner--\r\n"
	raw := partsMIME("Content-Type: text/plain\r\n\r\nBody", img, attached, attached, nested, opaque, container)
	s := testScope(t, Limits{})
	d, err := ParsePartsReader(context.Background(), strings.NewReader(raw), s)
	if err != nil {
		t.Fatal(err)
	}
	var paths, kinds []string
	for _, p := range d.Parts {
		paths = append(paths, p.Path)
		kinds = append(kinds, p.Kind)
	}
	if !reflect.DeepEqual(paths, []string{"1.2", "1.3", "1.4", "1.5.1", "1.6", "1.7"}) || !reflect.DeepEqual(kinds, []string{"inline_image", "attachment", "attachment", "inline_image", "attachment", "attachment"}) {
		t.Fatalf("paths %v kinds %v", paths, kinds)
	}
	for _, p := range d.Parts {
		r, e := p.Data.Open()
		if e != nil {
			t.Fatal(e)
		}
		data, e := io.ReadAll(r)
		e = errors.Join(e, r.Close())
		if e != nil {
			t.Fatal(e)
		}
		if p.Kind == "inline_image" && !bytes.Equal(data, imageBytes.Bytes()) {
			t.Fatal("generated image changed")
		}
		if p.Filename == "same.bin" && !bytes.Equal(data, []byte{0, 255, 'e', 'x', 'a', 'c', 't', '\r', '\n'}) {
			t.Fatalf("QP changed %q", data)
		}
		if p.Path == "1.6" && string(data) != "From: nested@example.net\r\n"+img {
			t.Fatal("attached message bytes changed")
		}
	}
	inspected, err := d.Materialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inspected, legacy) {
		t.Fatal("inspection changed legacy materialization")
	}
	if s.Usage().Files != 0 || s.Usage().Disk != 0 {
		t.Fatal("retained spools")
	}
}

func TestPartsAlternativesRelatedAndSourceOrder(t *testing.T) {
	imagePart := "Content-Type: image/png\r\nContent-ID: <picture@example.org>\r\n\r\nimage"
	for _, ct := range []string{"alternative", "related"} {
		raw := "From: sender@example.org\r\nContent-Type: multipart/" + ct + "; boundary=x\r\n\r\n--x\r\nContent-Type: text/html\r\n\r\n<p>body</p>\r\n--x\r\n" + imagePart + "\r\n--x--\r\n"
		s := testScope(t, Limits{})
		d, e := ParsePartsReader(context.Background(), strings.NewReader(raw), s)
		if e != nil {
			t.Fatal(e)
		}
		if len(d.Parts) != 1 || d.Parts[0].Path != "1.2" || len(d.Inline) != 1 {
			t.Fatal(d.Parts)
		}
		if e = d.Close(); e != nil {
			t.Fatal(e)
		}
	}
	var parts []string
	for range 12 {
		parts = append(parts, "Content-Type: image/png\r\n\r\n")
	}
	s := testScope(t, Limits{})
	d, e := ParsePartsReader(context.Background(), strings.NewReader(partsMIME(parts...)), s)
	if e != nil {
		t.Fatal(e)
	}
	if len(d.Parts) != 12 || d.Parts[9].Path != "1.10" || d.Parts[11].Data.Size() != 0 {
		t.Fatal(d.Parts)
	}
	if e = d.Close(); e != nil {
		t.Fatal(e)
	}
}

type partsZeroReader struct{ largest int }

func (r *partsZeroReader) Read(p []byte) (int, error) {
	if len(p) > r.largest {
		r.largest = len(p)
	}
	clear(p)
	return len(p), nil
}

func TestPartsLargeStreamingDigestAndCleanup(t *testing.T) {
	const size = 8 << 20
	input := &partsZeroReader{}
	raw := io.MultiReader(strings.NewReader("From: synthetic@example.org\r\nContent-Type: image/png\r\n\r\n"), io.LimitReader(input, size))
	s := testScope(t, Limits{})
	d, e := ParsePartsReader(context.Background(), raw, s)
	if e != nil {
		t.Fatal(e)
	}
	if len(d.Parts) != 1 || d.Parts[0].Data.Size() != size || input.largest > 32768 {
		t.Fatal("large streaming contract", input.largest, d.Parts)
	}
	r, e := d.Parts[0].Data.Open()
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.New()
	n, e := io.Copy(h, r)
	e = errors.Join(e, r.Close())
	if e != nil || n != size {
		t.Fatal(n, e)
	}
	expected := sha256.New()
	chunk := make([]byte, 32768)
	for range size / len(chunk) {
		expected.Write(chunk)
	}
	if !bytes.Equal(h.Sum(nil), expected.Sum(nil)) {
		t.Fatal("large payload changed")
	}
	if e = d.Close(); e != nil {
		t.Fatal(e)
	}
	if s.Usage().Disk != 0 || s.Usage().Files != 0 {
		t.Fatal("large payload leaked")
	}
}

func TestPartsProtectiveFailuresAndQuotaCleanup(t *testing.T) {
	for name, raw := range map[string]string{
		"truncated":    strings.TrimSuffix(partsMIME("Content-Type: image/png\r\n\r\nbytes"), "--outer--\r\n"),
		"base64":       "From: a@example.org\r\nContent-Type: image/png\r\nContent-Transfer-Encoding: base64\r\n\r\nYWJj!",
		"QP":           "From: a@example.org\r\nContent-Type: image/png\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n=ZZ",
		"charset":      "From: a@example.org\r\nContent-Type: text/plain; charset=unknown-charset\r\n\r\nbody",
		"duplicateCID": partsMIME("Content-Type: image/png\r\nContent-ID: <same>\r\n\r\na", "Content-Type: image/png\r\nContent-ID: <same>\r\n\r\nb"),
		"depth":        nestedMIME(22), "nodes": manyParts(257),
		"header": "From: a@example.org\r\nX-Huge: " + strings.Repeat("x", 65<<10) + "\r\n\r\nbody",
	} {
		t.Run(name, func(t *testing.T) {
			s := testScope(t, Limits{})
			d, e := ParsePartsReader(context.Background(), strings.NewReader(raw), s)
			if e == nil || d != nil || !errors.Is(e, ErrIncomplete) {
				t.Fatalf("partial success %v %v", d, e)
			}
			if s.Usage().Files != 0 || s.Usage().Disk != 0 {
				t.Fatal("failed parse leaked")
			}
		})
	}
	for _, field := range []string{"raw", "part", "decoded", "disk"} {
		for _, plus := range []bool{false, true} {
			t.Run(field+map[bool]string{false: "exact", true: "plus"}[plus], func(t *testing.T) {
				raw := "From: a@example.org\r\nContent-Type: image/png\r\n\r\n" + strings.Repeat("x", 100)
				l := DefaultLimits()
				limit := int64(100)
				if plus {
					limit--
				}
				switch field {
				case "raw":
					l.RawBytes = int64(len(raw))
					if plus {
						l.RawBytes--
					}
				case "part":
					l.PartBytes = limit
				case "decoded":
					l.DecodedBytes = limit
				case "disk":
					l.DiskBytes = limit
				}
				s := testScope(t, l)
				d, e := ParsePartsReader(context.Background(), strings.NewReader(raw), s)
				if plus {
					if e == nil {
						d.Close()
						t.Fatal("plus-one accepted")
					}
				} else {
					if e != nil {
						t.Fatal(e)
					}
					if e = d.Close(); e != nil {
						t.Fatal(e)
					}
				}
				if s.Usage().Disk != 0 {
					t.Fatal("quota failure leaked")
				}
			})
		}
	}
	s := testScope(t, Limits{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if d, e := ParsePartsReader(ctx, strings.NewReader(partsMIME("Content-Type: image/png\r\n\r\nx")), s); d != nil || !errors.Is(e, context.Canceled) {
		t.Fatalf("cancel %v %v", d, e)
	}
	if _, e := ParsePartsReader(context.Background(), strings.NewReader(""), nil); e == nil {
		t.Fatal("nil scope accepted")
	}
}
