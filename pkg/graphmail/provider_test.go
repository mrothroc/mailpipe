package graphmail_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

var account = mailbox.Account{ID: "invented-account", ProviderID: "graph"}

const received = "2026-10-20T10:00:00Z"

type serverBox struct {
	mu             sync.Mutex
	folders        map[string]string
	sender         string
	delta          bool
	gone           bool
	uncertain      bool
	malformedMove  bool
	incompleteRead bool
	badDelta       string
	largeText      bool
	moveRequests   int
	sizes          []int
	tokens         atomic.Int32
}

func (b *serverBox) wire(id string) map[string]any {
	return map[string]any{"id": id, "internetMessageId": "<" + id + "@example.org>", "subject": "Complete fixture", "sender": map[string]any{"emailAddress": map[string]any{"address": b.sender}}, "toRecipients": []any{map[string]any{"emailAddress": map[string]any{"address": "to@example.org"}}}, "ccRecipients": []any{map[string]any{"emailAddress": map[string]any{"address": "cc@example.net"}}}, "receivedDateTime": received, "parentFolderId": b.folders[id], "changeKey": "revision-1", "isRead": b.delta, "hasAttachments": false, "bodyPreview": ""}
}
func fixture(t *testing.T, n int) (*graphmail.Provider, *serverBox) {
	t.Helper()
	b := &serverBox{folders: map[string]string{}, sender: "sender@example.org"}
	for i := 1; i <= n; i++ {
		b.folders[fmt.Sprintf("m%d", i)] = "F-inbox"
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
			t.Error("immutable preference missing")
		}
		path := strings.TrimPrefix(r.URL.Path, "/v1")
		write := func(v any) {
			if err := json.NewEncoder(w).Encode(v); err != nil {
				t.Error(err)
			}
		}
		switch {
		case path == "/me/mailFolders/deleteditems":
			write(map[string]any{"id": "F-trash"})
		case path == "/me/mailFolders/inbox":
			write(map[string]any{"id": "F-inbox"})
		case path == "/me/mailFolders/F-inbox/messages/delta":
			switch b.badDelta {
			case "empty":
				return
			case "shape":
				write(map[string]any{"@odata.deltaLink": server.URL + "/v1/changes"})
				return
			case "terminal":
				write(map[string]any{"value": []any{}})
				return
			}
			if !strings.Contains(r.Header.Get("Prefer"), "odata.maxpagesize=200") {
				t.Error("sync page-size preference lost")
			}
			write(map[string]any{"value": []any{b.wire("m1")}, "@odata.deltaLink": server.URL + "/v1/changes"})
		case path == "/changes":
			write(map[string]any{"value": []any{map[string]any{"id": "m1", "isRead": true}}, "@odata.deltaLink": server.URL + "/v1/changes"})
		case path == "/$batch":
			var body struct {
				Requests []struct {
					ID, Method, URL string
					Headers         map[string]string
					Body            map[string]string
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			b.sizes = append(b.sizes, len(body.Requests))
			var responses []any
			for _, request := range body.Requests {
				if !strings.Contains(request.Headers["Prefer"], "ImmutableId") {
					t.Error("subrequest immutable preference missing")
				}
				u, _ := url.Parse(request.URL)
				id := strings.TrimPrefix(u.Path, "/me/messages/")
				id, move := strings.CutSuffix(id, "/move")
				response := map[string]any{"id": request.ID, "status": 200}
				if move {
					b.moveRequests++
					if b.uncertain {
						w.WriteHeader(503)
						return
					}
					if request.Body["destinationId"] != "F-trash" {
						t.Error("wrong recoverable destination")
					}
					b.folders[id] = "F-trash"
					response["status"] = 201
				}
				if _, ok := b.folders[id]; !ok {
					response["status"] = 404
				} else if move && b.malformedMove {
					response["body"] = map[string]any{}
				} else {
					response["body"] = b.wire(id)
				}
				responses = append(responses, response)
			}
			// Real Graph may reorder answers; transport must restore request order.
			for i, j := 0, len(responses)-1; i < j; i, j = i+1, j-1 {
				responses[i], responses[j] = responses[j], responses[i]
			}
			write(map[string]any{"responses": responses})
		case strings.HasSuffix(path, "/$value"):
			_, _ = w.Write([]byte("From: sender@example.org\r\nSubject: fixture\r\n\r\ncomplete source tail"))
		case strings.HasPrefix(path, "/me/messages/"):
			id := strings.TrimPrefix(path, "/me/messages/")
			if b.gone {
				w.WriteHeader(404)
				write(map[string]any{"error": map[string]string{"code": "ErrorItemNotFound"}})
				return
			}
			if b.incompleteRead {
				if r.URL.Query().Get("$select") == "body" {
					write(map[string]any{"body": map[string]string{"contentType": "text"}})
				} else {
					write(map[string]any{"id": id, "parentFolderId": b.folders[id]})
				}
				return
			}
			if r.URL.Query().Get("$select") == "body" {
				if b.largeText {
					_, _ = w.Write([]byte(`{"body":{"contentType":"text","content":"`))
					chunk := bytes.Repeat([]byte("x"), 32768)
					for n := 0; n < 2049; n++ {
						if _, err := w.Write(chunk); err != nil {
							return
						}
					}
					_, _ = w.Write([]byte(`"}}`))
					return
				}
				write(map[string]any{"body": map[string]string{"contentType": "text", "content": "complete text tail"}})
				return
			}
			write(b.wire(id))
		default:
			t.Errorf("unexpected path %s", path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	client := graph.New(func(context.Context) (string, error) { b.tokens.Add(1); return "invented-fixture-token", nil })
	client.Base = server.URL + "/v1"
	p, err := graphmail.New(client, account)
	if err != nil {
		t.Fatal(err)
	}
	return p, b
}
func ref(id string) mailbox.MessageRef {
	return mailbox.MessageRef{AccountID: account.ID, MessageID: mailbox.MessageID(id), Locator: mailbox.Locator(id)}
}
func expected(id string) mailbox.State {
	date, _ := time.Parse(time.RFC3339, received)
	return mailbox.State{Ref: ref(id), Metadata: mailbox.Metadata{InternetMessageID: "<" + id + "@example.org>", Sender: mailbox.Address{Address: "sender@example.org"}, Received: date}, Memberships: []mailbox.Membership{{Collection: mailbox.CollectionRef{AccountID: account.ID, CollectionID: "F-inbox"}}}, Revision: "revision-1"}
}
func TestGraphProviderReadsAndHydrates(t *testing.T) {
	p, b := fixture(t, 1)
	ctx := context.Background()
	scope := mailbox.CollectionRef{AccountID: account.ID, CollectionID: "F-inbox"}
	page, states, err := p.SyncStates(ctx, mailbox.SyncRequest{Collection: scope})
	if err != nil || !page.Complete || len(states) != 1 || len(states[0].Metadata.Recipients) != 2 || states[0].Metadata.Sender.Address != "sender@example.org" {
		t.Fatalf("%+v %+v %v", page, states, err)
	}
	b.mu.Lock()
	b.delta = true
	b.mu.Unlock()
	next, err := p.Sync(ctx, mailbox.SyncRequest{Collection: scope, Cursor: page.Checkpoint})
	if err != nil || len(next.Messages) != 1 || !next.Messages[0].Metadata.IsRead {
		t.Fatalf("partial hydration %+v %v", next, err)
	}
	b.mu.Lock()
	b.gone = true
	b.mu.Unlock()
	gone, err := p.Sync(ctx, mailbox.SyncRequest{Collection: scope, Cursor: next.Checkpoint})
	if err != nil || len(gone.Removed) != 1 {
		t.Fatalf("vanished %+v %v", gone, err)
	}
	before := b.tokens.Load()
	bad := ref("m1")
	bad.AccountID = "other"
	if _, err = p.Text(ctx, bad); !errors.Is(err, mailbox.ErrAccountMismatch) || b.tokens.Load() != before {
		t.Fatal("cross-account transport", err)
	}
	if _, err = p.Sync(ctx, mailbox.SyncRequest{Collection: scope, Cursor: mailbox.Cursor{AccountID: account.ID, CollectionID: scope.CollectionID, Token: "unrecognized"}}); !errors.Is(err, mailbox.ErrStaleIdentity) || b.tokens.Load() != before {
		t.Fatal("bad cursor transport", err)
	}
	b.mu.Lock()
	b.gone = false
	b.mu.Unlock()
	text, err := p.Text(ctx, ref("m1"))
	if err != nil || text != "complete text tail" {
		t.Fatal(text, err)
	}
	var source bytes.Buffer
	if err = p.WriteMIME(ctx, ref("m1"), &source); err != nil || !strings.HasSuffix(source.String(), "complete source tail") {
		t.Fatal(source.String(), err)
	}
}
func TestGraphReviewedBatchesAndUncertainty(t *testing.T) {
	t.Run("ordered chunks and complete returned state", func(t *testing.T) {
		p, b := fixture(t, 45)
		var requests []mailbox.ReviewedAction
		for i := 1; i <= 45; i++ {
			requests = append(requests, mailbox.ReviewedAction{PlanID: 1, Local: mailbox.LocalRef{AccountID: account.ID, N: int64(i)}, Expected: expected(fmt.Sprintf("m%d", i))})
		}
		states, errs, err := p.TrashBatch(context.Background(), requests)
		if err != nil {
			t.Fatal(err)
		}
		for i, state := range states {
			if errs[i] != nil || state.Ref != requests[i].Expected.Ref || state.Memberships[0].Collection.CollectionID != "F-trash" {
				t.Fatalf("result %d %+v %v", i, state, errs[i])
			}
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.moveRequests != 45 {
			t.Fatal(b.moveRequests)
		}
		for _, size := range b.sizes {
			if size > 20 {
				t.Fatal("oversized batch", size)
			}
		}
	})
	for _, mode := range []string{"changed sender", "uncertain", "partial move response"} {
		t.Run(mode, func(t *testing.T) {
			p, b := fixture(t, 1)
			b.mu.Lock()
			switch mode {
			case "changed sender":
				b.sender = "changed@example.net"
			case "uncertain":
				b.uncertain = true
			case "partial move response":
				b.malformedMove = true
			}
			b.mu.Unlock()
			states, errs, err := p.TrashBatch(context.Background(), []mailbox.ReviewedAction{{PlanID: 1, Local: mailbox.LocalRef{AccountID: account.ID, N: 1}, Expected: expected("m1")}})
			b.mu.Lock()
			defer b.mu.Unlock()
			switch mode {
			case "changed sender":
				if err != nil || !errors.Is(errs[0], mailbox.ErrStaleIdentity) || b.moveRequests != 0 {
					t.Fatalf("changed target %v %v moves %d", err, errs, b.moveRequests)
				}
			case "uncertain":
				if err == nil || b.moveRequests != 1 {
					t.Fatalf("uncertain replay %v moves %d", err, b.moveRequests)
				}
			case "partial move response":
				if err != nil || errs[0] != nil || states[0].Ref != ref("m1") {
					t.Fatalf("missing complete hydration %+v %v %v", states, errs, err)
				}
			}
		})
	}
}

func TestIncompleteProviderReadsFail(t *testing.T) {
	p, b := fixture(t, 1)
	b.mu.Lock()
	b.incompleteRead = true
	b.mu.Unlock()
	if _, err := p.Inspect(context.Background(), ref("m1")); err == nil {
		t.Fatal("partial metadata succeeded")
	}
	if _, err := p.Text(context.Background(), ref("m1")); err == nil {
		t.Fatal("missing content became successful empty body")
	}
}

func TestHydratedMoveDetachesOnlyRequestedScope(t *testing.T) {
	p, b := fixture(t, 1)
	ctx := context.Background()
	scope := mailbox.CollectionRef{AccountID: account.ID, CollectionID: "F-inbox"}
	initial, err := p.Sync(ctx, mailbox.SyncRequest{Collection: scope})
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.folders["m1"] = "F-trash"
	b.mu.Unlock()
	page, err := p.Sync(ctx, mailbox.SyncRequest{Collection: scope, Cursor: initial.Checkpoint})
	if err != nil || len(page.Messages) != 0 || len(page.Removed) != 1 || page.Removed[0] != ref("m1") {
		t.Fatalf("moved hydration forged source membership: %+v %v", page, err)
	}
}

func TestIncompleteDeltaNeverEstablishesAbsence(t *testing.T) {
	for _, mode := range []string{"empty", "shape", "terminal"} {
		t.Run(mode, func(t *testing.T) {
			p, b := fixture(t, 1)
			b.mu.Lock()
			b.badDelta = mode
			b.mu.Unlock()
			if page, err := p.Sync(context.Background(), mailbox.SyncRequest{Collection: mailbox.CollectionRef{AccountID: account.ID, CollectionID: "F-inbox"}}); err == nil {
				t.Fatalf("missing terminal evidence succeeded: %+v", page)
			}
		})
	}
}

func TestTextLimitUsesCanonicalCompleteOrErrorDetail(t *testing.T) {
	p, b := fixture(t, 1)
	b.mu.Lock()
	b.largeText = true
	b.mu.Unlock()
	text, err := p.Text(context.Background(), ref("m1"))
	if text != "" || err == nil || !strings.Contains(err.Error(), "read text: message exceeds 67108864-byte content limit; incomplete content was discarded") {
		t.Fatalf("text limit %d %v", len(text), err)
	}
}
