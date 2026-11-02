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

// Homebrew gives Go a package-manager GOPATH below its cache. Extracted
// modules contain read-only directories, unlike the empty lifecycle fixtures.
func TestOwnedCandidateReadonlyArtifacts(t *testing.T) {
	parent := temp(t)
	root := filepath.Join(parent, "mailpipe-candidate.fixture")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	identity, e := runTool(t, "owned-root", "--root", root)
	if e != nil {
		t.Fatalf("owner registration: %v %s", e, identity)
	}
	module := filepath.Join(root, "cache/go_mod_cache/pkg/mod/example.org/fixture")
	if e = os.MkdirAll(filepath.Join(module, "nested"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(module, "nested/example.txt"), []byte("invented example.org"), 0444); e != nil {
		t.Fatal(e)
	}
	outside := filepath.Join(parent, "outside")
	if e = os.WriteFile(outside, []byte("untouched example.net"), 0400); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(outside, filepath.Join(module, "outside-link")); e != nil {
		t.Fatal(e)
	}
	directories := []string{filepath.Join(module, "nested"), module, filepath.Dir(module)}
	t.Cleanup(func() {
		for i := len(directories) - 1; i >= 0; i-- {
			_ = os.Chmod(directories[i], 0700)
		}
	})
	for _, path := range directories {
		if e = os.Chmod(path, 0555); e != nil {
			t.Fatal(e)
		}
	}
	b, e := runTool(t, "remove-owned", "--root", root, "--identity", strings.TrimSpace(string(identity)))
	if e != nil {
		t.Fatalf("owned read-only build/cache artifacts not removed: %v %s", e, b)
	}
	if _, e = os.Lstat(root); !os.IsNotExist(e) {
		t.Fatal("owned root remains")
	}
	if b, e = os.ReadFile(outside); e != nil || string(b) != "untouched example.net" {
		t.Fatal("outside link target damaged")
	}
	if st, e := os.Stat(outside); e != nil || st.Mode().Perm() != 0400 {
		t.Fatal("outside link target mode changed")
	}
}

func TestOwnedCandidateCleanupBoundaries(t *testing.T) {
	for _, kind := range []string{"identity", "marker-hardlink", "marker-mode", "marker-replacement", "root-replacement", "descendant-hardlink", "special-file", "depth-limit"} {
		t.Run(kind, func(t *testing.T) {
			parent := temp(t)
			root := filepath.Join(parent, "mailpipe-candidate.fixture")
			if e := os.Mkdir(root, 0700); e != nil {
				t.Fatal(e)
			}
			b, e := runTool(t, "owned-root", "--root", root)
			if e != nil {
				t.Fatalf("register: %v %s", e, b)
			}
			identity := strings.TrimSpace(string(b))
			outside := filepath.Join(parent, "outside")
			if e = os.Mkdir(outside, 0700); e != nil {
				t.Fatal(e)
			}
			sentinel := filepath.Join(outside, "example.txt")
			if e = os.WriteFile(sentinel, []byte("untouched example.org"), 0400); e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "identity":
				identity += "invalid"
			case "marker-hardlink":
				e = os.Link(filepath.Join(root, ".owner"), filepath.Join(outside, "marker"))
			case "marker-mode":
				e = os.Chmod(filepath.Join(root, ".owner"), 0644)
			case "marker-replacement":
				privateTestHook = func() {
					privateTestHook = nil
					if e := os.Rename(filepath.Join(root, ".owner"), filepath.Join(root, ".previous-owner")); e != nil {
						t.Fatal(e)
					}
					if e := os.WriteFile(filepath.Join(root, ".owner"), []byte(identity), 0600); e != nil {
						t.Fatal(e)
					}
				}
			case "root-replacement":
				privateTestHook = func() {
					privateTestHook = nil
					if e := os.Rename(root, root+"-moved"); e != nil {
						t.Fatal(e)
					}
					if e := os.Symlink(outside, root); e != nil {
						t.Fatal(e)
					}
				}
			case "descendant-hardlink":
				e = os.Link(sentinel, filepath.Join(root, "linked"))
			case "special-file":
				e = syscall.Mkfifo(filepath.Join(root, "fifo"), 0600)
			case "depth-limit":
				path := root
				for i := 0; i < 130; i++ {
					path = filepath.Join(path, "d")
					if e = os.Mkdir(path, 0700); e != nil {
						t.Fatal(e)
					}
				}
			}
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { privateTestHook = nil })
			if e = ownedRoot("remove-owned", root, identity); e == nil {
				t.Fatal("uncertain artifact ownership accepted")
			}
			if b, e = os.ReadFile(sentinel); e != nil || string(b) != "untouched example.org" {
				t.Fatal("unrelated namespace damaged")
			}
			if st, e := os.Stat(sentinel); e != nil || st.Mode().Perm() != 0400 {
				t.Fatal("unrelated mode changed")
			}
			if kind == "root-replacement" {
				if st, e := os.Lstat(root); e != nil || st.Mode()&os.ModeSymlink == 0 {
					t.Fatal("replacement namespace removed")
				}
			}
		})
	}
}
