package graph_test

import (
	"bytes"
	"context"
	"errors"
	"github.com/mrothroc/mailpipe/pkg/graph"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestMutationResponseFailureIsNotReplayed(t *testing.T) {
	var calls atomic.Int32
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) })
	if err := c.Do(context.Background(), http.MethodPost, "/move", nil, "", map[string]string{"destinationId": "deleteditems"}, nil); err == nil {
		t.Fatal("503 succeeded")
	}
	if calls.Load() != 1 {
		t.Fatalf("uncertain mutation replayed %d times", calls.Load())
	}
}

func TestWriteRawCompleteAndBounded(t *testing.T) {
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("complete tail")) })
	var b bytes.Buffer
	if err := c.WriteRaw(context.Background(), "/source", &b, 13); err != nil || b.String() != "complete tail" {
		t.Fatalf("%q %v", b.String(), err)
	}
	b.Reset()
	if err := c.WriteRaw(context.Background(), "/source", &b, 12); err == nil {
		t.Fatal("overflow succeeded")
	}
}

type retryTransport struct {
	calls  atomic.Int32
	closed atomic.Bool
}
type closedBody struct {
	io.Reader
	closed *atomic.Bool
}

func (b closedBody) Close() error { b.closed.Store(true); return nil }
func (r *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls.Add(1)
	return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"60"}}, Body: closedBody{strings.NewReader("throttled"), &r.closed}, Request: req}, nil
}
func TestDefaultRetryWaitHonorsCancellation(t *testing.T) {
	for _, blocking := range []bool{false, true} {
		name := "production"
		if blocking {
			name = "blocking-negative-control"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				transport := &retryTransport{}
				c := graph.New(func(context.Context) (string, error) { return "invented-token", nil })
				c.Base = "https://mail.example.org"
				c.HTTP = &http.Client{Transport: transport}
				release := make(chan struct{})
				var waits atomic.Int32
				if blocking {
					c.Sleep = func(time.Duration) { waits.Add(1); <-release }
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- c.Get(ctx, "/read", nil, nil) }()
				synctest.Wait() // response consumed; the only remaining block is retry wait
				if !transport.closed.Load() || transport.calls.Load() != 1 {
					t.Fatal("retry response not consumed")
				}
				cancel()
				synctest.Wait()
				var result error
				prompt := false
				select {
				case result = <-done:
					prompt = true
				default:
				}
				if blocking {
					close(release)
					synctest.Wait()
					if !prompt {
						result = <-done
					}
					if waits.Load() != 1 {
						t.Fatal("negative control never entered blocking wait")
					}
				}
				if !prompt && !blocking {
					// Bound teardown even for a regressed production wait. This timeout is
					// only cleanup; synctest.Wait above established the phase without time.
					select {
					case result = <-done:
					case <-time.After(61 * time.Second):
						t.Fatal("retry worker did not stop during bounded teardown")
					}
				}
				if prompt == blocking {
					t.Fatal("cancellation probe did not distinguish production from entered blocking wait")
				}
				if !errors.Is(result, context.Canceled) {
					t.Fatal(result)
				}
				if transport.calls.Load() != 1 {
					t.Fatal("canceled request replayed")
				}
			})
		})
	}
}
