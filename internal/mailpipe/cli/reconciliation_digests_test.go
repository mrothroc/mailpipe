package cli_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/store"
)

// The ordinary command uses the real HTTP collector and independent store review.
// Only enumeration presentation varies; every inspected/history input is fixed.
func TestReconcileApprovalStableAuditFreshHTTP(t *testing.T) {
	for _, kind := range []string{"paired-to-paired", "paired-to-absent", "absent-to-paired"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
			enumeratedName := "First enumeration"
			if kind == "absent-to-paired" {
				enumeratedName = "Current sender"
			}
			enumerations, batches, inspections := 0, 0, 0
			invalidEnumeration := false
			wire := func(name string) map[string]any {
				return map[string]any{"id": "original", "parentFolderId": "inbox", "internetMessageId": "<original@example.org>", "subject": "Invented subject", "from": map[string]any{"emailAddress": map[string]string{"address": "sender@example.org", "name": name}}, "toRecipients": []any{map[string]any{"emailAddress": map[string]string{"address": "reader@example.org", "name": "Reader"}}}, "ccRecipients": []any{}, "receivedDateTime": "2026-01-01T00:00:00Z", "hasAttachments": false, "isRead": false, "bodyPreview": "", "changeKey": "revision-1"}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
					t.Error("missing immutable HTTP preference")
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/me/messages":
					enumerations++
					m := wire(enumeratedName)
					if invalidEnumeration {
						m["subject"] = "Invalid enumeration subject"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{m}})
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/me/mailFolders/"):
					id := strings.TrimPrefix(r.URL.Path, "/me/mailFolders/")
					_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "displayName": id})
				case r.Method == http.MethodPost && r.URL.Path == "/$batch":
					if enumerations != batches+1 {
						t.Error("inspection did not follow complete enumeration")
					}
					batches++
					var in struct {
						Requests []struct {
							ID, Method, URL string
							Headers         map[string]string
							Body            json.RawMessage
						}
					}
					if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if len(in.Requests) != 1 {
						t.Error("inspection count changed")
					}
					out := []any{}
					for _, req := range in.Requests {
						u, err := url.Parse(req.URL)
						if err != nil || u.Path != "/me/messages/original" || req.Method != "GET" || len(req.Body) != 0 || !strings.Contains(req.Headers["Prefer"], `IdType="ImmutableId"`) {
							t.Error("unexpected request or mutation")
							w.WriteHeader(400)
							return
						}
						inspections++
						out = append(out, map[string]any{"id": req.ID, "status": 200, "body": wire("Current sender")})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"responses": out})
				default:
					t.Error("unexpected request/action")
					w.WriteHeader(400)
				}
			}))
			defer srv.Close()
			g := graph.New(func(context.Context) (string, error) { return "invented-token", nil })
			g.Base, g.HTTP = srv.URL, srv.Client()
			subject, err := e.auth.Subject(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			e.provider, err = graphmail.New(g, subject.Account())
			if err != nil {
				t.Fatal(err)
			}
			code, out, stderr := e.run("account", "reconcile", "--json")
			if code != 0 {
				t.Fatalf("first preview: %d %s", code, stderr)
			}
			first := reconciliationRows(t, out)[0]
			enumeratedName = "Second enumeration"
			if kind == "paired-to-absent" {
				enumeratedName = "Current sender"
			}
			code, out, stderr = e.run("account", "reconcile", "--json")
			if code != 0 {
				t.Fatalf("second preview: %d %s", code, stderr)
			}
			second := reconciliationRows(t, out)[0]
			if first["reconciliation_digest"] != second["reconciliation_digest"] {
				t.Fatal("permitted enumeration invalidated identical inspected approval")
			}
			if first["audit_digest"] == nil || first["audit_digest"] == second["audit_digest"] {
				t.Fatal("fresh raw observation audit missing/unchanged")
			}
			legacy, err := store.Open(e.db)
			if err != nil {
				t.Fatal("preview attributed history", err)
			}
			legacy.Close()
			enumeratedName = "Execution enumeration"
			args := []string{"account", "reconcile", "--json", "--execute", "--confirm-account", first["account"].(string), "--confirm-history", first["source_digest"].(string), "--confirm-reconciliation", first["reconciliation_digest"].(string)}
			invalidEnumeration = true
			code, out, stderr = e.run(args...)
			if code == 0 || out != "" {
				t.Fatal("invalid fresh enumeration reused stable approval")
			}
			unbound, err := store.Open(e.db)
			if err != nil {
				t.Fatal("invalid pass bound history", err)
			}
			unbound.Close()
			invalidEnumeration = false
			args[len(args)-1] = second["audit_digest"].(string)
			if code, out, stderr = e.run(args...); code == 0 || out != "" {
				t.Fatal("audit identity authorized binding")
			}
			args[len(args)-1] = first["reconciliation_digest"].(string)
			code, out, stderr = e.run(args...)
			if code != 0 {
				t.Fatalf("fresh valid enumeration rejected approved inspection: %d %s", code, stderr)
			}
			rows := reconciliationRows(t, out)
			committed := rows[0]
			if committed["reconciliation_digest"] != first["reconciliation_digest"] || committed["audit_digest"] == second["audit_digest"] {
				t.Fatal("stored approval/fresh audit confused")
			}
			item := rows[1]
			state := item["state"].(map[string]any)
			before := item["enumerated_state"].(map[string]any)
			if state["Metadata"].(map[string]any)["Sender"].(map[string]any)["Name"] != "Current sender" || before["Metadata"].(map[string]any)["Sender"].(map[string]any)["Name"] != enumeratedName {
				t.Fatal("complete current/raw pair lost")
			}
			if enumerations != 5 || batches != 5 || inspections != 5 {
				t.Fatal("digest separation changed request counts")
			}
			e.auth.signedIn = false
			code, out, stderr = e.run("account", "reconcile", "--json")
			if code != 0 || reconciliationRows(t, out)[0]["audit_digest"] != committed["audit_digest"] {
				t.Fatalf("offline audit receipt: %d %s", code, stderr)
			}
			if enumerations != 5 || batches != 5 || inspections != 5 {
				t.Fatal("offline receipt read remote state")
			}
		})
	}
}
