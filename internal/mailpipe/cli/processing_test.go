package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
)

func TestAccountProcessingConsentMatrix(t *testing.T) {
	for _, provider := range []string{"graph", "yahoo"} {
		t.Run(provider, func(t *testing.T) {
			a := mailbox.Account{ID: "invented-account", ProviderID: mailbox.ProviderID(provider)}
			db, e := store.OpenAccount(context.Background(), filepath.Join(t.TempDir(), "mail.db"), a)
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			c := config.Config{Provider: provider, AccountID: string(a.ID)}
			s := &commandStore{bound: db, cfg: c}
			for _, kind := range []string{"jev", "template", "exec", "hook"} {
				e := authorizeProcessing(s, kind)
				if (e == nil) != (provider == "graph") {
					t.Fatal("legacy Graph/Yahoo nil consent distinction lost")
				}
			}
			for _, models := range []bool{false, true} {
				for _, external := range []bool{false, true} {
					s.cfg.Processing = &config.Processing{Version: 1, AccountID: string(a.ID), Models: models, ExternalStages: external}
					for _, kind := range []string{"jev", "template", "exec", "hook"} {
						want := models
						if kind == "exec" || kind == "hook" {
							want = external
						}
						if (authorizeProcessing(s, kind) == nil) != want {
							t.Fatal("consent scopes crossed")
						}
					}
					for _, kind := range []string{"allowlist", "honeypot", "sender_cache"} {
						if authorizeProcessing(s, kind) != nil {
							t.Fatal("local stage blocked")
						}
					}
				}
			}
			s.cfg.Processing = &config.Processing{Version: 1, AccountID: "another", Models: true, ExternalStages: true}
			for _, kind := range []string{"jev", "template", "exec", "hook"} {
				if authorizeProcessing(s, kind) == nil {
					t.Fatal("wrong-account consent accepted")
				}
			}
		})
	}
}
