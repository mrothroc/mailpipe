package graphmail_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// readOnlyBatchHandler exercises the real Graph client's batch envelope while
// reusing a GET-only synthetic mailbox handler for each exact inspection.
func readOnlyBatchHandler(t *testing.T, get http.Handler) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			get.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/$batch" {
			t.Error("unexpected mutation envelope")
			w.WriteHeader(400)
			return
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
			return
		}
		out := []any{}
		for _, req := range in.Requests {
			if req.Method != http.MethodGet || len(req.Body) != 0 || !strings.Contains(req.Headers["Prefer"], `IdType="ImmutableId"`) {
				t.Error("batch inspection must be an immutable GET without a body")
				w.WriteHeader(400)
				return
			}
			sub := httptest.NewRequest(req.Method, req.URL, nil)
			for key, value := range req.Headers {
				sub.Header.Set(key, value)
			}
			rec := httptest.NewRecorder()
			get.ServeHTTP(rec, sub)
			out = append(out, map[string]any{"id": req.ID, "status": rec.Code, "body": json.RawMessage(rec.Body.Bytes())})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"responses": out})
	})
}

func TestGraphInspectionRejectsNullSelectedMetadata(t *testing.T) {
	for _, mode := range []string{"single", "batch"} {
		for _, field := range []string{"id", "parentFolderId", "internetMessageId", "subject", "toRecipients", "ccRecipients", "receivedDateTime", "hasAttachments", "isRead", "bodyPreview", "valid-empty"} {
			t.Run(mode+"/"+field, func(t *testing.T) {
				box := &serverBox{folders: map[string]string{"one": "inbox"}, sender: "sender@example.org"}
				srv := httptest.NewServer(readOnlyBatchHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					m := box.wire("one")
					if field == "valid-empty" {
						for _, k := range []string{"subject", "internetMessageId", "receivedDateTime", "bodyPreview"} {
							m[k] = ""
						}
						m["toRecipients"], m["ccRecipients"] = []any{}, []any{}
					} else {
						m[field] = nil
					}
					_ = json.NewEncoder(w).Encode(m)
				})))
				defer srv.Close()
				g := graph.New(func(context.Context) (string, error) { return "invented-token", nil })
				g.Base, g.HTTP = srv.URL, srv.Client()
				p, err := graphmail.New(g, account)
				if err != nil {
					t.Fatal(err)
				}
				ref := mailbox.MessageRef{AccountID: account.ID, MessageID: "one", Locator: "one"}
				if mode == "single" {
					_, err = p.Inspect(context.Background(), ref)
				} else {
					_, failures, aggregate := p.InspectBatch(context.Background(), []mailbox.MessageRef{ref})
					err = aggregate
					if err == nil {
						err = failures[0]
					}
				}
				if (field == "valid-empty") != (err == nil) {
					t.Fatalf("selected metadata %s: %v", field, err)
				}
			})
		}
	}
}

func TestReconciliationExactOriginals(t *testing.T) {
	for _, kind := range []string{"shared-header", "moved-collection", "missing-with-counterpart"} {
		t.Run(kind, func(t *testing.T) {
			ids := []string{"first", "second"}
			box := &serverBox{folders: map[string]string{"first": "old", "second": "old", "counterpart": "new"}, sender: "sender@example.org"}
			if kind == "moved-collection" {
				box.folders["first"] = "new"
			}
			wire := func(id string) map[string]any {
				m := box.wire(id)
				m["internetMessageId"] = "<shared@example.org>"
				if kind == "moved-collection" {
					m["internetMessageId"] = "<" + id + "@example.org>"
				}
				return m
			}
			inspected := map[string]int{}
			var inspectedMu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
					t.Error("missing immutable preference")
				}
				if r.Method == http.MethodGet && r.URL.Path == "/me/messages" {
					values := []any{wire("first"), wire("second")}
					if kind == "missing-with-counterpart" {
						values = []any{wire("first"), wire("counterpart")}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"value": values})
					return
				}
				lookup := func(path string) (int, any) {
					u, err := url.Parse(path)
					if err != nil || !strings.HasPrefix(u.Path, "/me/messages/") {
						t.Error("unexpected inspection path")
						return 400, nil
					}
					id := strings.TrimPrefix(u.Path, "/me/messages/")
					inspectedMu.Lock()
					inspected[id]++
					inspectedMu.Unlock()
					if id == "second" && kind == "missing-with-counterpart" {
						return 404, map[string]any{"error": map[string]any{"code": "ErrorItemNotFound"}}
					}
					return 200, wire(id)
				}
				if r.Method == http.MethodGet {
					status, body := lookup(r.URL.String())
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(body)
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/$batch" {
					t.Error("unexpected mutation")
					w.WriteHeader(400)
					return
				}
				var batch struct {
					Requests []struct {
						ID, Method, URL string
						Headers         map[string]string
						Body            json.RawMessage
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
					t.Error(err)
				}
				responses := []any{}
				for _, req := range batch.Requests {
					if req.Method != http.MethodGet || len(req.Body) != 0 || !strings.Contains(req.Headers["Prefer"], `IdType="ImmutableId"`) {
						t.Error("inspection must be an immutable GET without a body")
					}
					status, body := lookup(req.URL)
					responses = append(responses, map[string]any{"id": req.ID, "status": status, "body": body})
				}
				for i, j := 0, len(responses)-1; i < j; i, j = i+1, j-1 {
					responses[i], responses[j] = responses[j], responses[i]
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"responses": responses})
			}))
			defer srv.Close()
			g := graph.New(func(context.Context) (string, error) { return "invented-token", nil })
			g.Base, g.HTTP = srv.URL, srv.Client()
			p, err := graphmail.New(g, account)
			if err != nil {
				t.Fatal(err)
			}
			var refs []graphmail.LegacyReference
			for i, id := range ids {
				header := "<shared@example.org>"
				if kind == "moved-collection" {
					header = "<" + id + "@example.org>"
				}
				refs = append(refs, graphmail.LegacyReference{N: int64(i + 1), Ref: mailbox.MessageRef{AccountID: account.ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator(id)}, InternetMessageID: header, Sender: "sender@example.org", Collection: mailbox.CollectionRef{AccountID: account.ID, CollectionID: "old"}})
			}
			evidence, err := p.ReconcileLegacyReferences(context.Background(), refs)
			if err != nil {
				t.Fatalf("independent exact originals blocked: %v", err)
			}
			inspectedMu.Lock()
			defer inspectedMu.Unlock()
			for _, id := range ids {
				if inspected[id] != 1 {
					t.Fatalf("original %s inspected %d times", id, inspected[id])
				}
			}
			if inspected["counterpart"] != 0 || evidence.States[1].Ref != refs[0].Ref {
				t.Fatal("counterpart replaced original")
			}
			if kind == "missing-with-counterpart" {
				if !evidence.Unavailable[2] || len(evidence.States) != 1 {
					t.Fatal("missing original lost its own disposition")
				}
			} else if len(evidence.States) != 2 {
				t.Fatal("distinct originals merged")
			}
			if kind == "moved-collection" && evidence.States[1].Memberships[0].Collection.CollectionID != "new" {
				t.Fatal("current membership was replaced by old source")
			}
		})
	}
}

func TestReconciliationBatchFailuresNeverBecomeAbsence(t *testing.T) {
	for _, kind := range []string{"missing-response", "duplicate-response", "malformed-body", "wrong-id", "null-field", "observed-404", "401", "403", "410", "429", "500", "unknown-404"} {
		t.Run(kind, func(t *testing.T) {
			box := &serverBox{folders: map[string]string{"anchor": "inbox", "counterpart": "inbox"}, sender: "sender@example.org"}
			counterpart := box.wire("counterpart")
			counterpart["internetMessageId"] = "<missing@example.org>"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/me/messages" {
					_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{box.wire("anchor"), counterpart}})
					return
				}
				if r.Method != "POST" || r.URL.Path != "/$batch" {
					t.Error("unexpected mutation or direct inspection")
					w.WriteHeader(400)
					return
				}
				var in struct {
					Requests []struct {
						ID, Method, URL string
						Body            json.RawMessage
						Headers         map[string]string
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					t.Error(err)
					return
				}
				out := []any{}
				for _, req := range in.Requests {
					if req.Method != "GET" || len(req.Body) != 0 || !strings.Contains(req.Headers["Prefer"], `IdType="ImmutableId"`) {
						t.Error("invalid read-only subrequest")
					}
					status := 200
					var body any = box.wire("anchor")
					if strings.Contains(req.URL, "/missing?") {
						status = 404
						code := "ErrorItemNotFound"
						if kind == "unknown-404" {
							code = "Unknown"
						}
						if _, err := fmt.Sscanf(kind, "%d", &status); err != nil {
							status = 404
						}
						body = map[string]any{"error": map[string]any{"code": code}}
					} else {
						switch kind {
						case "malformed-body":
							body = []any{"invalid"}
						case "wrong-id":
							body = counterpart
						case "null-field":
							m := box.wire("anchor")
							m["bodyPreview"] = nil
							body = m
						case "observed-404":
							status = 404
							body = map[string]any{"error": map[string]any{"code": "ErrorItemNotFound"}}
						}
					}
					entry := map[string]any{"id": req.ID, "status": status, "body": body}
					if kind == "missing-response" && len(out) == 0 {
						continue
					}
					out = append(out, entry)
					if kind == "duplicate-response" {
						out = append(out, entry)
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"responses": out})
			}))
			defer srv.Close()
			g := graph.New(func(context.Context) (string, error) { return "invented-token", nil })
			g.Base, g.HTTP, g.Sleep = srv.URL, srv.Client(), func(time.Duration) {}
			p, err := graphmail.New(g, account)
			if err != nil {
				t.Fatal(err)
			}
			refs := []graphmail.LegacyReference{}
			for i, id := range []string{"anchor", "missing"} {
				refs = append(refs, graphmail.LegacyReference{N: int64(i + 1), Ref: mailbox.MessageRef{AccountID: account.ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator(id)}, InternetMessageID: "<" + id + "@example.org>", Sender: "sender@example.org", Collection: mailbox.CollectionRef{AccountID: account.ID, CollectionID: "inbox"}})
			}
			proof, err := p.ReconcileLegacyReferences(context.Background(), refs)
			if err == nil || !reflect.DeepEqual(proof, graphmail.LegacyEvidence{}) {
				t.Fatalf("counterpart turned failed batch into evidence: %+v %v", proof, err)
			}
		})
	}
}
