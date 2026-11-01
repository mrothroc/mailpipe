package config_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/imapauth"
	"github.com/mrothroc/mailpipe/pkg/store"
)

func TestYahooConfigurationIdentity(t *testing.T) {
	p, _ := imapauth.NewPrincipal("yahoo", "MAIL.Example.org", " Owner@Example.org ")
	raw := `{"provider":"yahoo","account_id":"` + string(p.Account().ID) + `","imap":{"identity_version":1,"endpoint":"MAIL.Example.org","login":" Owner@Example.org ","private":{"x":1}},"processing":{"version":1,"account_id":"` + string(p.Account().ID) + `","models":false,"external_stages":false,"private":7},"private_top":[1,2]}`
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(raw), 0600)
	c, e := config.Load(path)
	if e != nil {
		t.Fatal(e)
	}
	if c.IMAP.Endpoint != "mail.example.org:993" || c.IMAP.Login != p.Login {
		t.Fatal("principal changed")
	}
	if e = config.Save(path, c); e != nil {
		t.Fatal(e)
	}
	var out map[string]json.RawMessage
	b, _ := os.ReadFile(path)
	json.Unmarshal(b, &out)
	if out["private_top"] == nil {
		t.Fatal("private field lost")
	}
	for _, key := range []string{"imap", "processing"} {
		var nested map[string]json.RawMessage
		json.Unmarshal(out[key], &nested)
		if nested["private"] == nil {
			t.Fatal("nested private field lost")
		}
	}
	c.AccountID = "arbitrary"
	if config.Save(path, c) == nil {
		t.Fatal("arbitrary identity accepted")
	}
	for _, v := range []string{`{"provider":"yahoo","provider":"graph"}`, `{"provider":"graph","Provider":"yahoo"}`, `{"provider":"yahoo","imap":{"login":"a","login":"b"}}`, `{"processing":{"models":false,"Models":true}}`} {
		os.WriteFile(path, []byte(v), 0600)
		if _, e = config.Load(path); e == nil {
			t.Fatal("duplicate security key accepted")
		}
	}
}

func TestCanonicalPrincipalReopensPermanentDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mail.db")
	a, _ := imapauth.NewPrincipal("yahoo", "MAIL.Example.org", "Owner@example.org")
	db, e := store.OpenAccount(ctx, path, a.Account())
	if e != nil {
		t.Fatal(e)
	}
	if e = db.Close(); e != nil {
		t.Fatal(e)
	}
	b, _ := imapauth.NewPrincipal("yahoo", "mail.example.org:993", "Owner@example.org")
	db, e = store.OpenAccount(ctx, path, b.Account())
	if e != nil {
		t.Fatal("equivalent endpoint failed across reopen")
	}
	db.Close()
	for _, values := range [][2]string{{"mail.example.org:1993", "Owner@example.org"}, {"other.example.org", "Owner@example.org"}, {"mail.example.org", "owner@example.org"}} {
		p, e := imapauth.NewPrincipal("yahoo", values[0], values[1])
		if e != nil {
			t.Fatal(e)
		}
		if d, e := store.OpenAccount(ctx, path, p.Account()); e == nil {
			d.Close()
			t.Fatal("changed principal reused database")
		}
	}
}

func TestYahooVersionsAndGraphEnvironmentIsolation(t *testing.T) {
	t.Setenv("MAILPIPE_CLIENT_ID", "not-a-uuid")
	t.Setenv("MAILPIPE_TENANT_ID", "not-a-uuid")
	p, _ := imapauth.NewPrincipal("yahoo", "mail.example.org", "Owner@example.org")
	c := config.Config{Provider: "yahoo", AccountID: string(p.Account().ID), IMAP: &config.IMAPConfig{IdentityVersion: 1, Endpoint: p.Endpoint, Login: p.Login}}
	path := filepath.Join(t.TempDir(), "config.json")
	if e := config.Save(path, c); e != nil {
		t.Fatal(e)
	}
	if _, e := config.Load(path); e != nil {
		t.Fatal("Graph environment affected Yahoo")
	}
	c.IMAP.IdentityVersion = 2
	if config.Save(path, c) == nil {
		t.Fatal("unknown identity version accepted")
	}
	c.IMAP.IdentityVersion = 1
	c.Processing = &config.Processing{Version: 2}
	if config.Save(path, c) == nil {
		t.Fatal("unknown processing version accepted")
	}
	c.Processing = nil
	c.Credential = &config.CredentialState{Version: 2}
	if config.Save(path, c) == nil {
		t.Fatal("unknown credential state accepted")
	}
}
