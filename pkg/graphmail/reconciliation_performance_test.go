package graphmail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

type evidenceBenchmarkTransport struct{ n, calls, pages, inspections, batches int }

func (t *evidenceBenchmarkTransport) wire(i int) map[string]any {
	id := fmt.Sprintf("invented-%d", i)
	return map[string]any{"id": id, "parentFolderId": "F-inbox", "internetMessageId": fmt.Sprintf("<invented-%d@example.org>", i), "subject": "Invented history", "sender": map[string]any{"emailAddress": map[string]any{"address": "sender@example.org"}}, "toRecipients": []any{}, "ccRecipients": []any{}, "receivedDateTime": "2026-01-01T00:00:00Z", "hasAttachments": false, "isRead": false, "bodyPreview": "", "changeKey": "revision"}
}
func (t *evidenceBenchmarkTransport) Get(ctx context.Context, path string, _ url.Values, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.calls++
	var result any
	if strings.HasPrefix(path, "/me/messages/") {
		t.inspections++
		i, e := strconv.Atoi(strings.TrimPrefix(path, "/me/messages/invented-"))
		if e != nil || i >= t.n-1 {
			return &graph.APIError{Status: 404, Code: "ErrorItemNotFound"}
		}
		result = t.wire(i)
	} else {
		t.pages++
		offset := 0
		if path != "/me/messages" {
			var e error
			offset, e = strconv.Atoi(strings.TrimPrefix(path, "/evidence-pages/"))
			if e != nil {
				return e
			}
		}
		values := []any{}
		end := min(offset+200, t.n-1)
		for i := offset; i < end; i++ {
			values = append(values, t.wire(i))
		}
		page := map[string]any{"value": values}
		if end < t.n-1 {
			page["@odata.nextLink"] = fmt.Sprintf("/evidence-pages/%d", end)
		}
		result = page
	}
	raw, e := json.Marshal(result)
	if e != nil {
		return e
	}
	return json.Unmarshal(raw, out)
}
func (*evidenceBenchmarkTransport) Do(context.Context, string, string, url.Values, string, any, any) error {
	panic("unexpected mutation")
}
func (t *evidenceBenchmarkTransport) Batch(ctx context.Context, reqs []graph.BatchRequest) ([]graph.BatchResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t.calls++
	t.batches++
	out := make([]graph.BatchResponse, len(reqs))
	for i, r := range reqs {
		if r.Method != "GET" || r.Body != nil || !strings.HasPrefix(r.URL, "/me/messages/") {
			return nil, fmt.Errorf("unexpected mutation or inspection path")
		}
		u, err := url.Parse(r.URL)
		if err != nil {
			return nil, err
		}
		var body map[string]any
		err = t.Get(ctx, u.Path, u.Query(), &body)
		t.calls-- // the simulated subrequest is inside this one batch envelope
		out[i] = graph.BatchResponse{ID: r.ID, Status: 200}
		if err != nil {
			api, ok := err.(*graph.APIError)
			if !ok {
				return nil, err
			}
			out[i].Status = api.Status
			body = map[string]any{"error": map[string]any{"code": api.Code}}
		}
		out[i].Body, _ = json.Marshal(body)
	}
	return out, nil
}
func (*evidenceBenchmarkTransport) WriteRaw(context.Context, string, io.Writer, int64) error {
	panic("unexpected body")
}
func BenchmarkGraphLegacyEvidence(b *testing.B) {
	for _, n := range []int{10000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			a := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
			transport := &evidenceBenchmarkTransport{n: n}
			p, e := New(transport, a)
			if e != nil {
				b.Fatal(e)
			}
			refs := make([]LegacyReference, n)
			for i := range refs {
				id := fmt.Sprintf("invented-%d", i)
				refs[i] = LegacyReference{N: int64(i + 1), Ref: mailbox.MessageRef{AccountID: a.ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator(id)}, InternetMessageID: fmt.Sprintf("<invented-%d@example.org>", i), Sender: "sender@example.org", Collection: mailbox.CollectionRef{AccountID: a.ID, CollectionID: "F-inbox"}}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				proof, e := p.ReconcileLegacyReferences(context.Background(), refs)
				if e != nil || len(proof.States) != n-1 || len(proof.Unavailable) != 1 {
					b.Fatalf("evidence: %v", e)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(transport.calls)/float64(b.N), "read-calls/op")
			b.ReportMetric(float64(transport.pages)/float64(b.N), "pages/op")
			b.ReportMetric(float64(transport.batches)/float64(b.N), "batches/op")
			b.ReportMetric(float64(transport.inspections)/float64(b.N), "inspections/op")
		})
	}
}
