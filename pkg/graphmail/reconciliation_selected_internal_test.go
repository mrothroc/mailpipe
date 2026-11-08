package graphmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

type selectedObservationTransport struct {
	reconciliationTransport
	listed, inspected []map[string]any
	status            map[string]int
	code              string
	readErr, batchErr error
	cancel            func()
	requests          []string
	batchCalls        int
	partial           bool
}

func (g *selectedObservationTransport) Get(_ context.Context, _ string, _ url.Values, out any) error {
	g.calls++
	if g.readErr != nil {
		return g.readErr
	}
	raw, err := json.Marshal(map[string]any{"value": g.listed})
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
func (g *selectedObservationTransport) Batch(_ context.Context, reqs []graph.BatchRequest) ([]graph.BatchResponse, error) {
	g.batchCalls++
	if g.calls != 1 {
		return nil, errors.New("inspection before enumeration")
	}
	if g.batchErr != nil {
		return nil, g.batchErr
	}
	var out []graph.BatchResponse
	for _, req := range reqs {
		if req.Method != "GET" {
			return nil, errors.New("mutation requested")
		}
		u, _ := url.Parse(req.URL)
		id := strings.TrimPrefix(u.Path, "/me/messages/")
		g.requests = append(g.requests, id)
		status := g.status[id]
		if status == 0 {
			status = 200
		}
		var body any
		if status != 200 {
			body = map[string]any{"error": map[string]string{"code": g.code, "message": "untrusted-provider-value\x1b"}}
		} else {
			for _, m := range g.inspected {
				if m["id"] == id {
					body = m
				}
			}
		}
		raw, _ := json.Marshal(body)
		// The Graph adapter contract already orders batch responses. Shuffled
		// wire responses are exercised through the real HTTP adapter separately.
		out = append(out, graph.BatchResponse{ID: req.ID, Status: status, Body: raw})
	}
	if g.partial {
		out = out[:len(out)-1]
	}
	if g.cancel != nil {
		g.cancel()
	}
	return out, nil
}

func selectedObservationFixture(t *testing.T) (*Provider, *selectedObservationTransport, []LegacyReference) {
	t.Helper()
	a := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
	g := &selectedObservationTransport{status: map[string]int{}, code: "ErrorItemNotFound"}
	var refs []LegacyReference
	for i, id := range []string{"target", "other"} {
		refs = append(refs, LegacyReference{N: int64(i + 1), Ref: mailbox.MessageRef{AccountID: a.ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator(id)}, Collection: mailbox.CollectionRef{AccountID: a.ID, CollectionID: "inbox"}, InternetMessageID: "<" + id + "@example.org>", Sender: "sender@example.org"})
		for _, dst := range []*[]map[string]any{&g.listed, &g.inspected} {
			m := conflictWire(id, "<"+id+"@example.org>", "sender@example.org", "inbox")
			m["toRecipients"], m["ccRecipients"] = []any{}, []any{}
			*dst = append(*dst, m)
		}
	}
	p, err := New(g, a)
	if err != nil {
		t.Fatal(err)
	}
	return p, g, refs
}

func TestSelectedObservationIsNotAdmission(t *testing.T) {
	for _, kind := range []string{"equal", "sender-different", "subject", "original-header", "original-sender", "revision", "membership", "recipient-name", "recipient-order", "recipient-shape", "other-unavailable", "other-origin-unsupported"} {
		t.Run(kind, func(t *testing.T) {
			p, g, refs := selectedObservationFixture(t)
			switch kind {
			case "sender-different":
				g.listed[0]["from"] = map[string]any{"emailAddress": map[string]string{"address": "enumerated@example.org"}}
			case "subject":
				g.inspected[0]["subject"] = "Different invented subject"
			case "original-header":
				refs[0].InternetMessageID = "<different@example.org>"
			case "original-sender":
				refs[0].Sender = "different@example.org"
			case "revision":
				g.listed[0]["changeKey"], g.inspected[0]["changeKey"] = "old", "new"
			case "membership":
				g.inspected[0]["parentFolderId"] = "archive"
			case "recipient-shape":
				g.inspected[0]["toRecipients"] = []any{map[string]any{"emailAddress": map[string]string{"address": "to@example.org"}}}
			case "recipient-name", "recipient-order":
				left := []any{map[string]any{"emailAddress": map[string]string{"address": "to@example.org", "name": "First"}}, map[string]any{"emailAddress": map[string]string{"address": "cc@example.org", "name": "Second"}}}
				right := []any{left[1], left[0]}
				if kind == "recipient-name" {
					right = []any{map[string]any{"emailAddress": map[string]string{"address": "to@example.org", "name": "Changed"}}, left[1]}
				}
				g.listed[0]["toRecipients"], g.inspected[0]["toRecipients"] = left, right
			case "other-unavailable":
				g.listed = g.listed[:1]
				g.status["other"] = 404
			case "other-origin-unsupported":
				g.listed[1]["From"] = g.listed[1]["from"]
				delete(g.listed[1], "from")
			}
			d, err := p.ObserveLegacySenderAddress(context.Background(), refs, 1)
			if err != nil || !d.RequestedOriginal || d.N != 1 || d.ImmutableID != "target" || !d.EnumeratedOrigin.MatchesMappedSender(d.EnumeratedSenderAddress) || !d.InspectedOrigin.MatchesMappedSender(d.InspectedSenderAddress) {
				t.Fatal("selected pair confused admission with observation", err)
			}
			if g.calls != 1 || g.batchCalls != 1 || !reflect.DeepEqual(g.requests, []string{"target", "other"}) {
				t.Fatal("selected-only or extra request")
			}
			if kind != "equal" && kind != "sender-different" && kind != "other-unavailable" && kind != "other-origin-unsupported" {
				g.calls, g.batchCalls, g.requests = 0, 0, nil
				proof, err := p.ReconcileLegacyReferences(context.Background(), refs)
				if err == nil || !reflect.DeepEqual(proof, LegacyEvidence{}) {
					t.Fatal("diagnostic relaxed normal admission")
				}
			}
		})
	}
}

func TestSelectedObservationReadFailuresReturnNoData(t *testing.T) {
	for _, kind := range []string{"enumeration", "aggregate", "partial", "target-not-found", "target-appeared", "target-missing", "other-observed-404", "other-unrecognized-404", "other-auth", "other-throttle", "wrong-id", "unsupported-enumeration", "unsupported-inspection", "malformed", "duplicate-enumeration", "canceled-before", "canceled-after", "budget"} {
		t.Run(kind, func(t *testing.T) {
			p, g, refs := selectedObservationFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cause := io.ErrUnexpectedEOF
			switch kind {
			case "enumeration":
				g.readErr = cause
			case "aggregate":
				g.batchErr = cause
			case "partial":
				g.partial = true
			case "target-not-found":
				g.status["target"] = 404
			case "target-appeared":
				g.listed = g.listed[1:]
			case "target-missing":
				g.listed = g.listed[1:]
				g.status["target"] = 404
			case "other-observed-404":
				g.status["other"] = 404
			case "other-unrecognized-404":
				g.listed = g.listed[:1]
				g.status["other"] = 404
				g.code = "Unknown"
			case "other-auth":
				g.status["other"] = 401
			case "other-throttle":
				g.status["other"] = 429
			case "wrong-id":
				g.inspected[0]["id"] = "wrong"
			case "unsupported-enumeration":
				g.listed[0]["From"] = g.listed[0]["from"]
				delete(g.listed[0], "from")
			case "unsupported-inspection":
				g.inspected[0]["From"] = g.inspected[0]["from"]
				delete(g.inspected[0], "from")
			case "malformed":
				g.inspected[0]["toRecipients"] = nil
			case "duplicate-enumeration":
				g.listed = append(g.listed, g.listed[0])
			case "canceled-before":
				cancel()
			case "canceled-after":
				g.cancel = cancel
			case "budget":
				g.listed[0]["from"] = map[string]any{"emailAddress": map[string]string{"address": strings.Repeat("x", reconciliationOriginLimit/2+1)}}
			}
			d, err := p.ObserveLegacySenderAddress(ctx, refs, 1)
			if err == nil || !reflect.DeepEqual(d, ReconciliationSenderAddressDiagnostic{}) {
				t.Fatal("failed read returned observation")
			}
			if (kind == "enumeration" || kind == "aggregate") && !errors.Is(err, cause) {
				t.Fatal("read cause lost")
			}
			if strings.Contains(err.Error(), "untrusted-provider") {
				t.Fatal("provider value escaped")
			}
			if kind == "canceled-before" && (g.calls != 0 || g.batchCalls != 0) {
				t.Fatal("canceled observation read")
			}
			if kind == "budget" && g.batchCalls != 0 {
				t.Fatal("over-budget observation continued")
			}
		})
	}
}

func TestSelectedObservationIdentityPreflight(t *testing.T) {
	for _, kind := range []string{"zero", "negative", "unknown", "duplicate-number", "duplicate-id", "account", "locator", "collection"} {
		t.Run(kind, func(t *testing.T) {
			p, g, refs := selectedObservationFixture(t)
			n := int64(1)
			switch kind {
			case "zero":
				n = 0
			case "negative":
				n = -1
			case "unknown":
				n = 9
			case "duplicate-number":
				refs[1].N = 1
			case "duplicate-id":
				refs[1].Ref = refs[0].Ref
			case "account":
				refs[0].Ref.AccountID = "different"
			case "locator":
				refs[0].Ref.Locator = "different"
			case "collection":
				refs[0].Collection.AccountID = "different"
			}
			d, err := p.ObserveLegacySenderAddress(context.Background(), refs, n)
			if err == nil || !reflect.DeepEqual(d, ReconciliationSenderAddressDiagnostic{}) || g.calls != 0 || g.batchCalls != 0 {
				t.Fatal("invalid identity caused reads/data")
			}
		})
	}
}

func TestSelectedObservationReadsEveryOriginalAcrossBatches(t *testing.T) {
	p, g, refs := selectedObservationFixture(t)
	// More than two Graph batches; selecting the first original must not turn
	// this into a single-item or first-batch lookup.
	for i := 2; i < 41; i++ {
		id := fmt.Sprintf("invented-%d", i)
		r := refs[0]
		r.N = int64(i + 1)
		r.Ref.MessageID = mailbox.MessageID(id)
		r.Ref.Locator = mailbox.Locator(id)
		refs = append(refs, r)
		for _, dst := range []*[]map[string]any{&g.listed, &g.inspected} {
			raw, err := json.Marshal((*dst)[0])
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err = json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			m["id"] = id
			*dst = append(*dst, m)
		}
	}
	d, err := p.ObserveLegacySenderAddress(context.Background(), refs, 1)
	if err != nil || d.N != 1 {
		t.Fatal("multi-batch selected observation failed", err)
	}
	var ids []string
	for _, r := range refs {
		ids = append(ids, string(r.Ref.MessageID))
	}
	if g.calls != 1 || g.batchCalls != 3 || !reflect.DeepEqual(g.requests, ids) {
		t.Fatal("selected observation did not inspect every original in order")
	}
}
