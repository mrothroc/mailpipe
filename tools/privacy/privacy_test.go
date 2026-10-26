package privacy_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// Keep the guard generic: listing real private identifiers here would itself
// disclose them. Example domains are reserved fixture data, not live senders.
func TestPublicTextUsesExampleEmailAddresses(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	email := regexp.MustCompile("[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9-]+(?:\\.[A-Za-z0-9-]+)+")
	for _, name := range []string{".github", "cmd", "internal", "pkg", "tools", "docs", "examples", "AGENTS.md", "PLAN.md", "README.md"} {
		err := filepath.WalkDir(filepath.Join(root, name), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			switch filepath.Ext(path) {
			case ".go", ".md", ".json", ".sh", ".yml", ".yaml":
			default:
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(raw), "\n") {
				for _, address := range email.FindAllString(line, -1) {
					_, domain, _ := strings.Cut(address, "@")
					domain = strings.ToLower(domain)
					allowed := domain == "example.com" || domain == "example.org" || domain == "example.net" || strings.HasSuffix(domain, ".example.com") || strings.HasSuffix(domain, ".example.org") || strings.HasSuffix(domain, ".example.net") || strings.HasSuffix(domain, ".example") || strings.HasSuffix(domain, ".invalid")
					if !allowed {
						relative, _ := filepath.Rel(root, path)
						t.Errorf("%s:%d: email fixture must use a reserved example domain", relative, i+1)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
