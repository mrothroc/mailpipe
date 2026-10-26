// Command docgen generates and checks source-derived package documentation.
//
//	docgen packages   print the package table
//	docgen sync       update the package table in docs/architecture.md
//	docgen check      fail if the package table is stale, without writing
package main

import (
	"errors"
	"fmt"
	"go/doc"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// sourceDirs are the trees whose packages are listed.
var sourceDirs = []string{"cmd", "internal", "pkg", "tools"}

// minPackages guards against a degenerate table: the repository has far more.
const minPackages = 10

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "packages" && os.Args[1] != "sync" && os.Args[1] != "check") {
		fmt.Fprintln(os.Stderr, "usage: docgen <packages|sync|check>")
		os.Exit(2)
	}
	out, err := packagesTable(".", sourceDirs, minPackages)
	if err != nil {
		fmt.Fprintln(os.Stderr, "docgen:", err)
		os.Exit(1)
	}
	if os.Args[1] == "packages" {
		fmt.Print(out)
		return
	}
	if err := updateRegion("docs/architecture.md", out, os.Args[1] == "check"); err != nil {
		fmt.Fprintln(os.Stderr, "docgen:", err)
		os.Exit(1)
	}
}

// updateRegion replaces exactly one well-formed, explicitly marked table.
// Validation finishes before any write; check mode never changes the file.
func updateRegion(path, table string, check bool) error {
	const start = "<!-- docgen:packages -->"
	const end = "<!-- /docgen:packages -->"
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(raw)
	if strings.Count(text, start) != 1 || strings.Count(text, end) != 1 {
		return fmt.Errorf("%s must contain exactly one package region", path)
	}
	a, b := strings.Index(text, start)+len(start), strings.Index(text, end)
	if a > b {
		return fmt.Errorf("%s has reversed package region markers", path)
	}
	want := text[:a] + "\n" + strings.TrimRight(table, "\n") + "\n" + text[b:]
	if want == text {
		return nil
	}
	if check {
		return fmt.Errorf("%s package table is stale; run make docs", path)
	}
	return os.WriteFile(path, []byte(want), 0o644)
}

// packagesTable renders a Markdown table of the Go packages under dirs (relative
// to root), each with its doc comment's first sentence. Every package must have
// a doc comment, and there must be at least min packages.
func packagesTable(root string, dirs []string, min int) (string, error) {
	synopses := map[string]string{}
	for _, dir := range dirs {
		top := filepath.Join(root, dir)
		if _, err := os.Stat(top); err != nil {
			return "", err
		}
		err := filepath.WalkDir(top, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return err
			}
			text, found, err := packageDoc(path)
			if err != nil || !found {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if strings.TrimSpace(text) == "" {
				return fmt.Errorf("package %s has no doc comment", rel)
			}
			synopses[rel] = new(doc.Package).Synopsis(text)
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	if len(synopses) < min {
		return "", fmt.Errorf("found %d packages, want at least %d", len(synopses), min)
	}
	paths := make([]string, 0, len(synopses))
	for p := range synopses {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	b.WriteString("| Package | What it does |\n|---|---|\n")
	esc := strings.NewReplacer("|", `\|`)
	for _, p := range paths {
		fmt.Fprintf(&b, "| `%s` | %s |\n", esc.Replace(p), esc.Replace(synopses[p]))
	}
	return b.String(), nil
}

// packageDoc returns the doc comment of the non-test Go package in dir, and
// whether dir holds one.
func packageDoc(dir string) (string, bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false, err
	}
	fset := token.NewFileSet()
	name, text := "", ""
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.PackageClauseOnly|parser.ParseComments)
		if err != nil {
			return "", false, err
		}
		if name != "" && f.Name.Name != name {
			return "", false, errors.New(dir + " holds more than one package")
		}
		name = f.Name.Name
		if f.Doc != nil {
			text += f.Doc.Text()
		}
	}
	return text, name != "", nil
}
