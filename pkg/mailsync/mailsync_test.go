package mailsync_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/mailsync"
	"github.com/mrothroc/mailpipe/pkg/store"
)

func msg(id, sender, subject, received string) json.RawMessage {
	m := map[string]any{"id": id, "internetMessageId": "<" + id + "@example.org>", "subject": subject,
		"from":             map[string]any{"emailAddress": map[string]any{"address": sender, "name": "A Sender"}},
		"toRecipients":     []any{map[string]any{"emailAddress": map[string]any{"address": "Alex@Example.COM"}}},
		"receivedDateTime": received, "hasAttachments": true, "isRead": false, "bodyPreview": "preview"}
	m["ccRecipients"] = []any{}
	b, _ := json.Marshal(m)
	return b
}

func removed(id string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"id": id, "@removed": map[string]any{"reason": "deleted"}})
	return b
}

const when = "2026-10-20T10:00:00Z"

type fakeSource struct {
	pages     map[string][]graph.Page
	totals    map[string]int
	requested []string
	queries   []url.Values
	failAfter int // fail after this many pages when > 0
	// server holds each message's full record for Get; a missing id is 404.
	server  map[string]json.RawMessage
	fetched []string
}

func (f *fakeSource) Get(_ context.Context, path string, q url.Values, out any) error {
	if pages, ok := f.pages[path]; ok {
		f.requested = append(f.requested, path)
		f.queries = append(f.queries, q)
		if f.failAfter > 0 && len(f.requested) > f.failAfter {
			return errors.New("network")
		}
		var page graph.Page
		if len(pages) > 0 {
			page = pages[0]
			if len(pages) > 1 {
				f.pages[page.NextLink] = pages[1:]
			}
		}
		if page.Value == nil {
			page.Value = []json.RawMessage{}
		}
		raw, _ := json.Marshal(page)
		return json.Unmarshal(raw, out)
	}
	id := strings.TrimPrefix(path, "/me/messages/")
	f.fetched = append(f.fetched, id+"?"+q.Get("$select"))
	raw, ok := f.server[id]
	if !ok {
		return &graph.APIError{Status: 404, Code: "ErrorItemNotFound"}
	}
	return json.Unmarshal(raw, out)
}

func (f *fakeSource) Pages(_ context.Context, start string, q url.Values, fn func(graph.Page) error) error {
	f.requested = append(f.requested, start)
	f.queries = append(f.queries, q)
	for i, p := range f.pages[start] {
		if f.failAfter > 0 && i == f.failAfter {
			return errors.New("network")
		}
		if err := fn(p); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeSource) FolderTotal(_ context.Context, folder string) (int, error) {
	return f.totals[folder], nil
}

func start(folder string) string { return "/me/mailFolders/" + folder + "/messages/delta" }

var testAccount = mailbox.Account{ID: "fixture", ProviderID: "graph"}

type testStore struct{ *store.AccountStore }

func open(t *testing.T) *testStore {
	t.Helper()
	s, err := store.OpenAccount(context.Background(), filepath.Join(t.TempDir(), "mail.db"), testAccount)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &testStore{s}
}
func (s *testStore) Messages(ctx context.Context, f store.Filter) ([]store.Message, error) {
	af := store.AccountFilter{Filter: f}
	af.Filter.Folder = ""
	if f.Folder != "" {
		c := mailbox.CollectionRef{AccountID: testAccount.ID, CollectionID: mailbox.CollectionID(f.Folder)}
		af.Collection = &c
	}
	ms, err := s.AccountStore.Messages(ctx, af)
	var out []store.Message
	for _, m := range ms {
		v := store.Message{N: m.Local.N, GraphID: string(m.State.Ref.MessageID), InternetMessageID: m.State.Metadata.InternetMessageID, Sender: strings.ToLower(m.State.Metadata.Sender.Address), Subject: m.State.Metadata.Subject, HasAttachments: m.State.Metadata.HasAttachments, IsRead: m.State.Metadata.IsRead}
		if !m.State.Metadata.Received.IsZero() {
			v.Received = m.State.Metadata.Received.Format(time.RFC3339)
		}
		for _, r := range m.State.Metadata.Recipients {
			v.Recipients = append(v.Recipients, strings.ToLower(r.Address))
		}
		out = append(out, v)
	}
	return out, err
}
func (s *testStore) DeltaLink(ctx context.Context, folder string) (string, error) {
	c, e := s.Checkpoint(ctx, mailbox.CollectionRef{AccountID: testAccount.ID, CollectionID: mailbox.CollectionID(folder)})
	if e != nil || c.Token == "" {
		return "", e
	}
	var decoded struct {
		Link string `json:"link"`
	}
	e = json.Unmarshal([]byte(c.Token), &decoded)
	return decoded.Link, e
}
func (f *fakeSource) Do(ctx context.Context, method, path string, q url.Values, _ string, _ any, out any) error {
	if method == "GET" {
		return f.Get(ctx, path, q, out)
	}
	return errors.New("unexpected write transport")
}
func (f *fakeSource) Batch(context.Context, []graph.BatchRequest) ([]graph.BatchResponse, error) {
	return nil, errors.New("unexpected batch transport")
}
func (f *fakeSource) WriteRaw(context.Context, string, io.Writer, int64) error {
	return errors.New("unexpected MIME transport")
}
func (f *fakeSource) Account() mailbox.Account { return testAccount }
func (f *fakeSource) Total(_ context.Context, c mailbox.CollectionRef) (int, error) {
	return f.totals[string(c.CollectionID)], nil
}
func (f *fakeSource) ReadPage(ctx context.Context, r mailbox.SyncRequest) (mailbox.SyncPage, []store.MessageRecord, error) {
	p, err := graphmail.New(f, testAccount)
	if err != nil {
		return mailbox.SyncPage{}, nil, err
	}
	page, states, err := p.SyncStates(ctx, r)
	var records []store.MessageRecord
	for _, state := range states {
		records = append(records, store.MessageRecord{Message: mailbox.Message{Ref: state.Ref, Metadata: state.Metadata}, GraphID: string(state.Ref.MessageID), Revision: state.Revision})
	}
	return page, records, err
}
func syncFolder(ctx context.Context, s *testStore, src *fakeSource, folder string, full bool) (mailsync.Result, error) {
	return mailsync.SyncCollection(ctx, s.AccountStore, src, mailbox.Collection{Ref: mailbox.CollectionRef{AccountID: testAccount.ID, CollectionID: mailbox.CollectionID(folder)}, Name: folder}, full)
}

func ids(t *testing.T, s *testStore, folder string) string {
	t.Helper()
	ms, err := s.Messages(context.Background(), store.Filter{Folder: folder})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range ms {
		out = append(out, m.GraphID)
	}
	return strings.Join(out, ",")
}

func TestFirstSyncStoresMessagesAndDeltaLink(t *testing.T) {
	s := open(t)
	src := &fakeSource{pages: map[string][]graph.Page{start("inbox"): {
		{Value: []json.RawMessage{msg("g1", "a@example.org", "Hello", when), msg("g2", "B@Example.NET", "Hi", when)}, NextLink: "next"},
		{Value: []json.RawMessage{msg("g3", "a@example.org", "Yo", when)}, DeltaLink: "delta-1"},
	}}, totals: map[string]int{"inbox": 3}}
	r, err := syncFolder(context.Background(), s, src, "inbox", false)
	if err != nil {
		t.Fatal(err)
	}
	if r != (mailsync.Result{Folder: "inbox", Added: 3, Local: 3, Remote: 3}) {
		t.Fatalf("result %+v", r)
	}
	if got := src.queries[0].Get("$select"); got != "id,internetMessageId,subject,from,sender,toRecipients,ccRecipients,receivedDateTime,hasAttachments,isRead,bodyPreview,parentFolderId,changeKey" {
		t.Fatalf("$select = %q", got)
	}
	if got := ids(t, s, "inbox"); got != "g1,g2,g3" {
		t.Fatalf("ids %s", got)
	}
	ms, _ := s.Messages(context.Background(), store.Filter{SenderLike: "example.net"})
	if len(ms) != 1 || ms[0].Sender != "b@example.net" || ms[0].Recipients[0] != "alex@example.com" || ms[0].InternetMessageID != "<g2@example.org>" || !ms[0].HasAttachments {
		t.Fatalf("message %+v", ms)
	}
	if link, _ := s.DeltaLink(context.Background(), "inbox"); link != "delta-1" {
		t.Fatalf("delta link %q", link)
	}
}

func TestSecondSyncResumesFromDeltaLink(t *testing.T) {
	s := open(t)
	src := &fakeSource{pages: map[string][]graph.Page{
		start("inbox"): {{Value: []json.RawMessage{msg("g1", "a@example.org", "a", when)}, DeltaLink: "delta-1"}},
		"delta-1":      {{Value: []json.RawMessage{msg("g2", "a@example.org", "b", when)}, DeltaLink: "delta-2"}},
	}, totals: map[string]int{"inbox": 2}}
	ctx := context.Background()
	if _, err := syncFolder(ctx, s, src, "inbox", false); err != nil {
		t.Fatal(err)
	}
	r, err := syncFolder(ctx, s, src, "inbox", false)
	if err != nil {
		t.Fatal(err)
	}
	if src.requested[1] != "delta-1" || src.queries[1] != nil || r.Added != 1 {
		t.Fatalf("requested %v result %+v", src.requested, r)
	}
}

func TestRemovalOnlyAffectsTheFolderItWasReportedIn(t *testing.T) {
	s := open(t)
	src := &fakeSource{pages: map[string][]graph.Page{
		start("inbox"):        {{Value: []json.RawMessage{msg("g1", "a@example.org", "a", when), msg("g2", "a@example.org", "b", when)}, DeltaLink: "di"}},
		start("deleteditems"): {{Value: []json.RawMessage{msg("g1", "a@example.org", "a", when)}, DeltaLink: "dd"}}, // moved, same immutable id
		"di":                  {{Value: []json.RawMessage{removed("g1")}, DeltaLink: "di2"}},
	}, totals: map[string]int{"inbox": 1, "deleteditems": 1}}
	ctx := context.Background()
	for _, f := range []string{"inbox", "deleteditems", "inbox"} {
		if _, err := syncFolder(ctx, s, src, f, false); err != nil {
			t.Fatal(err)
		}
	}
	if ids(t, s, "inbox") != "g2" || ids(t, s, "deleteditems") != "g1" {
		t.Fatalf("inbox=%s deleted=%s", ids(t, s, "inbox"), ids(t, s, "deleteditems"))
	}
}

func TestRemovalOfMessageStillInFolder(t *testing.T) {
	s := open(t)
	src := &fakeSource{pages: map[string][]graph.Page{
		start("inbox"): {{Value: []json.RawMessage{msg("g1", "a@example.org", "a", when)}, DeltaLink: "di"}},
		"di":           {{Value: []json.RawMessage{removed("g1")}, DeltaLink: "di2"}},
	}}
	ctx := context.Background()
	_, _ = syncFolder(ctx, s, src, "inbox", false)
	r, err := syncFolder(ctx, s, src, "inbox", false)
	if err != nil || r.Removed != 1 || ids(t, s, "inbox") != "" {
		t.Fatalf("result %+v err %v ids %q", r, err, ids(t, s, "inbox"))
	}
}

func TestFullResyncDropsMessagesGraphNoLongerHas(t *testing.T) {
	s := open(t)
	src := &fakeSource{pages: map[string][]graph.Page{
		start("inbox"): {{Value: []json.RawMessage{msg("g1", "a@example.org", "a", when), msg("g2", "a@example.org", "b", when)}, DeltaLink: "d"}},
	}}
	ctx := context.Background()
	_, _ = syncFolder(ctx, s, src, "inbox", false)
	src.pages[start("inbox")] = []graph.Page{{Value: []json.RawMessage{msg("g2", "a@example.org", "b", when)}, DeltaLink: "d2"}}
	r, err := syncFolder(ctx, s, src, "inbox", true)
	if err != nil || r.Removed != 1 || ids(t, s, "inbox") != "g2" {
		t.Fatalf("result %+v err %v", r, err)
	}
}

func TestInterruptedSyncNeverRecordsADeltaLink(t *testing.T) {
	s := open(t)
	src := &fakeSource{failAfter: 1, pages: map[string][]graph.Page{start("inbox"): {
		{Value: []json.RawMessage{msg("g1", "a@example.org", "a", when)}, NextLink: "n"},
		{Value: nil, DeltaLink: "d"},
	}}}
	if _, err := syncFolder(context.Background(), s, src, "inbox", false); err == nil {
		t.Fatal("no error")
	}
	if link, _ := s.DeltaLink(context.Background(), "inbox"); link != "" {
		t.Fatalf("delta link %q recorded for a partial pass", link)
	}
}

func TestLocalNumbersAreStableAcrossUpdates(t *testing.T) {
	s := open(t)
	src := &fakeSource{pages: map[string][]graph.Page{
		start("inbox"): {{Value: []json.RawMessage{msg("g1", "a@example.org", "a", when), msg("g2", "a@example.org", "b", when)}, DeltaLink: "d"}},
		"d":            {{Value: []json.RawMessage{msg("g1", "a@example.org", "Edited", when)}, DeltaLink: "d2"}},
	}}
	ctx := context.Background()
	_, _ = syncFolder(ctx, s, src, "inbox", false)
	before, _ := s.Messages(ctx, store.Filter{Folder: "inbox"})
	r, _ := syncFolder(ctx, s, src, "inbox", false)
	after, _ := s.Messages(ctx, store.Filter{Folder: "inbox"})
	if r.Updated != 1 || after[0].N != before[0].N || after[0].Subject != "Edited" {
		t.Fatalf("result %+v before %+v after %+v", r, before[0], after[0])
	}
}

// An incremental delta may report a changed message with only some of its
// properties. Storing that as is would blank the rest, so the full message is
// fetched instead; one that is gone by then is removed.
func TestAPartialDeltaRecordIsFetchedInFull(t *testing.T) {
	s := open(t)
	partial := func(id string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"id": id, "isRead": true})
		return b
	}
	src := &fakeSource{pages: map[string][]graph.Page{
		start("inbox"): {{Value: []json.RawMessage{msg("g1", "a@example.org", "Receipt", when), msg("g2", "b@example.org", "Gone", when)}, DeltaLink: "delta-1"}},
		"delta-1":      {{Value: []json.RawMessage{partial("g1"), partial("g2")}, DeltaLink: "delta-2"}},
	}, totals: map[string]int{"inbox": 1}, server: map[string]json.RawMessage{}}
	ctx := context.Background()
	if _, err := syncFolder(ctx, s, src, "inbox", false); err != nil {
		t.Fatal(err)
	}
	read := map[string]any{}
	_ = json.Unmarshal(msg("g1", "a@example.org", "Receipt", when), &read)
	read["isRead"] = true
	src.server["g1"], _ = json.Marshal(read)
	r, err := syncFolder(ctx, s, src, "inbox", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.fetched) != 2 || !strings.HasPrefix(src.fetched[0], "g1?") || !strings.Contains(src.fetched[0], "receivedDateTime") || r.Removed != 1 || r.Updated != 1 {
		t.Fatalf("fetched %v result %+v", src.fetched, r)
	}
	msgs, err := s.Messages(ctx, store.Filter{Folder: "inbox"})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messages %+v err %v", msgs, err)
	}
	m := msgs[0]
	if m.Sender != "a@example.org" || m.Subject != "Receipt" || m.Received != when || m.InternetMessageID != "<g1@example.org>" || !m.IsRead {
		t.Fatalf("stored %+v", m)
	}
}
