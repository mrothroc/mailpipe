package document

import (
	"context"
	"crypto/sha256"
	"io"
	"runtime"
	"strings"
	"testing"
)

var allocationControl []byte

func allocated(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}
func TestStreamingAttachmentAllocationControl(t *testing.T) {
	ctx := context.Background()
	run := func(n int64) {
		s, e := NewScope(Limits{})
		if e != nil {
			t.Fatal(e)
		}
		head := "From: a@example.org\r\nSubject: generated\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=generated.pdf\r\n\r\n"
		generator := &generatedReader{left: n, value: 'X'}
		source, e := StreamSource(ctx, s, func(w io.Writer) error {
			return copyChunks(ctx, w, io.MultiReader(strings.NewReader(head), generator, strings.NewReader("\r\n--x--\r\n")))
		})
		if e != nil {
			t.Fatal(e)
		}
		rd, e := source.Open()
		if e != nil {
			t.Fatal(e)
		}
		d, e := ParseReader(ctx, rd, s)
		if e != nil {
			t.Fatal(e)
		}
		if e = rd.Close(); e != nil {
			t.Fatal(e)
		}
		if len(d.Attachments) != 1 || d.Attachments[0].Data.Size() != n {
			t.Fatal("incomplete attachment")
		}
		data, e := d.Attachments[0].Data.Open()
		if e != nil {
			t.Fatal(e)
		}
		actual := sha256.New()
		if e = copyChunks(ctx, actual, data); e != nil {
			t.Fatal(e)
		}
		if e = data.Close(); e != nil {
			t.Fatal(e)
		}
		expected := sha256.New()
		if e = copyChunks(ctx, expected, &generatedReader{left: n, value: 'X'}); e != nil {
			t.Fatal(e)
		}
		if string(actual.Sum(nil)) != string(expected.Sum(nil)) {
			t.Fatal("attachment bytes changed")
		}
		if generator.maxRequest > 32768 {
			t.Fatal("unbounded input request", generator.maxRequest)
		}
		if e = d.Close(); e != nil {
			t.Fatal(e)
		}
		if e = source.Close(); e != nil {
			t.Fatal(e)
		}
		if u := s.Usage(); u.Files != 0 || u.Disk != 0 || u.Memory != 0 {
			t.Fatalf("resources retained %+v", u)
		}
		if e = s.Close(); e != nil {
			t.Fatal(e)
		}
	}
	run(1 << 20) // warm module/filesystem paths before supplementary allocation samples
	small := allocated(func() { run(1 << 20) })
	large := allocated(func() { run(32 << 20) })
	if large > small+8<<20 {
		t.Fatalf("payload-proportional allocations: small=%d large=%d", small, large)
	}
	control := allocated(func() {
		allocationControl = make([]byte, 32<<20)
		for i := range allocationControl {
			allocationControl[i] = 'X'
		}
	})
	if control < 32<<20 {
		t.Fatal("allocation positive observer missed full buffer", control)
	}
	allocationControl = nil
	t.Logf("stream allocation bytes: 1MiB=%d 32MiB=%d; full-buffer positive control=%d", small, large, control)
}
