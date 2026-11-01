package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

type privateRoot struct {
	fd   int
	path string
	dev  uint64
	ino  uint64
}

var privateTestHook func() // Test-only in-process race observer; no command/env switch exposes it.
func gitRoots() ([]string, error) {
	cmd := exec.Command("git", "worktree", "list", "--porcelain", "-z")
	cmd.Env = cleanEnv(map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null"})
	raw, e := cmd.Output()
	if e != nil {
		return nil, errInvalid
	}
	var roots []string
	for _, part := range bytes.Split(raw, []byte{0}) {
		if bytes.HasPrefix(part, []byte("worktree ")) {
			roots = append(roots, string(part[len("worktree "):]))
		}
	}
	cmd = exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.Env = cleanEnv(map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null"})
	b, e := cmd.Output()
	if e != nil || len(roots) == 0 {
		return nil, errInvalid
	}
	roots = append(roots, strings.TrimSpace(string(b)))
	for i, r := range roots {
		roots[i], e = filepath.EvalSymlinks(r)
		if e != nil {
			return nil, errInvalid
		}
	}
	return roots, nil
}
func openPrivate(path string) (*privateRoot, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, errInvalid
	}
	roots, e := gitRoots()
	if e != nil {
		return nil, errInvalid
	}
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, errInvalid
	}
	current := "/"
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, part := range parts {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, errInvalid
		}
		fd = next
		current = filepath.Join(current, part)
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil {
			unix.Close(fd)
			return nil, errInvalid
		}
		if st.Uid != uint32(os.Geteuid()) && st.Uid != 0 || st.Mode&0022 != 0 && st.Mode&unix.S_ISVTX == 0 {
			unix.Close(fd)
			return nil, errInvalid
		}
		for _, r := range roots {
			info, e := os.Stat(r)
			if e != nil {
				unix.Close(fd)
				return nil, errInvalid
			}
			s := info.Sys().(*syscall.Stat_t)
			if uint64(s.Dev) == uint64(st.Dev) && s.Ino == st.Ino {
				unix.Close(fd)
				return nil, errInvalid
			}
		}
		exists := func(n string) bool {
			var s unix.Stat_t
			e := unix.Fstatat(fd, n, &s, unix.AT_SYMLINK_NOFOLLOW)
			return e == nil || e != unix.ENOENT
		}
		if exists(".git") || exists("HEAD") && exists("objects") && exists("refs") {
			unix.Close(fd)
			return nil, errInvalid
		}
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&0777 != 0700 {
		unix.Close(fd)
		return nil, errInvalid
	}
	return &privateRoot{fd, path, uint64(st.Dev), st.Ino}, nil
}
func (p *privateRoot) close() { unix.Close(p.fd) }
func (p *privateRoot) stable() bool {
	// Rewalk the namespace without following aliases, then compare the pinned root.
	other, e := openPrivate(p.path)
	if e != nil {
		return false
	}
	defer other.close()
	return other.dev == p.dev && other.ino == p.ino
}
func safeLeaf(s string) bool {
	return s == "receipt.json" || s == "consent.json" || s == "summary-proposal.json"
}
func (p *privateRoot) write(name string, data []byte) error {
	if !safeLeaf(name) || len(data) > 65536 {
		return errInvalid
	}
	if privateTestHook != nil {
		privateTestHook()
	}
	if !p.stable() {
		return errInvalid
	}
	return atomicWrite(p.fd, name, data, p.stable)
}

func (p *privateRoot) read(name string) ([]byte, error) {
	if !safeLeaf(name) || !p.stable() {
		return nil, errInvalid
	}
	fd, e := unix.Openat(p.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, errInvalid
	}
	f := os.NewFile(uintptr(fd), "private evidence")
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) || st.Mode&0777 != 0600 || st.Nlink != 1 || st.Size > 65536 {
		return nil, errInvalid
	}
	b, e := io.ReadAll(io.LimitReader(f, 65537))
	if e != nil || len(b) > 65536 || !p.stable() || !p.matches(name, &st) {
		return nil, errInvalid
	}
	if privateClose(f) != nil {
		return nil, errInvalid
	}
	return b, nil
}

func exclusiveWrite(path string, data []byte) error {
	fd, e := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return errInvalid
	}
	defer unix.Close(fd)
	return atomicWrite(fd, filepath.Base(path), data, func() bool { return true })
}

var publicationHook func() // In-process test hook, never exposed by arguments or environment.
func atomicWrite(dirfd int, name string, data []byte, stable func() bool) error {
	random := make([]byte, 16)
	if _, e := rand.Read(random); e != nil {
		return errInvalid
	}
	temporary := ".acceptance-" + hex.EncodeToString(random)
	fd, e := unix.Openat(dirfd, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return errInvalid
	}
	f := os.NewFile(uintptr(fd), "owned publication")
	var created unix.Stat_t
	defer func() {
		f.Close()
		_ = unix.Unlinkat(dirfd, temporary, 0)

	}()
	if unix.Fstat(fd, &created) != nil || created.Nlink != 1 || created.Mode&0777 != 0600 || created.Uid != uint32(os.Geteuid()) {
		return errInvalid
	}
	if n, e := f.Write(data); e != nil || n != len(data) {
		return errInvalid
	}
	if privateSync(f) != nil || privateClose(f) != nil {
		return errInvalid
	}
	if publicationHook != nil {
		publicationHook()
	}
	if !stable() {
		return errInvalid
	}
	// Link publishes complete fsynced bytes without replacing any existing name.
	if unix.Linkat(dirfd, temporary, dirfd, name, 0) != nil {
		return errInvalid
	}
	if unix.Unlinkat(dirfd, temporary, 0) != nil || unix.Fsync(dirfd) != nil || !stable() {
		return errInvalid
	}
	var current unix.Stat_t
	if unix.Fstatat(dirfd, name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil || current.Dev != created.Dev || current.Ino != created.Ino || current.Nlink != 1 || current.Mode&unix.S_IFMT != unix.S_IFREG || current.Mode&0777 != 0600 || current.Uid != uint32(os.Geteuid()) {
		return errInvalid
	}
	return nil
}

func ownedRoot(verb, path, identity string) error {
	if !strings.HasPrefix(filepath.Base(path), "mailpipe-candidate.") {
		return errInvalid
	}
	p, e := openPrivate(path)
	if e != nil {
		return errInvalid
	}
	defer p.close()
	if verb == "owned-root" {
		b := make([]byte, 16)
		if _, e = rand.Read(b); e != nil {
			return errInvalid
		}
		identity = fmt.Sprintf("%d:%d:%s", p.dev, p.ino, hex.EncodeToString(b))
		fd, e := unix.Openat(p.fd, ".owner", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0600)
		if e != nil {
			return errInvalid
		}
		f := os.NewFile(uintptr(fd), "owner")
		_, e = f.WriteString(identity)
		e = errors.Join(e, f.Sync(), f.Close())
		if e != nil {
			return errInvalid
		}
		fmt.Println(identity)
		return nil
	}
	fd, e := unix.Openat(p.fd, ".owner", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if e != nil {
		return errInvalid
	}
	f := os.NewFile(uintptr(fd), "owner")
	var marker unix.Stat_t
	if unix.Fstat(fd, &marker) != nil || marker.Mode&unix.S_IFMT != unix.S_IFREG || marker.Mode&0777 != 0600 || marker.Nlink != 1 || marker.Uid != uint32(os.Geteuid()) || marker.Size > 256 {
		f.Close()
		return errInvalid
	}
	b, e := io.ReadAll(io.LimitReader(f, 256))
	f.Close()
	if e != nil || string(b) != identity || !strings.HasPrefix(identity, fmt.Sprintf("%d:%d:", p.dev, p.ino)) || !p.stable() {
		return errInvalid
	}
	parent, e := os.OpenRoot(filepath.Dir(path))
	if e != nil {
		return errInvalid
	}
	defer parent.Close()
	if parent.RemoveAll(filepath.Base(path)) != nil {
		return errInvalid
	}
	return nil
}
func brewLayout(prefix, cellar, repository string) error {
	for _, p := range []string{prefix, cellar, repository} {
		r, e := filepath.EvalSymlinks(p)
		if e != nil || r != p {
			return errInvalid
		}
		st, e := os.Stat(p)
		if e != nil || !st.IsDir() || (st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) && !(p == prefix && st.Sys().(*syscall.Stat_t).Uid == 0)) {
			return errInvalid
		}
	}
	if cellar != filepath.Join(prefix, "Cellar") || !(repository == prefix || strings.HasPrefix(repository, prefix+"/")) {
		return errInvalid
	}
	fmt.Printf("[candidate-formula] prefix=%s cellar=%s repository=%s\n", prefix, cellar, repository)
	return nil
}
func installedLocation(prefix, cellar string) error {
	r, e := filepath.EvalSymlinks(prefix)
	if e != nil || !strings.HasPrefix(r, filepath.Join(cellar, "mailpipe")+"/") {
		return errInvalid
	}
	binary, e := filepath.EvalSymlinks(filepath.Join(r, "bin/mailpipe"))
	if e != nil || !strings.HasPrefix(binary, r+string(filepath.Separator)) {
		return errInvalid
	}
	st, e := os.Stat(binary)
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return errInvalid
	}
	return nil
}

func tapLocation(path, repo string) error {
	canonical, e := filepath.EvalSymlinks(path)
	if e != nil || canonical != path || filepath.Dir(path) != filepath.Join(repo, "Library/Taps/candidate") {
		return errInvalid
	}
	st, e := os.Stat(path)
	if e != nil || !st.IsDir() || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) || st.Mode().Perm()&0022 != 0 {
		return errInvalid
	}
	return nil
}

var privateSync = func(f *os.File) error { return f.Sync() }
var privateClose = func(f *os.File) error { return f.Close() }

func (p *privateRoot) matches(name string, expected *unix.Stat_t) bool {
	var current unix.Stat_t
	return unix.Fstatat(p.fd, name, &current, unix.AT_SYMLINK_NOFOLLOW) == nil && current.Dev == expected.Dev && current.Ino == expected.Ino && current.Nlink == 1 && current.Mode&unix.S_IFMT == unix.S_IFREG && current.Mode&0777 == 0600 && current.Uid == uint32(os.Geteuid())
}
