package document

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func testScope(t *testing.T, l Limits) *Scope {
	t.Helper()
	s, e := NewScope(l)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := s.Close(); e != nil {
			t.Error(e)
		}
	})
	return s
}
func TestHTMLOnlyReadableText(t *testing.T) {
	input := `<html><head><style>secret-style</style></head><body><p>Deadline &amp; dates</p><script>secret-script</script><img src="file:///example.net/canary" alt="Diagram"><table><tr><td>2030-12-31</td></tr></table><iframe src="https://example.org/private">secret-frame</iframe></body></html>`
	var out bytes.Buffer
	if e := HTMLReadable(context.Background(), strings.NewReader(input), &out, Limits{}); e != nil {
		t.Fatal(e)
	}
	text := out.String()
	for _, want := range []string{"Deadline & dates", "Diagram", "2030-12-31"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	for _, bad := range []string{"secret-", "file:", "https:"} {
		if strings.Contains(text, bad) {
			t.Fatalf("unexpected %q in %q", bad, text)
		}
	}
	// Resource names are inert data: canary paths are never passed to an opener.
	path := filepath.Join(t.TempDir(), "canary")
	if e := os.WriteFile(path, []byte("PRIVATE-CANARY"), 0600); e != nil {
		t.Fatal(e)
	}
	var b bytes.Buffer
	if e := HTMLReadable(context.Background(), strings.NewReader(`<img src="`+path+`"><object data="`+path+`">hidden</object>`), &b, Limits{}); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(b.String(), "CANARY") {
		t.Fatal("local resource loaded")
	}
	got, e := os.ReadFile(path)
	if e != nil || string(got) != "PRIVATE-CANARY" {
		t.Fatal("positive canary control failed")
	}
}

// generatedReader never owns a payload-sized byte slice and observes every
// production read request. Allocation measurements are supplementary below.
type generatedReader struct {
	left, maxRequest int64
	value            byte
}

func (r *generatedReader) Read(p []byte) (int, error) {
	if int64(len(p)) > r.maxRequest {
		r.maxRequest = int64(len(p))
	}
	if r.left == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), r.left)
	for i := range p[:n] {
		p[i] = r.value
	}
	r.left -= n
	return int(n), nil
}
func TestIMAPContentResourceBounds(t *testing.T) {
	ctx := context.Background()
	t.Run("aggregate-default-exact-and-plus-one", func(t *testing.T) {
		s := testScope(t, Limits{})
		var blobs []*Blob
		for range 4 {
			b, e := s.NewBlob(ctx, 64<<20, 64<<20)
			if e != nil {
				t.Fatal(e)
			}
			blobs = append(blobs, b)
			r := &generatedReader{left: 64 << 20, value: 'Z'}
			if e = copyChunks(ctx, b, r); e != nil {
				t.Fatal(e)
			}
			if r.maxRequest > 32768 {
				t.Fatal("unbounded read", r.maxRequest)
			}
			if e = b.Seal(); e != nil {
				t.Fatal(e)
			}
		}
		if u := s.Usage(); u.Disk != 256<<20 || u.Reservations != 0 {
			t.Fatalf("quota %+v", u)
		}
		b, e := s.NewBlob(ctx, 1, 1)
		if !errors.Is(e, ErrIncomplete) || b != nil {
			t.Fatalf("quota+1 %v", e)
		}
		for _, b := range blobs {
			if e = b.Close(); e != nil {
				t.Fatal(e)
			}
		}
		if u := s.Usage(); u.Disk != 0 || u.Files != 0 {
			t.Fatalf("release %+v", u)
		}
	})
	t.Run("concurrent-reservations", func(t *testing.T) {
		l := DefaultLimits()
		l.DiskBytes = 32768
		s := testScope(t, l)
		start := make(chan struct{})
		results := make(chan *Blob, 2)
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; b, _ := s.NewBlob(ctx, 32768, 32768); results <- b }()
		}
		close(start)
		wg.Wait()
		close(results)
		success := 0
		for b := range results {
			if b != nil {
				success++
				if e := b.Close(); e != nil {
					t.Fatal(e)
				}
			}
		}
		if success != 1 {
			t.Fatalf("overcommitted: %d", success)
		}
		if s.Usage().Disk != 0 {
			t.Fatal("undercount/release")
		}
	})
	t.Run("actual-writes-and-decoded-expansion", func(t *testing.T) {
		l := DefaultLimits()
		l.DiskBytes = 100
		s := testScope(t, l)
		a, e := s.NewBlob(ctx, 100, 0)
		if e != nil {
			t.Fatal(e)
		}
		b, e := s.NewBlob(ctx, 100, 0)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = a.Write(bytes.Repeat([]byte{'a'}, 60)); e != nil {
			t.Fatal(e)
		}
		if _, e = b.Write(bytes.Repeat([]byte{'b'}, 40)); e != nil {
			t.Fatal(e)
		}
		if _, e = b.Write([]byte("x")); !errors.Is(e, ErrIncomplete) {
			t.Fatal("actual quota overrun", e)
		}
		if s.Usage().Disk != 100 {
			t.Fatal("overrun charged/written")
		}
		if e = errors.Join(a.Close(), b.Close()); e != nil {
			t.Fatal(e)
		}
		l = DefaultLimits()
		l.DecodedBytes = 5
		s2 := testScope(t, l)
		_, e = ParseReader(ctx, strings.NewReader("From: a@example.org\r\nContent-Type: text/plain; charset=windows-1252\r\n\r\n\x80\x80"), s2)
		if !errors.Is(e, ErrIncomplete) {
			t.Fatal("UTF8 expansion not counted", e)
		}
	})
	t.Run("long-pass-and-private-modes", func(t *testing.T) {
		s := testScope(t, Limits{})
		f, e := s.root.Open(".")
		if e != nil {
			t.Fatal(e)
		}
		info, e := f.Stat()
		if e != nil || !owned(info, true) {
			t.Fatal("directory proof", e)
		}
		if e = f.Close(); e != nil {
			t.Fatal(e)
		}
		for i := 0; i < 257; i++ {
			b, e := s.NewBlob(ctx, 32768, 0)
			if e != nil {
				t.Fatal(e)
			}
			if !owned(b.info, false) {
				t.Fatal("file proof")
			}
			st := b.info.Sys().(*syscall.Stat_t)
			if st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
				t.Fatal("fstat proof")
			}
			if _, e = b.Write([]byte("complete")); e != nil {
				t.Fatal(e)
			}
			if e = b.Close(); e != nil {
				t.Fatal(e)
			}
			if i%16 == 0 {
				if e = s.Checkpoint(ctx); e != nil {
					t.Fatal(e)
				}
			}
			if s.Usage().Disk != 0 {
				t.Fatal("in-pass growth")
			}
		}
		// Root aliases resolve to one pinned parent; actual sticky and private
		// macOS temp parents exercise the production stat adapter without skips.
		alias := filepath.Join(t.TempDir(), "alias")
		if e = os.Symlink(os.TempDir(), alias); e != nil {
			t.Fatal(e)
		}
		t.Setenv("TMPDIR", alias)
		other := testScope(t, Limits{})
		if other.path == s.path {
			t.Fatal("operation names reused")
		}
	})
	t.Run("cleanup-failure-retains-charge-and-owned-only", func(t *testing.T) {
		s, e := NewScope(Limits{})
		if e != nil {
			t.Fatal(e)
		}
		b, e := s.NewBlob(ctx, 100, 0)
		if e != nil {
			t.Fatal(e)
		}
		_, e = b.Write([]byte("owned"))
		if e != nil {
			t.Fatal(e)
		}
		other := testScope(t, Limits{})
		live, e := other.NewBlob(ctx, 100, 0)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = live.Write([]byte("active")); e != nil {
			t.Fatal(e)
		}
		defer live.Close()
		remove := s.remove
		s.remove = func(context.Context, string) error { return syscall.EACCES }
		if e = b.Close(); !errors.Is(e, ErrCleanup) {
			t.Fatal("cleanup hidden", e)
		}
		if s.Usage().Disk != 5 || !s.Usage().Poisoned {
			t.Fatal("false disk release")
		}
		if _, e = s.Enter(ctx); !errors.Is(e, ErrCleanup) {
			t.Fatal("poison admission", e)
		}
		if _, e = live.Write([]byte("still-active")); e != nil {
			t.Fatal("other scope swept", e)
		}
		s.remove = remove
		if e = s.Checkpoint(ctx); !errors.Is(e, ErrCleanup) {
			t.Fatal("cleanup uncertainty lost", e)
		}
		if s.Usage().Disk != 0 {
			t.Fatal("verified removal not released")
		}
		if e = s.Close(); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("active-reader-and-symlink-refusal", func(t *testing.T) {
		s, e := NewScope(Limits{})
		if e != nil {
			t.Fatal(e)
		}
		b, e := s.NewBlob(ctx, 100, 0)
		if e != nil {
			t.Fatal(e)
		}
		_, _ = b.Write([]byte("owned"))
		if e = b.Seal(); e != nil {
			t.Fatal(e)
		}
		r, e := b.Open()
		if e != nil {
			t.Fatal(e)
		}
		if e = b.Close(); !errors.Is(e, ErrCleanup) {
			t.Fatal("active reader deleted", e)
		}
		if _, e = io.ReadAll(r); e != nil {
			t.Fatal(e)
		}
		if e = r.Close(); e != nil {
			t.Fatal(e)
		}
		if e = b.Close(); e != nil {
			t.Fatal(e)
		}
		if e = s.Close(); e != nil {
			t.Fatal(e)
		}
		s2 := testScope(t, Limits{})
		b2, e := s2.NewBlob(ctx, 100, 0)
		if e != nil {
			t.Fatal(e)
		}
		_, _ = b2.Write([]byte("owned"))
		if e = b2.Seal(); e != nil {
			t.Fatal(e)
		}
		if e = s2.root.Rename(b2.name, "original"); e != nil {
			t.Fatal(e)
		}
		if e = s2.root.Symlink("original", b2.name); e != nil {
			t.Fatal(e)
		}
		if _, e = b2.Open(); e == nil {
			t.Fatal("symlink reopened")
		}
		if e = s2.root.Remove(b2.name); e != nil {
			t.Fatal(e)
		}
		if e = s2.root.Rename("original", b2.name); e != nil {
			t.Fatal(e)
		}
		if e = b2.Close(); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("cleanup-context-cancel-values-deadline", func(t *testing.T) {
		type key struct{}
		parent, cancel := context.WithCancel(context.WithValue(ctx, key{}, "operation"))
		cancel()
		scope := testScope(t, Limits{})
		blob, e := scope.NewBlob(ctx, 10, 0)
		if e != nil {
			t.Fatal(e)
		}
		original := scope.remove
		observed := false
		scope.remove = func(actual context.Context, name string) error {
			observed = true
			if actual.Err() != nil || actual.Value(key{}) != "operation" {
				t.Fatal("actual cleanup inherited cancellation/lost values")
			}
			deadline, ok := actual.Deadline()
			if !ok || time.Until(deadline) > 5*time.Second {
				t.Fatal("actual cleanup has no bounded independent deadline")
			}
			return original(actual, name)
		}
		if e = blob.CloseContext(parent); e != nil {
			t.Fatal(e)
		}
		scope.remove = original
		if !observed {
			t.Fatal("actual remove observer not invoked")
		}
		cleanup, stop := cleanupContext(parent)
		defer stop()
		if cleanup.Err() != nil || cleanup.Value(key{}) != "operation" {
			t.Fatal("cleanup inherited cancellation/lost values")
		}
		d, ok := cleanup.Deadline()
		if !ok || time.Until(d) > 5*time.Second || time.Until(d) < 4*time.Second {
			t.Fatal("cleanup deadline", d)
		}
		expired, cancel := context.WithDeadline(parent, time.Now().Add(-time.Second))
		defer cancel()
		cc, stop := cleanupContext(expired)
		defer stop()
		if cc.Err() != nil {
			t.Fatal("cleanup inherited expired deadline")
		}
	})
	t.Run("raw-declared-actual-and-cancel", func(t *testing.T) {
		l := DefaultLimits()
		l.RawBytes = 100
		s := testScope(t, l)
		if b, e := s.NewBlob(ctx, 100, 101); b != nil || !errors.Is(e, ErrIncomplete) {
			t.Fatal("declared cap", e)
		}
		source, e := StreamSource(ctx, s, func(w io.Writer) error { _, _ = w.Write(bytes.Repeat([]byte{'x'}, 101)); return nil })
		if source != nil || !errors.Is(e, ErrIncomplete) {
			t.Fatal("ignored writer overflow accepted", e)
		}
		if s.Usage().Disk != 0 {
			t.Fatal("partial source leaked")
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		called := false
		source, e = StreamSource(canceled, s, func(io.Writer) error { called = true; return nil })
		if source != nil || !errors.Is(e, context.Canceled) || called {
			t.Fatal("canceled source invoked", e)
		}
	})
}

func TestParserBoundsAndCompleteBoundary(t *testing.T) {
	ctx := context.Background()
	header := "From: a@example.org\r\nSubject: complete\r\n\r\n"
	for _, n := range []int{100, 101} {
		l := DefaultLimits()
		l.TextBytes = 100
		s := testScope(t, l)
		d, e := ParseReader(ctx, strings.NewReader(header+strings.Repeat("x", n)), s)
		if n == 100 {
			if e != nil {
				t.Fatal(e)
			}
			got, e := d.Text(ctx)
			if e != nil || len(got) != 100 {
				t.Fatal("boundary incomplete", e)
			}
			if e = d.Close(); e != nil {
				t.Fatal(e)
			}
		} else if !errors.Is(e, ErrIncomplete) {
			t.Fatal("boundary+1", e)
		}
	}
	for _, raw := range []string{
		"From: a@example.org\r\nContent-Transfer-Encoding: base64\r\n\r\naW52YWxpZA",
		"From: a@example.org\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n\r\nmissing closing boundary",
		"From: a@example.org\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n\xff",
	} {
		s := testScope(t, Limits{})
		if d, e := ParseReader(ctx, strings.NewReader(raw), s); e == nil {
			d.Close()
			t.Fatal("incomplete MIME accepted")
		}
		if s.Usage().Disk != 0 {
			t.Fatal("error spools leaked")
		}
	}
}
func TestLimitsFiniteValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Limits){"disk": func(l *Limits) { l.DiskBytes = 1<<30 + 1 }, "memory": func(l *Limits) { l.MemoryBytes = 1 }, "negative": func(l *Limits) { l.PartBytes = -1 }, "zero": func(l *Limits) { l.RawBytes = 0 }, "depth": func(l *Limits) { l.Depth = 33 }, "timeout": func(l *Limits) { l.Timeout = 11 * time.Minute }} {
		t.Run(name, func(t *testing.T) {
			l := DefaultLimits()
			mutate(&l)
			if _, e := l.Resolve(); e == nil {
				t.Fatal("invalid limit accepted")
			}
		})
	}
	l := DefaultLimits()
	l.DiskBytes = 1 << 30
	if _, e := l.Resolve(); e != nil {
		t.Fatal("hard ceiling", e)
	}
}

func TestMIMEStructuralBounds(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		raw    string
		limits Limits
	}{
		{"deep", nestedMIME(22), DefaultLimits()},
		{"parts", manyParts(257), DefaultLimits()},
		{"header", "From: a@example.org\r\nX-Long: " + strings.Repeat("x", 65<<10) + "\r\n\r\nbody", DefaultLimits()},
		{"part-header", "From: a@example.org\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nX-Long: " + strings.Repeat("x", 80<<10) + "\r\n\r\nbody\r\n--x--\r\n", DefaultLimits()},
		{"QP", "From: a@example.org\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n=ZZ", DefaultLimits()},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := testScope(t, test.limits)
			d, e := ParseReader(ctx, strings.NewReader(test.raw), s)
			if e == nil {
				d.Close()
				t.Fatal("hostile input accepted")
			}
			if !errors.Is(e, ErrIncomplete) {
				t.Fatalf("untyped protective failure %v", e)
			}
			if s.Usage().Files != 0 {
				t.Fatal("failure retained spools")
			}
		})
	}
	l := DefaultLimits()
	l.HTMLBuffer = 32
	var out bytes.Buffer
	if e := HTMLReadable(ctx, strings.NewReader("<p>"+strings.Repeat("x", 100)+"</p>"), &out, l); !errors.Is(e, ErrIncomplete) {
		t.Fatal("HTML token buffer", e)
	}
}
func nestedMIME(depth int) string {
	body := "Content-Type: text/plain\r\n\r\ncomplete"
	for i := 0; i < depth; i++ {
		boundary := strings.Repeat("x", i+1)
		body = "Content-Type: multipart/mixed; boundary=" + boundary + "\r\n\r\n--" + boundary + "\r\n" + body + "\r\n--" + boundary + "--\r\n"
	}
	return "From: a@example.org\r\n" + body
}
func manyParts(n int) string {
	var b strings.Builder
	b.WriteString("From: a@example.org\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n")
	for range n {
		b.WriteString("--x\r\nContent-Type: text/plain\r\n\r\ncomplete\r\n")
	}
	b.WriteString("--x--\r\n")
	return b.String()
}

func TestExactStructuralAndHeaderBoundaries(t *testing.T) {
	ctx := context.Background()
	for _, depth := range []int{20, 21, 32, 33} {
		l := DefaultLimits()
		if depth >= 32 {
			l.Depth = 32
		}
		s := testScope(t, l)
		d, e := ParseReader(ctx, strings.NewReader(nestedMIME(depth)), s)
		want := depth == 20 || depth == 32
		if (e == nil) != want {
			t.Fatalf("depth%d: %v", depth, e)
		}
		if d != nil {
			if e = d.Close(); e != nil {
				t.Fatal(e)
			}
		}
	}
	for _, nodes := range []int{256, 257, 1024, 1025} {
		l := DefaultLimits()
		if nodes >= 1024 {
			l.Parts = 1024
			l.Files = 4096
		}
		s := testScope(t, l)
		d, e := ParseReader(ctx, strings.NewReader(manyParts(nodes-1)), s)
		want := nodes == 256 || nodes == 1024
		if (e == nil) != want {
			t.Fatalf("nodes%d: %v", nodes, e)
		}
		if d != nil {
			if e = d.Close(); e != nil {
				t.Fatal(e)
			}
		}
	}
	for _, extra := range []int{0, 1} {
		l := DefaultLimits()
		s := testScope(t, l)
		fixed := "Content-Type: text/plain\r\nX-Fold: value\r\n \r\n\r\n"
		// Folded whitespace must be charged as original wire bytes, even though
		// net/textproto unfolds and trims it from parsed header values.
		fill := int(l.HeaderBytes) - len(fixed) + extra
		part := strings.Replace(fixed, " \r\n", " "+strings.Repeat(" ", fill)+"\r\n", 1)
		raw := "From: a@example.org\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n" + part + "complete\r\n--x--\r\n"
		d, e := ParseReader(ctx, strings.NewReader(raw), s)
		if (e == nil) != (extra == 0) {
			t.Fatalf("folded header extra%d: %v", extra, e)
		}
		if d != nil {
			if e = d.Close(); e != nil {
				t.Fatal(e)
			}
		}
	}
}
func TestCancellationDuringDecodeCleansAllSpools(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := testScope(t, Limits{})
	r := &cancelReader{r: io.MultiReader(strings.NewReader("From: a@example.org\r\nContent-Type: application/octet-stream\r\n\r\n"), &generatedReader{left: 4 << 20, value: 'X'}), cancel: cancel}
	d, e := ParseReader(ctx, r, s)
	if d != nil || !errors.Is(e, context.Canceled) {
		t.Fatalf("cancellation %v", e)
	}
	if u := s.Usage(); u.Files != 0 || u.Disk != 0 || u.Memory != 0 {
		t.Fatal("canceled resources retained", u)
	}
}

type cancelReader struct {
	r      io.Reader
	cancel context.CancelFunc
	read   int
}

func (r *cancelReader) Read(b []byte) (int, error) {
	n, e := r.r.Read(b)
	r.read += n
	if r.read > 65536 {
		r.cancel()
	}
	return n, e
}
func TestAnnouncedSourceQuotaBeforeBodyWrite(t *testing.T) {
	l := DefaultLimits()
	l.DiskBytes = 100
	s := testScope(t, l)
	wrote := false
	source, e := StreamSource(context.Background(), s, func(w io.Writer) error {
		a, ok := w.(interface{ AnnounceSourceSize(int64) error })
		if !ok {
			t.Fatal("transport reservation seam unavailable")
		}
		if e := a.AnnounceSourceSize(101); e != nil {
			return e
		}
		wrote = true
		_, e := w.Write([]byte("partial"))
		return e
	})
	if source != nil || !errors.Is(e, ErrIncomplete) || wrote {
		t.Fatal("announced quota checked too late", e)
	}
	if s.Usage().Disk != 0 {
		t.Fatal("failed announcement retained source")
	}
}

func TestHTMLResourceObserverPositiveControl(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, "RESOURCE-CANARY")
	}))
	defer server.Close()
	var out bytes.Buffer
	if e := HTMLReadable(context.Background(), strings.NewReader(`<img src="`+server.URL+`/image"><iframe src="`+server.URL+`/frame"></iframe><style>@import url(`+server.URL+`/css)</style><p>Visible</p>`), &out, Limits{}); e != nil {
		t.Fatal(e)
	}
	if requests.Load() != 0 || strings.Contains(out.String(), "RESOURCE-CANARY") {
		t.Fatal("HTML extraction fetched resource")
	}
	response, e := http.Get(server.URL + "/positive-control")
	if e != nil {
		t.Fatal(e)
	}
	_, e = io.Copy(io.Discard, response.Body)
	e = errors.Join(e, response.Body.Close())
	if e != nil || requests.Load() != 1 {
		t.Fatal("resource observer positive control failed", e)
	}
}

type contentFixture struct{ raw string }

func (s contentFixture) Account() mailbox.Account {
	return mailbox.Account{ID: "fixture", ProviderID: "imap"}
}
func (s contentFixture) WriteMIME(ctx context.Context, _ mailbox.MessageRef, w io.Writer) error {
	if a, ok := w.(interface{ AnnounceSourceSize(int64) error }); ok {
		if e := a.AnnounceSourceSize(int64(len(s.raw))); e != nil {
			return e
		}
	}
	_, e := io.WriteString(w, s.raw)
	return e
}
func TestRawDecodedDerivedShareActualQuota(t *testing.T) {
	raw := "From: a@example.org\r\nContent-Type: text/plain; charset=windows-1252\r\n\r\n" + strings.Repeat("\x80", 10)
	expected := strings.Repeat("€", 10)
	quota := int64(len(raw) + 2*len(expected))
	ref := mailbox.MessageRef{AccountID: "fixture", MessageID: "one", Locator: "one"}
	for _, extra := range []int64{0, -1} {
		l := DefaultLimits()
		l.DiskBytes = quota + extra
		s := testScope(t, l)
		var peak int64
		remove := s.remove
		s.remove = func(ctx context.Context, name string) error {
			peak = max(peak, s.Usage().Disk)
			return remove(ctx, name)
		}
		text, e := ReadText(context.Background(), contentFixture{raw}, ref, s)
		if extra == 0 {
			if e != nil || text != expected || peak != quota {
				t.Fatalf("exact expanded quota text=%q peak=%d want=%d err=%v", text, peak, quota, e)
			}
		} else if !errors.Is(e, ErrIncomplete) || text != "" {
			t.Fatal("expanded quota-1 returned partial text", e)
		}
		if u := s.Usage(); u.Files != 0 || u.Disk != 0 || u.Memory != 0 {
			t.Fatal("expanded resources leaked", u)
		}
		t.Logf("quota=%d peak=%d returned-bytes=%d error=%v", quota+extra, peak, len(text), e)
	}
}
func TestReadTextCleanupFailureDiscardsCompleteText(t *testing.T) {
	s, e := NewScope(Limits{})
	if e != nil {
		t.Fatal(e)
	}
	remove := s.remove
	s.remove = func(context.Context, string) error { return syscall.EACCES }
	ref := mailbox.MessageRef{AccountID: "fixture", MessageID: "one", Locator: "one"}
	raw := "From: a@example.org\r\n\r\ncomplete"
	text, e := ReadText(context.Background(), contentFixture{raw}, ref, s)
	if text != "" || !errors.Is(e, ErrCleanup) || !errors.Is(e, ErrIncomplete) {
		t.Fatalf("cleanup accepted content %q %v", text, e)
	}
	u := s.Usage()
	if u.Disk <= 0 || !u.Poisoned || u.Memory != 0 || u.Active != 0 {
		t.Fatalf("uncertain cleanup false release %+v", u)
	}
	t.Logf("failed cleanup retained %+v", u)
	s.remove = remove
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	if s.Usage().Disk != 0 {
		t.Fatal("verified final cleanup did not release")
	}
}
func TestHeaderFieldAndParameterBoundaries(t *testing.T) {
	for _, count := range []int{256, 257} {
		var raw strings.Builder
		raw.WriteString("From: a@example.org\r\n")
		for i := 1; i < count; i++ {
			fmt.Fprintf(&raw, "X-%d: v\r\n", i)
		}
		raw.WriteString("\r\ncomplete")
		s := testScope(t, Limits{})
		d, e := ParseReader(context.Background(), strings.NewReader(raw.String()), s)
		if (e == nil) != (count == 256) {
			t.Fatalf("header fields%d: %v", count, e)
		}
		if d != nil {
			if e = d.Close(); e != nil {
				t.Fatal(e)
			}
		}
		raw.Reset()
		raw.WriteString("From: a@example.org\r\nContent-Type: text/plain")
		for i := 0; i < count; i++ {
			fmt.Fprintf(&raw, "; x%d=v", i)
		}
		raw.WriteString("\r\n\r\ncomplete")
		d, e = ParseReader(context.Background(), strings.NewReader(raw.String()), s)
		if (e == nil) != (count == 256) {
			t.Fatalf("parameters%d: %v", count, e)
		}
		if d != nil {
			if e = d.Close(); e != nil {
				t.Fatal(e)
			}
		}
	}
}

func TestUnsafeRootAndFileModesFailClosed(t *testing.T) {
	parent := t.TempDir()
	if e := os.Chmod(parent, 0777); e != nil {
		t.Fatal(e)
	}
	t.Setenv("TMPDIR", parent)
	if scope, e := NewScope(Limits{}); e == nil {
		scope.Close()
		t.Fatal("nonsticky writable parent accepted")
	}
	if e := os.Chmod(parent, 0700); e != nil {
		t.Fatal(e)
	}
	s := testScope(t, Limits{})
	b, e := s.NewBlob(context.Background(), 100, 0)
	if e != nil {
		t.Fatal(e)
	}
	if e = b.Seal(); e != nil {
		t.Fatal(e)
	}
	if e = s.root.Chmod(b.name, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e = b.Open(); !errors.Is(e, ErrIncomplete) {
		t.Fatal("unproved mode accepted", e)
	}
	if e = s.root.Chmod(b.name, 0600); e != nil {
		t.Fatal(e)
	}
	if e = b.Close(); e != nil {
		t.Fatal(e)
	}
}

func TestFoldedRootHeaderAndDisplayAreComplete(t *testing.T) {
	raw := "From: a@example.org\nX-Fold: first\n " + strings.Repeat(" ", 9000) + "\n \nSubject: trailing preserved\n\nComplete body"
	s := testScope(t, Limits{})
	source, e := StreamSource(context.Background(), s, func(w io.Writer) error { _, e := io.WriteString(w, raw); return e })
	if e != nil {
		t.Fatal(e)
	}
	header, e := source.Header(context.Background())
	if e != nil || !strings.Contains(header, "Subject: trailing preserved\n") || strings.Contains(header, "Complete body") {
		t.Fatalf("header incomplete %q %v", header, e)
	}
	r, e := source.Open()
	if e != nil {
		t.Fatal(e)
	}
	d, e := ParseReader(context.Background(), r, s)
	e = errors.Join(e, r.Close())
	if e != nil {
		t.Fatal(e)
	}
	text, e := d.Text(context.Background())
	if e != nil || text != "Complete body" {
		t.Fatalf("folded header corrupted body %q %v", text, e)
	}
	if e = errors.Join(d.Close(), source.Close()); e != nil {
		t.Fatal(e)
	}
}
