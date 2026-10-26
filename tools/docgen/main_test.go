package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPackagesTableListsEachPackagesSynopsisInPathOrder(t *testing.T) {
	root := t.TempDir()
	write(t, root, "pkg/store/store.go", "// Package store keeps mail. It is SQLite.\npackage store\n")
	write(t, root, "pkg/store/store_test.go", "package store\n")
	write(t, root, "cmd/tool/main.go", "// Command tool is the entry point.\npackage main\n")
	write(t, root, "internal/a|b/x.go", "// Package x has a | pipe.\npackage x\n")
	got, err := packagesTable(root, []string{"cmd", "internal", "pkg"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := "| Package | What it does |\n|---|---|\n" +
		"| `cmd/tool` | Command tool is the entry point. |\n" +
		"| `internal/a\\|b` | Package x has a \\| pipe. |\n" +
		"| `pkg/store` | Package store keeps mail. |\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestPackagesTableFailsLoudly(t *testing.T) {
	root := t.TempDir()
	write(t, root, "pkg/a/a.go", "package a\n")
	if _, err := packagesTable(root, []string{"pkg"}, 1); err == nil || !strings.Contains(err.Error(), "pkg/a") {
		t.Fatalf("undocumented package: %v", err)
	}
	write(t, root, "pkg/a/a.go", "// Package a does a.\npackage a\n")
	if _, err := packagesTable(root, []string{"pkg"}, 2); err == nil {
		t.Fatal("accepted fewer packages than the minimum")
	}
	if _, err := packagesTable(root, []string{"missing"}, 1); err == nil {
		t.Fatal("accepted a missing source directory")
	}
}

func TestGeneratedRegionCheckAndSync(t *testing.T) {
	root := t.TempDir()
	write(t, root, "docs/architecture.md", "Before\n<!-- docgen:packages -->\nstale\n<!-- /docgen:packages -->\nAfter\n")
	path := filepath.Join(root, "docs/architecture.md")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := updateRegion(path, "| Generated |\n", true); err == nil {
		t.Fatal("stale documentation passed")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("check changed the document")
	}
	if err := updateRegion(path, "| Generated |\n", false); err != nil {
		t.Fatal(err)
	}
	after, err = os.ReadFile(path)
	if err != nil || string(after) != "Before\n<!-- docgen:packages -->\n| Generated |\n<!-- /docgen:packages -->\nAfter\n" {
		t.Fatalf("sync: %q %v", after, err)
	}
	if err := updateRegion(path, "| Generated |\n", true); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedRegionRejectsMalformedMarkersWithoutWriting(t *testing.T) {
	for name, body := range map[string]string{
		"missing":       "no markers\n",
		"missing close": "<!-- docgen:packages -->\n",
		"reversed":      "<!-- /docgen:packages -->\n<!-- docgen:packages -->\n",
		"duplicate":     "<!-- docgen:packages -->\n<!-- docgen:packages -->\n<!-- /docgen:packages -->\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "doc.md")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := updateRegion(path, "new\n", false); err == nil {
				t.Fatal("malformed region accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != body {
				t.Fatal("malformed document changed")
			}
		})
	}
}
