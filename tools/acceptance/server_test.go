package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func TestCandidateArchiveServer(t *testing.T) {
	d := temp(t)
	archive := filepath.Join(d, "mailpipe-"+strings.Repeat("a", 40)+".tar.gz")
	data := []byte("invented archive example.org\n")
	os.WriteFile(archive, data, 0600)
	ready := filepath.Join(d, "ready")
	c := exec.Command(tool(t), "serve", "--archive", archive, "--candidate", strings.Repeat("a", 40), "--sha256", digest(data), "--ready", ready)
	if e := c.Start(); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	t.Cleanup(func() {
		c.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			c.Process.Kill()
			t.Error("server did not stop")
		}
	})
	var r struct {
		Port      int    `json:"port"`
		PID       int    `json:"pid"`
		Candidate string `json:"candidate"`
		SHA       string `json:"sha256"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, e := os.ReadFile(ready)
		if e == nil && json.Unmarshal(b, &r) == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.Port == 0 || r.PID != c.Process.Pid || r.SHA != digest(data) {
		t.Fatal("invalid readiness")
	}
	base := "http://127.0.0.1:" + itoa(r.Port)
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, method := range []string{"GET", "HEAD"} {
		req, _ := http.NewRequest(method, base+"/"+filepath.Base(archive), nil)
		resp, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.ContentLength != int64(len(data)) || (method == "GET" && !bytes.Equal(b, data)) || (method == "HEAD" && len(b) != 0) {
			t.Fatal("inexact archive response")
		}
	}
	for _, path := range []string{"/", "/ready", "/../ready", "/%2e%2e/ready", "/" + filepath.Base(archive) + "?secret=example.org", "//" + filepath.Base(archive)} {
		resp, e := client.Get(base + path)
		if e != nil {
			t.Fatal(e)
		}
		resp.Body.Close()
		if resp.StatusCode == 200 || resp.StatusCode >= 300 && resp.StatusCode < 400 {
			t.Errorf("unsafe path accepted: %s", path)
		}
	}
	req, _ := http.NewRequest("POST", base+"/"+filepath.Base(archive), nil)
	resp, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatal("method accepted")
	}
	os.WriteFile(archive, []byte("changed"), 0600)
	resp, e = client.Get(base + "/" + filepath.Base(archive))
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("modified archive served")
	}
}
func TestArchiveServerRejectsWrongDigestAndLinks(t *testing.T) {
	d := temp(t)
	name := "mailpipe-" + strings.Repeat("a", 40) + ".tar.gz"
	a := filepath.Join(d, name)
	data := []byte("synthetic")
	os.WriteFile(a, data, 0600)
	if _, e := runTool(t, "serve", "--archive", a, "--candidate", strings.Repeat("a", 40), "--sha256", strings.Repeat("0", 64), "--ready", filepath.Join(d, "ready")); e == nil {
		t.Fatal("wrong digest accepted")
	}
	nested := filepath.Join(d, "nested")
	os.Mkdir(nested, 0700)
	link := filepath.Join(nested, name)
	os.Symlink(a, link)
	if _, e := runTool(t, "serve", "--archive", link, "--candidate", strings.Repeat("a", 40), "--sha256", digest(data), "--ready", filepath.Join(d, "ready")); e == nil {
		t.Fatal("symlink accepted")
	}
}

func TestAtomicExclusivePublication(t *testing.T) {
	d := temp(t)
	target := filepath.Join(d, "ready")
	paused := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	publicationHook = func() { close(paused); <-release }
	t.Cleanup(func() { publicationHook = nil })
	go func() { done <- exclusiveWrite(target, []byte(`{"complete":true}`)) }()
	<-paused
	if _, e := os.Stat(target); !os.IsNotExist(e) {
		t.Error("incomplete target visible before publication")
	}
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	publicationHook = nil
	before, _ := os.ReadFile(target)
	if e := exclusiveWrite(target, []byte("replacement")); e == nil {
		t.Fatal("existing readiness replaced")
	}
	after, _ := os.ReadFile(target)
	if !bytes.Equal(before, after) {
		t.Fatal("existing publication changed")
	}
	entries, _ := os.ReadDir(d)
	if len(entries) != 1 {
		t.Fatal("publication temporary remains")
	}
}
