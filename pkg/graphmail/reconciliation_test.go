package graphmail_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func TestLegacyReconciliationEvidence(t *testing.T) {
	for _, kind := range []string{"complete", "partial", "missing-value", "null-value", "duplicate", "cycle", "foreign-link", "metadata", "delta", "conflict", "unknown-404", "forbidden", "no-anchor", "changed", "appeared", "vanished", "sender", "header", "folder", "null-metadata"} {
		t.Run(kind, func(t *testing.T) {
			wire := func(id string) map[string]any {
				return (&serverBox{folders: map[string]string{id: "F-inbox"}, sender: "sender@example.org"}).wire(id)
			}
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("reconciliation attempted mutation")
				}
				if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
					t.Error("missing immutable preference")
				}
				write := func(x any) { _ = json.NewEncoder(w).Encode(x) }
				switch r.URL.Path {
				case "/me/messages":
					switch kind {
					case "missing-value":
						write(map[string]any{})
						return
					case "null-value":
						write(map[string]any{"value": nil})
						return
					case "no-anchor":
						write(map[string]any{"value": []any{}})
						return
					case "null-metadata":
						m := wire("anchor")
						m["subject"] = nil
						write(map[string]any{"value": []any{m}})
						return
					case "metadata":
						write(map[string]any{"value": []any{map[string]any{"id": "anchor"}}})
						return
					case "duplicate":
						write(map[string]any{"value": []any{wire("anchor"), wire("anchor")}})
						return
					case "delta":
						write(map[string]any{"value": []any{wire("anchor")}, "@odata.deltaLink": "unused"})
						return
					case "foreign-link":
						write(map[string]any{"value": []any{wire("anchor")}, "@odata.nextLink": "https://other.example.org/page"})
						return
					case "conflict":
						m := wire("other")
						m["internetMessageId"] = "<missing@example.org>"
						write(map[string]any{"value": []any{wire("anchor"), m}})
						return
					}
					write(map[string]any{"value": []any{wire("anchor")}, "@odata.nextLink": srv.URL + "/next?opaque=1"})
				case "/next":
					if r.URL.RawQuery != "opaque=1" {
						t.Error("opaque nextLink changed")
					}
					if kind == "partial" {
						http.Error(w, "unavailable", http.StatusForbidden)
						return
					}
					if kind == "cycle" {
						write(map[string]any{"value": []any{}, "@odata.nextLink": srv.URL + "/next?opaque=1"})
						return
					}
					write(map[string]any{"value": []any{}})
				case "/me/messages/anchor":
					if kind == "vanished" {
						w.WriteHeader(404)
						write(map[string]any{"error": map[string]any{"code": "ErrorItemNotFound"}})
						return
					}
					if kind == "sender" || kind == "header" || kind == "folder" {
						m := wire("anchor")
						switch kind {
						case "sender":
							m["sender"] = map[string]any{"emailAddress": map[string]any{"address": "other@example.org"}}
						case "header":
							m["internetMessageId"] = "<other@example.net>"
						case "folder":
							m["parentFolderId"] = "elsewhere"
						}
						write(m)
						return
					}

					if kind == "changed" {
						m := wire("anchor")
						m["changeKey"] = "new"
						write(m)
						return
					}
					write(wire("anchor"))
				default:
					if kind == "appeared" {
						write(wire("missing"))
						return
					}
					code := "ErrorItemNotFound"
					status := 404
					if kind == "unknown-404" {
						code = "Unknown"
					}
					if kind == "forbidden" {
						status = 403
					}
					w.WriteHeader(status)
					write(map[string]any{"error": map[string]any{"code": code}})
				}
			}))
			defer srv.Close()
			g := graph.New(func(context.Context) (string, error) { return "invented-token", nil })
			g.Base = srv.URL
			g.HTTP = srv.Client()
			p, e := graphmail.New(g, account)
			if e != nil {
				t.Fatal(e)
			}
			refs := []graphmail.LegacyReference{}
			for i, id := range []string{"anchor", "missing"} {
				refs = append(refs, graphmail.LegacyReference{N: int64(i + 1), Ref: mailbox.MessageRef{AccountID: account.ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator(id)}, InternetMessageID: "<" + id + "@example.org>", Sender: "sender@example.org", Collection: mailbox.CollectionRef{AccountID: account.ID, CollectionID: "F-inbox"}})
			}
			evidence, e := p.ReconcileLegacyReferences(context.Background(), refs)
			if kind == "complete" {
				if e != nil || len(evidence.States) != 1 || !evidence.Unavailable[2] || evidence.Pages != 2 {
					t.Fatalf("evidence: %+v %v", evidence, e)
				}
			} else if e == nil {
				t.Fatalf("incomplete/conflicting %s accepted: %+v", kind, evidence)
			} else if !reflect.DeepEqual(evidence, graphmail.LegacyEvidence{}) {
				t.Fatalf("failed %s returned partial proof: %+v", kind, evidence)
			}
		})
	}
}
