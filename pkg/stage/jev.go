package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ErrBudget reports that a run has spent its cost cap. It is also ErrFatal.
var ErrBudget = errors.New("budget exhausted")

// DecisionsURL is OpenRouter's Decisions endpoint.
const DecisionsURL = "https://openrouter.ai/api/alpha/decisions"

// DefaultMaxInputBytes guards exceptionally large model inputs without truncating them.
// It is a byte limit, not a model token-context estimate.
const DefaultMaxInputBytes = 1 << 20

// ErrInputTooLarge prevents model calls on an incomplete or excessive input.
var ErrInputTooLarge = errors.New("model input exceeds max_input_bytes; retained for review")

// Budget is a cost cap shared by every call in a run.
type Budget struct {
	mu    sync.Mutex
	max   float64
	spent float64
}

// NewBudget returns a cap of max US dollars.
func NewBudget(max float64) *Budget { return &Budget{max: max} }

// Spent reports the dollars charged so far.
func (b *Budget) Spent() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

func (b *Budget) check() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.spent >= b.max {
		return fmt.Errorf("%w: %w: spent $%.4f of the $%.2f cap; raise it with --max-cost-usd", ErrFatal, ErrBudget, b.spent, b.max)
	}
	return nil
}

func (b *Budget) charge(cost float64) {
	b.mu.Lock()
	b.spent += cost
	b.mu.Unlock()
}

// Question is one yes/no ("noul") decision question.
type Question struct {
	Key string `json:"key"`
	// Threshold overrides the stage threshold; zero inherits it.
	Threshold    float64 `json:"threshold,omitempty"`
	Instructions string  `json:"instructions"`
	True         string  `json:"criteria_true"`
	False        string  `json:"criteria_false"`
}

// DecisionsError is a non-retryable HTTP failure from the Decisions API.
// Statuses 400-404 (bad request, bad key, no credits, forbidden, unknown
// model) are fatal for the run: every later call would fail the same way.
type DecisionsError struct {
	Status  int
	Message string
}

func (e *DecisionsError) Error() string {
	return fmt.Sprintf("decisions API returned HTTP %d: %s", e.Status, e.Message)
}

// Is makes a fatal status match ErrFatal.
func (e *DecisionsError) Is(target error) bool {
	return target == ErrFatal && e.Status >= 400 && e.Status <= 404
}

// JevClient calls the Decisions API.
type JevClient struct {
	URL   string
	Model string
	HTTP  *http.Client
	// Sleep waits between retries; nil means a timer that stops early when
	// ctx is cancelled.
	Sleep    func(ctx context.Context, d time.Duration) error
	MaxTries int
	// MaxInputBytes bounds the complete readable message; zero uses DefaultMaxInputBytes.
	MaxInputBytes int
	apiKey        string
}

// NewJevClient returns a client for model, authenticated with apiKey.
func NewJevClient(apiKey, model string) *JevClient {
	return &JevClient{URL: DecisionsURL, Model: model, HTTP: &http.Client{Timeout: 60 * time.Second},
		MaxTries: 5, apiKey: apiKey}
}

func (c *JevClient) wait(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ask returns the yes-probability ("noul") of each question about state, by
// key, from one call, and the charge.
func (c *JevClient) ask(ctx context.Context, state string, qs []Question) (map[string]float64, float64, error) {
	questions := make(map[string]any, len(qs))
	for _, q := range qs {
		questions[q.Key] = map[string]any{"type": "noul", "instructions": q.Instructions,
			"criteria": map[string]string{"true": q.True, "false": q.False}}
	}
	data, err := c.post(ctx, state, questions)
	if err != nil {
		return nil, 0, err
	}
	var out struct {
		Answers map[string]struct {
			Noul json.RawMessage `json:"noul"`
		} `json:"answers"`
		Usage struct {
			Cost float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, 0, errors.New("decisions API response was not JSON")
	}
	scores := make(map[string]float64, len(qs))
	for _, q := range qs {
		var noul float64
		a, ok := out.Answers[q.Key]
		if !ok || len(a.Noul) == 0 {
			return nil, out.Usage.Cost, fmt.Errorf("decisions API gave no answer for %s", q.Key)
		}
		raw := a.Noul
		if err := json.Unmarshal(raw, &noul); err != nil || bytes.Equal(raw, []byte("null")) || noul < 0 || noul > 1 {
			return nil, out.Usage.Cost, fmt.Errorf("decisions API gave a malformed answer for %s: %s", q.Key, strings.TrimSpace(string(raw)))
		}
		scores[q.Key] = noul
	}
	return scores, out.Usage.Cost, nil
}

// Choice is one multiple-choice question: Options maps each answer's key to
// what it means.
type Choice struct {
	Key          string
	Instructions string
	Options      map[string]string
}

// ChoiceAnswer is the chosen option, the probability of every option, and
// the charge.
type ChoiceAnswer struct {
	Choice        string
	Probabilities map[string]float64
	Cost          float64
}

// probabilitySlack is how far a choice's probabilities may sum from one.
const probabilitySlack = 0.02

// Choose asks one choice question about state, charged to budget.
func (c *JevClient) Choose(ctx context.Context, budget *Budget, state string, q Choice) (ChoiceAnswer, error) {
	if err := budget.check(); err != nil {
		return ChoiceAnswer{}, err
	}
	data, err := c.post(ctx, state, map[string]any{q.Key: map[string]any{"type": "choice", "instructions": q.Instructions, "criteria": q.Options}})
	if err != nil {
		return ChoiceAnswer{}, err
	}
	var out struct {
		Answers map[string]struct {
			Choice        string             `json:"choice"`
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"answers"`
		Usage struct {
			Cost float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return ChoiceAnswer{}, errors.New("decisions API response was not JSON")
	}
	budget.charge(out.Usage.Cost)
	a := ChoiceAnswer{Cost: out.Usage.Cost}
	got, ok := out.Answers[q.Key]
	if !ok {
		return a, fmt.Errorf("decisions API gave no answer for %s", q.Key)
	}
	if _, ok := q.Options[got.Choice]; !ok {
		return a, fmt.Errorf("decisions API chose %q, which is not an option for %s", got.Choice, q.Key)
	}
	sum := 0.0
	for option := range q.Options {
		p, ok := got.Probabilities[option]
		if !ok || p < 0 || p > 1 {
			return a, fmt.Errorf("decisions API gave a malformed probability for %s option %s", q.Key, option)
		}
		sum += p
	}
	if len(got.Probabilities) != len(q.Options) || sum < 1-probabilitySlack || sum > 1+probabilitySlack {
		return a, fmt.Errorf("decisions API's probabilities for %s are not a distribution over its options", q.Key)
	}
	a.Choice, a.Probabilities = got.Choice, got.Probabilities
	return a, nil
}

// post sends questions about state and returns the response body, retrying
// transient failures with backoff.
func (c *JevClient) post(ctx context.Context, state string, questions map[string]any) ([]byte, error) {
	limit := c.MaxInputBytes
	if limit <= 0 {
		limit = DefaultMaxInputBytes
	}
	if len(state) > limit {
		return nil, fmt.Errorf("%w (%d bytes; limit %d)", ErrInputTooLarge, len(state), limit)
	}
	body, err := json.Marshal(map[string]any{"model": c.Model, "state": state, "questions": questions})
	if err != nil {
		return nil, err
	}
	var last string
	for attempt := 0; attempt < c.MaxTries; attempt++ {
		if attempt > 0 {
			if err := c.wait(ctx, min(60*time.Second, time.Duration(1<<attempt)*time.Second)); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = "connection failed"
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = "response cut off"
			continue
		}
		switch {
		case resp.StatusCode == 408 || resp.StatusCode == 409 || resp.StatusCode == 429 || resp.StatusCode >= 500:
			last = fmt.Sprintf("HTTP %d", resp.StatusCode)
			continue
		case resp.StatusCode >= 300:
			return nil, &DecisionsError{Status: resp.StatusCode, Message: c.scrub(apiMessage(data))}
		}
		return data, nil
	}
	return nil, fmt.Errorf("decisions API: gave up after %d tries (%s)", c.MaxTries, last)
}

// scrub removes the API key from text that came back from the server.
func (c *JevClient) scrub(s string) string {
	if c.apiKey == "" {
		return s
	}
	return strings.ReplaceAll(s, c.apiKey, "[REDACTED]")
}

func apiMessage(data []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
		return truncate(e.Error.Message, 300)
	}
	return truncate(strings.TrimSpace(string(data)), 300)
}

// truncate cuts s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Jev is a stage that asks yes/no questions about each message, all in one
// call.
type Jev struct {
	name      string
	client    *JevClient
	questions []Question
	threshold float64
	budget    *Budget
	// Now gives the date that "{today}" in a question becomes (local time);
	// nil means time.Now.
	Now func() time.Time
}

// todayMark in a question's text is replaced with the date of the call, for
// judgements that change with time.
const todayMark = "{today}"

// dated returns the questions with todayMark filled in.
func (j *Jev) dated() []Question {
	now := time.Now
	if j.Now != nil {
		now = j.Now
	}
	r := strings.NewReplacer(todayMark, now().Format(time.DateOnly))
	out := make([]Question, len(j.questions))
	for i, q := range j.questions {
		out[i] = q
		out[i].Instructions, out[i].True, out[i].False = r.Replace(q.Instructions), r.Replace(q.True), r.Replace(q.False)
	}
	return out
}

// NewJev returns a Jev stage. Each score at or above its question threshold
// (or the stage threshold) adds a label; every call is charged to budget.
func NewJev(name string, client *JevClient, qs []Question, threshold float64, budget *Budget) *Jev {
	return &Jev{name: name, client: client, questions: qs, threshold: threshold, budget: budget}
}

func (j *Jev) Name() string    { return j.name }
func (j *Jev) NeedsBody() bool { return true }

func (j *Jev) Evaluate(ctx context.Context, m Message) (Verdict, error) {
	if err := j.budget.check(); err != nil {
		return Verdict{}, err
	}
	scores, cost, err := j.client.ask(ctx, State(m), j.dated())
	j.budget.charge(cost)
	if errors.Is(err, ErrInputTooLarge) {
		return Verdict{Stage: j.name, Labels: []string{"review"}, Action: Keep, Reason: err.Error(), Provenance: "jev:" + j.client.Model}, nil
	}
	if err != nil {
		return Verdict{Stage: j.name, CostUSD: cost}, err // a failed call may still have been charged
	}
	keys := make([]string, 0, len(scores))
	for k := range scores {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	v := Verdict{Stage: j.name, Scores: scores, CostUSD: cost, Provenance: "jev:" + j.client.Model}
	reasons := make([]string, len(keys))
	for i, k := range keys {
		reasons[i] = fmt.Sprintf("%s %.2f", k, scores[k])
		threshold := j.threshold
		for _, q := range j.questions {
			if q.Key == k && q.Threshold > 0 {
				threshold = q.Threshold
				break
			}
		}
		if scores[k] >= threshold {
			v.Labels = append(v.Labels, k)
		}
	}
	v.Reason = strings.Join(reasons, ", ")
	return v, nil
}

// State renders a message as the text a model reads: headers, then the
// complete readable body (see Readable). The client rejects oversized input;
// it never makes a decision from a silently truncated message.
func State(m Message) string {
	body := Readable(m.Body)
	if body == "" {
		body = Readable(m.Preview)
	}
	return fmt.Sprintf("From: %s\nTo: %s\nDate: %s\nSubject: %s\n\n%s", m.Sender, strings.Join(m.Recipients, ", "), m.Received, m.Subject, body)
}
