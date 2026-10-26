package store_test

import (
	"context"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/store"
)

func seed(t *testing.T, s *store.Store) []store.Message {
	t.Helper()
	ctx := context.Background()
	for _, r := range []struct {
		folder string
		rec    store.Record
	}{
		{"inbox", store.Record{GraphID: "a", Sender: "news@campaign.example.org", Received: "2026-10-01T00:00:00Z"}},
		{"inbox", store.Record{GraphID: "b", Sender: "friend@example.org", Received: "2026-10-02T00:00:00Z"}},
		{"sentitems", store.Record{GraphID: "c", Sender: "alex@example.com", Recipients: []string{"Friend@Example.org", "other@example.net"}}},
		{"sentitems", store.Record{GraphID: "d", Sender: "alex@example.com", Recipients: []string{"friend@example.org"}}},
	} {
		if _, err := s.Upsert(ctx, r.folder, r.rec); err != nil {
			t.Fatal(err)
		}
	}
	ms, err := s.Messages(ctx, store.Filter{Folder: "inbox"})
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func TestVerdictsAreStoredWithProvenanceAndListedPerMessage(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ms := seed(t, s)
	v := store.Verdict{N: ms[0].N, Stage: "political", Labels: []string{"political_appeal"},
		Scores: map[string]float64{"political_appeal": 0.97}, Action: "", Reason: "political_appeal 0.97",
		CostUSD: 0.0002, Provenance: "jev:typesafe/jev-1.14"}
	if err := s.AddVerdict(ctx, v); err != nil {
		t.Fatal(err)
	}
	if err := s.AddVerdict(ctx, store.Verdict{N: ms[0].N, Stage: "political", Error: "timed out"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Verdicts(ctx, ms[0].N)
	if err != nil || len(got) != 2 {
		t.Fatalf("verdicts %+v err %v", got, err)
	}
	first := got[0]
	if first.Stage != "political" || first.Scores["political_appeal"] != 0.97 || first.Labels[0] != "political_appeal" ||
		first.Provenance != "jev:typesafe/jev-1.14" || first.At == "" || got[1].Error != "timed out" {
		t.Fatalf("verdicts %+v", got)
	}
}

func TestPendingSkipsMessagesWithASuccessfulVerdictFromTheStage(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ms := seed(t, s) // newest first: b, a
	_ = s.AddVerdict(ctx, store.Verdict{N: ms[1].N, Stage: "political"})
	_ = s.AddVerdict(ctx, store.Verdict{N: ms[0].N, Stage: "political", Error: "timed out"}) // a failure is retried
	_ = s.AddVerdict(ctx, store.Verdict{N: ms[0].N, Stage: "other"})
	pending, err := s.Pending(ctx, store.Filter{Folder: "inbox"}, "political")
	if err != nil || len(pending) != 1 || pending[0].N != ms[0].N {
		t.Fatalf("pending %+v err %v", pending, err)
	}
}

func TestSentRecipientsAreDistinctAndLowerCase(t *testing.T) {
	s, _ := open(t)
	seed(t, s)
	got, err := s.SentRecipients(context.Background())
	if err != nil || len(got) != 2 || got[0] != "friend@example.org" || got[1] != "other@example.net" {
		t.Fatalf("recipients %v err %v", got, err)
	}
}
