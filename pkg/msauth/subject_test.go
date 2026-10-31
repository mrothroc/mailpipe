package msauth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/public"
	"github.com/mrothroc/mailpipe/pkg/msauth"
)

func TestSubjectPinsResourceTenantAndObject(t *testing.T) {
	r := good()
	r.IDToken.TenantID = "22222222-2222-4222-8222-222222222222"
	r.IDToken.Oid = "33333333-3333-4333-8333-333333333333"
	f := &fakeClient{accounts: []public.Account{account}, silent: r}
	a := msauth.NewWithClient(f, scopes, &memStore{items: map[string]string{}})
	sub, err := a.Subject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.silent.Account.PreferredUsername = "renamed@example.org"
	if _, err := a.TokenFor(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	f.silent.IDToken.Oid = "44444444-4444-4444-8444-444444444444"
	if _, err := a.TokenFor(context.Background(), sub); !errors.Is(err, msauth.ErrNotSignedIn) {
		t.Fatalf("subject switch: %v", err)
	}
	f.silent.IDToken.Oid = ""
	if _, err := a.Subject(context.Background()); err == nil {
		t.Fatal("display username became an identity")
	}
}
