package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"maps"
	"path/filepath"
	"time"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/imapauth"
	"github.com/mrothroc/mailpipe/pkg/imapmail"
	"github.com/mrothroc/mailpipe/pkg/keychain"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/spf13/cobra"
)

func saveConfig(deps Deps, c config.Config) error {
	if deps.SaveConfig != nil {
		return deps.SaveConfig(config.DefaultPath(), c)
	}
	return config.Save(config.DefaultPath(), c)
}

// configSession shares the existing current-user credential/config session in
// production. Legacy Graph dependency fixtures need only its local flock, not a
// credential backend; Exclusive never invokes the security runner here.
func configSession(ctx context.Context, deps Deps, fn func() error) error {
	if deps.Credentials != nil {
		return setupSession(ctx, deps, func(imapauth.Items) error { return fn() })
	}
	lock := filepath.Join(filepath.Dir(config.DefaultPath()), "imap-keychain-v1.lock")
	return keychain.New(imapauth.Service, nil).WithLock(lock).Exclusive(ctx, func(*keychain.Keychain) error { return fn() })
}

// saveRoutingConfig owns only account/collection routing. Remote discovery and
// binding finish before entering the session; authorization and private state
// are always taken from the validated current configuration, never a snapshot.
func saveRoutingConfig(ctx context.Context, deps Deps, selected config.Config, account mailbox.Account, collections map[string]string) (out config.Config, err error) {
	err = configSession(ctx, deps, func() error {
		fresh, e := routing()
		if e != nil {
			return e
		}
		expected := selected
		if expected.AccountID == "" && fresh.AccountID == string(account.ID) {
			expected.AccountID = fresh.AccountID
		}
		if e = samePrincipal(fresh, expected); e != nil {
			return e
		}
		if account.ProviderID != mailbox.ProviderID(fresh.Mode()) || (fresh.AccountID != "" && fresh.AccountID != string(account.ID)) {
			return &mailbox.AccountMismatchError{Operation: "save routing", Expected: account.ID, Actual: fresh.Account().ID}
		}
		if fresh.Mode() == "yahoo" && !maps.Equal(fresh.IMAP.RoleMappings, selected.IMAP.RoleMappings) {
			return errors.New("IMAP role mappings changed; rerun discovery")
		}
		fresh.AccountID = string(account.ID)
		if fresh.Collections == nil {
			fresh.Collections = map[string]string{}
		}
		for name, id := range collections {
			fresh.Collections[name] = id
		}
		if e = saveConfig(deps, fresh); e != nil {
			return e
		}
		out = fresh
		return nil
	})
	return
}
func setupSession(ctx context.Context, deps Deps, fn func(imapauth.Items) error) error {
	if deps.Credentials == nil {
		return credentialError(ctx, "credential store required")
	}
	s, e := deps.Credentials()
	if e != nil || s == nil {
		return credentialError(ctx, "credential store unavailable")
	}
	var inner error
	e = s.Session(ctx, func(k imapauth.Items) error { inner = fn(k); return inner })
	if inner != nil {
		return inner
	}
	if e != nil {
		return credentialError(ctx, "credential session failed")
	}
	return nil
}
func unavailable(ctx context.Context) error { return errors.Join(imapauth.ErrCredential, ctx.Err()) }
func credentials(ctx context.Context, deps Deps, selected config.Config) (out imapmail.Credentials, err error) {
	err = setupSession(ctx, deps, func(k imapauth.Items) error {
		c, e := routing()
		if e != nil {
			return e
		}
		if c.Account() != selected.Account() || c.Credential == nil || selected.Credential == nil || !c.Credential.Ready || c.Credential.Generation != selected.Credential.Generation {
			return unavailable(ctx)
		}
		p, e := c.Principal()
		if e != nil {
			return e
		}
		v, e := k.Get(ctx, p.Item())
		if e != nil {
			return unavailable(ctx)
		}
		secret, e := p.Decode(v, c.Credential.Generation)
		if e != nil {
			return unavailable(ctx)
		}
		agentio.RegisterSecret(secret)
		out = imapmail.Credentials{Account: p.Account(), Login: p.Login, Password: secret}
		return nil
	})
	return
}
func newYahooClient(deps Deps, c config.Config, f imapmail.CredentialFunc) (*imapmail.Client, error) {
	p, e := c.Principal()
	if e != nil {
		return nil, e
	}
	roles := map[imapmail.Role]string{}
	for role, name := range c.IMAP.RoleMappings {
		roles[imapmail.Role(role)] = name
	}
	factory := deps.NewIMAP
	if factory == nil {
		factory = imapmail.New
	}
	client, e := factory(imapmail.Config{Account: p.Account(), Address: p.Endpoint, ServerName: p.Host(), Profile: imapmail.Yahoo, RoleMappings: roles}, f)
	if e != nil || client == nil {
		return nil, errors.New("IMAP setup: client construction failed")
	}
	return client, nil
}
func samePrincipal(c, expected config.Config) error {
	if c.Mode() != expected.Mode() || c.Account() != expected.Account() || (c.Mode() == "graph" && (c.ClientID != expected.ClientID || c.TenantID != expected.TenantID)) {
		return &mailbox.AccountMismatchError{Operation: "setup", Expected: expected.Account().ID, Actual: c.Account().ID}
	}
	return nil
}

func yahooLogin(cmd *cobra.Command, agent *agentio.AgentIO, deps Deps, c config.Config) (commandErr error) {
	ctx := cmd.Context()
	db, e := openBound(ctx, deps, dbPath(cmd), c.Account())
	if e != nil {
		return yahooBindingError(e)
	}
	defer func() { commandErr = errors.Join(commandErr, db.Close()) }()
	input := deps.CredentialInput
	if input == nil {
		input = readCredentialInput
	}
	stdin, _ := cmd.Flags().GetBool("password-stdin")
	raw, e := input(ctx, cmd.InOrStdin(), agent.Stderr(), stdin)
	defer clear(raw)
	if e != nil {
		return credentialError(ctx, "input failed")
	}
	secret := string(raw)
	agentio.RegisterSecret(secret)
	p, e := c.Principal()
	if e != nil {
		return e
	}
	// Validate before the candidate can reach transport, including fake input.
	if _, e = p.Encode("00000000000000000000000000000000", secret); e != nil {
		return unavailable(ctx)
	}
	probe, e := newYahooClient(deps, c, func(context.Context) (imapmail.Credentials, error) {
		return imapmail.Credentials{Account: p.Account(), Login: p.Login, Password: secret}, nil
	})
	if e != nil {
		return e
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	_, probeErr := probe.Discover(probeCtx)
	cancel()
	closeErr := probe.Close()
	if probeErr != nil || closeErr != nil {
		return credentialError(ctx, "candidate authentication or cleanup failed")
	}
	e = setupSession(ctx, deps, func(k imapauth.Items) error {
		fresh, e := routing()
		if e != nil {
			return e
		}
		if e = samePrincipal(fresh, c); e != nil {
			return e
		}
		if ctx.Err() != nil {
			return unavailable(ctx)
		}
		var entropy [16]byte
		if _, e = rand.Read(entropy[:]); e != nil {
			return credentialError(ctx, "generation unavailable")
		}
		gen := hex.EncodeToString(entropy[:])
		value, e := p.Encode(gen, secret)
		if e != nil {
			return unavailable(ctx)
		}
		existing, e := k.Get(ctx, p.Item())
		if e != nil && !errors.Is(e, keychain.ErrNotFound) {
			return unavailable(ctx)
		}
		if e == nil && !p.Replaceable(existing) {
			return unavailable(ctx)
		}
		state := config.CredentialState{Version: 1, Generation: gen, Ready: false}
		if fresh.Credential != nil {
			state.Extra = fresh.Credential.Extra
		}
		fresh.Credential = &state
		if saveConfig(deps, fresh) != nil {
			return credentialError(ctx, "pending state could not be saved")
		}
		if k.Set(ctx, p.Item(), value) != nil {
			return unavailable(ctx)
		}
		back, e := k.Get(ctx, p.Item())
		if e != nil {
			return unavailable(ctx)
		}
		read, e := p.Decode(back, gen)
		if e != nil || read != secret || ctx.Err() != nil {
			return unavailable(ctx)
		}
		fresh.Credential.Ready = true
		if saveConfig(deps, fresh) != nil {
			return credentialError(ctx, "ready state could not be saved; sign in again")
		}
		return nil
	})
	if e != nil {
		return e
	}
	return agentio.Tabled(agent, []string{"account", "credential", "authenticated"}, []string{c.AccountID}, func(a string) []string { return []string{a, "stored", "true"} }, func(a string) map[string]any {
		return map[string]any{"account": a, "credential": "stored", "authenticated": true}
	})
}
func yahooStatus(ctx context.Context, deps Deps, c config.Config, agent *agentio.AgentIO) error {
	if c.Credential == nil || !c.Credential.Ready {
		return unavailable(ctx)
	}
	if _, e := credentials(ctx, deps, c); e != nil {
		return e
	}
	return agentio.Tabled(agent, []string{"account", "credential", "authenticated"}, []string{c.AccountID}, func(a string) []string { return []string{a, "stored", "false"} }, func(a string) map[string]any {
		return map[string]any{"account": a, "credential": "stored", "authenticated": false}
	})
}
func yahooLogout(ctx context.Context, deps Deps, c config.Config) error {
	return setupSession(ctx, deps, func(k imapauth.Items) error {
		fresh, e := routing()
		if e != nil {
			return e
		}
		if e = samePrincipal(fresh, c); e != nil {
			return e
		}
		p, e := fresh.Principal()
		if e != nil {
			return e
		}
		if fresh.Credential == nil {
			fresh.Credential = &config.CredentialState{Version: 1}
		}
		fresh.Credential.Ready = false
		if saveConfig(deps, fresh) != nil {
			return credentialError(ctx, "signed-out state could not be saved")
		}
		if k.Delete(ctx, p.Item()) != nil {
			return unavailable(ctx)
		}
		_, e = k.Get(ctx, p.Item())
		if !errors.Is(e, keychain.ErrNotFound) {
			return unavailable(ctx)
		}
		return nil
	})
}
func yahooBindingError(e error) error {
	return agentio.NewStaleError("configured principal/database binding refused; select a separate database for a different account: "+e.Error(), "mailpipe account show")
}

func pilotGuard(cmd *cobra.Command, operation string) error {
	c, e := routing()
	if e != nil && !errors.Is(e, config.ErrNotConfigured) {
		return e
	}
	if c.Mode() == "yahoo" {
		return &mailbox.UnsupportedError{Operation: "Yahoo read-only pilot " + operation}
	}
	return nil
}
