package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestGoSupportAndPatchChecks(t *testing.T) {
	releases := []release{{"go1.29.2", true}, {"go1.28.7", true}, {"go1.30rc1", false}, {"go1.27.9", true}}
	for name, tc := range map[string]struct {
		compiler, module string
		wantError        bool
	}{
		"current":            {"go1.29.2", "1.29.0", false},
		"previous supported": {"go1.28.7", "1.28.0", false},
		"retired compiler":   {"go1.27.9", "1.28.0", true},
		"old patch":          {"go1.29.1", "1.29.0", true},
		"retired minimum":    {"go1.29.2", "1.27.0", true},
		"development":        {"devel go1.30", "1.29.0", true},
		"prerelease":         {"go1.30rc1", "1.29.0", true},
		"unpublished":        {"go1.29.3", "1.29.0", true},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkVersions(tc.compiler, tc.module, releases)
			if (err != nil) != tc.wantError {
				t.Fatalf("err %v", err)
			}
		})
	}
	if err := checkVersions("go1.29.2", "1.29.0", releases[:1]); err == nil {
		t.Fatal("incomplete support feed accepted")
	}
}

func TestReleaseFeedFailuresDoNotPass(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"unavailable": {503, "unavailable"},
		"malformed":   {200, "not json"},
		"truncated":   {200, `[{"version":"go1.29.2"`},
		"empty":       {200, `[]`},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			if _, err := fetchReleases(context.Background(), srv.Client(), srv.URL); err == nil {
				t.Fatal("bad feed accepted")
			}
		})
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, b)
	}
}

func TestSecretSnapshotExcludesIgnoredFilesAndNeverFollowsLinks(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	for name, text := range map[string]string{".gitignore": "private/\n", "notes with spaces.txt": "public data", "new.txt": "new data"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "private", "settings.json"), []byte("not public"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("must never be copied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".gitignore", "notes with spaces.txt", "link.txt")
	snapshot, cleanup, err := publicSnapshot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, name := range []string{"notes with spaces.txt", "new.txt"} {
		if _, err := os.Stat(filepath.Join(snapshot, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(snapshot, "private")); !os.IsNotExist(err) {
		t.Fatalf("ignored data copied: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(snapshot, "link.txt"))
	if err != nil || string(b) != outside {
		t.Fatalf("symlink followed: %q %v", b, err)
	}
	if info, err := os.Stat(filepath.Join(snapshot, "notes with spaces.txt")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot permissions: %v %v", info, err)
	}
}

func TestPreCommitScansTheIndexAndRedactsFindings(t *testing.T) {
	if _, err := exec.LookPath("gitleaks"); err != nil {
		t.Skip("gitleaks not installed; make security requires it")
	}
	hook, err := filepath.Abs("../../.githooks/pre-commit")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	// Invented token, assembled at runtime so this fixture is not itself a leak.
	token := "gh" + "p_" + strings.Join([]string{"n8Q4", "u2V6", "x9Z1", "a7B3", "c5D0", "e8F4", "g2H6", "j9K1", "m7P3"}, "")
	path := filepath.Join(dir, "example.txt")
	if err := os.WriteFile(path, []byte("token="+token), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "example.txt")
	// Hiding it in the working copy must not hide the staged secret.
	if err := os.WriteFile(path, []byte("clean"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", hook)
	cmd.Dir = dir
	b, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("staged secret accepted: %s", b)
	}
	if strings.Contains(string(b), token) {
		t.Fatal("scanner printed the token")
	}
	git(t, dir, "add", "example.txt")
	cmd = exec.Command("sh", hook)
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clean staged file rejected: %v %s", err, b)
	}
}

func TestSecretSnapshotResolvesRepositoryRoot(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "root.txt"), []byte("public"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "root.txt")
	subdir := filepath.Join(root, "nested")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshot, cleanup, err := publicSnapshot(context.Background(), subdir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(snapshot, "root.txt")); err != nil {
		t.Fatalf("root file omitted: %v", err)
	}
}

func TestCommandFailureIncludesDiagnostic(t *testing.T) {
	_, err := output(context.Background(), t.TempDir(), "sh", "-c", "printf 'example failure' >&2; exit 2")
	if err == nil || !strings.Contains(err.Error(), "example failure") {
		t.Fatalf("diagnostic omitted: %v", err)
	}
}

func TestCommandDiagnosticRemainsValidUTF8AndHandlesEmptyStderr(t *testing.T) {
	_, err := output(context.Background(), t.TempDir(), "sh", "-c", "printf '%s' \"$1\" >&2; exit 2", "sh", strings.Repeat("€", 2000))
	if err == nil || !utf8.ValidString(err.Error()) {
		t.Fatal("diagnostic must be present and valid UTF-8")
	}
	_, err = output(context.Background(), t.TempDir(), "sh", "-c", "exit 2")
	if err == nil || strings.HasSuffix(err.Error(), ": ") {
		t.Fatalf("empty diagnostic: %v", err)
	}
}
