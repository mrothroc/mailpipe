// Package stage defines the contract between mailpipe and the stages that
// judge messages. A stage receives one message and returns a verdict; it never
// acts on mail itself. Policy decides what verdicts lead to.
//
// The contract is the same for built-in stages and for external programs (see
// Exec), so an agent or tool written in any language can be plugged in.
package stage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Actions a verdict may suggest. Empty means no suggestion.
const (
	Keep       = "keep"
	Quarantine = "quarantine"
	Delete     = "delete"
	Digest     = "digest"
)

// ErrFatal marks a failure that will not go away by moving on to the next
// message (bad credentials, exhausted budget, malformed requests); the run stops.
var ErrFatal = errors.New("stage cannot continue")

// Message is what a stage receives: the stored metadata, plus Body when the
// stage declares that it needs one.
type Message struct {
	N                 int64    `json:"n"`
	GraphID           string   `json:"graph_id"`
	InternetMessageID string   `json:"internet_message_id"`
	Folder            string   `json:"folder"`
	Received          string   `json:"received"`
	Sender            string   `json:"sender"`
	SenderName        string   `json:"sender_name"`
	Recipients        []string `json:"recipients"`
	Subject           string   `json:"subject"`
	Preview           string   `json:"preview"`
	HasAttachments    bool     `json:"has_attachments"`
	Body              string   `json:"body,omitempty"`
	// SourcePath is a temporary 0600 MIME file available only during a hook.
	SourcePath string `json:"source_path,omitempty"`
	// MaxCostUSD is the remaining run allowance. Paid hooks must honour it.
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`
}

// Verdict is a stage's judgement of one message.
type Verdict struct {
	Stage      string             `json:"stage"`
	Labels     []string           `json:"labels,omitempty"`
	Scores     map[string]float64 `json:"scores,omitempty"`
	Action     string             `json:"action,omitempty"`
	Reason     string             `json:"reason,omitempty"`
	CostUSD    float64            `json:"cost_usd,omitempty"`
	Final      bool               `json:"final,omitempty"`
	Provenance string             `json:"provenance,omitempty"`
}

// Validate checks a verdict against the contract.
func (v Verdict) Validate() error {
	switch v.Action {
	case "", Keep, Quarantine, Delete, Digest:
	default:
		return fmt.Errorf("unknown action %q (want keep, quarantine, delete, digest or empty)", v.Action)
	}
	for _, l := range v.Labels {
		if strings.TrimSpace(l) == "" {
			return errors.New("labels must not be blank")
		}
	}
	for k, s := range v.Scores {
		if strings.TrimSpace(k) == "" || math.IsNaN(s) || math.IsInf(s, 0) {
			return fmt.Errorf("score %q must be a finite number with a name", k)
		}
	}
	if v.CostUSD < 0 || math.IsNaN(v.CostUSD) || math.IsInf(v.CostUSD, 0) {
		return errors.New("cost_usd must be a non-negative number")
	}
	return nil
}

// Stage judges messages.
type Stage interface {
	Name() string
	// NeedsBody reports whether Evaluate needs Message.Body filled in.
	NeedsBody() bool
	Evaluate(ctx context.Context, m Message) (Verdict, error)
}

// BodyChooser is a stage that needs the bodies of only some messages: Run
// fetches a body only when NeedsBody and NeedsBodyOf both say so.
type BodyChooser interface {
	NeedsBodyOf(m Message) bool
}
