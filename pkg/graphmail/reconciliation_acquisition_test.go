package graphmail

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
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

const acquisitionHostile = "untrusted-acquisition-value\n\x1b[31m https://host.example.org/private"

func acquisitionWire(id string) map[string]any {
	return map[string]any{"id": id, "parentFolderId": "inbox", "internetMessageId": "<" + id + "@example.org>", "subject": "Invented subject", "from": map[string]any{"emailAddress": map[string]string{"address": "sender@example.org"}}, "toRecipients": []any{}, "ccRecipients": []any{}, "receivedDateTime": "2026-01-01T00:00:00Z", "hasAttachments": false, "isRead": false, "bodyPreview": "", "changeKey": "revision"}
}

type acquisitionCalls struct{ pages, batches, inspections int }

// Real HTTP with ordered read-only batch subrequests. No body or mutation route
// exists, and an acquisition failure must stop before the first inspection.
func acquisitionHTTP(t *testing.T, kind string) (*Provider, []LegacyReference, *acquisitionCalls) {
	t.Helper()
	first, second := acquisitionWire("original"), acquisitionWire("original")
	pages := [][]map[string]any{{first, second}}
	ids := []string{"original"}
	switch kind {
	case "complete":
		pages = [][]map[string]any{{first}, {acquisitionWire("second")}}
		ids = append(ids, "second")
	case "case-sensitive":
		pages = [][]map[string]any{{first, acquisitionWire("ORIGINAL")}, {}}
		ids = append(ids, "ORIGINAL")
	case "unavailable":
		pages = [][]map[string]any{{first}, {}}
		ids = append(ids, "missing")
	case "across":
		pages = [][]map[string]any{{first}, {second}}
	case "hostile-duplicate":
		first["id"], second["id"] = acquisitionHostile, acquisitionHostile
		first["subject"], second["subject"] = acquisitionHostile, acquisitionHostile
	case "duplicate-invalid-state":
		second["parentFolderId"] = ""
	case "duplicate-invalid-time":
		second["receivedDateTime"] = "not-a-time"
	case "duplicate-null-metadata":
		second["subject"] = nil
	case "invalid-id":
		first["id"] = ""
		pages = [][]map[string]any{{first}}
	case "repeat-removed":
		second["@removed"] = map[string]any{"reason": acquisitionHostile}
	case "across-null":
		second["@removed"] = nil
		pages = [][]map[string]any{{first}, {second}}
	case "null", "object", "object-reason-shape", "array", "string", "boolean", "number", "missing-metadata-removed", "typed-metadata-removed", "malformed-json":
		marker := map[string]any{"reason": acquisitionHostile}
		var value any = marker
		switch kind {
		case "null":
			value = nil
		case "object-reason-shape":
			value = map[string]any{"reason": []any{false, acquisitionHostile}}
		case "array":
			value = []any{acquisitionHostile}
		case "string":
			value = acquisitionHostile
		case "boolean":
			value = false
		case "number":
			value = 1
		case "missing-metadata-removed":
			delete(first, "subject")
		case "typed-metadata-removed":
			first["subject"] = 42
		}
		first["@removed"] = value
		pages = [][]map[string]any{{first}}
	}
	calls := &acquisitionCalls{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
			t.Error("missing immutable HTTP preference")
		}
		if r.Method == http.MethodGet && (r.URL.Path == "/me/messages" || r.URL.Path == "/continue") {
			index := calls.pages
			calls.pages++
			if index == 0 {
				if r.URL.Path != "/me/messages" || r.URL.Query().Get("$top") != "200" || r.URL.Query().Get("$select") != selectFields {
					t.Error("initial acquisition request changed")
				}
			} else if r.URL.Path != "/continue" || r.URL.RawQuery != "opaque=keep%2Btoken" {
				t.Error("opaque continuation changed")
			}
			if kind == "malformed-json" {
				_, _ = w.Write([]byte(`{"value":[{"@removed": }]}`))
				return
			}
			if index >= len(pages) {
				t.Error("unexpected fresh acquisition pass")
				w.WriteHeader(400)
				return
			}
			page := map[string]any{"value": pages[index]}
			if index+1 < len(pages) {
				page["@odata.nextLink"] = srv.URL + "/continue?opaque=keep%2Btoken"
			}
			_ = json.NewEncoder(w).Encode(page)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/$batch" {
			t.Error("unexpected content or mutation request")
			w.WriteHeader(400)
			return
		}
		calls.batches++
		if calls.pages != len(pages) {
			t.Error("inspection before complete enumeration")
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
		if len(in.Requests) != len(ids) {
			t.Error("not every original was inspected")
		}
		responses := []any{}
		for i, req := range in.Requests {
			u, err := url.Parse(req.URL)
			if err != nil || i >= len(ids) || u.Path != "/me/messages/"+ids[i] || u.Query().Get("$select") != selectFields || req.Method != "GET" || len(req.Body) != 0 || !strings.Contains(req.Headers["Prefer"], `IdType="ImmutableId"`) {
				t.Error("ordered exact inspection request changed")
				w.WriteHeader(400)
				return
			}
			calls.inspections++
			status, body := 200, any(acquisitionWire(ids[i]))
			if ids[i] == "missing" {
				status, body = 404, map[string]any{"error": map[string]any{"code": "ErrorItemNotFound"}}
			}
			// Wire order differs; Graph must preserve request association.
			responses = append([]any{map[string]any{"id": req.ID, "status": status, "body": body}}, responses...)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"responses": responses})
	}))
	t.Cleanup(srv.Close)
	g := graph.New(func(context.Context) (string, error) { return "invented-token", nil })
	g.Base, g.HTTP = srv.URL, srv.Client()
	a := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
	p, err := New(g, a)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]LegacyReference, len(ids))
	for i, id := range ids {
		refs[i] = LegacyReference{N: int64(i + 1), Ref: p.ref(id), InternetMessageID: "<" + id + "@example.org>", Sender: "sender@example.org", Collection: mailbox.CollectionRef{AccountID: a.ID, CollectionID: "inbox"}}
	}
	return p, refs, calls
}

func TestReconciliationAcquisitionCategoriesHTTP(t *testing.T) {
	for _, tc := range []struct {
		kind, category string
		pages          int
	}{
		{"within", "duplicate-id-within-page", 1}, {"across", "duplicate-id-across-pages", 2},
		{"null", "removed-marker-null", 1}, {"object", "removed-marker-object", 1}, {"object-reason-shape", "removed-marker-object", 1},
		{"array", "removed-marker-invalid-shape", 1}, {"string", "removed-marker-invalid-shape", 1}, {"boolean", "removed-marker-invalid-shape", 1}, {"number", "removed-marker-invalid-shape", 1},
		{"repeat-removed", "removed-marker-object", 1}, {"across-null", "removed-marker-null", 2}, {"hostile-duplicate", "duplicate-id-within-page", 1},
		{"missing-metadata-removed", "incomplete-metadata", 1}, {"typed-metadata-removed", "malformed-response", 1}, {"duplicate-null-metadata", "incomplete-metadata", 1},
		{"duplicate-invalid-state", "duplicate-id-within-page", 1}, {"duplicate-invalid-time", "duplicate-id-within-page", 1}, {"invalid-id", "invalid-message-state", 1}, {"malformed-json", "malformed-response", 1},
	} {
		for _, mode := range []string{"default", "capture", "selected"} {
			t.Run(tc.kind+"/"+mode, func(t *testing.T) {
				p, refs, calls := acquisitionHTTP(t, tc.kind)
				var err error
				if mode == "selected" {
					d, e := p.ObserveLegacySenderAddress(context.Background(), refs, 1)
					err = e
					if !reflect.DeepEqual(d, ReconciliationSenderAddressDiagnostic{}) {
						t.Fatal("failed acquisition returned scalar data")
					}
				} else {
					var proof LegacyEvidence
					if mode == "capture" {
						proof, err = p.ReconcileLegacyReferencesWithSenderAddressDiagnostics(context.Background(), refs)
					} else {
						proof, err = p.ReconcileLegacyReferences(context.Background(), refs)
					}
					if !reflect.DeepEqual(proof, LegacyEvidence{}) {
						t.Fatal("failed acquisition returned proof")
					}
				}
				var failure *ReconciliationError
				if !errors.As(err, &failure) || failure.Structural || failure.Cause == nil || errors.Unwrap(failure) != failure.Cause || failure.SenderAddressDiagnostic != nil || failure.Conflict != nil || !strings.Contains(failure.Error(), "phase=enumeration; category="+tc.category+";") {
					t.Fatalf("wrong acquisition category/error boundary: %v", err)
				}
				if len(failure.Error()) > 256 {
					t.Fatal("unbounded diagnostic")
				}
				for _, value := range []string{acquisitionHostile, "untrusted-acquisition-value", "sender@example.org", "host.example.org", "original@example.org", "keep%2Btoken"} {
					if strings.Contains(failure.Error(), value) {
						t.Fatal("diagnostic exposed untrusted values")
					}
				}
				if calls.pages != tc.pages || calls.batches != 0 || calls.inspections != 0 {
					t.Fatalf("failure changed requests: %+v", calls)
				}
			})
		}
	}
}

func TestReconciliationAcquisitionSuccessHTTP(t *testing.T) {
	for _, kind := range []string{"complete", "case-sensitive", "unavailable"} {
		for _, mode := range []string{"default", "capture", "selected"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				p, refs, calls := acquisitionHTTP(t, kind)
				if mode == "selected" {
					d, err := p.ObserveLegacySenderAddress(context.Background(), refs, 1)
					if err != nil || !d.RequestedOriginal || d.N != 1 || d.ImmutableID != "original" {
						t.Fatal("successful scalar observation changed", err)
					}
				} else {
					proof, err := p.collectLegacyReferences(context.Background(), refs, legacyMessageLimit, legacyPageLimit, mode == "capture")
					want := 2
					if kind == "unavailable" {
						want = 1
					}
					if err != nil || len(proof.States) != want || len(proof.EnumeratedStates) != 0 || len(proof.SenderSourceIntegrity) != 0 || proof.Pages != 2 || proof.Observed != want || (kind == "unavailable" && !proof.Unavailable[2]) {
						t.Fatal("successful admission changed", err)
					}
				}
				if calls.pages != 2 || calls.batches != 1 || calls.inspections != 2 {
					t.Fatalf("all-original protocol changed: %+v", calls)
				}
			})
		}
	}
}

func TestReconciliationAcquisitionLimitPrecedenceHTTP(t *testing.T) {
	for _, kind := range []string{"message-before-duplicate", "metadata-before-message", "page-before-read", "page-before-repeat", "canceled-before-read"} {
		t.Run(kind, func(t *testing.T) {
			fixture, messages, pages, wantReads := "within", legacyMessageLimit, legacyPageLimit, 1
			if kind == "metadata-before-message" {
				fixture, messages = "missing-metadata-removed", 0
			}
			if kind == "message-before-duplicate" {
				messages = 1
			}
			if kind == "page-before-read" {
				pages, wantReads = 0, 0
			}
			if kind == "page-before-repeat" {
				fixture, pages = "across", 1
			}
			if kind == "canceled-before-read" {
				wantReads = 0
			}
			p, refs, calls := acquisitionHTTP(t, fixture)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "canceled-before-read" {
				cancel()
			}
			proof, err := p.reconcileLegacyReferences(ctx, refs, messages, pages)
			if err == nil || !reflect.DeepEqual(proof, LegacyEvidence{}) {
				t.Fatal("limit/cancel returned proof")
			}
			if kind == "metadata-before-message" && !strings.Contains(err.Error(), "category=incomplete-metadata;") {
				t.Fatal("metadata precedence changed", err)
			}
			if strings.Contains(err.Error(), "category=duplicate-") || strings.Contains(err.Error(), "category=removed-") {
				t.Fatal("later guard replaced prior failure")
			}
			if kind == "canceled-before-read" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancel identity lost", err)
			}
			if calls.pages != wantReads || calls.batches != 0 {
				t.Fatal("limit/cancel changed requests")
			}
		})
	}
}
