package msauth_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/public"

	"github.com/mrothroc/mailpipe/pkg/keychain"
	"github.com/mrothroc/mailpipe/pkg/msauth"
)

type memStore struct {
	items  map[string]string
	getErr error
}

func (m *memStore) Get(_ context.Context, account string) (string, error) {
	if m.getErr != nil {
		return "", m.getErr
	}
	v, ok := m.items[account]
	if !ok {
		return "", keychain.ErrNotFound
	}
	return v, nil
}
func (m *memStore) Set(_ context.Context, account, value string) error {
	m.items[account] = value
	return nil
}
func (m *memStore) Delete(_ context.Context, account string) error {
	delete(m.items, account)
	return nil
}

type fakeFlow struct {
	result public.AuthResult
	err    error
}

func (f fakeFlow) Message() string {
	return "To sign in, enter ABC123 at https://microsoft.com/devicelogin"
}
func (f fakeFlow) Wait(context.Context) (public.AuthResult, error) { return f.result, f.err }

type fakeClient struct {
	accounts    []public.Account
	accountsErr error
	silent      public.AuthResult
	silentErr   error
	flow        fakeFlow
	flowErr     error
	removed     int
	removeErr   error
}

func (f *fakeClient) Accounts(context.Context) ([]public.Account, error) {
	return f.accounts, f.accountsErr
}
func (f *fakeClient) AcquireTokenSilent(_ context.Context, _ []string, _ ...public.AcquireSilentOption) (public.AuthResult, error) {
	return f.silent, f.silentErr
}
func (f *fakeClient) StartDeviceCode(context.Context, []string) (msauth.DeviceFlow, error) {
	return f.flow, f.flowErr
}
func (f *fakeClient) RemoveAccount(context.Context, public.Account) error {
	f.removed++
	return f.removeErr
}

var scopes = []string{"Mail.ReadWrite", "MailboxSettings.ReadWrite"}
var account = public.Account{PreferredUsername: "alex@example.com"}

func good() public.AuthResult {
	return public.AuthResult{Account: account, AccessToken: "SECRET-TOKEN",
		GrantedScopes: []string{"Mail.ReadWrite", "MailboxSettings.ReadWrite", "User.Read"}}
}

func TestTokenWithoutAccountIsNotSignedIn(t *testing.T) {
	a := msauth.NewWithClient(&fakeClient{}, scopes, &memStore{items: map[string]string{}})
	if _, err := a.Token(context.Background()); !errors.Is(err, msauth.ErrNotSignedIn) {
		t.Fatalf("err = %v, want ErrNotSignedIn", err)
	}
}

func TestTokenSilentSuccess(t *testing.T) {
	a := msauth.NewWithClient(&fakeClient{accounts: []public.Account{account}, silent: good()}, scopes, &memStore{items: map[string]string{}})
	tok, err := a.Token(context.Background())
	if err != nil || tok != "SECRET-TOKEN" {
		t.Fatalf("Token = %q, %v", tok, err)
	}
}

func TestTokenSilentFailureIsExpiredSignIn(t *testing.T) {
	c := &fakeClient{accounts: []public.Account{account}, silentErr: errors.New("AADSTS70043: refresh token expired")}
	a := msauth.NewWithClient(c, scopes, &memStore{items: map[string]string{}})
	_, err := a.Token(context.Background())
	if !errors.Is(err, msauth.ErrNotSignedIn) || !strings.Contains(err.Error(), "AADSTS70043") {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusReportsAccountAndGrantedScopesNotTheToken(t *testing.T) {
	a := msauth.NewWithClient(&fakeClient{accounts: []public.Account{account}, silent: good()}, scopes, &memStore{items: map[string]string{}})
	s, err := a.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Account != "alex@example.com" || strings.Join(s.Scopes, " ") != "Mail.ReadWrite MailboxSettings.ReadWrite User.Read" {
		t.Fatalf("status = %+v", s)
	}
}

func TestLoginShowsTheCodeAndReturnsTheAccount(t *testing.T) {
	c := &fakeClient{flow: fakeFlow{result: good()}}
	a := msauth.NewWithClient(c, scopes, &memStore{items: map[string]string{}})
	var shown []string
	who, err := a.Login(context.Background(), func(s string) { shown = append(shown, s) })
	if err != nil || who != "alex@example.com" {
		t.Fatalf("Login = %q, %v", who, err)
	}
	if len(shown) != 1 || !strings.Contains(shown[0], "ABC123") {
		t.Fatalf("shown = %v", shown)
	}
}

func TestLoginErrorsAreExplained(t *testing.T) {
	for name, c := range map[string]*fakeClient{
		"start":    {flowErr: errors.New("AADSTS7000218: public client flows disabled")},
		"declined": {flow: fakeFlow{err: errors.New("authorization_declined")}},
	} {
		a := msauth.NewWithClient(c, scopes, &memStore{items: map[string]string{}})
		if _, err := a.Login(context.Background(), func(string) {}); err == nil {
			t.Fatalf("%s: no error", name)
		}
	}
}

func TestLogoutRemovesAccountsAndStoredCache(t *testing.T) {
	c := &fakeClient{accounts: []public.Account{account}}
	store := &memStore{items: map[string]string{msauth.CacheAccount: "cache"}}
	a := msauth.NewWithClient(c, scopes, store)
	if err := a.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.removed != 1 || len(store.items) != 0 {
		t.Fatalf("removed=%d items=%v", c.removed, store.items)
	}
}

type bytesCache struct{ data []byte }

func (b *bytesCache) Marshal() ([]byte, error) { return b.data, nil }
func (b *bytesCache) Unmarshal(d []byte) error { b.data = d; return nil }

func TestKeychainCacheRoundTrip(t *testing.T) {
	store := &memStore{items: map[string]string{}}
	c := msauth.NewKeychainCache(store)
	ctx := context.Background()
	empty := &bytesCache{}
	if err := c.Replace(ctx, empty, cacheReplaceHints()); err != nil || empty.data != nil {
		t.Fatalf("Replace on empty store: %v %q", err, empty.data)
	}
	if err := c.Export(ctx, &bytesCache{data: []byte(`{"x":1}`)}, cacheExportHints()); err != nil {
		t.Fatal(err)
	}
	loaded := &bytesCache{}
	if err := c.Replace(ctx, loaded, cacheReplaceHints()); err != nil || string(loaded.data) != `{"x":1}` {
		t.Fatalf("Replace = %v %q", err, loaded.data)
	}
}

type rejectingCache struct{ calls int }

func (r *rejectingCache) Unmarshal([]byte) error {
	r.calls++
	return errors.New("Unmarshal expected opening {")
}

// TestUnreadableStoredCacheIsTreatedAsEmpty: a cache MSAL cannot parse (written
// by another tool, or corrupted) must not block signing in again, because MSAL
// loads the cache during the login flow too. Login then overwrites it.
func TestUnreadableStoredCacheIsTreatedAsEmpty(t *testing.T) {
	store := &memStore{items: map[string]string{msauth.CacheAccount: "7b2268657822"}}
	c := msauth.NewKeychainCache(store)
	r := &rejectingCache{}
	if err := c.Replace(context.Background(), r, cacheReplaceHints()); err != nil {
		t.Fatalf("Replace = %v, want nil so sign-in can proceed", err)
	}
	if r.calls != 1 {
		t.Fatalf("Unmarshal called %d times", r.calls)
	}
}

func TestAnInterruptedCacheReadIsNotASignInFailure(t *testing.T) {
	c := &fakeClient{accountsErr: fmt.Errorf("keychain: read interrupted: %w", context.Canceled)}
	_, err := msauth.NewWithClient(c, nil, &memStore{items: map[string]string{}}).Token(context.Background())
	if !errors.Is(err, context.Canceled) || errors.Is(err, msauth.ErrNotSignedIn) {
		t.Fatalf("Token = %v, want an interruption, not a sign-in failure", err)
	}
}

func TestAnInterruptedRefreshIsNotAnExpiredSignIn(t *testing.T) {
	c := &fakeClient{accounts: []public.Account{{PreferredUsername: "owner@example.com"}}, silentErr: context.DeadlineExceeded}
	_, err := msauth.NewWithClient(c, nil, &memStore{items: map[string]string{}}).Token(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, msauth.ErrNotSignedIn) {
		t.Fatalf("Token = %v, want an interruption, not an expired sign-in", err)
	}
}

func TestAnInterruptedLoginIsAnInterruption(t *testing.T) {
	for name, c := range map[string]*fakeClient{
		"start": {flowErr: context.Canceled},
		"wait":  {flow: fakeFlow{err: context.DeadlineExceeded}},
	} {
		_, err := msauth.NewWithClient(c, scopes, &memStore{items: map[string]string{}}).Login(context.Background(), func(string) {})
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s: Login = %v, want the interruption kept", name, err)
		}
	}
}

func TestAnInterruptedLogoutKeepsTheStoredCache(t *testing.T) {
	c := &fakeClient{accountsErr: fmt.Errorf("keychain: read interrupted: %w", context.Canceled)}
	store := &memStore{items: map[string]string{msauth.CacheAccount: "cache"}}
	if err := msauth.NewWithClient(c, scopes, store).Logout(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Logout = %v, want the interruption", err)
	}
	if store.items[msauth.CacheAccount] != "cache" {
		t.Fatal("an interrupted logout deleted the cache")
	}
}

// A damaged stored cache must not block signing in again, like an unparsable
// one; an interrupted read is still reported.
func TestADamagedStoredCacheIsEmptyButAnInterruptedReadIsNot(t *testing.T) {
	damaged := msauth.NewKeychainCache(&memStore{getErr: fmt.Errorf("chunk 2 missing: %w", keychain.ErrDamaged)})
	if err := damaged.Replace(context.Background(), &bytesCache{}, cacheReplaceHints()); err != nil {
		t.Fatalf("damaged: Replace = %v, want an empty cache", err)
	}
	cut := msauth.NewKeychainCache(&memStore{getErr: fmt.Errorf("read interrupted: %w", context.Canceled)})
	if err := cut.Replace(context.Background(), &bytesCache{}, cacheReplaceHints()); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted: Replace = %v", err)
	}
}

func TestAnInterruptedAccountRemovalIsAnInterruption(t *testing.T) {
	c := &fakeClient{accounts: []public.Account{account}, removeErr: context.Canceled}
	store := &memStore{items: map[string]string{msauth.CacheAccount: "cache"}}
	if err := msauth.NewWithClient(c, scopes, store).Logout(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Logout = %v, want the interruption", err)
	}
}
