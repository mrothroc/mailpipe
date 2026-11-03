package cli_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
)

func TestAttachmentsRealGraphReadOnlyHTTP(t *testing.T) {
	e, _ := attachmentFixture(t)
	raw := e.graph.mime["g1"]
	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Method != "GET" || r.URL.Path != "/me/messages/g1/$value" {
			http.Error(w, "unexpected request", 500)
			return
		}
		if !strings.Contains(r.Header.Get("Prefer"), "ImmutableId") {
			t.Error("missing immutable identity preference")
		}
		io.WriteString(w, raw)
	}))
	defer srv.Close()
	c := graph.New(func(context.Context) (string, error) { return "synthetic-token", nil })
	c.Base = srv.URL
	c.HTTP = srv.Client()
	subject, err := e.auth.Subject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, err := graphmail.New(c, subject.Account())
	if err != nil {
		t.Fatal(err)
	}
	e.provider = p
	row := attachmentList(t, e, "1")[0]
	if code, out, stderr := e.run("attachments", "extract", "1", "--part", row["part"].(string), "--out-dir", attachmentPrivateDir(t), "--execute", "--json"); code != 0 {
		t.Fatalf("Graph extract %d %s %s", code, out, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatal(requests)
	}
	if len(e.rendered) != 0 || len(e.graph.moved) != 0 {
		t.Fatal("unexpected effects")
	}
}

func TestAttachmentsIMAPCancellationCleansOperationSpools(t *testing.T) {
	e := newYahooEnv(t)
	e.ok("auth", "login")
	e.ok("sync", "--folder", "inbox")
	tmp := attachmentPrivateDir(t)
	t.Setenv("TMPDIR", tmp)
	e.server.mu.Lock()
	e.server.sourcePause = make(chan struct{})
	e.server.sourceReady = make(chan struct{}, 1)
	pause, ready := e.server.sourcePause, e.server.sourceReady
	e.server.mu.Unlock()
	defer close(pause)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		code        int
		out, stderr string
	}
	done := make(chan result, 1)
	go func() {
		code, out, stderr := e.runContext(ctx, "attachments", "list", "1", "--json")
		done <- result{code, out, stderr}
	}()
	select {
	case <-ready:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("source read did not begin")
	}
	select {
	case r := <-done:
		if r.code != 1 || !strings.Contains(r.out, `"error_code":"cancelled"`) || strings.Contains(r.out, `"status":"saved"`) {
			t.Fatalf("cancel %d %s %s", r.code, r.out, r.stderr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled operation did not finish")
	}
	entries, err := os.ReadDir(tmp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("operation leaked spools %v %v", entries, err)
	}
	e.server.audit(t)
}

func TestAttachmentsRealIMAPReadOnlyAndStaleEpoch(t *testing.T) {
	e := newYahooEnv(t)
	e.server.mu.Lock()
	e.server.messages[0].source = "From: sender@example.org\r\nContent-Type: image/png\r\n\r\nexact-image-bytes"
	e.server.mu.Unlock()
	e.ok("auth", "login")
	e.ok("sync", "--folder", "inbox")
	code, out, stderr := e.run("attachments", "list", "1", "--json")
	if code != 0 {
		t.Fatalf("IMAP list %d %s %s", code, out, stderr)
	}
	row := attachmentRows(t, out)[0]
	root := attachmentPrivateDir(t)
	args := []string{"attachments", "extract", "1", "--part", row["part"].(string), "--out-dir", root, "--execute", "--json"}
	code, out, stderr = e.run(args...)
	if code != 0 {
		t.Fatalf("IMAP extract %d %s %s", code, out, stderr)
	}
	data, err := os.ReadFile(filepath.Join(root, row["output_name"].(string)))
	if err != nil || string(data) != "exact-image-bytes" {
		t.Fatalf("bytes %q %v", data, err)
	}
	e.server.audit(t)
	e.server.mu.Lock()
	if e.server.messages[0].flags != "" || e.server.sourceFetches < 2 {
		t.Error("read state changed or MIME not fetched")
	}
	e.server.validity++
	e.server.mu.Unlock()
	code, out, stderr = e.run("attachments", "list", "1", "--json")
	if code != 3 || !strings.Contains(out, `"error_code":"stale_identity"`) {
		t.Fatalf("stale IMAP %d %s %s", code, out, stderr)
	}
	if e.models != 0 {
		t.Fatal("content requested model")
	}
}
