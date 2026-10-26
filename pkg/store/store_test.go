package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/store"
)

func open(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "mail.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestSenderFilterTreatsWildcardsLiterally(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	for _, r := range []store.Record{{GraphID: "a", Sender: "info_x@campaign.example.org"}, {GraphID: "b", Sender: "infoyx@campaign.example.org"}} {
		if _, err := s.Upsert(ctx, "inbox", r); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Count(ctx, store.Filter{SenderLike: "INFO_X"})
	if err != nil || n != 1 {
		t.Fatalf("count = %d, %v; '_' must not match any character", n, err)
	}
}

func TestGetMissingAndBodyCache(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if _, ok, err := s.Get(ctx, 99); ok || err != nil {
		t.Fatalf("Get(99) ok=%v err=%v", ok, err)
	}
	if _, err := s.Upsert(ctx, "inbox", store.Record{GraphID: "a"}); err != nil {
		t.Fatal(err)
	}
	ms, _ := s.Messages(ctx, store.Filter{})
	if ms[0].Body != nil {
		t.Fatal("body cached before it was fetched")
	}
	if err := s.SetBody(ctx, ms[0].N, "hello"); err != nil {
		t.Fatal(err)
	}
	m, ok, _ := s.Get(ctx, ms[0].N)
	if !ok || m.Body == nil || *m.Body != "hello" {
		t.Fatalf("Get = %+v", m)
	}
}

func TestDatabaseIsPrivate(t *testing.T) {
	_, path := open(t)
	for p, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s mode %v, want %v", p, info.Mode().Perm(), want)
		}
	}
}

func TestSinceKeepsMessagesReceivedAtOrAfterIt(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	for id, at := range map[string]string{"old": "2026-10-20T10:00:00Z", "edge": "2026-10-21T09:30:00Z", "new": "2026-10-22T08:00:00Z"} {
		if _, err := s.Upsert(ctx, "inbox", store.Record{GraphID: id, Sender: "a@example.org", Received: at}); err != nil {
			t.Fatal(err)
		}
	}
	since := time.Date(2026, 10, 21, 2, 30, 0, 0, time.FixedZone("PDT", -7*3600)) // 09:30Z
	f := store.Filter{Folder: "inbox", Since: since}
	ms, err := s.Messages(ctx, f)
	if err != nil || len(ms) != 2 || ms[0].GraphID != "new" || ms[1].GraphID != "edge" {
		t.Fatalf("Messages = %v, %v", ms, err)
	}
	pending, err := s.Pending(ctx, f, "known")
	if err != nil || len(pending) != 2 {
		t.Fatalf("Pending = %d, %v", len(pending), err)
	}
	if n, _ := s.Count(ctx, store.Filter{}); n != 3 {
		t.Fatalf("a zero Since must not filter: %d", n)
	}
}

func TestUnreadKeepsOnlyUnreadMessages(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	for id, read := range map[string]bool{"seen": true, "fresh": false} {
		if _, err := s.Upsert(ctx, "inbox", store.Record{GraphID: id, Sender: "a@example.org", IsRead: read}); err != nil {
			t.Fatal(err)
		}
	}
	ms, err := s.Messages(ctx, store.Filter{Unread: true})
	if err != nil || len(ms) != 1 || ms[0].GraphID != "fresh" {
		t.Fatalf("Messages = %v, %v", ms, err)
	}
}

// Labelled follows each message's latest successful verdict from the stage,
// ignores failures and other stages, and a malformed value matches nothing.
func TestLabelledFollowsTheLatestSuccessfulVerdict(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ms := seed(t, s)
	add := func(n int64, stage string, labels []string, errText string) {
		t.Helper()
		if err := s.AddVerdict(ctx, store.Verdict{N: n, Stage: stage, Labels: labels, Error: errText, Provenance: "p"}); err != nil {
			t.Fatal(err)
		}
	}
	add(ms[0].N, "category", []string{"event"}, "")
	add(ms[0].N, "category", nil, "timed out") // a later failure does not count
	add(ms[1].N, "category", []string{"event"}, "")
	add(ms[1].N, "category", []string{"promotion"}, "") // relabelled since
	add(ms[1].N, "other", []string{"event"}, "")        // another stage's label does not count
	got, err := s.Messages(ctx, store.Filter{Labelled: " category : event "})
	if err != nil || len(got) != 1 || got[0].N != ms[0].N {
		t.Fatalf("got %+v err %v", got, err)
	}
	for _, bad := range []string{"event", ":event", "category:"} {
		if got, _ := s.Messages(ctx, store.Filter{Labelled: bad}); len(got) != 0 {
			t.Fatalf("%q matched %d messages, want none", bad, len(got))
		}
	}
}
