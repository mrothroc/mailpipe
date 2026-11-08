package graphmail_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestReconciliationProvenanceHTTP(t *testing.T) {
	before := func(m map[string]any) {
		m["from"] = map[string]any{"emailAddress": map[string]string{"address": "enumerated-from@example.org", "name": "Private From Name"}}
		m["sender"] = map[string]any{"emailAddress": map[string]string{"address": "enumerated-agent@example.org", "name": "Private Sender Name"}}
	}
	after := func(m map[string]any) {
		m["subject"] = "Changed invented subject"
		m["from"] = map[string]any{"emailAddress": map[string]string{"address": "sender@example.org", "name": "Current From Name"}}
		m["sender"] = map[string]any{"emailAddress": map[string]string{"address": "inspected-agent@example.org", "name": "Current Sender Name"}}
	}
	proof, err := collectDiagnosticObservations(t, true, "sender@example.org", []mailbox.Address{{Address: "recipient@example.org", Name: "Private Recipient"}}, []mailbox.Address{{Address: "recipient@example.org", Name: "Private Recipient"}}, before, after)
	var failure *graphmail.ReconciliationError
	if !errors.As(err, &failure) || failure.SenderAddressDiagnostic == nil || !reflect.DeepEqual(proof, graphmail.LegacyEvidence{}) {
		t.Fatal("strict rejection/zero evidence lost")
	}
	raw, e := json.Marshal(failure.SenderAddressDiagnostic)
	if e != nil {
		t.Fatal(e)
	}
	var d map[string]any
	if e = json.Unmarshal(raw, &d); e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"EnumeratedOrigin", "InspectedOrigin"} {
		origin, ok := d[key].(map[string]any)
		if !ok {
			t.Fatalf("missing independent raw origin %s", key)
		}
		if origin["selected_source"] != "from" || origin["mapped_sender_matches_wire"] != true || origin["recipients_match_wire"] != true {
			t.Fatal("origin did not validate mapping")
		}
	}
	for _, v := range []string{"Private From Name", "Current From Name", "Private Recipient", "recipient@example.org"} {
		if strings.Contains(string(raw), v) {
			t.Fatal("nonminimal provenance payload")
		}
	}
}

func TestReconciliationProvenanceShuffledHTTPBatch(t *testing.T) {
	box := &serverBox{folders: map[string]string{"one": "inbox", "two": "inbox"}, sender: "sender@example.org"}
	wire := func(id string, inspection bool) map[string]any {
		m := box.wire(id)
		address := "enumerated-" + id + "@example.org"
		if inspection {
			m["subject"] = "Changed invented subject"
			address = "history-" + id + "@example.org"
		}
		m["from"] = map[string]any{"emailAddress": map[string]string{"address": address}}
		m["sender"] = map[string]any{"emailAddress": map[string]string{"address": "agent-" + id + "@example.org"}}
		m["toRecipients"], m["ccRecipients"] = []any{}, []any{}
		return m
	}
	pages, batches, lookups := 0, 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
			t.Error("missing immutable preference")
		}
		switch r.URL.Path {
		case "/me/messages":
			pages++
			json.NewEncoder(w).Encode(map[string]any{"value": []any{wire("one", false), wire("two", false)}})
		case "/$batch":
			if pages != 1 {
				t.Error("inspection preceded complete enumeration")
			}
			batches++
			var in struct {
				Requests []struct {
					ID, Method, URL string
					Headers         map[string]string
				}
			}
			if e := json.NewDecoder(r.Body).Decode(&in); e != nil {
				t.Error(e)
				http.Error(w, "invalid fixture", 500)
				return
			}
			responses := []any{}
			for _, request := range in.Requests {
				if request.Method != "GET" || !strings.Contains(request.Headers["Prefer"], `IdType="ImmutableId"`) {
					t.Error("non-read-only or mutable-ID subrequest")
				}
				u, e := url.Parse(request.URL)
				if e != nil {
					t.Error(e)
					return
				}
				id := strings.TrimPrefix(u.Path, "/me/messages/")
				if id != "one" && id != "two" {
					t.Error("wrong exact lookup")
				}
				lookups++
				responses = append([]any{map[string]any{"id": request.ID, "status": 200, "body": wire(id, true)}}, responses...)
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
	p, e := graphmail.New(g, account)
	if e != nil {
		t.Fatal(e)
	}
	refs := []graphmail.LegacyReference{}
	for i, id := range []string{"two", "one"} {
		refs = append(refs, graphmail.LegacyReference{N: int64(i + 1), Ref: mailbox.MessageRef{AccountID: account.ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator(id)}, InternetMessageID: "<" + id + "@example.org>", Sender: "history-" + id + "@example.org", Collection: mailbox.CollectionRef{AccountID: account.ID, CollectionID: "inbox"}})
	}
	proof, e := p.ReconcileLegacyReferencesWithSenderAddressDiagnostics(context.Background(), refs)
	var failure *graphmail.ReconciliationError
	if !errors.As(e, &failure) || failure.SenderAddressDiagnostic == nil || !reflect.DeepEqual(proof, graphmail.LegacyEvidence{}) {
		t.Fatal("shuffled batch produced proof or lost origin", e)
	}
	d := failure.SenderAddressDiagnostic
	if d.ImmutableID != "two" || d.N != 1 || d.EnumeratedSenderAddress != "enumerated-two@example.org" || d.InspectedSenderAddress != "history-two@example.org" || d.EnumeratedOrigin.Sender.Address != "agent-two@example.org" || d.InspectedOrigin.Sender.Address != "agent-two@example.org" {
		t.Fatal("provenance associated by response position instead of exact ID")
	}
	if pages != 1 || batches != 1 || lookups != 2 {
		t.Fatal("origin capture changed requests")
	}
}

func TestReconciliationProvenanceAggregateLimitHTTP(t *testing.T) {
	// Nine bounded records exceed the retained-field budget (which charges both
	// origin and mapped spellings), while each HTTP response stays below 16 MiB.
	address := strings.Repeat("a", 1<<20) + "@example.org"
	box := &serverBox{folders: map[string]string{}, sender: address}
	refs := []graphmail.LegacyReference{}
	for i := 0; i < 9; i++ {
		id := fmt.Sprintf("invented-%d", i)
		box.folders[id] = "inbox"
		refs = append(refs, graphmail.LegacyReference{N: int64(i + 1), Ref: mailbox.MessageRef{AccountID: account.ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator(id)}, InternetMessageID: "<" + id + "@example.org>", Sender: address, Collection: mailbox.CollectionRef{AccountID: account.ID, CollectionID: "inbox"}})
	}
	for _, capture := range []bool{false, true} {
		pages, inspections, batches := 0, 0, 0
		handler := readOnlyBatchHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			makeWire := func(id string) map[string]any {
				m := box.wire(id)
				m["toRecipients"], m["ccRecipients"] = []any{}, []any{}
				return m
			}
			if r.URL.Path == "/me/messages" {
				pages++
				records := []any{}
				for _, ref := range refs {
					records = append(records, makeWire(string(ref.Ref.MessageID)))
				}
				json.NewEncoder(w).Encode(map[string]any{"value": records})
				return
			}
			id := strings.TrimPrefix(r.URL.Path, "/me/messages/")
			if _, ok := box.folders[id]; !ok {
				t.Error("extra lookup")
				http.NotFound(w, r)
				return
			}
			inspections++
			json.NewEncoder(w).Encode(makeWire(id))
		}))
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/$batch" {
				batches++
			}
			handler.ServeHTTP(w, r)
		}))
		g := graph.New(func(context.Context) (string, error) { return "invented-token", nil })
		g.Base, g.HTTP = srv.URL, srv.Client()
		p, e := graphmail.New(g, account)
		if e != nil {
			t.Fatal(e)
		}
		var proof graphmail.LegacyEvidence
		if capture {
			proof, e = p.ReconcileLegacyReferencesWithSenderAddressDiagnostics(context.Background(), refs)
		} else {
			proof, e = p.ReconcileLegacyReferences(context.Background(), refs)
		}
		srv.Close()
		if capture {
			if e == nil || !strings.Contains(e.Error(), "category=provenance-limit") || !reflect.DeepEqual(proof, graphmail.LegacyEvidence{}) || pages != 1 || batches != 0 || inspections != 0 {
				t.Fatal("origin budget did not stop incomplete pass safely")
			}
		} else if e != nil || len(proof.States) != 9 || pages != 1 || batches != 1 || inspections != 9 {
			t.Fatal("optional capture altered normal large-field protocol", e)
		}
	}
}
