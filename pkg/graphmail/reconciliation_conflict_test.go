package graphmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mrothroc/mailpipe/pkg/graph"
	"net/url"
	"reflect"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func conflictWire(id, header, sender, collection string) map[string]any {
	w := (&evidenceBenchmarkTransport{}).wire(0)
	w["id"], w["internetMessageId"], w["parentFolderId"] = id, header, collection
	w["sender"] = map[string]any{"emailAddress": map[string]any{"address": sender}}
	w["subject"], w["bodyPreview"] = "excluded-subject", "excluded-preview"
	return w
}

func conflictOriginal(n int64, id, header, sender, collection string, a mailbox.Account) LegacyReference {
	return LegacyReference{N: n, Ref: mailbox.MessageRef{AccountID: a.ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator(id)},
		InternetMessageID: header, Sender: sender, Collection: mailbox.CollectionRef{AccountID: a.ID, CollectionID: mailbox.CollectionID(collection)}}
}

// Batching retains every exact inspection while reducing envelope round trips.
func TestReconciliationSuccessBatchReadCounts(t *testing.T) {
	a := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
	tr := &evidenceBenchmarkTransport{n: 4}
	p, err := New(tr, a)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]LegacyReference, 4)
	for i := range refs {
		refs[i] = conflictOriginal(int64(i+1), fmt.Sprintf("invented-%d", i), fmt.Sprintf("<invented-%d@example.org>", i), "sender@example.org", "F-inbox", a)
	}
	proof, err := p.ReconcileLegacyReferences(context.Background(), refs)
	if err != nil || len(proof.States) != 3 || !proof.Unavailable[4] || tr.calls != 2 || tr.pages != 1 || tr.batches != 1 || tr.inspections != 4 {
		t.Fatalf("success behavior changed: %+v %v calls=%+v", proof, err, tr)
	}
}

type conflictInspectionTransport struct {
	*reconciliationTransport
	message map[string]any
}

func (t *conflictInspectionTransport) Get(_ context.Context, path string, _ url.Values, out any) error {
	t.calls++
	var value any = t.message
	if path == "/me/messages" {
		value = map[string]any{"value": []any{t.message}}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func TestReconciliationExactOriginalMetadataPreconditions(t *testing.T) {
	for _, kind := range []string{"same-id", "empty-header", "different-header", "different-sender"} {
		t.Run(kind, func(t *testing.T) {
			a := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
			ref := conflictOriginal(1, "observed-id", "<shared@example.org>", "sender@example.org", "inbox", a)
			switch kind {
			case "empty-header":
				ref.InternetMessageID = ""
			case "different-header":
				ref.InternetMessageID = "<other@example.org>"
			case "different-sender":
				ref.Sender = "other@example.org"
			}
			tr := &conflictInspectionTransport{reconciliationTransport: &reconciliationTransport{}, message: conflictWire("observed-id", "<shared@example.org>", "sender@example.org", "inbox")}
			p, err := New(tr, a)
			if err != nil {
				t.Fatal(err)
			}
			proof, err := p.ReconcileLegacyReferences(context.Background(), []LegacyReference{ref})
			var failure *ReconciliationError
			if errors.As(err, &failure) && failure.Conflict != nil || tr.calls != 2 {
				t.Fatalf("collector produced legacy payload or skipped exact inspection: %v calls=%d", err, tr.calls)
			}
			if kind == "same-id" {
				if err != nil || len(proof.States) != 1 {
					t.Fatalf("same identity rejected: %+v %v", proof, err)
				}
			} else if err == nil || !reflect.DeepEqual(proof, LegacyEvidence{}) {
				t.Fatalf("exact metadata refusal weakened: %+v %v", proof, err)
			}
		})
	}
}

func (t *conflictInspectionTransport) Batch(ctx context.Context, reqs []graph.BatchRequest) ([]graph.BatchResponse, error) {
	out := make([]graph.BatchResponse, len(reqs))
	for i, r := range reqs {
		if r.Method != "GET" || r.Body != nil {
			return nil, errors.New("unexpected mutation")
		}
		u, err := url.Parse(r.URL)
		if err != nil {
			return nil, err
		}
		var body map[string]any
		if err = t.Get(ctx, u.Path, u.Query(), &body); err != nil {
			return nil, err
		}
		out[i] = graph.BatchResponse{ID: r.ID, Status: 200}
		out[i].Body, _ = json.Marshal(body)
	}
	return out, nil
}
