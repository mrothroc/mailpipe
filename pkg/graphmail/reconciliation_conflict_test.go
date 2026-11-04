package graphmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
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

func requireConflict(t *testing.T, proof LegacyEvidence, err error) *ReconciliationConflict {
	t.Helper()
	var failure *ReconciliationError
	if !errors.As(err, &failure) || !failure.Structural || failure.Conflict == nil || !reflect.DeepEqual(proof, LegacyEvidence{}) {
		t.Fatalf("conflict missing or acquired proof: %+v %v", proof, err)
	}
	if strings.Contains(err.Error(), "example.org") || strings.Contains(err.Error(), "observed-id") {
		t.Fatal("metadata leaked into generic error", err)
	}
	return failure.Conflict
}

func TestReconciliationConflictKeepsEveryOriginalDistinctWithoutMoreReads(t *testing.T) {
	for _, knownObserved := range []bool{false, true} {
		for _, next := range []bool{false, true} {
			t.Run(fmt.Sprintf("known=%t/next=%t", knownObserved, next), func(t *testing.T) {
				a := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
				refs := []LegacyReference{
					conflictOriginal(7, "original-one", "<shared@example.org>", " Sender@Example.Org ", "inbox", a),
					conflictOriginal(9, "original-two", "<shared@example.org>", "sender@example.org", "other-folder", a),
					conflictOriginal(11, "different-sender", "<shared@example.org>", "other@example.org", "inbox", a),
					conflictOriginal(12, "different-header", "<other@example.org>", "sender@example.org", "inbox", a),
				}
				if knownObserved {
					refs = append(refs, conflictOriginal(13, "observed-id", "<shared@example.org>", "sender@example.org", "inbox", a))
				}
				page := map[string]any{"value": []any{conflictWire("observed-id", "<shared@example.org>", "sender@example.org", "current-folder")}}
				if next {
					page["@odata.nextLink"] = "/must-not-read"
				}
				tr := &reconciliationTransport{page: page}
				p, err := New(tr, a)
				if err != nil {
					t.Fatal(err)
				}
				proof, err := p.ReconcileLegacyReferences(context.Background(), refs)
				c := requireConflict(t, proof, err)
				if tr.calls != 1 || c.MatchingOriginals != 2 || len(c.Originals) != 2 || c.Originals[0].N != 7 || c.Originals[1].N != 9 || c.Originals[0].Message.ID.Value != "original-one" || c.Originals[1].Message.ID.Value != "original-two" || c.ObservedMessage.ID.Value != "observed-id" || c.Pages != 1 || c.Observed != 1 || c.Started.IsZero() || c.ObservedAt.Before(c.Started) {
					t.Fatalf("lost distinct references or performed extra reads: %+v calls=%d", c, tr.calls)
				}
				if c.Originals[0].Message.Sender.Value != refs[0].Sender || c.Originals[1].Message.CollectionID.Value != "other-folder" || c.ObservedMessage.CollectionID.Value != "current-folder" {
					t.Fatal("diagnostics replaced original metadata")
				}
				raw, _ := json.Marshal(c)
				if strings.Contains(string(raw), "excluded-") || strings.Contains(string(raw), "must-not-read") {
					t.Fatal("unnecessary metadata retained")
				}
			})
		}
	}
}

func TestReconciliationConflictBoundsAndSimultaneousFieldOmissions(t *testing.T) {
	for _, count := range []int{32, 33, 100} {
		for _, size := range []int{1024, 1025} {
			t.Run(fmt.Sprintf("originals=%d/bytes=%d", count, size), func(t *testing.T) {
				id := "observed-" + strings.Repeat("i", size-len("observed-"))
				header := "<" + strings.Repeat("h", size-len("<@example.org>")) + "@example.org>"
				sender := strings.Repeat("s", size-len("@example.org")) + "@example.org"
				collection := strings.Repeat("f", size)
				a := mailbox.Account{ID: mailbox.AccountID(strings.Repeat("a", size)), ProviderID: "graph"}
				refs := make([]LegacyReference, count)
				for i := range refs {
					prefix := fmt.Sprintf("original-%d-", i)
					refs[i] = conflictOriginal(int64(i+1), prefix+strings.Repeat("i", size-len(prefix)), header, sender, collection, a)
				}
				tr := &reconciliationTransport{page: map[string]any{"value": []any{conflictWire(id, header, sender, collection)}}}
				p, err := New(tr, a)
				if err != nil {
					t.Fatal(err)
				}
				proof, err := p.ReconcileLegacyReferences(context.Background(), refs)
				c := requireConflict(t, proof, err)
				if c.MatchingOriginals != count || len(c.Originals) != 32 || tr.calls != 1 {
					t.Fatalf("bounds/counts: %+v calls=%d", c, tr.calls)
				}
				fields := []ReconciliationDiagnosticField{c.Account, c.ObservedMessage.ID, c.ObservedMessage.InternetMessageID, c.ObservedMessage.Sender, c.ObservedMessage.CollectionID}
				for i, original := range c.Originals {
					if original.N != int64(i+1) {
						t.Fatal("original order changed")
					}
					fields = append(fields, original.Message.ID, original.Message.InternetMessageID, original.Message.Sender, original.Message.CollectionID)
				}
				for _, f := range fields {
					if size == 1024 && (len(f.Value) != size || f.OmittedBytes != 0) || size > 1024 && (f.Value != "" || f.OmittedBytes != size) {
						t.Fatalf("field not exact or wholly omitted: %+v", f)
					}
				}
				raw, _ := json.Marshal(c)
				if len(raw) > 160000 {
					t.Fatalf("diagnostic retention grew beyond bounded fields: %d", len(raw))
				}
			})
		}
	}
}

// A successful collector still performs its existing enumeration and exact
// inspection sequence; diagnostic collection adds no successful-path reads.
func TestReconciliationSuccessReadCountsUnchanged(t *testing.T) {
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
	if err != nil || len(proof.States) != 3 || !proof.Unavailable[4] || tr.calls != 5 || tr.pages != 1 || tr.inspections != 4 {
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

func TestReconciliationDoesNotDiagnoseNonCollisions(t *testing.T) {
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
				t.Fatalf("noncollision became conflict or skipped inspection: %v calls=%d", err, tr.calls)
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
