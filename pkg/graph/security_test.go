package graph_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/graph"
)

func TestPagingAndRedirectsCannotSendCredentialsOutsideAPIOrigin(t *testing.T) {
	for _, mode := range []string{"nextLink", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			var outsideCalls, originCalls atomic.Int32
			outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				outsideCalls.Add(1)
				fmt.Fprint(w, `{"value":[]}`)
			}))
			defer outside.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				originCalls.Add(1)
				if mode == "redirect" {
					http.Redirect(w, r, outside.URL+"/collect", http.StatusTemporaryRedirect)
				} else {
					fmt.Fprintf(w, `{"value":[],"@odata.nextLink":%q}`, outside.URL+"/collect")
				}
			}))
			defer origin.Close()
			c := graph.New(func(context.Context) (string, error) { return "synthetic-token", nil })
			c.Base = origin.URL
			c.Sleep = func(_ time.Duration) {}
			err := c.Pages(context.Background(), "/me/messages", nil, func(graph.Page) error { return nil })
			if err == nil || outsideCalls.Load() != 0 || originCalls.Load() != 1 {
				t.Fatalf("err=%v outside calls=%d origin calls=%d", err, outsideCalls.Load(), originCalls.Load())
			}
		})
	}
}

func TestUnsafeAbsoluteURLsAreRejectedBeforeObtainingToken(t *testing.T) {
	for _, target := range []string{"http://graph.example.net/me", "https://other.example.net/me", "//other.example.net/me", "https://reader:fixture@graph.example.net/me", "https://graph.example.net/me#fragment", "https://graph.example.net:444/me", "https://graph.example.net:443/me", "https://graph.example.net./me"} {
		t.Run(target, func(t *testing.T) {
			var tokens int
			c := graph.New(func(context.Context) (string, error) { tokens++; return "synthetic-token", nil })
			c.Base = "https://graph.example.net/v1.0"
			err := c.Get(context.Background(), target, nil, nil)
			if err == nil || tokens != 0 {
				t.Fatalf("unsafe URL accepted: err=%v tokens=%d", err, tokens)
			}
		})
	}
}

func TestSameOriginRedirectKeepsAuthAndCustomRedirectPolicy(t *testing.T) {
	c, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/finish", http.StatusFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer TOKEN" {
			t.Error("redirect lost auth")
		}
		fmt.Fprint(w, `{}`)
	})
	if err := c.Get(context.Background(), "/start", nil, nil); err != nil {
		t.Fatal(err)
	}
	c.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return fmt.Errorf("custom redirect refusal") }
	if err := c.Get(context.Background(), "/start", nil, nil); err == nil || !strings.Contains(err.Error(), "custom redirect refusal") {
		t.Fatalf("custom policy lost: %v", err)
	}
}
