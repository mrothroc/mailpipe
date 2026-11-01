package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCandidateFormula(t *testing.T) {
	p := filepath.Join(temp(t), "mailpipe.rb")
	sha := strings.Repeat("b", 64)
	candidate := strings.Repeat("a", 40)
	b, e := runTool(t, "formula", "--candidate", candidate, "--sha256", sha, "--port", "12345", "--out", p)
	if e != nil {
		t.Fatalf("%v %s", e, b)
	}
	b, e = os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"127.0.0.1:12345/mailpipe-" + candidate + ".tar.gz", sha, "std_go_args", "pkgshare.install", `ENV["HOME"]`, "MAILPIPE_CONFIG", "Graph", "read-only Yahoo"} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("missing %s", want)
		}
	}
	for _, bad := range []string{"refs/tags", "livecheck", "NO_REQUIRE_TAP_TRUST", "Microsoft 365"} {
		if bytes.Contains(b, []byte(bad)) {
			t.Errorf("unsafe formula %s", bad)
		}
	}
	if _, e = runTool(t, "formula", "--candidate", "alex@example.com", "--sha256", sha, "--port", "12345", "--out", p+".bad"); e == nil {
		t.Fatal("candidate injection accepted")
	}
}
func TestCandidateTrustRefusal(t *testing.T) {
	for _, mode := range []string{"missing", "option", "nonzero", "broad", "canary"} {
		t.Run(mode, func(t *testing.T) {
			d := temp(t)
			p := filepath.Join(d, "brew-double")
			body := "#!/bin/sh\n"
			if mode == "missing" {
				body += "exit 1\n"
			} else {
				body += "if [ \"$1\" = help ]; then echo --formula; exit 0; fi\n"
				if mode == "option" || mode == "nonzero" {
					body += "exit 1\n"
				} else {
					body += "if [ \"$2\" = --json=v1 ]; then echo '{\"formulae\":[\"candidate/runfixture/mailpipe\"],\"taps\":[\"token@example.org\"],\"casks\":[],\"commands\":[]}'; fi\n"
				}
			}
			os.WriteFile(p, []byte(body), 0700)
			e := checkTrust(context.Background(), p, "candidate/runfixture/mailpipe")
			if e == nil {
				t.Fatal("trust mismatch accepted")
			}
			if strings.Contains(e.Error(), "token@example.org") {
				t.Fatal("trust input leaked")
			}
		})
	}
	good := []byte(`{"formulae":["candidate/runfixture/mailpipe"],"taps":[],"casks":[],"commands":[]}`)
	if validateTrust(good, "candidate/runfixture/mailpipe") != nil {
		t.Fatal("valid candidate-only trust rejected")
	}
}

func TestInstalledExecutableCannotEscapeKeg(t *testing.T) {
	d := temp(t)
	keg := filepath.Join(d, "mailpipe/0.0.0")
	os.MkdirAll(filepath.Join(keg, "bin"), 0700)
	outside := filepath.Join(d, "other")
	os.WriteFile(outside, []byte("synthetic"), 0700)
	os.Symlink(outside, filepath.Join(keg, "bin/mailpipe"))
	if installedLocation(keg, d) == nil {
		t.Fatal("escaped executable accepted")
	}
	os.Remove(filepath.Join(keg, "bin/mailpipe"))
	os.WriteFile(filepath.Join(keg, "bin/mailpipe"), []byte("synthetic"), 0700)
	if installedLocation(keg, d) != nil {
		t.Fatal("owned keg positive control rejected")
	}
}
