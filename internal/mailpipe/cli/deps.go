package cli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/apply"
	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/keychain"
	"github.com/mrothroc/mailpipe/pkg/mailsync"
	"github.com/mrothroc/mailpipe/pkg/msauth"
	"github.com/mrothroc/mailpipe/pkg/render"
	"github.com/mrothroc/mailpipe/pkg/rules"
	"github.com/mrothroc/mailpipe/pkg/store"
)

// The Entra app registration (client and tenant IDs) comes from the user's
// configuration (~/mail/config.json, see internal/mailpipe/config), never from
// code.
const (
	// keychainService holds the MSAL token cache (account msauth.CacheAccount).
	keychainService = "mailpipe"
	dbEnv           = "MAILPIPE_DB"
	dataDir         = "mail"
	loginHint       = "mailpipe auth login"
	// rerunHint follows an interruption: every command resumes where it stopped.
	rerunHint = "rerun the same command to continue"
)

var scopes = []string{"Mail.ReadWrite", "MailboxSettings.ReadWrite"}

// Authenticator is the sign-in surface the commands use.
type Authenticator interface {
	Token(ctx context.Context) (string, error)
	Status(ctx context.Context) (msauth.Status, error)
	Login(ctx context.Context, notify func(string)) (string, error)
	Logout(ctx context.Context) error
}

// Graph is the Graph surface the commands use.
type Graph interface {
	mailsync.Source
	apply.Graph
	rules.Graph
	// Raw returns a response body as is, such as a message's MIME source.
	Raw(ctx context.Context, path string) ([]byte, error)
}

// Deps builds the collaborators lazily, so `mailpipe --help` needs no
// Keychain, network or database. Tests replace every field.
type Deps struct {
	Auth  func() (Authenticator, error)
	Store func(path string) (*store.Store, error)
	Graph func(auth Authenticator) Graph
	// Secret reads a Keychain item (keychain.ErrNotFound when absent).
	Secret func(ctx context.Context, service, account string) (string, error)
	// Render turns an offline HTML page into a PDF.
	Render func(ctx context.Context, page string) ([]byte, error)
}

// DefaultDeps wires the real Keychain, MSAL, Graph and SQLite.
func DefaultDeps() Deps {
	return Deps{
		Auth: func() (Authenticator, error) {
			cfg, err := config.Load(config.DefaultPath())
			if err != nil {
				return nil, err
			}
			// The lock sits beside the configuration, so every mailpipe process
			// using this configuration shares it.
			kc := keychain.New(keychainService, nil).WithLock(filepath.Join(filepath.Dir(config.DefaultPath()), "keychain.lock"))
			return msauth.New(msauth.Config{ClientID: cfg.ClientID, TenantID: cfg.TenantID, Scopes: scopes}, kc)
		},
		Store: store.Open,
		Graph: func(a Authenticator) Graph { return graph.New(a.Token) },
		Secret: func(ctx context.Context, service, account string) (string, error) {
			return keychain.New(service, nil).Get(ctx, account)
		},
		Render: func(ctx context.Context, page string) ([]byte, error) {
			return render.Chrome{Path: render.FindChrome()}.PDF(ctx, page)
		},
	}
}

// dbPath applies --db, then $MAILPIPE_DB, then ~/mail/mailpipe.db.
func dbPath(cmd *cobra.Command) string {
	if f, err := cmd.Flags().GetString("db"); err == nil && strings.TrimSpace(f) != "" {
		return strings.TrimSpace(f)
	}
	if p := strings.TrimSpace(os.Getenv(dbEnv)); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "mailpipe.db"
	}
	return filepath.Join(home, dataDir, "mailpipe.db")
}

// interrupted reports a cancelled or timed-out command (Ctrl-C), which says
// nothing about sign-in or the mailbox.
func interrupted(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// classify maps collaborator errors onto the contract's exit codes and hints.
func classify(err error) error {
	var coded *agentio.CodedError
	if errors.As(err, &coded) {
		return err
	}
	if interrupted(err) {
		return agentio.NewError(err.Error(), rerunHint)
	}
	if errors.Is(err, config.ErrNotConfigured) {
		return agentio.NewStaleError(err.Error(), config.InitHint)
	}
	if errors.Is(err, msauth.ErrNotSignedIn) {
		return agentio.NewStaleError(err.Error(), loginHint)
	}
	var apiErr *graph.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return agentio.NewStaleError(apiErr.Error(), "mailpipe auth status")
		case http.StatusTooManyRequests:
			return agentio.NewStaleError(apiErr.Error()+" (throttled; wait, then rerun the command)", "")
		}
	}
	return err
}
