package stage

import (
	"context"
	"fmt"
)

// Prior is one earlier verdict from a source stage for another message from
// the same sender.
type Prior struct {
	N        int64
	Labelled bool    // the source label was present
	Score    float64 // the source label's score
}

// SenderCache reuses a source stage's verdicts: when a sender's other messages
// have at least min verdicts from that stage and they all agree on the label,
// it returns the same judgement (labelled or not) with the mean score, at no
// cost. Mixed or too little history gives no verdict, so the message can go
// to the source stage instead.
type SenderCache struct {
	name, source, label string
	min                 int
	lookup              func(ctx context.Context, sender string, exclude int64) ([]Prior, error)
}

// NewSenderCache builds a SenderCache over lookup, which returns the sender's
// latest source-stage verdicts for messages other than exclude.
func NewSenderCache(name, source, label string, min int, lookup func(ctx context.Context, sender string, exclude int64) ([]Prior, error)) *SenderCache {
	return &SenderCache{name: name, source: source, label: label, min: min, lookup: lookup}
}

func (s *SenderCache) Name() string    { return s.name }
func (s *SenderCache) NeedsBody() bool { return false }

func (s *SenderCache) Evaluate(ctx context.Context, m Message) (Verdict, error) {
	v := Verdict{Stage: s.name, Provenance: "sender_cache:" + s.source}
	prior, err := s.lookup(ctx, m.Sender, m.N)
	if err != nil || len(prior) < s.min {
		return v, err
	}
	labelled, sum := 0, 0.0
	for _, p := range prior {
		if p.Labelled {
			labelled++
		}
		sum += p.Score
	}
	if labelled != 0 && labelled != len(prior) {
		return v, nil // the sender's mail is judged both ways: no shortcut
	}
	v.Scores = map[string]float64{s.label: sum / float64(len(prior))}
	if labelled > 0 {
		v.Labels = []string{s.label}
	}
	v.Reason = fmt.Sprintf("%d earlier %s verdicts for this sender agree", len(prior), s.source)
	return v, nil
}
