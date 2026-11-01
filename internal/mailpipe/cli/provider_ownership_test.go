package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/imapauth"
	"github.com/mrothroc/mailpipe/pkg/imapmail"
)

func TestLazyIMAPClientOwnership(t *testing.T) {
	p, e := imapauth.NewPrincipal("yahoo", "127.0.0.1:1993", "owner@example.org")
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("MAILPIPE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	if e = config.Save(config.DefaultPath(), config.Config{Provider: "yahoo", AccountID: string(p.Account().ID), IMAP: &config.IMAPConfig{IdentityVersion: 1, Endpoint: p.Endpoint, Login: p.Login}}); e != nil {
		t.Fatal(e)
	}
	builds := 0
	var client *imapmail.Client
	deps := Deps{NewIMAP: func(c imapmail.Config, f imapmail.CredentialFunc) (*imapmail.Client, error) {
		builds++
		var e error
		client, e = imapmail.New(c, f)
		return client, e
	}}
	get, close := lazyProvider(context.Background(), deps)
	first, e := get()
	if e != nil {
		t.Fatal(e)
	}
	second, e := get()
	if e != nil || first != second || builds != 1 {
		t.Fatal("lazy client constructed more than once")
	}
	if close() != nil || close() != nil {
		t.Fatal("idempotent close failed")
	}
	if _, e = client.Discover(context.Background()); e == nil || !strings.Contains(e.Error(), "client closed") {
		t.Fatal("owned actual client was not closed")
	}
	if _, e = get(); e == nil {
		t.Fatal("closed owner admitted another getter")
	}
	get, close = lazyProvider(context.Background(), deps)
	if close() != nil {
		t.Fatal("empty owner close failed")
	}
	if _, e = get(); e == nil || builds != 1 {
		t.Fatal("close created or later admitted an unused client")
	}
}
