package graph_test

import (
	"bytes"
	"context"
	"errors"
	"github.com/mrothroc/mailpipe/pkg/graph"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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

func TestDefaultRetryWaitHonorsCancellation(t *testing.T) {
	var calls atomic.Int32
	reached := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(reached)
		}
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	}))
	defer server.Close()
	c := graph.New(func(context.Context) (string, error) { return "invented-token", nil })
	c.Base = server.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Get(ctx, "/read", nil, nil) }()
	<-reached
	// Give the completed local response a chance to enter its 60-second wait.
	select {
	case err := <-done:
		t.Fatalf("request returned before cancellation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry wait ignored cancellation")
	}
	if calls.Load() != 1 {
		t.Fatal("canceled request replayed")
	}
}
