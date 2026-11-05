package graphmail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

type diagnosticOpaqueError struct{}

func (*diagnosticOpaqueError) Error() string { panic("diagnostic must not stringify an unknown cause") }

type diagnosticFailureTransport struct {
	*evidenceBenchmarkTransport
	phase string
	cause error
}

func (t *diagnosticFailureTransport) Get(ctx context.Context, path string, q url.Values, out any) error {
	if t.phase == "enumeration" {
		return t.cause
	}
	return t.evidenceBenchmarkTransport.Get(ctx, path, q, out)
}
func (t *diagnosticFailureTransport) Batch(context.Context, []graph.BatchRequest) ([]graph.BatchResponse, error) {
	return nil, t.cause
}

func TestReconciliationReadDiagnosticClassesPreserveFailure(t *testing.T) {
	const hostile = "untrusted-diagnostic-value\n\x1b https://host.example.org/private"
	cases := []struct {
		name, category string
		cause          error
	}{
		{"cancel", "canceled", context.Canceled},
		{"deadline", "deadline", errors.Join(errors.New(hostile), context.DeadlineExceeded)},
		{"io-deadline", "deadline", &net.OpError{Op: hostile, Err: os.ErrDeadlineExceeded}},
		{"account", "account-mismatch", &mailbox.AccountMismatchError{Operation: hostile, Expected: "hidden-expected", Actual: "hidden-actual"}},
		{"limit", "response-limit", &graph.ResponseLimitError{Limit: 1234567}},
		{"json-syntax", "malformed-response", &json.SyntaxError{Offset: 1234567}},
		{"json-shape", "malformed-response", &json.UnmarshalTypeError{Value: hostile, Field: hostile}},
		{"identity", "stale-identity", &mailbox.StaleIdentityError{Operation: hostile, Reason: hostile}},
		{"eof", "read-failure", io.EOF},
		{"short-read", "read-failure", io.ErrUnexpectedEOF},
		{"request", "transport-read-failure", &url.Error{Op: hostile, URL: hostile, Err: errors.New(hostile)}},
		{"network", "transport-read-failure", &net.OpError{Op: hostile, Net: hostile, Err: errors.New(hostile)}},
		{"unknown", "unknown-read-failure", errors.New(hostile)},
		{"unknown-string-mimics-check", "unknown-read-failure", errors.New("metadata changed after enumeration")},
		{"opaque", "unknown-read-failure", &diagnosticOpaqueError{}},
	}
	for _, tc := range []struct {
		name, category string
		status         int
	}{
		{"http401", "authorization-http-401", 401}, {"http403", "authorization-http-403", 403},
		{"http404", "not-found-http-404", 404}, {"http410", "gone-http-410", 410},
		{"http429", "throttled-http-429", 429}, {"http500", "server-response", 500},
		{"http502", "server-response", 502}, {"http503", "server-response", 503}, {"http504", "server-response", 504},
		{"unknown-status", "unknown-api-response", 799},
	} {
		cases = append(cases, struct {
			name, category string
			cause          error
		}{tc.name, tc.category, &graph.APIError{Status: tc.status, Code: hostile, Message: hostile}})
	}
	for _, phase := range []string{"enumeration", "inspection"} {
		for _, tc := range cases {
			t.Run(phase+"/"+tc.name, func(t *testing.T) {
				a := mailbox.Account{ID: "invented-account", ProviderID: "graph"}
				tr := &diagnosticFailureTransport{evidenceBenchmarkTransport: &evidenceBenchmarkTransport{n: 2}, phase: phase, cause: tc.cause}
				p, err := New(tr, a)
				if err != nil {
					t.Fatal(err)
				}
				proof, err := p.ReconcileLegacyReferences(context.Background(), batchOriginals(2, a))
				var failure *ReconciliationError
				if !errors.As(err, &failure) || failure.Structural || !errors.Is(err, tc.cause) || !reflect.DeepEqual(proof, LegacyEvidence{}) {
					t.Fatal("diagnostic changed error identity, classification or zero-proof boundary")
				}
				if !strings.Contains(failure.Reason, "phase="+phase+";") || !strings.Contains(failure.Reason, "category="+tc.category+";") {
					t.Fatal("wrong fixed phase/category")
				}
				text := failure.Error()
				if len(text) > 256 {
					t.Fatal("unbounded diagnostic")
				}
				for _, value := range []string{"untrusted-diagnostic-value", "host.example.org", "hidden-expected", "hidden-actual", "1234567", "\x1b"} {
					if strings.Contains(text, value) {
						t.Fatal("diagnostic rendered cause data")
					}
				}
				if tr.inspections != 0 || tr.batches != 0 {
					t.Fatal("diagnostic added subrequests")
				}
			})
		}
	}
}
