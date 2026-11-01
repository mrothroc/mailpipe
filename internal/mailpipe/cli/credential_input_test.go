package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type failingCredentialReader struct{ sent bool }

func (r *failingCredentialReader) Read(b []byte) (int, error) {
	if !r.sent {
		r.sent = true
		b[0] = 'x'
		return 1, nil
	}
	return 0, errors.New("credential-input-canary")
}
func TestCredentialLineFailures(t *testing.T) {
	for _, in := range []string{"", "\n", "\r\n", "a\nb\n", "a\x00", "a\r", "ü", strings.Repeat("a", 257)} {
		if _, e := credentialLine(context.Background(), strings.NewReader(in), false); e == nil {
			t.Fatal("invalid input accepted")
		}
	}
	for _, in := range []string{"line", "line\n", "line\r\n"} {
		b, e := credentialLine(context.Background(), strings.NewReader(in), false)
		if e != nil || string(b) != "line" {
			t.Fatal("valid single line rejected")
		}
	}
	if _, e := credentialLine(context.Background(), &failingCredentialReader{}, false); e == nil || strings.Contains(e.Error(), "credential-input-canary") {
		t.Fatal("partial reader failure not redacted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := credentialLine(ctx, strings.NewReader("line"), false); !errors.Is(e, context.Canceled) {
		t.Fatal("cancellation lost")
	}
}
