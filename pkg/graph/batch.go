package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// MaxBatch is the most sub-requests Graph accepts in one $batch request.
const MaxBatch = 20

// BatchRequest is one sub-request of a $batch. URL is relative to the API
// version ("/me/messages/{id}"); Body, when set, is sent as JSON.
type BatchRequest struct {
	ID     string
	Method string
	URL    string
	Body   any
}

// BatchResponse is the answer to one sub-request.
type BatchResponse struct {
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

type wireRequest struct {
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    any               `json:"body,omitempty"`
}

// Batch sends reqs as one $batch request and returns their responses in the
// order of reqs. In read-only batches, throttled/server-error sub-requests are
// re-sent on their own, after the longest Retry-After any of them asked for,
// up to MaxTries rounds; a sub-request still failing then is returned with its
// last status, for the caller to treat as failed. Mutation batches are not
// replayed here: their adapter must recheck explicit rejections before retry.
func (c *Client) Batch(ctx context.Context, reqs []BatchRequest) ([]BatchResponse, error) {
	if len(reqs) > MaxBatch {
		return nil, fmt.Errorf("batch of %d requests; Graph accepts at most %d", len(reqs), MaxBatch)
	}
	seen := map[string]bool{}
	for _, r := range reqs {
		if seen[r.ID] {
			return nil, fmt.Errorf("batch request id %q is used twice", r.ID)
		}
		seen[r.ID] = true
	}
	readOnly := true
	for _, r := range reqs {
		if r.Method != http.MethodGet {
			readOnly = false
		}
	}
	done := map[string]BatchResponse{}
	pending := reqs
	for attempt := 0; attempt < MaxTries && len(pending) > 0; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		wire := make([]wireRequest, len(pending))
		for i, r := range pending {
			headers := map[string]string{"Prefer": `IdType="ImmutableId"`}
			if r.Body != nil {
				headers["Content-Type"] = "application/json"
			}
			wire[i] = wireRequest{ID: r.ID, Method: r.Method, URL: r.URL, Headers: headers, Body: r.Body}
		}
		var out struct {
			Responses []BatchResponse `json:"responses"`
		}
		if err := c.do(ctx, http.MethodPost, "/$batch", nil, "", map[string]any{"requests": wire}, &out, readOnly); err != nil {
			return nil, err
		}
		byID := make(map[string]BatchResponse, len(out.Responses))
		for _, r := range out.Responses {
			if _, duplicate := byID[r.ID]; duplicate {
				return nil, fmt.Errorf("batch response repeats request %q", r.ID)
			}
			byID[r.ID] = r
		}
		var retry []BatchRequest
		var wait time.Duration
		for _, r := range pending {
			resp, ok := byID[r.ID]
			if !ok {
				return nil, fmt.Errorf("batch response has no answer for request %q", r.ID)
			}
			done[r.ID] = resp
			if readOnly && retryable(resp.Status) && attempt < MaxTries-1 {
				retry = append(retry, r)
				wait = max(wait, backoff(attempt, resp.header("Retry-After")))
			}
		}
		if len(retry) > 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := c.wait(ctx, wait); err != nil {
				return nil, err
			}
		}
		pending = retry
	}
	out := make([]BatchResponse, len(reqs))
	for i, r := range reqs {
		out[i] = done[r.ID]
	}
	return out, nil
}

// header returns a sub-response header; names match case-insensitively.
func (r BatchResponse) header(name string) string {
	for k, v := range r.Headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// Err returns nil for a successful sub-request, and its APIError otherwise.
func (r BatchResponse) Err() error {
	if r.Status >= 200 && r.Status < 300 {
		return nil
	}
	return apiError(r.Status, r.Body)
}
