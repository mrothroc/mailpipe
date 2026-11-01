package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestAcceptanceReceiptConfinement(t *testing.T) {
	c := strings.Repeat("a", 40)
	root, _ := initReceipt(t)
	s, e := os.Stat(filepath.Join(root, "receipt.json"))
	if e != nil || s.Mode().Perm() != 0600 {
		t.Fatal("receipt permissions")
	}
	if _, e = runTool(t, "receipt-init", "--private-root", root, "--candidate", c); e == nil {
		t.Fatal("replaced receipt")
	}
	for _, kind := range []string{"symlink", "mode", "git", "bare", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			parent := temp(t)
			target := filepath.Join(parent, "private")
			os.Mkdir(target, 0700)
			switch kind {
			case "symlink":
				link := filepath.Join(parent, "link")
				os.Symlink(target, link)
				target = link
			case "mode":
				os.Chmod(target, 0755)
			case "git":
				cmd := fixtureGit("init", "-q", parent)
				if b, e := cmd.CombinedOutput(); e != nil {
					t.Fatalf("%v %s", e, b)
				}
			case "bare":
				cmd := fixtureGit("init", "--bare", "-q", parent)
				if b, e := cmd.CombinedOutput(); e != nil {
					t.Fatalf("%v %s", e, b)
				}
			case "hardlink":
				b, e := runTool(t, "receipt-init", "--private-root", target, "--candidate", c)
				if e != nil {
					t.Fatalf("%v %s", e, b)
				}
				os.Link(filepath.Join(target, "receipt.json"), filepath.Join(parent, "linked"))
			}
			verb := "receipt-init"
			if kind == "hardlink" {
				verb = "receipt-validate"
			}
			if _, e := runTool(t, verb, "--private-root", target, "--candidate", c); e == nil {
				t.Fatal("unsafe private path admitted")
			}
		})
	}
	repo, e := os.Getwd()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = runTool(t, "receipt-init", "--private-root", repo, "--candidate", c); e == nil {
		t.Fatal("repository admitted")
	}
}

func TestPrivateRootRaceAndSpecialFiles(t *testing.T) {
	root := temp(t)
	p, e := openPrivate(root)
	if e != nil {
		t.Fatal(e)
	}
	defer p.close()
	outside := temp(t)
	moved := root + "-moved"
	privateTestHook = func() {
		privateTestHook = nil
		if e := os.Rename(root, moved); e != nil {
			t.Fatal(e)
		}
		if e := os.Symlink(outside, root); e != nil {
			t.Fatal(e)
		}
	}
	t.Cleanup(func() { privateTestHook = nil; os.Remove(root); os.Rename(moved, root) })
	if p.write("receipt.json", []byte(`{}`)) == nil {
		t.Fatal("renamed root accepted")
	}
	if _, e = os.Stat(filepath.Join(outside, "receipt.json")); !os.IsNotExist(e) {
		t.Fatal("race escaped pinned root")
	}
}
func TestReceiptUnknownNestedAndNullValues(t *testing.T) {
	root, c := initReceipt(t)
	path := filepath.Join(root, "receipt.json")
	original, _ := os.ReadFile(path)
	var r map[string]any
	json.Unmarshal(original, &r)
	for key, value := range r {
		_ = value
		bad := map[string]any{}
		json.Unmarshal(original, &bad)
		bad[key] = nil
		b, _ := json.Marshal(bad)
		os.WriteFile(path, b, 0600)
		if _, e := runTool(t, "receipt-validate", "--private-root", root, "--candidate", c); e == nil {
			t.Fatalf("null accepted at %s", key)
		}
	}
	json.Unmarshal(original, &r)
	technical := r["stage1_technical"].(map[string]any)
	for key := range technical {
		for _, v := range []any{nil, "token@example.org", 137, map[string]any{"body": "private@example.net"}} {
			var bad map[string]any
			json.Unmarshal(original, &bad)
			bad["stage1_technical"].(map[string]any)[key] = v
			b, _ := json.Marshal(bad)
			os.WriteFile(path, b, 0600)
			if _, e := runTool(t, "receipt-validate", "--private-root", root, "--candidate", c); e == nil {
				t.Fatalf("technical value accepted at %s", key)
			}
		}
	}
}

func TestPrivateReceiptIOFailures(t *testing.T) {
	for _, which := range []string{"sync", "close"} {
		t.Run(which, func(t *testing.T) {
			root := temp(t)
			p, e := openPrivate(root)
			if e != nil {
				t.Fatal(e)
			}
			defer p.close()
			originalSync, originalClose := privateSync, privateClose
			t.Cleanup(func() { privateSync, privateClose = originalSync, originalClose })
			if which == "sync" {
				privateSync = func(*os.File) error { return syscall.EIO }
			} else {
				privateClose = func(f *os.File) error { f.Close(); return syscall.EIO }
			}
			if p.write("receipt.json", []byte(`{}`)) == nil {
				t.Fatal("I/O failure acknowledged")
			}
			if _, e = os.Stat(filepath.Join(root, "receipt.json")); !os.IsNotExist(e) {
				t.Fatal("failed write published")
			}
		})
	}
	root := temp(t)
	p, e := openPrivate(root)
	if e != nil {
		t.Fatal(e)
	}
	defer p.close()
	fifo := filepath.Join(root, "receipt.json")
	if e = syscall.Mkfifo(fifo, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = p.read("receipt.json"); e == nil {
		t.Fatal("FIFO admitted")
	}
}
func TestOtherWorktreeAndFileSubstitution(t *testing.T) {
	parent := temp(t)
	repo := filepath.Join(parent, "repo")
	os.Mkdir(repo, 0700)
	git := func(args ...string) {
		t.Helper()
		c := fixtureGit(append([]string{"-C", repo}, args...)...)
		if b, e := c.CombinedOutput(); e != nil {
			t.Fatalf("fixture Git: %v %s", e, b)
		}
	}
	git("init", "-q")
	os.WriteFile(filepath.Join(repo, "example.txt"), []byte("invented example.org"), 0600)
	git("add", "example.txt")
	git("-c", "user.name=Example", "-c", "user.email=builder@example.org", "commit", "-qm", "fixture")
	other := filepath.Join(parent, "other")
	git("worktree", "add", "-q", "--detach", other)
	os.Chmod(other, 0700)
	if _, e := openPrivate(other); e == nil {
		t.Fatal("secondary worktree admitted")
	}
	root := temp(t)
	p, e := openPrivate(root)
	if e != nil {
		t.Fatal(e)
	}
	defer p.close()
	outside := temp(t)
	target := filepath.Join(outside, "canary")
	os.WriteFile(target, []byte("private@example.org"), 0600)
	os.Symlink(target, filepath.Join(root, "receipt.json"))
	if _, e = p.read("receipt.json"); e == nil {
		t.Fatal("target link read")
	}
	if b, _ := os.ReadFile(target); string(b) != "private@example.org" {
		t.Fatal("outside file damaged")
	}
}

func fixtureGit(args ...string) *exec.Cmd {
	c := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	c.Env = cleanEnv(map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null"})
	return c
}
