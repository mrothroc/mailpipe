package template_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/pkg/stage"
	"github.com/mrothroc/mailpipe/pkg/template"
)

// jevChoosing answers every choice question with choice at probability p,
// the rest spread over the other options.
func jevChoosing(t *testing.T, choice string, p float64, calls *atomic.Int32, asked *map[string]any) *stage.JevClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if asked != nil {
			*asked = req
		}
		q := req["questions"].(map[string]any)["template"].(map[string]any)
		options := q["criteria"].(map[string]any)
		probs := map[string]float64{}
		for k := range options {
			probs[k] = (1 - p) / float64(len(options)-1)
		}
		probs[choice] = p
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"template": map[string]any{"type": "choice", "choice": choice, "probabilities": probs}},
			"usage": map[string]any{"cost": 0.0002}})
	}))
	t.Cleanup(srv.Close)
	c := stage.NewJevClient("sk-test", "typesafe/jev-1.14")
	c.URL = srv.URL
	c.Sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func evaluate(t *testing.T, set *template.Set, msg stage.Message, choice string, p float64) (stage.Verdict, int32, map[string]any) {
	t.Helper()
	var calls atomic.Int32
	var asked map[string]any
	st := template.NewStage("template", set, jevChoosing(t, choice, p, &calls, &asked), 0.7, stage.NewBudget(1), pacific)
	v, err := st.Evaluate(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("invalid verdict %+v: %v", v, err)
	}
	return v, calls.Load(), asked
}

func TestStageMatchesWhenFingerprintAndJevAgree(t *testing.T) {
	set := mustParse(t, giftJSON)
	v, calls, asked := evaluate(t, set, giftMessage(), "gift-receipt", 0.95)
	if !reflect.DeepEqual(v.Labels, []string{"matched", "gift-receipt", "file"}) || calls != 1 || v.CostUSD != 0.0002 ||
		v.Provenance != "template+jev:typesafe/jev-1.14" || v.Scores["gift-receipt"] != 0.95 {
		t.Fatalf("verdict %+v calls %d", v, calls)
	}
	// Jev chooses between the sender's descriptions and none, reading the email.
	q := asked["questions"].(map[string]any)["template"].(map[string]any)
	options := q["criteria"].(map[string]any)
	if q["type"] != "choice" || options["gift-receipt"] != "A gift receipt for a donation, with a receipt number and a gift date" ||
		options["none"] == nil || len(options) != 2 || !strings.Contains(asked["state"].(string), "Subject: Gift Receipt") {
		t.Fatalf("question %v", asked)
	}
}

func TestStageMatchOfAnUnfiledTemplateHasNoFileLabel(t *testing.T) {
	set := mustParse(t, giftJSON)
	msg := stage.Message{Sender: "rides@example.net", Subject: "Your ride with Sam", Received: "2026-01-05T17:00:00Z"}
	v, _, _ := evaluate(t, set, msg, "ride", 0.9)
	if !reflect.DeepEqual(v.Labels, []string{"matched", "ride"}) {
		t.Fatalf("verdict %+v", v)
	}
}

func TestStageSendsDisagreementToReview(t *testing.T) {
	set := mustParse(t, giftJSON)
	changed := giftMessage()
	changed.Body = strings.Replace(changed.Body, "Gift date", "Date of gift", 1)
	for name, c := range map[string]struct {
		msg    stage.Message
		choice string
		p      float64
		why    string
	}{
		"jev says none":                  {giftMessage(), "none", 0.9, "jev"},
		"jev unsure":                     {giftMessage(), "gift-receipt", 0.55, "0.55"},
		"fingerprint fails (new format)": {changed, "gift-receipt", 0.95, "Gift date"},
	} {
		v, _, _ := evaluate(t, set, c.msg, c.choice, c.p)
		if !reflect.DeepEqual(v.Labels, []string{"review"}) || !strings.Contains(v.Reason, c.why) {
			t.Errorf("%s: verdict %+v, want review naming %q", name, v, c.why)
		}
	}
}

func TestStageSendsTwoFingerprintsToReview(t *testing.T) {
	data := strings.Replace(giftJSON, `{"id": "ride", "senders": ["rides@example.net"]`,
		`{"id": "any-gift", "senders": ["gifts@school.example.org"], "describe": "Any gift email", "subject": "Gift"},
		 {"id": "ride", "senders": ["rides@example.net"]`, 1)
	v, _, _ := evaluate(t, mustParse(t, data), giftMessage(), "gift-receipt", 0.95)
	if !reflect.DeepEqual(v.Labels, []string{"review"}) || !strings.Contains(v.Reason, "any-gift") {
		t.Fatalf("verdict %+v", v)
	}
}

func TestStageCallsANewTypeNew(t *testing.T) {
	set := mustParse(t, giftJSON)
	// Both say none: a kind of email this sender has no template for.
	other := giftMessage()
	other.Subject = "Our spring newsletter"
	v, calls, _ := evaluate(t, set, other, "none", 0.9)
	if !reflect.DeepEqual(v.Labels, []string{"new"}) || calls != 1 || !strings.Contains(v.Reason, "subject") {
		t.Fatalf("verdict %+v calls %d", v, calls)
	}
	// A sender with no templates gets no opinion, at no cost: most of its
	// mail is not a record at all, and the queue lists the records.
	stranger := giftMessage()
	stranger.Sender = "someone@example.com"
	v, calls, _ = evaluate(t, set, stranger, "none", 0.9)
	if len(v.Labels) != 0 || calls != 0 || v.CostUSD != 0 || !strings.Contains(v.Reason, "no template") {
		t.Fatalf("stranger: verdict %+v calls %d", v, calls)
	}
}

func TestParseReservesNone(t *testing.T) {
	if _, err := template.Parse([]byte(`{"templates": [{"id": "none", "senders": ["x@example.com"], "describe": "d", "subject": "s"}]}`)); err == nil {
		t.Fatal("parsed a template called none")
	}
}

func TestStageNeedsBodiesOnlyFromSendersWithTemplates(t *testing.T) {
	st := template.NewStage("template", mustParse(t, giftJSON), nil, 0.7, stage.NewBudget(1), pacific)
	if !st.NeedsBodyOf(giftMessage()) || st.NeedsBodyOf(stage.Message{Sender: "someone@example.com"}) {
		t.Fatal("wrong body choice")
	}
}
