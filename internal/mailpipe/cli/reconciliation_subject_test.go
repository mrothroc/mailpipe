package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestReconciliationSubjectChangeBetweenBatchesAndRetry(t *testing.T) {
	for _, phase := range []string{"next-batch", "retry"} {
		t.Run(phase, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			s, err := store.Open(e.db)
			if err != nil {
				t.Fatal(err)
			}
			e.graph.server = map[string]serverMsg{}
			for i := 0; i < 21; i++ {
				id := fmt.Sprintf("original-%d", i)
				header := "<" + id + "@example.org>"
				e.graph.server[id] = serverMsg{"inbox", header, "sender@example.org"}
				if _, err = s.Upsert(ctx, "inbox", store.Record{GraphID: id, InternetMessageID: header, Sender: "sender@example.org"}); err != nil {
					t.Fatal(err)
				}
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			auth := &changingReconciliationAuth{fakeAuth: e.auth}
			var batches atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/me/mailFolders/inbox":
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "F-inbox"})
				case "/me/messages":
					value := []any{}
					for id := range e.graph.server {
						value = append(value, e.graph.message(id))
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"value": value})
				case "/$batch":
					if r.Method != http.MethodPost || batches.Add(1) != 1 {
						t.Error("request after authenticated subject changed")
					}
					var in struct {
						Requests []struct {
							ID, Method, URL string
							Headers         map[string]string
							Body            json.RawMessage
						}
					}
					if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
						t.Error(err)
					}
					responses := []any{}
					for j, req := range in.Requests {
						if req.Method != http.MethodGet || len(req.Body) != 0 || !strings.Contains(req.Headers["Prefer"], `IdType="ImmutableId"`) {
							t.Error("invalid inspection request")
						}
						id := strings.Split(strings.TrimPrefix(req.URL, "/me/messages/"), "?")[0]
						status := 200
						var body any = e.graph.message(id)
						if phase == "retry" && j == 0 {
							status = 429
							body = map[string]any{"error": map[string]any{"code": "TooManyRequests"}}
						}
						responses = append(responses, map[string]any{"id": req.ID, "status": status, "body": body})
					}
					auth.changed.Store(true)
					_ = json.NewEncoder(w).Encode(map[string]any{"responses": responses})
				default:
					t.Error("unexpected request")
					w.WriteHeader(400)
				}
			}))
			defer srv.Close()
			deps := cli.Deps{Auth: func() (cli.Authenticator, error) { return auth, nil }, Store: store.Open, Graph: func(a cli.Authenticator) cli.Graph {
				g := graph.New(a.Token)
				g.Base, g.HTTP, g.Sleep = srv.URL, srv.Client(), func(time.Duration) {}
				return g
			}}
			root := cli.NewRootCommand(deps)
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.SetArgs([]string{"--db", e.db, "account", "reconcile", "--json"})
			if err = root.ExecuteContext(ctx); err == nil || out.Len() != 0 || batches.Load() != 1 {
				t.Fatalf("changed subject acquired partial proof: %v %s %s", err, out.String(), stderr.String())
			}
			s, err = store.Open(e.db)
			if err != nil {
				t.Fatal("changed subject attributed history", err)
			}
			s.Close()
		})
	}
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
			root.SetArgs(append([]string{"--db", e.db}, legacyDiagnosticArgs(t)...))
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
