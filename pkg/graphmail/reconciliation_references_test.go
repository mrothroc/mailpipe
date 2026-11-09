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

func referenceHTTP(t *testing.T, kind string) (*Provider, []LegacyReference, *acquisitionCalls) {
	t.Helper()
	calls := &acquisitionCalls{}
	ids := []string{"original", "second"}
	if kind == "case-sensitive-ids" {
		ids[1] = "ORIGINAL"
	}
	a := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
	refs := []LegacyReference{}
	for i, id := range ids {
		refs = append(refs, LegacyReference{N: int64(i + 1), Ref: mailbox.MessageRef{AccountID: a.ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator(id)}, InternetMessageID: "<" + id + "@example.org>", Sender: "sender@example.org", Collection: mailbox.CollectionRef{AccountID: a.ID, CollectionID: "inbox"}})
	}
	if kind == "empty-source" {
		for i := range refs {
			refs[i].Sender = ""
		}
	}
	if kind == "header" {
		refs[0].InternetMessageID = "<changed@example.org>"
	}
	if kind == "sender" {
		refs[0].Sender = "changed@example.org"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/$batch" {
			t.Error("unexpected discovery/body/action")
			w.WriteHeader(400)
			return
		}
		calls.batches++
		if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
			t.Error("immutable header missing")
		}
		var in struct {
			Requests []struct {
				ID, Method, URL string
				Headers         map[string]string
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		out := []any{}
		for i, q := range in.Requests {
			u, err := url.Parse(q.URL)
			if err != nil || i >= len(ids) || u.Path != "/me/messages/"+ids[i] || q.Method != "GET" || !strings.Contains(q.Headers["Prefer"], `IdType="ImmutableId"`) {
				t.Error("ordered exact request changed")
				w.WriteHeader(400)
				return
			}
			calls.inspections++
			status := 200
			var body any = acquisitionWire(ids[i])
			raw := json.RawMessage(nil)
			if kind == "empty-source" {
				delete(body.(map[string]any), "from")
				delete(body.(map[string]any), "sender")
				delete(body.(map[string]any), "changeKey")
			}
			// The first record remains a live anchor except for the all-missing case.
			if i == 1 || kind == "all-missing" {
				switch kind {
				case "missing", "all-missing":
					status = 404
					raw = json.RawMessage(`{"@odata.context":"ignored","error":{"code":"ErrorItemNotFound","message":"untrusted","innerError":{"ignored":true}}}`)
				case "unknown404":
					status = 404
					raw = json.RawMessage(`{"error":{"code":"Unknown"}}`)
				case "duplicate-error":
					status = 404
					raw = json.RawMessage(`{"error":{"code":"Unknown"},"error":{"code":"ErrorItemNotFound"}}`)
				case "case-error":
					status = 404
					raw = json.RawMessage(`{"Error":{"code":"Unknown"},"error":{"code":"ErrorItemNotFound"}}`)
				case "duplicate-code":
					status = 404
					raw = json.RawMessage(`{"error":{"code":"Unknown","code":"ErrorItemNotFound"}}`)
				case "case-code":
					status = 404
					raw = json.RawMessage(`{"error":{"Code":"Unknown","code":"ErrorItemNotFound"}}`)
				case "null-error":
					status = 404
					raw = json.RawMessage(`{"error":null}`)
				case "array-error":
					status = 404
					raw = json.RawMessage(`{"error":[]}`)
				case "null-code":
					status = 404
					raw = json.RawMessage(`{"error":{"code":null}}`)
				case "number-code":
					status = 404
					raw = json.RawMessage(`{"error":{"code":1}}`)
				case "missing-code":
					status = 404
					raw = json.RawMessage(`{"error":{"message":"ErrorItemNotFound"}}`)
				case "invalid-utf8-error":
					status = 404
					raw = json.RawMessage("{\"error\":{\"code\":\"ErrorItemNotFound\",\"message\":\"\xff\"}}")
				case "wrong-status":
					status = 403
					raw = json.RawMessage(`{"error":{"code":"ErrorItemNotFound"}}`)
				case "wrong-success":
					status = 201
				case "partial":
					body = map[string]any{"id": ids[i]}
				case "null-metadata":
					body.(map[string]any)["subject"] = nil
				case "type-metadata":
					body.(map[string]any)["subject"] = 42
				case "removed-null":
					body.(map[string]any)["@removed"] = nil
				case "removed-object":
					body.(map[string]any)["@removed"] = map[string]any{"reason": "deleted"}
				case "removed-scalar":
					body.(map[string]any)["@removed"] = false
				case "wrong-id":
					body.(map[string]any)["id"] = "other"
				case "mapping":
					body.(map[string]any)["from"] = map[string]any{"emailAddress": map[string]any{"name": "missing address"}}
				case "duplicate-selected":
					raw = json.RawMessage(`{"id":"second","id":"second","parentFolderId":"inbox","internetMessageId":"<second@example.org>","subject":"","toRecipients":[],"ccRecipients":[],"receivedDateTime":"","hasAttachments":false,"isRead":false,"bodyPreview":""}`)
				}
			}
			if raw != nil {
				body = raw
			}
			out = append(out, map[string]any{"id": q.ID, "status": status, "body": body})
		}
		if kind == "incomplete" {
			out = out[:1]
		}
		if kind == "malformed-json" {
			_, _ = w.Write([]byte(`{"responses":[`))
			return
		}
		// Encode invalid UTF8 directly; Marshal would replace it and hide the fixture.
		if kind == "invalid-utf8-error" {
			_, _ = w.Write([]byte("{\"responses\":[{\"id\":\"1\",\"status\":200,\"body\":"))
			b, _ := json.Marshal(acquisitionWire("original"))
			_, _ = w.Write(b)
			_, _ = w.Write([]byte("},{\"id\":\"2\",\"status\":404,\"body\":{\"error\":{\"code\":\"ErrorItemNotFound\",\"message\":\"\xff\"}}}]}"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"responses": out})
	}))
	t.Cleanup(srv.Close)
	g := graph.New(func(context.Context) (string, error) { return "invented-token", nil })
	g.Base, g.HTTP = srv.URL, srv.Client()
	p, err := New(g, a)
	if err != nil {
		t.Fatal(err)
	}
	return p, refs, calls
}
func TestOriginalReferenceHTTPContract(t *testing.T) {
	for _, kind := range []string{"found", "case-sensitive-ids", "empty-source", "missing", "all-missing", "unknown404", "duplicate-error", "case-error", "duplicate-code", "case-code", "null-error", "array-error", "null-code", "number-code", "missing-code", "invalid-utf8-error", "wrong-status", "wrong-success", "partial", "null-metadata", "type-metadata", "removed-null", "removed-object", "removed-scalar", "wrong-id", "mapping", "duplicate-selected", "incomplete", "malformed-json", "header", "sender"} {
		t.Run(kind, func(t *testing.T) {
			p, refs, c := referenceHTTP(t, kind)
			v, err := p.ReconcileOriginalReferences(context.Background(), refs)
			if kind == "found" || kind == "missing" || kind == "case-sensitive-ids" || kind == "empty-source" {
				if err != nil || v.Method != ReferenceMethod || v.Coverage != ReferenceCoverage || len(v.Outcomes) != 2 || v.Context.CompletedReferences != 2 || v.Context.InspectionBatches != 1 {
					t.Fatal("valid reference coverage rejected", err)
				}
				if kind == "missing" && (v.Outcomes[2].State != nil || v.Outcomes[2].SourceValidated || v.Outcomes[2].Kind != ReferenceNotFound) {
					t.Fatal("404 acquired state/authority")
				}
			} else if err == nil || !reflect.DeepEqual(v, ReferenceEvidence{}) {
				t.Fatal("invalid response returned authority", err)
			}
			if c.pages != 0 || c.batches != 1 || (kind != "incomplete" && c.inspections != 2) {
				t.Fatal("lookup skipped or discovery added")
			}
		})
	}
}
func TestOriginalReferencePreflightAndBudgets(t *testing.T) {
	for _, kind := range []string{"count", "bytes", "deadline", "cancel", "invalid-account", "invalid-id", "invalid-locator", "duplicate-id", "duplicate-number"} {
		t.Run(kind, func(t *testing.T) {
			p, refs, c := referenceHTTP(t, "found")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			max, bytes, timeout := ReferenceLimit, ReferenceStateLimit, ReferenceReadTimeout
			switch kind {
			case "count":
				max = 1
			case "bytes":
				bytes = 1
			case "deadline":
				timeout = 0
			case "cancel":
				cancel()
			case "invalid-account":
				refs[0].Ref.AccountID = "\xff"
			case "invalid-id":
				refs[0].Ref.MessageID = "\xff"
				refs[0].Ref.Locator = "\xff"
			case "invalid-locator":
				refs[0].Ref.Locator = "\xff"
			case "duplicate-id":
				refs[1].Ref = refs[0].Ref
			case "duplicate-number":
				refs[1].N = refs[0].N
			}
			v, err := p.reconcileOriginalReferences(ctx, refs, max, bytes, timeout)
			if err == nil || !reflect.DeepEqual(v, ReferenceEvidence{}) {
				t.Fatal("limit/ref returned authority")
			}
			want := 0
			if kind == "bytes" {
				want = 1
			}
			if c.batches != want {
				t.Fatal("preflight/limit changed requests")
			}
			if kind == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancel identity lost")
			}
		})
	}
	p, _, c := referenceHTTP(t, "found")
	v, err := p.ReconcileOriginalReferences(context.Background(), nil)
	if err != nil || len(v.Outcomes) != 0 || v.Context != (ReferenceContext{}) || v.Started.IsZero() || v.Finished.Before(v.Started) || c.batches != 0 {
		t.Fatal("empty inventory invented reads", err)
	}
}
func TestReference404ObserverAgreement(t *testing.T) {
	ref := mailbox.MessageRef{AccountID: "account", MessageID: "opaque", Locator: "opaque"}
	valid := json.RawMessage(`{"error":{"code":"ErrorItemNotFound"}}`)
	for _, kind := range []string{"valid", "wrong-id", "wrong-status", "wrong-code", "nil-error", "trailing", "scalar"} {
		t.Run(kind, func(t *testing.T) {
			r := graph.BatchResponse{ID: "1", Status: 404, Body: valid}
			var err error = &graph.APIError{Status: 404, Code: "ErrorItemNotFound"}
			switch kind {
			case "wrong-id":
				r.ID = "2"
			case "wrong-status":
				err = &graph.APIError{Status: 403, Code: "ErrorItemNotFound"}
			case "wrong-code":
				err = &graph.APIError{Status: 404, Code: "Unknown"}
			case "nil-error":
				err = nil
			case "trailing":
				r.Body = append(append(json.RawMessage{}, valid...), []byte(` {}`)...)
			case "scalar":
				r.Body = json.RawMessage(`"ErrorItemNotFound"`)
			}
			got := validateReferenceResponse(context.Background(), "1", ref, r, mailbox.State{}, err)
			if (got == nil) != (kind == "valid") {
				t.Fatal("404 handoff disagreement accepted", got)
			}
		})
	}
}

func TestOriginalReferenceCompleteCountsAndInterruptedBatches(t *testing.T) {
	a := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
	for _, kind := range []string{"complete", "canceled-after-success", "failed-after-success", "historical-conflict-after-all-reads"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tr := &interruptedEvidenceTransport{evidenceBenchmarkTransport: &evidenceBenchmarkTransport{n: 43}}
			refs := batchOriginals(43, a)
			var cause error
			switch kind {
			case "canceled-after-success":
				tr.cancel = cancel
				cause = context.Canceled
			case "failed-after-success":
				cause = errors.New("untrusted-read-secret")
				tr.fail = cause
			case "historical-conflict-after-all-reads":
				refs[0].Sender = "other@example.org"
			}
			p, err := New(tr, a)
			if err != nil {
				t.Fatal(err)
			}
			proof, err := p.ReconcileOriginalReferences(ctx, refs)
			if kind == "complete" {
				if err != nil || len(proof.Outcomes) != 43 || proof.Context.RequestedReferences != 43 || proof.Context.CompletedReferences != 43 || proof.Context.InspectionBatches != 3 || proof.Outcomes[43].Kind != ReferenceNotFound || proof.Context.StateBytes <= 0 {
					t.Fatal("complete reference inventory lost", err)
				}
			} else {
				if err == nil || !reflect.DeepEqual(proof, ReferenceEvidence{}) || (cause != nil && !errors.Is(err, cause)) {
					t.Fatal("partial or historically invalid proof survived", err)
				}
				if strings.Contains(err.Error(), "untrusted-read-secret") || len(err.Error()) > 256 {
					t.Fatal("unsafe read failure")
				}
			}
			wantBatches, wantReads := 3, 43
			if cause != nil {
				wantBatches, wantReads = 1, 20
			}
			if tr.pages != 0 || tr.batches != wantBatches || tr.inspections != wantReads || tr.calls != wantBatches {
				t.Fatal("reference request counts changed", tr)
			}
		})
	}
}

// These closed contracts force new observations/containers to receive an
// explicit validation, retention and ownership decision before admission.
func TestOriginalReferenceSchemaFieldContracts(t *testing.T) {
	for _, c := range []struct {
		typ              reflect.Type
		fields, contract string
	}{
		{reflect.TypeFor[ReferenceContext](), "RequestedReferences CompletedReferences InspectionBatches StateBytes", "bounded complete acquisition measurements; value-owned and audit-bound"},
		{reflect.TypeFor[ReferenceOutcome](), "Ref Kind State SourceValidated", "exact associated outcome and source capability; complete State and nested slices retained/owned"},
		{reflect.TypeFor[ReferenceEvidence](), "Account Method Coverage Started Finished Context Outcomes", "pinned identity/scope, complete outcomes and acquisition context; independently store-validated"},
	} {
		t.Run(c.typ.Name(), func(t *testing.T) {
			fields := strings.Fields(c.fields)
			if c.contract == "" || len(fields) != c.typ.NumField() {
				t.Fatal("schema changed; review observation validation and ownership")
			}
			for i, name := range fields {
				if c.typ.Field(i).Name != name {
					t.Fatal("unclassified/reordered observation field", c.typ.Field(i).Name)
				}
			}
		})
	}
}
