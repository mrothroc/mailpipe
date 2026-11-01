package imapauth_test

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/imapauth"
)

func TestPrincipalCanonicalization(t *testing.T) {
	for input, want := range map[string]string{"MAIL.Example.org": "mail.example.org:993", "mail.example.org:993": "mail.example.org:993", "127.0.0.1:1993": "127.0.0.1:1993"} {
		p, e := imapauth.NewPrincipal("yahoo", input, " Owner@Example.org ")
		if e != nil || p.Endpoint != want || p.Login != " Owner@Example.org " {
			t.Fatalf("canonical endpoint/login: %v", e)
		}
	}
	for _, v := range []string{"imaps://mail.example.org", "user@mail.example.org", "mail.example.org.", "mail.example.org:0993", "mail.example.org:0", "mail.example.org:65536", "mail.example.org/a", "mail.example.org?q", "mail.example.org#f", "mail%2eexample.org", "mail..example.org", "mail_example.org", " mail.example.org", "localhost", "127.000.0.1", "[::1]:993", "xn--mail.example.org", "máil.example.org", "K.example.org"} {
		if _, e := imapauth.NewPrincipal("yahoo", v, "owner@example.org"); e == nil {
			t.Errorf("accepted ambiguous endpoint %q", v)
		}
	}
	a, _ := imapauth.NewPrincipal("yahoo", "MAIL.Example.org", "Owner@example.org")
	b, _ := imapauth.NewPrincipal("yahoo", "mail.example.org:993", "Owner@example.org")
	if a.Account() != b.Account() || a.Item() != b.Item() {
		t.Fatal("equivalent endpoints differ")
	}
	for _, v := range [][3]string{{"yahoo", "mail.example.org", "owner@example.org"}, {"yahoo", "mail.example.org", " Owner@example.org"}, {"yahoo", "mail.example.org:1993", "Owner@example.org"}, {"other", "mail.example.org", "Owner@example.org"}} {
		p, e := imapauth.NewPrincipal(v[0], v[1], v[2])
		if e != nil || p.Account() == a.Account() || p.Item() == a.Item() {
			t.Fatal("principal namespaces collided")
		}
	}
	for _, v := range []string{"", "a\n", "ü", strings.Repeat("a", 255)} {
		if _, e := imapauth.NewPrincipal("yahoo", "mail.example.org", v); e == nil {
			t.Fatal("bad login accepted")
		}
	}
}

func TestPrincipalTupleWireContract(t *testing.T) {
	p, e := imapauth.NewPrincipal("yahoo", "mail.example.org", "A")
	if e != nil {
		t.Fatal(e)
	}
	raw, e := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(string(p.Account().ID), "imap-principal-v1:"))
	if e != nil {
		t.Fatal(e)
	}
	want := "6d61696c706970652d696d61702d7072696e636970616c00" + "00000001" + "00000005" + "7961686f6f" + "00000014" + "6d61696c2e6578616d706c652e6f72673a393933" + "00000001" + "41"
	if hex.EncodeToString(raw) != want {
		t.Fatal("versioned length-prefixed tuple changed")
	}
	if p.Item() != "principal-v1."+base64.RawURLEncoding.EncodeToString(raw) {
		t.Fatal("Keychain tuple differs from durable principal")
	}
}
