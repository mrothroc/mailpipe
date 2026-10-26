package stage_test

import (
	"context"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/stage"
)

type history map[string][]stage.Prior

func (h history) lookup(_ context.Context, sender string, exclude int64) ([]stage.Prior, error) {
	var out []stage.Prior
	for _, p := range h[sender] {
		if p.N != exclude {
			out = append(out, p)
		}
	}
	return out, nil
}

func TestSenderCacheReusesConsistentHistory(t *testing.T) {
	h := history{
		"loud@campaign.example.org": {{N: 1, Labelled: true, Score: 0.9}, {N: 2, Labelled: true, Score: 0.96}, {N: 3, Labelled: true, Score: 0.99}},
		"quiet@example.net":         {{N: 4, Score: 0.01}, {N: 5, Score: 0.03}, {N: 6, Score: 0.02}},
		"mixed@example.org":         {{N: 7, Labelled: true, Score: 0.9}, {N: 8, Score: 0.1}, {N: 9, Labelled: true, Score: 0.8}},
		"new@example.org":           {{N: 10, Labelled: true, Score: 0.9}},
	}
	s := stage.NewSenderCache("political-cached", "political", "political_appeal", 3, h.lookup)
	cases := map[string]struct {
		labelled bool
		score    float64
		empty    bool
	}{
		"loud@campaign.example.org": {labelled: true, score: 0.95},
		"quiet@example.net":         {score: 0.02},
		"mixed@example.org":         {empty: true},
		"new@example.org":           {empty: true},
	}
	for sender, want := range cases {
		m := msg()
		m.N, m.Sender = 100, sender
		v, err := s.Evaluate(context.Background(), m)
		if err != nil || v.Validate() != nil || v.CostUSD != 0 || s.NeedsBody() {
			t.Fatalf("%s: verdict %+v err %v", sender, v, err)
		}
		if want.empty {
			if len(v.Labels) != 0 || len(v.Scores) != 0 {
				t.Errorf("%s: verdict %+v, want none", sender, v)
			}
			continue
		}
		if (len(v.Labels) == 1) != want.labelled || abs(v.Scores["political_appeal"]-want.score) > 1e-9 || v.Provenance != "sender_cache:political" {
			t.Errorf("%s: verdict %+v, want labelled=%v score %.2f", sender, v, want.labelled, want.score)
		}
	}
}

func TestSenderCacheNeverCountsTheMessageItself(t *testing.T) {
	h := history{"loud@campaign.example.org": {{N: 1, Labelled: true, Score: 0.9}, {N: 2, Labelled: true, Score: 0.9}, {N: 3, Labelled: true, Score: 0.9}}}
	s := stage.NewSenderCache("political-cached", "political", "political_appeal", 3, h.lookup)
	m := msg()
	m.N, m.Sender = 3, "loud@campaign.example.org" // only 2 other verdicts remain
	if v, _ := s.Evaluate(context.Background(), m); len(v.Labels) != 0 {
		t.Fatalf("message counted toward its own history: %+v", v)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
