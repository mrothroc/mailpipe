// Command security checks public files for secrets and verifies Go release support.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/version"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type release struct {
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
}

var stableVersion = regexp.MustCompile(`^go[0-9]+\.[0-9]+\.[0-9]+$`)

func fetchReleases(ctx context.Context, client *http.Client, url string) ([]release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch Go release feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Go release feed returned HTTP %d", resp.StatusCode)
	}
	var releases []release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&releases); err != nil {
		return nil, fmt.Errorf("invalid Go release feed: %w", err)
	}
	if len(releases) == 0 {
		return nil, errors.New("empty Go release feed")
	}
	return releases, nil
}

// The two most recent stable release families receive Go security updates.
// Use the newest patch in either family, and keep go.mod's minimum language
// family supported too. The go directive is a minimum, not a compiler pin.
func checkVersions(compiler, module string, releases []release) error {
	latest := map[string]string{}
	for _, r := range releases {
		if !r.Stable {
			continue
		}
		if !stableVersion.MatchString(r.Version) || !version.IsValid(r.Version) {
			return fmt.Errorf("invalid stable release in Go feed: %q", r.Version)
		}
		family := version.Lang(r.Version)
		if previous := latest[family]; previous == "" || version.Compare(r.Version, previous) > 0 {
			latest[family] = r.Version
		}
	}
	families := make([]string, 0, len(latest))
	for family := range latest {
		families = append(families, family)
	}
	sort.Slice(families, func(i, j int) bool { return version.Compare(families[i], families[j]) > 0 })
	if len(families) < 2 {
		return errors.New("Go release feed does not contain two stable release families")
	}
	families = families[:2]
	supported := func(v string) bool { family := version.Lang(v); return family == families[0] || family == families[1] }
	if !stableVersion.MatchString(compiler) || !version.IsValid(compiler) || !supported(compiler) {
		return fmt.Errorf("compiler %q is not a supported stable Go release (supported: %s)", compiler, strings.Join(families, ", "))
	}
	if want := latest[version.Lang(compiler)]; compiler != want {
		return fmt.Errorf("compiler %s is not the current patch; install %s", compiler, want)
	}
	minimum := "go" + module
	if !version.IsValid(minimum) || !supported(minimum) {
		return fmt.Errorf("go.mod minimum %q is not in a supported Go family (supported: %s)", module, strings.Join(families, ", "))
	}
	if version.Compare(compiler, minimum) < 0 {
		return fmt.Errorf("compiler %s is older than go.mod minimum %s", compiler, minimum)
	}
	return nil
}

func output(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	b, err := cmd.Output()
	if err != nil {
		text := strings.TrimSpace(strings.ToValidUTF8(diagnostic.String(), "\uFFFD"))
		if text == "" {
			return nil, fmt.Errorf("%s failed: %w", args[0], err)
		}
		if len(text) > 4096 {
			end := 4096
			for !utf8.RuneStart(text[end]) {
				end--
			}
			text = text[:end] + " (truncated)"
		}
		return nil, fmt.Errorf("%s failed: %w: %s", args[0], err, text)
	}
	return b, nil
}

// publicSnapshot uses Git's file inventory, not a recursive directory scan:
// ignored runtime data is excluded, but already tracked files are always scanned.
// Symlink contents are represented by their link target, never dereferenced.
func publicSnapshot(ctx context.Context, repo string) (string, func(), error) {
	root, err := output(ctx, repo, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", nil, err
	}
	// Remove Git's line ending; spaces can be part of a directory name.
	repo = strings.TrimRight(string(root), "\r\n")
	raw, err := output(ctx, repo, "git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "mailpipe-public-secrets-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	fail := func(err error) (string, func(), error) { cleanup(); return "", nil, err }
	seen := map[string]bool{}
	for _, name := range strings.Split(string(raw), "\x00") {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		clean := filepath.Clean(filepath.FromSlash(name))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fail(errors.New("Git returned an unsafe file path"))
		}
		path := filepath.Join(repo, clean)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fail(err)
		}
		var data []byte
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return fail(err)
			}
			data = []byte(target)
		} else if info.Mode().IsRegular() {
			data, err = os.ReadFile(path)
			if err != nil {
				return fail(err)
			}
		} else {
			return fail(fmt.Errorf("unsupported public file type: %s", name))
		}
		dest := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return fail(err)
		}
		if err := os.WriteFile(dest, data, 0o600); err != nil {
			return fail(err)
		}
	}
	return dir, cleanup, nil
}

func run(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: go run ./tools/security <secrets|go-version>")
	}
	switch args[0] {
	case "secrets":
		if _, err := exec.LookPath("gitleaks"); err != nil {
			return errors.New("gitleaks is required; run make install-security-tools")
		}
		repo, err := os.Getwd()
		if err != nil {
			return err
		}
		snapshot, cleanup, err := publicSnapshot(ctx, repo)
		if err != nil {
			return err
		}
		defer cleanup()
		cmd := exec.CommandContext(ctx, "gitleaks", "dir", snapshot, "--redact", "--no-banner", "--ignore-gitleaks-allow")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("public-file secret scan failed: %w", err)
		}
		return nil
	case "go-version":
		b, err := output(ctx, "", "go", "env", "GOVERSION")
		if err != nil {
			return err
		}
		compiler := strings.TrimSpace(string(b))
		b, err = output(ctx, "", "go", "mod", "edit", "-json")
		if err != nil {
			return err
		}
		var module struct{ Go string }
		if err := json.Unmarshal(b, &module); err != nil {
			return err
		}
		releases, err := fetchReleases(ctx, &http.Client{Timeout: 15 * time.Second}, "https://go.dev/dl/?mode=json")
		if err != nil {
			return err
		}
		if err := checkVersions(compiler, module.Go, releases); err != nil {
			return err
		}
		fmt.Printf("Go support check passed: %s; module minimum %s\n", compiler, module.Go)
		return nil
	default:
		return errors.New("usage: go run ./tools/security <secrets|go-version>")
	}
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
