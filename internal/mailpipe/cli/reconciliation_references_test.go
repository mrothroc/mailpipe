package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/store"
)

func TestReconcileReferenceScopeIgnoresUnrelatedListHTTP(t *testing.T) {
	e := newEnv(t)
	seedConflictHistory(t, e, []string{"first", "second"}, "<shared@example.org>", "sender@example.org")
	lists, batches, inspections := 0, 0, 0
	wire := func(id string) map[string]any {
		return map[string]any{"id": id, "parentFolderId": "inbox", "internetMessageId": "<shared@example.org>", "subject": "Invented", "from": map[string]any{"emailAddress": map[string]string{"address": "sender@example.org"}}, "toRecipients": []any{}, "ccRecipients": []any{}, "receivedDateTime": "2026-01-01T00:00:00Z", "hasAttachments": false, "isRead": false, "bodyPreview": "", "changeKey": "revision"}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
			t.Error("immutable preference missing")
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/me/messages":
			lists++
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{wire("unrelated"), wire("unrelated")}})
		case r.Method == "GET" && r.URL.Path == "/me/mailFolders/inbox":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "inbox", "displayName": "Inbox"})
		case r.Method == "POST" && r.URL.Path == "/$batch":
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
			if len(in.Requests) != 2 {
				t.Error("not every historical reference inspected")
			}
			out := []any{}
			for i, q := range in.Requests {
				u, err := url.Parse(q.URL)
				if err != nil || i >= 2 || u.Path != "/me/messages/"+[]string{"first", "second"}[i] || q.Method != "GET" || len(q.Body) != 0 || !strings.Contains(q.Headers["Prefer"], `IdType="ImmutableId"`) {
					t.Error("unexpected exact read")
					w.WriteHeader(400)
					return
				}
				inspections++
				out = append(out, map[string]any{"id": q.ID, "status": 200, "body": wire([]string{"first", "second"}[i])})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"responses": out})
		default:
			t.Error("unexpected discovery/content/mutation request")
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
	if !strings.Contains(stderr, "no mailbox enumeration") || strings.Contains(stderr, "legacy paired diagnostics") {
		t.Fatal("default hint misstates reference protocol", stderr)
	}
	if code != 0 || lists != 0 || batches != 1 || inspections != 2 {
		t.Fatalf("unrelated list still blocks exact attribution: exit=%d list=%d batch=%d inspect=%d %s", code, lists, batches, inspections, stderr)
	}
	rows := reconciliationRows(t, out)
	if rows[0]["version"] != float64(9) || rows[0]["method"] != "graph-exact-original-inspections-v1" || len(rows) != 3 {
		t.Fatal("new reference proposal not represented")
	}
	s, err := store.Open(e.db)
	if err != nil {
		t.Fatal("preview bound history", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
}

func legacyDiagnosticArgs(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return []string{"account", "reconcile", "--json", "--sender-address-diagnostic-out", filepath.Join(dir, "sender.json")}
}

func TestReferenceCommandFailedHTTPHasNoAuthority(t *testing.T) {
	for _, kind := range []string{"unknown404", "ambiguous404", "missing-metadata", "removed", "wrong-id", "original-sender", "read-denied"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
			before, err := store.PrepareLegacyBinding(context.Background(), e.db)
			if err != nil {
				t.Fatal(err)
			}
			configuration, err := os.ReadFile(config.DefaultPath())
			if err != nil {
				t.Fatal(err)
			}
			folders, batches := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/me/mailFolders/inbox" {
					folders++
					_ = json.NewEncoder(w).Encode(map[string]string{"id": "inbox"})
					return
				}
				if r.Method != "POST" || r.URL.Path != "/$batch" {
					t.Error("unexpected discovery/content/action")
					w.WriteHeader(400)
					return
				}
				batches++
				var req struct {
					Requests []struct{ ID, Method, URL string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Requests) != 1 {
					t.Error("incomplete request", err)
					w.WriteHeader(400)
					return
				}
				q := req.Requests[0]
				if q.Method != "GET" || !strings.HasPrefix(q.URL, "/me/messages/original?") {
					t.Error("wrong reference")
				}
				status := 200
				m := map[string]any{"id": "original", "parentFolderId": "inbox", "internetMessageId": "<original@example.org>", "subject": "invented-private-secret\x1b\nhttps://host.example.org/private", "from": map[string]any{"emailAddress": map[string]string{"address": "sender@example.org"}}, "toRecipients": []any{}, "ccRecipients": []any{}, "receivedDateTime": "2026-01-01T00:00:00Z", "hasAttachments": false, "isRead": false, "bodyPreview": "", "changeKey": "revision"}
				var body any = m
				switch kind {
				case "unknown404":
					status = 404
					body = map[string]any{"error": map[string]string{"code": "untrusted-code", "message": "invented-private-secret"}}
				case "ambiguous404":
					status = 404
					body = json.RawMessage(`{"error":{"code":"Wrong","code":"ErrorItemNotFound"}}`)
				case "missing-metadata":
					delete(m, "subject")
				case "removed":
					m["@removed"] = nil
				case "wrong-id":
					m["id"] = "other"
				case "original-sender":
					m["from"].(map[string]any)["emailAddress"].(map[string]string)["address"] = "other@example.org"
				case "read-denied":
					status = 403
					body = map[string]any{"error": map[string]string{"code": "untrusted-code", "message": "invented-private-secret"}}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"responses": []any{map[string]any{"id": q.ID, "status": status, "body": body}}})
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
			if code == 0 || out != "" || folders != 1 || batches != 1 {
				t.Fatalf("failed exact read produced output/requests: %d %s", code, stderr)
			}
			for _, value := range []string{"invented-private-secret", "host.example.org", "other@example.org", "ErrorItemNotFound", "untrusted-code", e.db, "--confirm-", "\x1b"} {
				if strings.Contains(stderr, value) {
					t.Fatal("failed read leaked value/authority")
				}
			}
			after, err := store.PrepareLegacyBinding(context.Background(), e.db)
			if err != nil || after.Digest() != before.Digest() {
				t.Fatal("failed read changed history", err)
			}
			now, err := os.ReadFile(config.DefaultPath())
			if err != nil || !bytes.Equal(configuration, now) {
				t.Fatal("failed read changed routing", err)
			}
			if _, found, err := store.ReadLegacyReconciliationReceipt(context.Background(), e.db, subject.Account()); err != nil || found {
				t.Fatal("failed read created receipt", err)
			}
		})
	}
}
