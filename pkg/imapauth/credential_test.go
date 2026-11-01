package imapauth_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/imapauth"
	"github.com/mrothroc/mailpipe/pkg/keychain"
)

func TestCredentialEnvelope(t *testing.T) {
	host := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	p, e := imapauth.NewPrincipal("yahoo", host+":65535", strings.Repeat("\"", 254))
	if e != nil {
		t.Fatal(e)
	}
	secret := strings.Repeat("\\", 256)
	gen := strings.Repeat("a", 32)
	value, e := p.Encode(gen, secret)
	if e != nil {
		t.Fatal(e)
	}
	line := "add-generic-password -U -s " + imapauth.Service + " -a " + p.Item() + " -X " + strings.Repeat("0", len(value)*2) + "\n"
	if len(value) > keychain.MaxUnchunkedBytes || len(line) >= 4000 {
		t.Fatal("single item stdin bound exceeded")
	}
	runner := &envelopeRunner{}
	if e = keychain.New(imapauth.Service, runner).Set(context.Background(), p.Item(), value); e != nil {
		t.Fatal("actual Keychain stdin construction failed")
	}
	if runner.writes != 1 || runner.line >= 4000 {
		t.Fatal("actual Keychain stdin exceeded single-item contract")
	}
	got, e := p.Decode(value, gen)
	if e != nil || got != secret {
		t.Fatal("roundtrip failed")
	}
	other, _ := imapauth.NewPrincipal("yahoo", "mail.example.org", "owner@example.org")
	if _, e = other.Decode(value, gen); e == nil {
		t.Fatal("wrong tuple accepted")
	}
	if _, e = p.Decode(value, strings.Repeat("b", 32)); e == nil {
		t.Fatal("wrong generation accepted")
	}
	if _, e = p.Decode(strings.Replace(value, "imap-credential-v1", "imap-credential-v2", 1), gen); e == nil {
		t.Fatal("unknown version accepted")
	}
	for _, broken := range []string{`["imap-credential-v2",`, `["imap-credential-v1","different-tuple",`} {
		if p.Replaceable(broken) {
			t.Fatal("recognizable foreign version/tuple was repairable")
		}
	}
	for _, v := range []string{"", "a\n", strings.Repeat("a", 257)} {
		if _, e = p.Encode(gen, v); e == nil {
			t.Fatal("invalid credential encoded")
		}
	}
}

type envelopeRunner struct{ writes, line int }

func (r *envelopeRunner) Run(_ context.Context, in []byte, _ string, args ...string) ([]byte, error) {
	if len(in) > 0 {
		r.writes++
		r.line = len(in)
		if len(args) != 1 || args[0] != "-i" {
			panic("credential argv")
		}
	}
	return nil, nil
}
