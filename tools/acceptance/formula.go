package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var errTrustInterface = errors.New("[candidate-formula] homebrew-trust-interface-unsupported: require brew trust --formula; candidate installation refused")
var errTrustScope = errors.New("[candidate-formula] homebrew-trust-scope-verification-failed: candidate installation refused")

func writeFormula(out, candidate, sha string, port int) error {
	if !validHex(candidate, 40) || !validHex(sha, 64) || port < 1 || port > 65535 {
		return errInvalid
	}
	text := fmt.Sprintf(`class Mailpipe < Formula
  desc "Read-only Yahoo and reviewed Graph mailbox processing"
  homepage "https://github.com/mrothroc/mailpipe"
  url "http://127.0.0.1:%d/mailpipe-%s.tar.gz"
  version "0.0.0-candidate.%s"
  sha256 "%s"
  license "MIT"
  depends_on "go" => :build
  depends_on :macos
  def install
    ENV["GOTOOLCHAIN"] = "local"
    system "go", "build", *std_go_args, "./cmd/mailpipe"
    pkgshare.install "examples"
  end
  test do
    # Homebrew supplies testpath; set every private path before the FIRST CLI call.
    ENV["HOME"] = (testpath/"home").to_s
    (testpath/"home").mkpath
    (testpath/"home").chmod 0700
    %%w[CONFIG DB OUT_DIR STAGES TEMPLATES].each do |key|
      ENV["MAILPIPE_#{key}"] = (testpath/"home"/key.downcase).to_s
    end
    ENV.delete "MAILPIPE_CLIENT_ID"
    ENV.delete "MAILPIPE_TENANT_ID"
    help = shell_output("#{bin}/mailpipe --help")
    assert_match "Graph", help
    assert_match "read-only Yahoo", help
    assert_path_exists pkgshare/"examples/stages.example.json"
    assert_path_exists pkgshare/"examples/templates.example.json"
    assert_predicate pkgshare/"examples/stages/newsletter.sh", :executable?
    system bin/"mailpipe", "config", "init", "--client-id", "11111111-1111-4111-8111-111111111111", "--tenant-id", "22222222-2222-4222-8222-222222222222"
    assert_equal 0600, File.stat(ENV.fetch("MAILPIPE_CONFIG")).mode & 0777
  end
end
`, port, candidate, candidate, sha)
	return exclusiveWrite(out, []byte(text))
}
func checkTrust(ctx context.Context, brew, name string) error {
	if !safeFormulaName(name) {
		return errTrustInterface
	}
	help, e := exec.CommandContext(ctx, brew, "help", "trust").Output()
	if e != nil || !strings.Contains(string(help), "--formula") {
		return errTrustInterface
	}
	if exec.CommandContext(ctx, brew, "trust", "--formula", name).Run() != nil {
		return errTrustInterface
	}
	raw, e := exec.CommandContext(ctx, brew, "trust", "--json=v1").Output()
	if e != nil || len(raw) > 65536 {
		return errTrustScope
	}
	return validateTrust(raw, name)
}
func validateTrust(raw []byte, name string) error {
	// Homebrew trust JSON is versioned; refuse unknown shapes instead of broadening trust.
	var r struct {
		Formulae []string `json:"formulae"`
		Taps     []string `json:"taps"`
		Casks    []string `json:"casks"`
		Commands []string `json:"commands"`
	}
	if strictJSON(raw, &r) != nil || len(r.Formulae) != 1 || r.Formulae[0] != name || len(r.Taps)+len(r.Casks)+len(r.Commands) != 0 {
		return errTrustScope
	}

	return nil
}

func brewVersion(ctx context.Context, brew string) error {
	b, e := exec.CommandContext(ctx, brew, "--version").Output()
	if e != nil || len(b) > 1024 {
		return errInvalid
	}
	v := strings.Split(string(b), "\n")[0]
	if !regexp.MustCompile(`^Homebrew [0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?$`).MatchString(v) {
		return errInvalid
	}
	fmt.Println(v)
	repository, e := exec.CommandContext(ctx, brew, "--repository").Output()
	if e != nil || len(repository) > 4096 {
		return errInvalid
	}
	revision, e := exec.CommandContext(ctx, "git", "-C", strings.TrimSpace(string(repository)), "rev-parse", "HEAD").Output()
	if e != nil || !validHex(strings.TrimSpace(string(revision)), 40) {
		return errInvalid
	}
	fmt.Printf("[candidate-formula] brew_revision=%s\n", strings.TrimSpace(string(revision)))
	return nil
}
