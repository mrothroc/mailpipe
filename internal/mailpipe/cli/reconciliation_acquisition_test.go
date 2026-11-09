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
	"strconv"
	"strings"
	"testing"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/store"
)

const commandAcquisitionHostile = "untrusted-acquisition-value\n\x1b[31m https://host.example.org/private"

type commandAcquisitionCalls struct{ folders, pages, batches, inspections int }

func commandAcquisitionHTTP(t *testing.T, e *env, kind string) *commandAcquisitionCalls {
	t.Helper()
	wire := func() map[string]any {
		return map[string]any{"id": "original", "parentFolderId": "inbox", "internetMessageId": "<original@example.org>", "subject": "Invented subject", "from": map[string]any{"emailAddress": map[string]string{"address": "sender@example.org", "name": commandAcquisitionHostile}}, "toRecipients": []any{}, "ccRecipients": []any{}, "receivedDateTime": "2026-01-01T00:00:00Z", "hasAttachments": false, "isRead": false, "bodyPreview": commandAcquisitionHostile, "changeKey": "revision"}
	}
	calls := &commandAcquisitionCalls{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
			t.Error("missing immutable HTTP preference")
		}
		if r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders/inbox" {
			calls.folders++
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "inbox", "displayName": "Inbox"})
			return
		}
		if r.Method == http.MethodGet && (r.URL.Path == "/me/messages" || r.URL.Path == "/continue") {
			calls.pages++
			if calls.pages == 1 {
				if r.URL.Path != "/me/messages" || r.URL.Query().Get("$top") != "200" || r.URL.Query().Get("$select") == "" {
					t.Error("initial query changed")
				}
			} else if r.URL.Path != "/continue" || r.URL.RawQuery != "opaque=keep%2Btoken" {
				t.Error("opaque continuation changed")
			}
			m := wire()
			records := []any{m}
			page := map[string]any{"value": records}
			if kind == "across" && calls.pages == 1 {
				page["@odata.nextLink"] = srv.URL + "/continue?opaque=keep%2Btoken"
			} else {
				switch kind {
				case "within":
					page["value"] = []any{m, wire()}
				case "null":
					m["@removed"] = nil
				case "object", "metadata":
					m["@removed"] = map[string]any{"reason": commandAcquisitionHostile}
					if kind == "metadata" {
						delete(m, "subject")
					}
				case "shape":
					m["@removed"] = []any{commandAcquisitionHostile}
				}
			}
			_ = json.NewEncoder(w).Encode(page)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/$batch" {
			t.Error("unexpected body or mutation request")
			w.WriteHeader(400)
			return
		}
		calls.batches++
		if kind != "success" {
			t.Error("inspection occurred after failed/incomplete acquisition")
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
			w.WriteHeader(400)
			return
		}
		if len(in.Requests) != 1 {
			t.Error("inspection cardinality changed")
		}
		out := []any{}
		for _, req := range in.Requests {
			u, err := url.Parse(req.URL)
			if err != nil || req.Method != "GET" || u.Path != "/me/messages/original" || len(req.Body) != 0 || !strings.Contains(req.Headers["Prefer"], `IdType="ImmutableId"`) {
				t.Error("inspection request changed")
				w.WriteHeader(400)
				return
			}
			calls.inspections++
			out = append(out, map[string]any{"id": req.ID, "status": 200, "body": wire()})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"responses": out})
	}))
	t.Cleanup(srv.Close)
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
	return calls
}

func TestReconciliationAcquisitionCommandHTTP(t *testing.T) {
	for _, tc := range []struct {
		kind, category string
		pages          int
	}{
		{"within", "duplicate-id-within-page", 1}, {"across", "duplicate-id-across-pages", 2},
		{"null", "removed-marker-null", 1}, {"object", "removed-marker-object", 1}, {"shape", "removed-marker-invalid-shape", 1},
		{"metadata", "incomplete-metadata", 1},
	} {
		for _, mode := range []string{"capture", "selected"} {
			t.Run(tc.kind+"/"+mode, func(t *testing.T) {
				e := newEnv(t)
				rows := seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
				before, err := store.PrepareLegacyBinding(context.Background(), e.db)
				if err != nil {
					t.Fatal(err)
				}
				configuration, err := os.ReadFile(config.DefaultPath())
				if err != nil {
					t.Fatal(err)
				}
				calls := commandAcquisitionHTTP(t, e, tc.kind)
				args := []string{"account", "reconcile", "--json"}
				dir := t.TempDir()
				if err = os.Chmod(dir, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "diagnostic.json")
				if mode != "default" {
					args = append(args, "--sender-address-diagnostic-out", path)
				}
				if mode == "selected" {
					args = append(args, "--sender-address-diagnostic-n", strconv.FormatInt(rows[0].N, 10))
				}
				code, out, stderr := e.run(args...)
				if !strings.Contains(stderr, "legacy paired diagnostics: enumerating primary-mailbox metadata") || strings.Contains(stderr, "no mailbox enumeration") {
					t.Fatal("legacy startup hint misstates acquisition protocol", stderr)
				}
				if code != agentio.ExitRuntime || out != "" {
					t.Fatalf("failed acquisition acquired output/authority: %d %q %s", code, out, stderr)
				}
				if mode == "selected" {
					if !strings.Contains(stderr, "selected-original observation could not be acquired; no reconciliation proposal was generated") || strings.Contains(stderr, "category=") {
						t.Fatal("selected fixed wrapper changed")
					}
				} else if !strings.Contains(stderr, "phase=enumeration; category="+tc.category+";") {
					t.Fatalf("missing acquisition category: %s", stderr)
				}
				for _, value := range []string{"untrusted-acquisition-value", "host.example.org", "sender@example.org", "original@example.org", "keep%2Btoken", path, e.db, "reconciliation_digest", "audit_digest", "--confirm-"} {
					if strings.Contains(stderr, value) {
						t.Fatal("failure leaked private values or proposal authority")
					}
				}
				if _, err = os.Lstat(path); !os.IsNotExist(err) {
					t.Fatal("failed acquisition wrote artifact", err)
				}
				if calls.folders != 1 || calls.pages != tc.pages || calls.batches != 0 || calls.inspections != 0 {
					t.Fatalf("failed command changed protocol: %+v", calls)
				}
				after, err := store.PrepareLegacyBinding(context.Background(), e.db)
				if err != nil || after.Digest() != before.Digest() {
					t.Fatal("failed acquisition changed historical inventory", err)
				}
				current, err := os.ReadFile(config.DefaultPath())
				if err != nil || !bytes.Equal(current, configuration) {
					t.Fatal("failed acquisition changed routing", err)
				}
				if _, found, err := store.ReadLegacyReconciliationReceipt(context.Background(), e.db, e.provider.Account()); err != nil || found {
					t.Fatal("failed acquisition created receipt", err)
				}
			})
		}
	}
}

func TestReconciliationReferenceCommandSuccessV9HTTP(t *testing.T) {
	e := newEnv(t)
	seedConflictHistory(t, e, []string{"original"}, "<original@example.org>", "sender@example.org")
	calls := commandAcquisitionHTTP(t, e, "success")
	code, out, stderr := e.run("account", "reconcile", "--json")
	if code != 0 {
		t.Fatalf("valid preview changed: %d %s", code, stderr)
	}
	rows := reconciliationRows(t, out)
	approval, approvalOK := rows[0]["reconciliation_digest"].(string)
	audit, auditOK := rows[0]["audit_digest"].(string)
	if rows[0]["version"] != float64(9) || !approvalOK || len(approval) != 64 || !auditOK || len(audit) != 64 || rows[1]["enumerated_state"] != nil || rows[1]["disposition"] != "verified" {
		t.Fatal("successful v9 review changed")
	}
	if calls.folders != 1 || calls.pages != 0 || calls.batches != 1 || calls.inspections != 1 {
		t.Fatal("successful protocol changed")
	}
	s, err := store.Open(e.db)
	if err != nil {
		t.Fatal("preview attributed history", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.ReadLegacyReconciliationReceipt(context.Background(), e.db, e.provider.Account()); err != nil || found {
		t.Fatal("preview committed a receipt", err)
	}
}
