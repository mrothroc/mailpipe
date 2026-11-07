package graphmail

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

func batchOriginals(n int, a mailbox.Account) []LegacyReference {
	refs := make([]LegacyReference, n)
	for i := range refs {
		id := fmt.Sprintf("invented-%d", i)
		refs[i] = conflictOriginal(int64(i+1), id, "<"+id+"@example.org>", "sender@example.org", "F-inbox", a)
	}
	return refs
}

type interruptedEvidenceTransport struct {
	*evidenceBenchmarkTransport
	cancel context.CancelFunc
	fail   error
}

func (t *interruptedEvidenceTransport) Batch(ctx context.Context, reqs []graph.BatchRequest) ([]graph.BatchResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t.batches == 1 && t.fail != nil {
		return nil, t.fail
	}
	out, err := t.evidenceBenchmarkTransport.Batch(ctx, reqs)
	if t.cancel != nil {
		t.cancel()
	}
	return out, err
}

func TestReconciliationBatchCancellationAndPartialFailure(t *testing.T) {
	a := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
	for _, kind := range []string{"cancellation", "aggregate-after-success"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tr := &interruptedEvidenceTransport{evidenceBenchmarkTransport: &evidenceBenchmarkTransport{n: 43}}
			want := errors.New("incomplete later batch")
			if kind == "cancellation" {
				tr.cancel = cancel
				want = context.Canceled
			} else {
				tr.fail = want
			}
			p, err := New(tr, a)
			if err != nil {
				t.Fatal(err)
			}
			proof, err := p.ReconcileLegacyReferences(ctx, batchOriginals(43, a))
			if !errors.Is(err, want) || !reflect.DeepEqual(proof, LegacyEvidence{}) || tr.batches != 1 || tr.inspections != graph.MaxBatch {
				t.Fatalf("partial batch survived: %+v %v calls=%+v", proof, err, tr.evidenceBenchmarkTransport)
			}
		})
	}
}

func TestReconciliationRealisticReadOperationCounts(t *testing.T) {
	for _, capture := range []bool{false, true} {
		a := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
		tr := &evidenceBenchmarkTransport{n: 10001}
		p, err := New(tr, a)
		if err != nil {
			t.Fatal(err)
		}
		var proof LegacyEvidence
		if capture {
			proof, err = p.ReconcileLegacyReferencesWithSenderAddressDiagnostics(context.Background(), batchOriginals(tr.n, a))
		} else {
			proof, err = p.ReconcileLegacyReferences(context.Background(), batchOriginals(tr.n, a))
		}
		if err != nil || len(proof.States) != 10000 || !proof.Unavailable[10001] || tr.inspections != 10001 || tr.pages != 50 || tr.batches != 501 || tr.calls != 551 {
			t.Fatalf("enumeration or exact inspections skipped: %v counts=%+v", err, tr)
		}
	}
}
