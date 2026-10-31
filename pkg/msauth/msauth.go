// Package msauth signs in to Microsoft 365 with the device code flow (a public
// client, no secret) and keeps MSAL's token cache in the macOS Keychain.
package msauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/cache"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/public"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/keychain"
)

// CacheAccount is the Keychain account that holds the serialized token cache.
const CacheAccount = "token-cache"

// ErrNotSignedIn reports that no usable sign-in exists; the caller should run
// the login flow.
var ErrNotSignedIn = errors.New("not signed in")

// Config identifies the app registration and the delegated scopes requested.
type Config struct {
	ClientID string
	TenantID string
	Scopes   []string
}

// Store is the secret storage the token cache lives in.
type Store interface {
	Get(ctx context.Context, account string) (string, error)
	Set(ctx context.Context, account, value string) error
	Delete(ctx context.Context, account string) error
}

// DeviceFlow is a started device code sign-in.
type DeviceFlow interface {
	Message() string
	Wait(ctx context.Context) (public.AuthResult, error)
}

// Client is the subset of MSAL's public client that Auth needs.
type Client interface {
	Accounts(ctx context.Context) ([]public.Account, error)
	AcquireTokenSilent(ctx context.Context, scopes []string, opts ...public.AcquireSilentOption) (public.AuthResult, error)
	StartDeviceCode(ctx context.Context, scopes []string) (DeviceFlow, error)
	RemoveAccount(ctx context.Context, account public.Account) error
}

// Status describes the current sign-in.
type Status struct {
	Account string
	Scopes  []string
}

// Auth acquires Graph access tokens for the signed-in user.
type Auth struct {
	client Client
	scopes []string
	store  Store
	tenant string
}

// New builds an Auth over the real MSAL client, with its cache in store.
func New(cfg Config, store Store) (*Auth, error) {
	pc, err := public.New(cfg.ClientID,
		public.WithAuthority("https://login.microsoftonline.com/"+cfg.TenantID),
		public.WithCache(NewKeychainCache(store)))
	if err != nil {
		return nil, fmt.Errorf("msal: %w", err)
	}
	a := NewWithClient(msalClient{pc}, cfg.Scopes, store)
	a.tenant = cfg.TenantID
	return a, nil
}

// NewWithClient builds an Auth over any Client (tests use a fake).
func NewWithClient(client Client, scopes []string, store Store) *Auth {
	return &Auth{client: client, scopes: scopes, store: store}
}

// interrupted reports a cancelled or timed-out call, which says nothing about
// the sign-in and must not send the user to sign in again.
func interrupted(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (a *Auth) silent(ctx context.Context) (public.AuthResult, error) {
	accounts, err := a.client.Accounts(ctx)
	if interrupted(err) {
		return public.AuthResult{}, fmt.Errorf("reading the token cache: %w", err)
	}
	if err != nil {
		return public.AuthResult{}, fmt.Errorf("%w: reading the token cache failed: %v", ErrNotSignedIn, err)
	}
	if len(accounts) == 0 {
		return public.AuthResult{}, ErrNotSignedIn
	}
	result, err := a.client.AcquireTokenSilent(ctx, a.scopes, public.WithSilentAccount(accounts[0]))
	if interrupted(err) {
		return public.AuthResult{}, fmt.Errorf("refreshing the sign-in: %w", err)
	}
	if err != nil {
		return public.AuthResult{}, fmt.Errorf("%w: sign-in expired (%v)", ErrNotSignedIn, err)
	}
	agentio.RegisterSecret(result.AccessToken)
	return result, nil
}

// Token returns a valid access token, refreshing it silently when needed.
func (a *Auth) Token(ctx context.Context) (string, error) {
	result, err := a.silent(ctx)
	if err != nil {
		return "", err
	}
	return result.AccessToken, nil
}

// Status reports the signed-in account and the scopes it was granted.
func (a *Auth) Status(ctx context.Context) (Status, error) {
	result, err := a.silent(ctx)
	if err != nil {
		return Status{}, err
	}
	return Status{Account: result.Account.PreferredUsername, Scopes: result.GrantedScopes}, nil
}

// Login runs the device code flow: notify receives the instructions (URL and
// code) to show the user, and Login returns once the user has signed in.
func (a *Auth) Login(ctx context.Context, notify func(string)) (string, error) {
	flow, err := a.client.StartDeviceCode(ctx, a.scopes)
	if err != nil {
		return "", fmt.Errorf("could not start sign-in: %w", err)
	}
	notify(flow.Message())
	result, err := flow.Wait(ctx)
	if err != nil {
		return "", fmt.Errorf("sign-in failed: %w", err)
	}
	agentio.RegisterSecret(result.AccessToken)
	return result.Account.PreferredUsername, nil
}

// Logout forgets every cached account and deletes the stored cache.
func (a *Auth) Logout(ctx context.Context) error {
	accounts, err := a.client.Accounts(ctx)
	if interrupted(err) {
		return fmt.Errorf("reading the token cache: %w", err) // keep the cache: nothing was decided
	}
	if err == nil {
		for _, acc := range accounts {
			if err := a.client.RemoveAccount(ctx, acc); err != nil {
				return fmt.Errorf("remove cached account: %w", err)
			}
		}
	}
	return a.store.Delete(ctx, CacheAccount)
}

// KeychainCache implements MSAL's cache.ExportReplace over a Store.
type KeychainCache struct{ store Store }

// NewKeychainCache returns a token cache persisted in store.
func NewKeychainCache(store Store) *KeychainCache { return &KeychainCache{store: store} }

// Replace loads the stored cache into MSAL. No stored cache is an empty cache,
// and so is a damaged one or one MSAL cannot parse (written by another tool,
// or corrupted): MSAL loads the cache during the login flow too, so failing
// here would make signing in again impossible. The next Export overwrites it.
func (k *KeychainCache) Replace(ctx context.Context, c cache.Unmarshaler, _ cache.ReplaceHints) error {
	data, err := k.store.Get(ctx, CacheAccount)
	if errors.Is(err, keychain.ErrNotFound) || errors.Is(err, keychain.ErrDamaged) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = c.Unmarshal([]byte(data)) // unreadable means empty, as documented above
	return nil
}

// Export saves MSAL's cache after every change.
func (k *KeychainCache) Export(ctx context.Context, c cache.Marshaler, _ cache.ExportHints) error {
	data, err := c.Marshal()
	if err != nil {
		return err
	}
	return k.store.Set(ctx, CacheAccount, string(data))
}

// msalClient adapts MSAL's public.Client to Client.
type msalClient struct{ c public.Client }

func (m msalClient) Accounts(ctx context.Context) ([]public.Account, error) { return m.c.Accounts(ctx) }

func (m msalClient) AcquireTokenSilent(ctx context.Context, scopes []string, opts ...public.AcquireSilentOption) (public.AuthResult, error) {
	return m.c.AcquireTokenSilent(ctx, scopes, opts...)
}

func (m msalClient) StartDeviceCode(ctx context.Context, scopes []string) (DeviceFlow, error) {
	dc, err := m.c.AcquireTokenByDeviceCode(ctx, scopes)
	if err != nil {
		return nil, err
	}
	return deviceFlow{dc}, nil
}

func (m msalClient) RemoveAccount(ctx context.Context, account public.Account) error {
	return m.c.RemoveAccount(ctx, account)
}

type deviceFlow struct{ dc public.DeviceCode }

func (d deviceFlow) Message() string { return d.dc.Result.Message }

func (d deviceFlow) Wait(ctx context.Context) (public.AuthResult, error) {
	return d.dc.AuthenticationResult(ctx)
}
