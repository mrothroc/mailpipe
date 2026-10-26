package template

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mrothroc/mailpipe/pkg/stage"
)

// Outcome labels of the template stage for a sender with templates. A match
// also carries the template's id, and Filed when that template is filed. New
// is a kind of email the sender has no template for.
const (
	Matched = "matched"
	Review  = "review"
	New     = "new"
	Filed   = "file"
)

// NoTemplate is the reason given for a sender with no templates.
const NoTemplate = "no template for this sender"

// none is Jev's option for "no template fits"; Parse reserves it.
const none = "none"

// Stage recognises a message's template: the deterministic fingerprint and
// Jev's choice between the sender's template descriptions must agree.
type Stage struct {
	name      string
	set       *Set
	client    *stage.JevClient
	threshold float64
	budget    *stage.Budget
	loc       *time.Location
}

// NewStage returns a template stage. Jev's choice counts only at or above
// threshold; every call is charged to budget; dates are read in loc.
func NewStage(name string, set *Set, client *stage.JevClient, threshold float64, budget *stage.Budget, loc *time.Location) *Stage {
	return &Stage{name: name, set: set, client: client, threshold: threshold, budget: budget, loc: loc}
}

func (s *Stage) Name() string    { return s.name }
func (s *Stage) NeedsBody() bool { return true }

// NeedsBodyOf reports a sender with templates: the rest get no opinion, and
// no body is fetched for them.
func (s *Stage) NeedsBodyOf(m stage.Message) bool { return len(s.set.For(m.Sender)) > 0 }

func (s *Stage) Evaluate(ctx context.Context, m stage.Message) (stage.Verdict, error) {
	candidates := s.set.For(m.Sender)
	if len(candidates) == 0 {
		// No opinion: most of an unknown sender's mail is not a record, so
		// the queue picks out the records among these (file queue --records).
		return stage.Verdict{Stage: s.name, Reason: NoTemplate, Provenance: "template"}, nil
	}
	var fits []*Template
	var misses []string
	options := map[string]string{none: "None of these: a different kind of email from this sender, or one whose layout has changed."}
	for _, t := range candidates {
		options[t.ID] = t.Describe
		if r := t.Match(m, s.loc); r.OK {
			fits = append(fits, t)
		} else {
			misses = append(misses, t.ID+": "+r.Why)
		}
	}
	a, err := s.client.Choose(ctx, s.budget, stage.State(m), stage.Choice{Key: "template", Options: options,
		Instructions: "Which description fits this email? Choose none unless one clearly fits."})
	v := stage.Verdict{Stage: s.name, CostUSD: a.Cost, Provenance: "template+jev:" + s.client.Model}
	if err != nil {
		return v, err
	}
	v.Scores = a.Probabilities
	p := a.Probabilities[a.Choice]
	jev := fmt.Sprintf("jev %s %.2f", a.Choice, p)
	ids := make([]string, len(fits))
	for i, t := range fits {
		ids[i] = t.ID
	}
	switch {
	case len(fits) == 1 && a.Choice == fits[0].ID && p >= s.threshold:
		v.Labels = []string{Matched, fits[0].ID}
		if fits[0].Files() {
			v.Labels = append(v.Labels, Filed)
		}
		v.Reason = "fingerprint and " + jev
	case len(fits) == 0 && a.Choice == none:
		v.Labels = []string{New}
		v.Reason = jev + "; " + strings.Join(misses, "; ")
	default:
		v.Labels = []string{Review}
		fp := "fingerprint " + none
		if len(fits) > 0 {
			fp = "fingerprint " + strings.Join(ids, ", ")
		}
		v.Reason = fp + "; " + jev
		if len(misses) > 0 {
			v.Reason += "; " + strings.Join(misses, "; ")
		}
	}
	return v, nil
}
