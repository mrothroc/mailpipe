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

func casingRecipients(values []mailbox.Address) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = map[string]any{"emailAddress": map[string]string{"address": v.Address, "name": v.Name}}
	}
	return out
}

// Use real immutable HTTP enumeration and batch inspection, not an alternate
// collector. The two reads intentionally retain different address spellings.
func collectCasing(t *testing.T, before, after []mailbox.Address, change func(map[string]any)) (graphmail.LegacyEvidence, error) {
	t.Helper()
	return collectSenderCasing(t, "sender@example.org", before, after, change)
}

func collectSenderCasing(t *testing.T, sender string, before, after []mailbox.Address, change func(map[string]any)) (graphmail.LegacyEvidence, error) {
	t.Helper()
	return collectObservations(t, sender, before, after, nil, change)
}

func collectObservations(t *testing.T, sender string, before, after []mailbox.Address, listedChange, change func(map[string]any)) (graphmail.LegacyEvidence, error) {
	t.Helper()
	return collectDiagnosticObservations(t, false, sender, before, after, listedChange, change)
}

func collectDiagnosticObservations(t *testing.T, capture bool, sender string, before, after []mailbox.Address, listedChange, change func(map[string]any)) (graphmail.LegacyEvidence, error) {
	t.Helper()
	box := &serverBox{folders: map[string]string{"one": "inbox"}, sender: sender}
	pages, inspections := 0, 0
	srv := httptest.NewServer(readOnlyBatchHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
			t.Error("missing immutable preference")
		}
		m := box.wire("one")
		m["ccRecipients"] = []any{}
		if r.URL.Path == "/me/messages" {
			pages++
			if listedChange != nil {
				listedChange(m)
			}
			m["toRecipients"] = casingRecipients(before)
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{m}})
		} else {
			if pages != 1 || r.URL.Path != "/me/messages/one" {
				t.Error("inspection did not follow complete enumeration")
			}
			inspections++
			m["toRecipients"] = casingRecipients(after)
			if change != nil {
				change(m)
			}
			_ = json.NewEncoder(w).Encode(m)
		}
	})))
	defer srv.Close()
	g := graph.New(func(context.Context) (string, error) { return "invented-token", nil })
	g.Base, g.HTTP = srv.URL, srv.Client()
	p, err := graphmail.New(g, account)
	if err != nil {
		t.Fatal(err)
	}
	refs := []graphmail.LegacyReference{{N: 1, Ref: mailbox.MessageRef{AccountID: account.ID, MessageID: "one", Locator: "one"}, InternetMessageID: "<one@example.org>", Sender: sender, Collection: mailbox.CollectionRef{AccountID: account.ID, CollectionID: "inbox"}}}
	var proof graphmail.LegacyEvidence
	if capture {
		if c, ok := any(p).(interface {
			ReconcileLegacyReferencesWithSenderAddressDiagnostics(context.Context, []graphmail.LegacyReference) (graphmail.LegacyEvidence, error)
		}); ok {
			proof, err = c.ReconcileLegacyReferencesWithSenderAddressDiagnostics(context.Background(), refs)
		} else {
			proof, err = p.ReconcileLegacyReferences(context.Background(), refs)
		}
	} else {
		proof, err = p.ReconcileLegacyReferences(context.Background(), refs)
	}
	if pages != 1 || inspections != 1 {
		t.Fatalf("changed read count: pages=%d inspections=%d", pages, inspections)
	}
	return proof, err
}

func TestReconciliationRecipientCasing(t *testing.T) {
	before := []mailbox.Address{{Address: "Reader@Example.org", Name: "Reader"}, {Address: "Reader@Example.org", Name: "Reader"}, {Address: "K@example.org", Name: "Other"}}
	after := []mailbox.Address{{Address: "reader@example.org", Name: "Reader"}, {Address: "READER@example.org", Name: "Reader"}, {Address: "k@example.org", Name: "Other"}}
	proof, err := collectCasing(t, before, after, nil)
	if err != nil {
		t.Fatalf("same-ID casing-only reads blocked: %v", err)
	}
	raw, _ := json.Marshal(proof)
	var evidence struct{ EnumeratedStates map[int64]mailbox.State }
	if err = json.Unmarshal(raw, &evidence); err != nil || !reflect.DeepEqual(evidence.EnumeratedStates[1].Metadata.Recipients, before) || !reflect.DeepEqual(proof.States[1].Metadata.Recipients, after) {
		t.Fatal("both exact ordered observations must survive without deduplication")
	}
}

func TestReconciliationCasingDoesNotMaskOtherChanges(t *testing.T) {
	base := []mailbox.Address{{Address: "First@Example.org", Name: "First"}, {Address: "second@example.org", Name: "Second"}}
	for _, kind := range []string{"name-and-preview", "order", "cardinality", "multiplicity", "trim", "plus", "dot", "different", "full-fold", "normalization", "subject", "preview", "received", "sender-content", "header", "read", "attachments", "membership", "revision", "id"} {
		t.Run(kind, func(t *testing.T) {
			after := append([]mailbox.Address(nil), base...)
			after[0].Address = "first@example.org"
			change := func(m map[string]any) {}
			switch kind {
			case "name-and-preview":
				after[0].Name = "FIRST"
				change = func(m map[string]any) { m["bodyPreview"] = "Changed" }
			case "order":
				after[0], after[1] = after[1], after[0]
			case "cardinality":
				after = after[:1]
			case "multiplicity":
				after[1] = after[0]
			case "trim":
				after[0].Address = " first@example.org "
			case "plus":
				after[0].Address = "first+tag@example.org"
			case "dot":
				after[0].Address = "fi.rst@example.org"
			case "different":
				after[0].Address = "third@example.org"
			case "full-fold":
				baseCopy := append([]mailbox.Address(nil), base...)
				baseCopy[0].Address = "ß@example.org"
				after[0].Address = "ss@example.org"
				proof, err := collectCasing(t, baseCopy, after, nil)
				if err == nil || !reflect.DeepEqual(proof, graphmail.LegacyEvidence{}) {
					t.Fatal("full fold accepted")
				}
				return
			case "normalization":
				after[0].Address = "fírst@example.org"
			case "subject":
				change = func(m map[string]any) { m["subject"] = "Changed" }
			case "preview":
				change = func(m map[string]any) { m["bodyPreview"] = "Changed" }
			case "received":
				change = func(m map[string]any) { m["receivedDateTime"] = "2026-01-01T00:00:00Z" }
			case "sender-content":
				change = func(m map[string]any) {
					m["sender"] = map[string]any{"emailAddress": map[string]string{"address": "other@example.org"}}
				}
			case "header":
				change = func(m map[string]any) { m["internetMessageId"] = "<changed@example.org>" }
			case "read":
				change = func(m map[string]any) { m["isRead"] = true }
			case "attachments":
				change = func(m map[string]any) { m["hasAttachments"] = true }
			case "membership":
				change = func(m map[string]any) { m["parentFolderId"] = "other" }
			case "revision":
				change = func(m map[string]any) { m["changeKey"] = "other" }
			case "id":
				change = func(m map[string]any) { m["id"] = "other" }
			}
			otherChange := change
			change = func(m map[string]any) {
				otherChange(m)
				field := "from"
				if m[field] == nil {
					field = "sender"
				}
				switch address := m[field].(map[string]any)["emailAddress"].(type) {
				case map[string]any:
					address["name"] = "Current Display"
				case map[string]string:
					address["name"] = "Current Display"
				default:
					t.Fatal("invalid synthetic address shape")
				}
			}
			proof, err := collectCasing(t, base, after, change)
			if err == nil || !reflect.DeepEqual(proof, graphmail.LegacyEvidence{}) {
				t.Fatal("noncasing change returned evidence")
			}
		})
	}
}
