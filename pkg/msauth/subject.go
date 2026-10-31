package msauth

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/public"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// Subject is the non-secret resource-tenant identity returned by MSAL. Display
// logins and home-account IDs are deliberately not mailbox identities.
type Subject struct{ TenantID, ObjectID string }

var subjectUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Account gives the versioned durable Graph mailbox key.
func (s Subject) Account() mailbox.Account {
	if !subjectUUID.MatchString(s.TenantID) || !subjectUUID.MatchString(s.ObjectID) {
		return mailbox.Account{}
	}
	return mailbox.Account{ID: mailbox.AccountID("graph:v1:" + strings.ToLower(s.TenantID) + ":" + strings.ToLower(s.ObjectID)), ProviderID: "graph"}
}

func (a *Auth) subject(r public.AuthResult) (Subject, error) {
	s := Subject{TenantID: strings.ToLower(r.IDToken.TenantID), ObjectID: strings.ToLower(r.IDToken.Oid)}
	if !subjectUUID.MatchString(s.TenantID) || !subjectUUID.MatchString(s.ObjectID) ||
		(a.tenant != "" && !strings.EqualFold(a.tenant, s.TenantID)) ||
		(r.Account.Realm != "" && !strings.EqualFold(r.Account.Realm, s.TenantID)) ||
		(r.Account.LocalAccountID != "" && !strings.EqualFold(r.Account.LocalAccountID, s.ObjectID)) {
		return Subject{}, fmt.Errorf("%w: authenticated mailbox subject is missing or inconsistent", ErrNotSignedIn)
	}
	return s, nil
}

// Subject authenticates lazily and obtains the immutable subject, without
// returning or persisting a token.
func (a *Auth) Subject(ctx context.Context) (Subject, error) {
	r, err := a.silent(ctx)
	if err != nil {
		return Subject{}, err
	}
	return a.subject(r)
}

// TokenFor refuses account changes on every silent acquisition, including a
// refresh between inspection and mutation.
func (a *Auth) TokenFor(ctx context.Context, expected Subject) (string, error) {
	r, err := a.silent(ctx)
	if err != nil {
		return "", err
	}
	s, err := a.subject(r)
	if err != nil {
		return "", err
	}
	if s != expected {
		return "", fmt.Errorf("%w: authenticated mailbox changed; reconcile the selected account", ErrNotSignedIn)
	}
	return r.AccessToken, nil
}
