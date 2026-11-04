package graphmail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

type reconciliationTransport struct {
	page  map[string]any
	calls int
	err   error
}

func (t *reconciliationTransport) Get(_ context.Context, _ string, _ url.Values, out any) error {
	t.calls++
	if t.err != nil {
		return t.err
	}
	raw, _ := json.Marshal(t.page)
	return json.Unmarshal(raw, out)
}
func (*reconciliationTransport) Do(context.Context, string, string, url.Values, string, any, any) error {
	panic("unexpected operation")
}
func (*reconciliationTransport) Batch(context.Context, []graph.BatchRequest) ([]graph.BatchResponse, error) {
	panic("unexpected operation")
}
func (*reconciliationTransport) WriteRaw(context.Context, string, io.Writer, int64) error {
	panic("unexpected body read")
}
func TestReconciliationResourceLimitsAndCancellation(t *testing.T) {
	for _, kind := range []string{"pages", "messages", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			m := map[string]any{"id": "one", "parentFolderId": "inbox", "internetMessageId": "<one@example.org>", "subject": "Invented", "toRecipients": []any{}, "ccRecipients": []any{}, "receivedDateTime": receivedForLimitTest, "hasAttachments": false, "isRead": false, "bodyPreview": ""}
			transport := &reconciliationTransport{page: map[string]any{"value": []any{m}, "@odata.nextLink": "/next"}}
			p, e := New(transport, mailbox.Account{ID: "invented-account", ProviderID: "graph"})
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			messages := 1
			if kind == "messages" {
				messages = 0
			}
			if kind == "canceled" {
				cancel()
			}
			proof, e := p.reconcileLegacyReferences(ctx, nil, messages, 1)
			if e == nil || proof.States != nil || proof.Pages != 0 {
				t.Fatalf("incomplete evidence leaked: %+v %v", proof, e)
			}
			if kind == "canceled" && transport.calls != 0 {
				t.Fatal("canceled evidence contacted provider")
			}
		})
	}
}

const receivedForLimitTest = "2026-01-01T00:00:00Z"

func TestReconciliationRemoteFailuresNeverBecomeAbsence(t *testing.T) {
	failures := []error{context.Canceled, context.DeadlineExceeded, io.ErrUnexpectedEOF, &graph.ResponseLimitError{Limit: 16 << 20}}
	for _, status := range []int{401, 403, 404, 410, 429, 500} {
		failures = append(failures, &graph.APIError{Status: status, Code: "invented-error"})
	}
	for _, failure := range failures {
		transport := &reconciliationTransport{err: failure}
		p, e := New(transport, mailbox.Account{ID: "invented-account", ProviderID: "graph"})
		if e != nil {
			t.Fatal(e)
		}
		proof, e := p.ReconcileLegacyReferences(context.Background(), nil)
		if e == nil || !errors.Is(e, failure) || proof.States != nil || proof.Unavailable != nil || proof.Pages != 0 {
			t.Fatalf("remote failure became evidence: %+v %v", proof, e)
		}
	}
}

func TestReconciliationRejectsInvalidOriginalReferencesBeforeRead(t *testing.T) {
	for _, kind := range []string{"number", "duplicate-number", "duplicate-id", "empty-id", "locator", "account", "collection"} {
		t.Run(kind, func(t *testing.T) {
			a := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
			ref := LegacyReference{N: 1, Ref: mailbox.MessageRef{AccountID: a.ID, MessageID: "original", Locator: "original"}, Collection: mailbox.CollectionRef{AccountID: a.ID, CollectionID: "inbox"}}
			refs := []LegacyReference{ref}
			switch kind {
			case "number":
				refs[0].N = 0
			case "empty-id":
				refs[0].Ref.MessageID = ""
			case "locator":
				refs[0].Ref.Locator = "other"
			case "account":
				refs[0].Ref.AccountID = "other-account"
			case "collection":
				refs[0].Collection.AccountID = "other-account"
			case "duplicate-number":
				other := ref
				other.Ref.MessageID = "other"
				other.Ref.Locator = "other"
				refs = append(refs, other)
			case "duplicate-id":
				other := ref
				other.N = 2
				refs = append(refs, other)
			}
			transport := &reconciliationTransport{}
			p, e := New(transport, a)
			if e != nil {
				t.Fatal(e)
			}
			proof, e := p.ReconcileLegacyReferences(context.Background(), refs)
			if e == nil || transport.calls != 0 || proof.States != nil {
				t.Fatalf("invalid original reference read or accepted: %+v %v", proof, e)
			}
		})
	}
}
