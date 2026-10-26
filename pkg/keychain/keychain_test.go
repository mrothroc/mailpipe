package keychain_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/keychain"
)

type call struct {
	stdin string
	args  []string
}

type fakeRunner struct {
	calls []call
	out   []byte
	err   error
}

func (f *fakeRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	if name != "security" {
		panic("unexpected binary " + name)
	}
	f.calls = append(f.calls, call{string(stdin), args})
	return f.out, f.err
}

// exitError builds a real *exec.ExitError with the given status.
func exitError(t *testing.T, code int) error {
	t.Helper()
	return exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
}

func TestGetReadsThePasswordForServiceAndAccount(t *testing.T) {
	r := &fakeRunner{out: []byte("{\"cache\":1}\n")}
	k := keychain.New("mailpipe", r)
	got, err := k.Get(context.Background(), "token-cache")
	if err != nil || got != `{"cache":1}` {
		t.Fatalf("Get = %q, %v", got, err)
	}
	want := []string{"find-generic-password", "-s", "mailpipe", "-a", "token-cache", "-w"}
	if strings.Join(r.calls[0].args, " ") != strings.Join(want, " ") || r.calls[0].stdin != "" {
		t.Fatalf("args %v stdin %q", r.calls[0].args, r.calls[0].stdin)
	}
}

func TestGetMissingItemIsErrNotFound(t *testing.T) {
	k := keychain.New("mailpipe", &fakeRunner{err: exitError(t, 44)})
	if _, err := k.Get(context.Background(), "token-cache"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestSetNeverPutsTheSecretOnTheCommandLine: the value travels hex-encoded on
// security's stdin, so it cannot be read from the process list.
func TestSetNeverPutsTheSecretOnTheCommandLine(t *testing.T) {
	r := &fakeRunner{}
	k := keychain.New("mailpipe", r)
	secret := `{"refresh":"s3cr3t value"}`
	if err := k.Set(context.Background(), "token-cache", secret); err != nil {
		t.Fatal(err)
	}
	c := r.calls[len(r.calls)-1]
	if strings.Join(c.args, " ") != "-i" {
		t.Fatalf("args %v, want only -i", c.args)
	}
	for _, a := range c.args {
		if strings.Contains(a, "s3cr3t") || strings.Contains(a, hex.EncodeToString([]byte(secret))) {
			t.Fatal("secret on the command line")
		}
	}
	want := "add-generic-password -U -s mailpipe -a token-cache -X " + hex.EncodeToString([]byte(secret)) + "\n"
	if c.stdin != want {
		t.Fatalf("stdin %q, want %q", c.stdin, want)
	}
}

func TestSetFailureIsReportedWithoutTheSecret(t *testing.T) {
	k := keychain.New("mailpipe", &fakeRunner{err: exitError(t, 2), out: []byte("security: boom")})
	err := k.Set(context.Background(), "token-cache", "s3cr3t")
	if err == nil || strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), hex.EncodeToString([]byte("s3cr3t"))) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeleteIgnoresMissingItem(t *testing.T) {
	r := &fakeRunner{err: exitError(t, 44)}
	k := keychain.New("mailpipe", r)
	if err := k.Delete(context.Background(), "token-cache"); err != nil {
		t.Fatalf("Delete = %v", err)
	}
	if got := strings.Join(r.calls[len(r.calls)-1].args, " "); got != "delete-generic-password -s mailpipe -a token-cache" {
		t.Fatalf("args %v", got)
	}
}

func TestRejectsAccountsThatWouldBreakTheStdinCommand(t *testing.T) {
	k := keychain.New("mailpipe", &fakeRunner{})
	if err := k.Set(context.Background(), "bad account", "x"); err == nil {
		t.Fatal("account with a space accepted")
	}
}
