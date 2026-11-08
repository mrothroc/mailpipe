package graphmail_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

type selectedSenderObserver interface {
	ObserveLegacySenderAddress(context.Context, []graphmail.LegacyReference, int64) (graphmail.ReconciliationSenderAddressDiagnostic, error)
}

// An earlier unrelated admission failure must not masquerade as the requested
// observation. Responses are deliberately reversed relative to original order.
func TestReconciliationSelectedOriginalHTTP(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "later-target", true: "earlier-target"}[reverse], func(t *testing.T) {
			box := &serverBox{folders: map[string]string{"first": "inbox", "target": "inbox"}, sender: "sender@example.org"}
			wire := func(id string, inspection bool) map[string]any {
				m := box.wire(id)
				m["toRecipients"], m["ccRecipients"] = []any{}, []any{}
				if id == "first" && inspection {
					m["subject"] = "Different invented subject"
				}
				address := "sender@example.org"
				if id == "target" && !inspection {
					address = "enumerated@example.org"
				}
				m["from"] = map[string]any{"emailAddress": map[string]string{"address": address}}
				return m
			}
			var calls, lookups []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
					t.Error("mutable request")
				}
				switch r.URL.Path {
				case "/me/messages":
					json.NewEncoder(w).Encode(map[string]any{"value": []any{wire("first", false), wire("target", false)}})
				case "/$batch":
					var in struct {
						Requests []struct {
							ID, Method, URL string
							Headers         map[string]string
						}
					}
					if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
						t.Error(err)
						return
					}
					var responses []any
					for _, q := range in.Requests {
						if q.Method != "GET" || !strings.Contains(q.Headers["Prefer"], `IdType="ImmutableId"`) {
							t.Error("unsafe subrequest")
						}
						u, _ := url.Parse(q.URL)
						id := strings.TrimPrefix(u.Path, "/me/messages/")
						lookups = append(lookups, id)
						responses = append([]any{map[string]any{"id": q.ID, "status": 200, "body": wire(id, true)}}, responses...)
					}
					json.NewEncoder(w).Encode(map[string]any{"responses": responses})
				default:
					t.Error("extra request")
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			g := graph.New(func(context.Context) (string, error) { return "invented-token", nil })
			g.Base, g.HTTP = srv.URL, srv.Client()
			p, err := graphmail.New(g, account)
			if err != nil {
				t.Fatal(err)
			}
			ids := []string{"first", "target"}
			if reverse {
				ids[0], ids[1] = ids[1], ids[0]
			}
			var refs []graphmail.LegacyReference
			var selected int64
			for i, id := range ids {
				n := int64(i + 1)
				refs = append(refs, graphmail.LegacyReference{N: n, Ref: mailbox.MessageRef{AccountID: account.ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator(id)}, Collection: mailbox.CollectionRef{AccountID: account.ID, CollectionID: "inbox"}, InternetMessageID: "<" + id + "@example.org>", Sender: "sender@example.org"})
				if id == "target" {
					selected = n
				}
			}
			proof, err := p.ReconcileLegacyReferencesWithSenderAddressDiagnostics(context.Background(), refs)
			var failure *graphmail.ReconciliationError
			if !errors.As(err, &failure) || !reflect.DeepEqual(proof, graphmail.LegacyEvidence{}) {
				t.Fatal("normal admission granted partial proof")
			}
			if !reverse && (!strings.Contains(failure.Reason, "fields=subject") || failure.SenderAddressDiagnostic != nil) {
				t.Fatal("normal first-error semantics changed")
			}
			calls, lookups = nil, nil
			observer, ok := any(p).(selectedSenderObserver)
			if !ok {
				t.Fatal("supported selected-original observation capability missing")
			}
			d, err := observer.ObserveLegacySenderAddress(context.Background(), refs, selected)
			if err != nil {
				t.Fatal(err)
			}
			if d.N != selected || d.ImmutableID != "target" || d.AccountID != account.ID || d.EnumeratedSenderAddress != "enumerated@example.org" || d.InspectedSenderAddress != "sender@example.org" || d.EnumeratedOrigin == nil || d.InspectedOrigin == nil {
				t.Fatal("wrong selected association")
			}
			if !reflect.DeepEqual(calls, []string{"GET /me/messages", "POST /$batch"}) || !reflect.DeepEqual(lookups, ids) {
				t.Fatalf("request protocol changed: %v %v", calls, lookups)
			}
		})
	}
}
