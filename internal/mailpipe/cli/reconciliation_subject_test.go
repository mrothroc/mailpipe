package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/mrothroc/mailpipe/internal/mailpipe/cli"
	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/msauth"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type changingReconciliationAuth struct {
	*fakeAuth
	changed atomic.Bool
	calls   atomic.Int32
}

func (a *changingReconciliationAuth) TokenFor(ctx context.Context, expected msauth.Subject) (string, error) {
	a.calls.Add(1)
	if a.changed.Load() {
		return "", msauth.ErrNotSignedIn
	}
	return a.fakeAuth.TokenFor(ctx, expected)
}

func TestReconciliationSubjectChangeStopsBeforeRemoteProof(t *testing.T) {
	for _, phase := range []string{"first-request", "next-page", "reinspection"} {
		t.Run(phase, func(t *testing.T) {
			e := newEnv(t)
			s, err := store.Open(e.db)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Upsert(context.Background(), "inbox", store.Record{GraphID: "g1", InternetMessageID: "<g1@example.org>", Sender: "info@campaign.example.org"}); err != nil {
				t.Fatal(err)
			}
			s.Close()
			auth := &changingReconciliationAuth{fakeAuth: e.auth}
			if phase == "first-request" {
				auth.changed.Store(true)
			}
			var unexpected atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/me/mailFolders/inbox":
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "F-inbox"})
				case "/me/messages":
					auth.changed.Store(true)
					page := map[string]any{"value": []any{e.graph.message("g1")}}
					if phase == "next-page" {
						page["@odata.nextLink"] = "/next"
					}
					_ = json.NewEncoder(w).Encode(page)
				default:
					unexpected.Add(1)
					http.Error(w, "unexpected request", 500)
				}
			}))
			defer srv.Close()
			deps := cli.Deps{Auth: func() (cli.Authenticator, error) { return auth, nil }, Store: store.Open, Graph: func(a cli.Authenticator) cli.Graph {
				g := graph.New(a.Token)
				g.Base = srv.URL
				g.HTTP = srv.Client()
				return g
			}}
			root := cli.NewRootCommand(deps)
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.SetArgs([]string{"--db", e.db, "account", "reconcile", "--json"})
			err = root.ExecuteContext(context.Background())
			if err == nil || out.Len() != 0 || unexpected.Load() != 0 || auth.calls.Load() == 0 {
				t.Fatalf("changed subject produced proof or another request: %v %s %s", err, out.String(), stderr.String())
			}
			s, err = store.Open(e.db)
			if err != nil {
				t.Fatal("changed subject attributed history", err)
			}
			s.Close()
		})
	}
}
