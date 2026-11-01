package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

type readiness struct {
	Port      int    `json:"port"`
	PID       int    `json:"pid"`
	Candidate string `json:"candidate"`
	SHA       string `json:"sha256"`
}

func fileDigest(f *os.File) (string, error) {
	h := sha256.New()
	_, e := io.Copy(h, io.NewSectionReader(f, 0, 1<<30))
	return hex.EncodeToString(h.Sum(nil)), e
}
func serveArchive(ctx context.Context, path, candidate, sha, ready string) error {
	if !validHex(candidate, 40) || !validHex(sha, 64) || filepath.Base(path) != "mailpipe-"+candidate+".tar.gz" {
		return errInvalid
	}
	fd, e := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return errInvalid
	}
	f := os.NewFile(uintptr(fd), "archive")
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > 256<<20 || st.Sys().(*syscall.Stat_t).Nlink != 1 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return errInvalid
	}
	sum, e := fileDigest(f)
	if e != nil || sum != sha {
		return errInvalid
	}
	l, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return errors.New("acceptance: loopback bind failed")
	}
	defer l.Close()
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, WriteTimeout: 60 * time.Second, ErrorLog: discardHTTPLog()}
	exact := "/" + filepath.Base(path)
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != exact || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.RequestURI != exact {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		current, x := f.Stat()
		s, x2 := fileDigest(f)
		if x != nil || x2 != nil || current.Size() != st.Size() || s != sha {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
		w.WriteHeader(http.StatusOK)
		if r.Method == "GET" {
			_, _ = io.Copy(w, io.NewSectionReader(f, 0, st.Size()))
		}
	})
	r := readiness{l.Addr().(*net.TCPAddr).Port, os.Getpid(), candidate, sha}
	raw, _ := json.Marshal(r)
	if e = exclusiveWrite(ready, raw); e != nil {
		return errInvalid
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	select {
	case <-ctx.Done():
		stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		e = srv.Shutdown(stop)
		if e != nil {
			e = errors.Join(e, srv.Close())
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			return errors.New("acceptance: server cleanup failed")
		}
		if e != nil {
			return errors.New("acceptance: server cleanup failed")
		}
		return nil
	case e = <-done:
		if !errors.Is(e, http.ErrServerClosed) {
			return errors.New("acceptance: server stopped")
		}
		return nil
	}
}
func waitReady(ctx context.Context, path, candidate, sha string, pid int, out string, timeout time.Duration) (readiness, error) {
	if !validHex(candidate, 40) || !validHex(sha, 64) || pid <= 0 || !filepath.IsAbs(path) || !filepath.IsAbs(out) {
		return readiness{}, errInvalid
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return readiness{}, errInvalid
		}
		if syscall.Kill(pid, 0) != nil {
			return readiness{}, errors.New("acceptance: server exited before readiness")
		}
		var raw []byte
		fd, e := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if e == nil {
			f := os.NewFile(uintptr(fd), "readiness")
			st, x := f.Stat()
			if x != nil || !st.Mode().IsRegular() || st.Size() > 1024 || st.Mode().Perm() != 0600 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
				f.Close()
				return readiness{}, errInvalid
			}
			if st.Sys().(*syscall.Stat_t).Nlink != 1 {
				f.Close()
				time.Sleep(10 * time.Millisecond)
				continue
			}
			raw, e = io.ReadAll(io.LimitReader(f, 1025))
			e = errors.Join(e, f.Close())
		}
		var r readiness
		if e == nil && len(raw) <= 1024 && strictJSON(raw, &r) == nil && r.PID == pid && r.Candidate == candidate && r.SHA == sha && r.Port > 0 && r.Port <= 65535 {
			client := &http.Client{Timeout: time.Until(deadline), Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, e := client.Get(fmt.Sprintf("http://127.0.0.1:%d/mailpipe-%s.tar.gz", r.Port, candidate))
			if e != nil {
				return readiness{}, errors.New("acceptance: archive handshake failed")
			}
			b, e := io.ReadAll(io.LimitReader(resp.Body, (256<<20)+1))
			e = errors.Join(e, resp.Body.Close())
			h := sha256.Sum256(b)
			if e != nil || resp.StatusCode != 200 || len(b) > 256<<20 || hex.EncodeToString(h[:]) != sha {
				return readiness{}, errors.New("acceptance: archive hash verification failed")
			}
			if exclusiveWrite(out, b) != nil {
				return readiness{}, errInvalid
			}
			return r, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return readiness{}, errors.New("acceptance: readiness deadline")
}

func discardHTTPLog() *log.Logger { return log.New(io.Discard, "", 0) }
