// Package graph is a thin Microsoft Graph client: bearer auth, immutable
// message ids (stable across folder moves), text bodies, throttling retries
// and paging. It knows nothing about the CLI or storage.
package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBase is the Graph v1.0 endpoint.
	DefaultBase = "https://graph.microsoft.com/v1.0"
	// MaxTries bounds attempts per request, including the first.
	MaxTries = 6
	// PageSize is the page size requested for collection reads.
	PageSize = 200
	// prefer asks for ids that survive moves between folders, and plain-text bodies.
	prefer  = `IdType="ImmutableId", outlook.body-content-type="text"`
	timeout = 60 * time.Second
)

// ResponseLimitError means a response was discarded after exceeding its bound.
type ResponseLimitError struct{ Limit int64 }

func (e *ResponseLimitError) Error() string {
	return fmt.Sprintf("graph: response exceeds %d-byte limit", e.Limit)
}

// APIError is a Graph error response.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	detail := strings.TrimSpace(e.Code + ": " + e.Message)
	return fmt.Sprintf("graph %d %s", e.Status, strings.Trim(detail, ": "))
}

// Page is one page of a collection; the last page of a delta query carries DeltaLink.
type Page struct {
	Value     []json.RawMessage `json:"value"`
	NextLink  string            `json:"@odata.nextLink"`
	DeltaLink string            `json:"@odata.deltaLink"`
}

// Client calls Graph as the signed-in user.
type Client struct {
	Token func(ctx context.Context) (string, error)
	HTTP  *http.Client
	Base  string
	Sleep func(time.Duration)
}

// New returns a Client that gets access tokens from token.
func New(token func(ctx context.Context) (string, error)) *Client {
	return &Client{Token: token, HTTP: &http.Client{Timeout: timeout}, Base: DefaultBase}
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// Do sends one request and decodes the response. GET requests retry throttling
// and server errors; mutating requests are never blindly replayed. It decodes the
// JSON response into out (when out is non-nil). path is either relative to
// Base ("/me/...") or an absolute link returned by Graph.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, extraPrefer string, body, out any) error {
	return c.do(ctx, method, path, query, extraPrefer, body, out, method == http.MethodGet)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, extraPrefer string, body, out any, readOnly bool) error {
	limit := int64(16 << 20)
	if query.Get("$select") == "body" {
		limit = 64 << 20
	}
	data, err := c.sendTo(ctx, method, path, query, extraPrefer, body, nil, limit, readOnly)
	if err != nil || out == nil {
		return err
	}
	if len(data) == 0 {
		return errors.New("graph: empty JSON response")
	}
	return json.Unmarshal(data, out)
}

// Raw GETs path and returns the response body as is: a message's MIME
// source from ".../$value", say. It retries and fails like Do.
func (c *Client) Raw(ctx context.Context, path string) ([]byte, error) {
	return c.send(ctx, http.MethodGet, path, nil, "", nil)
}

// send makes the request, retrying throttling and transient failures, and
// returns the successful response's body.
func (c *Client) send(ctx context.Context, method, path string, query url.Values, extraPrefer string, body any) ([]byte, error) {
	return c.sendTo(ctx, method, path, query, extraPrefer, body, nil, 64<<20, method == http.MethodGet)
}

// WriteRaw streams a complete bounded GET. After output begins any failure is
// returned; the caller discards partial output rather than receiving a replay.
func (c *Client) WriteRaw(ctx context.Context, path string, w io.Writer, limit int64) error {
	if w == nil || limit <= 0 {
		return errors.New("graph: writer and finite source limit required")
	}
	_, err := c.sendTo(ctx, http.MethodGet, path, nil, "", nil, w, limit, true)
	return err
}

func (c *Client) sendTo(ctx context.Context, method, path string, query url.Values, extraPrefer string, body any, sink io.Writer, limit int64, readOnly bool) ([]byte, error) {
	base, err := url.Parse(c.Base)
	if err != nil || !validAPIURL(base) {
		return nil, errors.New("graph: invalid API base URL")
	}
	target := path
	if strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") {
		target = c.Base + path
	}
	u, err := url.Parse(target)
	if err != nil || !sameOrigin(base, u) {
		return nil, errUnsafeURL
	}
	if len(query) > 0 {
		if u.RawQuery != "" {
			u.RawQuery += "&"
		}
		u.RawQuery += query.Encode()
	}
	target = u.String()
	// Keep caller transport/timeouts/redirect policy, without mutating a
	// shared client. Validate every redirect before any request can leave.
	hc := *c.HTTP
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOrigin(base, req.URL) {
			return errUnsafeURL
		}
		if c.HTTP.CheckRedirect != nil {
			if err := c.HTTP.CheckRedirect(req, via); err != nil {
				return err
			}
			if !sameOrigin(base, req.URL) {
				return errUnsafeURL
			}
			return nil
		}
		if len(via) >= 10 {
			return errors.New("graph: stopped after 10 redirects")
		}
		return nil
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	pref := prefer
	if extraPrefer != "" {
		pref += ", " + extraPrefer
	}
	var last error
	tries := 1
	if readOnly {
		tries = MaxTries
	}
	for attempt := 0; attempt < tries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err // cancelled: stop rather than sleep through the backoffs
		}
		token, err := c.Token(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Prefer", pref)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		// pause waits before the next attempt: never after the last one, and
		// never once the context has ended.
		pause := func(retryAfter string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if attempt < tries-1 {
				return c.wait(ctx, backoff(attempt, retryAfter))
			}
			return nil
		}
		resp, err := hc.Do(req)
		if err != nil {
			if errors.Is(err, errUnsafeURL) {
				return nil, err
			}
			last = err
			if err := pause(""); err != nil {
				return nil, err
			}
			continue
		}
		if sink != nil && resp.StatusCode < 400 {
			err := copyBounded(ctx, sink, resp.Body, limit)
			resp.Body.Close()
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if readErr == nil && int64(len(data)) > limit {
			readErr = &ResponseLimitError{Limit: limit}
			resp.Body.Close()
			return nil, readErr
		}
		resp.Body.Close()
		if readErr != nil {
			last = readErr
			if err := pause(""); err != nil {
				return nil, err
			}
			continue
		}
		if retryable(resp.StatusCode) {
			last = apiError(resp.StatusCode, data)
			if err := pause(resp.Header.Get("Retry-After")); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode >= 400 {
			return nil, apiError(resp.StatusCode, data)
		}
		return data, nil
	}
	return nil, last
}

var errUnsafeURL = errors.New("graph: refusing URL outside the configured API origin or containing user info or fragment")

func validAPIURL(u *url.URL) bool {
	return u != nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.Fragment == "" && u.Opaque == ""
}

// Keep explicit-port and trailing-dot spellings strict. Graph paging links
// match Base; suffix or subdomain matches must never widen credential scope.
func sameOrigin(base, u *url.URL) bool {
	return validAPIURL(u) && strings.EqualFold(base.Scheme, u.Scheme) && strings.EqualFold(base.Host, u.Host)
}

func backoff(attempt int, retryAfter string) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	return time.Duration(1<<attempt) * time.Second
}

func apiError(status int, data []byte) *APIError {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	e := &APIError{Status: status}
	if json.Unmarshal(data, &body) == nil && body.Error.Code != "" {
		e.Code, e.Message = body.Error.Code, body.Error.Message
	} else {
		e.Message = strings.TrimSpace(string(data))
		if len(e.Message) > 300 {
			e.Message = e.Message[:300]
		}
	}
	return e
}

// Get is Do for GET.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	return c.Do(ctx, http.MethodGet, path, query, "", nil, out)
}

// Pages walks a collection from start, calling fn for every page in order.
// The query applies to the first request only; next links already carry it.
func (c *Client) Pages(ctx context.Context, start string, query url.Values, fn func(Page) error) error {
	link := start
	for link != "" {
		var p Page
		if err := c.Do(ctx, http.MethodGet, link, query, fmt.Sprintf("odata.maxpagesize=%d", PageSize), nil, &p); err != nil {
			return err
		}
		if err := fn(p); err != nil {
			return err
		}
		link, query = p.NextLink, nil
	}
	return nil
}

// FolderTotal returns the number of items Graph reports for a mail folder.
func (c *Client) FolderTotal(ctx context.Context, folder string) (int, error) {
	var f struct {
		TotalItemCount int `json:"totalItemCount"`
	}
	err := c.Get(ctx, "/me/mailFolders/"+url.PathEscape(folder), url.Values{"$select": {"totalItemCount"}}, &f)
	return f.TotalItemCount, err
}

// copyBounded limits total input independently from its 32 KiB scratch buffer.
func copyBounded(ctx context.Context, w io.Writer, r io.Reader, limit int64) error {
	buf := make([]byte, 32<<10)
	var total int64
	empty := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := r.Read(buf)
		if n > 0 {
			empty = 0
			if int64(n) > limit-total {
				return fmt.Errorf("read MIME: message exceeds %d-byte content limit; incomplete content was discarded", limit)
			}
			written, e := w.Write(buf[:n])
			if e != nil {
				return e
			}
			if written != n {
				return io.ErrShortWrite
			}
			total += int64(n)
		} else {
			empty++
			if empty > 100 {
				return io.ErrNoProgress
			}
		}
		if err == io.EOF {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
	}
}

// wait uses the operation context for production backoff. Sleep is an optional
// deterministic collaborator; implementations must return promptly on cancel.
func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.Sleep != nil {
		c.Sleep(d)
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
