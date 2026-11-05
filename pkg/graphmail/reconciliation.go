package graphmail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// LegacyReference carries expected identity evidence, never a content-derived ID.
type LegacyReference struct {
	N                         int64
	Ref                       mailbox.MessageRef
	InternetMessageID, Sender string
	Collection                mailbox.CollectionRef
}

type LegacyEvidence struct {
	States map[int64]mailbox.State
	// EnumeratedStates retains the exact first observation only for accepted
	// recipient casing differences. States retains the exact inspection result.
	EnumeratedStates  map[int64]mailbox.State
	Unavailable       map[int64]bool
	Started, Finished time.Time
	Pages, Observed   int
}

// ReconciliationError distinguishes an ownership/identity block from incomplete
// observation that may be retried after its cause is resolved.
type ReconciliationError struct {
	Structural bool
	Reason     string
	Cause      error
	Conflict   *ReconciliationConflict
}

// ReconciliationDiagnosticField is display data, never an identity reference.
// An oversized value is omitted entirely, not shortened into a different ID.
type ReconciliationDiagnosticField struct {
	Value        string
	OmittedBytes int
}

// ReconciliationConflictMessage contains only the metadata used to locate a
// conflict. It deliberately cannot carry a State, locator or proof capability.
type ReconciliationConflictMessage struct {
	ID, InternetMessageID, Sender, CollectionID ReconciliationDiagnosticField
}

// ReconciliationConflictOriginal identifies an original local row separately
// from the observed message. Correlated originals remain distinct.
type ReconciliationConflictOriginal struct {
	N       int64
	Message ReconciliationConflictMessage
}

// ReconciliationConflict retains the legacy first-conflict diagnostic payload.
// The exact-ID collector no longer emits this correlation-only diagnostic.
// Enumeration and original inspection are incomplete, even without omissions.
// MatchingOriginals counts matches for this observed record only, not a mailbox.
type ReconciliationConflict struct {
	Account             ReconciliationDiagnosticField
	ObservedMessage     ReconciliationConflictMessage
	Originals           []ReconciliationConflictOriginal
	MatchingOriginals   int
	Started, ObservedAt time.Time
	Pages, Observed     int
}

func (e *ReconciliationError) Error() string { return "Graph reconciliation: " + e.Reason }
func (e *ReconciliationError) Unwrap() error { return e.Cause }
func reconciliationBlock(reason string) error {
	return &ReconciliationError{Structural: true, Reason: reason}
}

// phase and category are fixed labels chosen at the failing check, never
// provider strings. The original cause stays available to programmatic callers.
func incompleteEvidence(phase, category string, err error) error {
	if category == "" {
		category = reconciliationReadCategory(err)
	}
	return &ReconciliationError{Reason: "incomplete primary-mailbox evidence; phase=" + phase + "; category=" + category + "; resolve the read failure and retry a reconciliation preview", Cause: err}
}

// Classify only known error identities/types and allowlisted HTTP statuses.
// Error messages, API codes, URLs and dynamic type names are never rendered.
func reconciliationReadCategory(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "deadline"
	case errors.Is(err, mailbox.ErrAccountMismatch):
		return "account-mismatch"
	}
	var api *graph.APIError
	if errors.As(err, &api) && api != nil {
		switch api.Status {
		case 401:
			return "authorization-http-401"
		case 403:
			return "authorization-http-403"
		case 404:
			return "not-found-http-404"
		case 410:
			return "gone-http-410"
		case 429:
			return "throttled-http-429"
		case 500, 502, 503, 504:
			return "server-response"
		default:
			return "unknown-api-response"
		}
	}
	var limit *graph.ResponseLimitError
	var syntax *json.SyntaxError
	var shape *json.UnmarshalTypeError
	var request *url.Error
	var network *net.OpError
	switch {
	case errors.As(err, &limit):
		return "response-limit"
	case errors.As(err, &syntax), errors.As(err, &shape):
		return "malformed-response"
	case errors.Is(err, mailbox.ErrStaleIdentity):
		return "stale-identity"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "read-failure"
	case errors.As(err, &request), errors.As(err, &network):
		return "transport-read-failure"
	default:
		return "unknown-read-failure"
	}
}

const legacyMessageLimit = 1000000
const legacyPageLimit = 100000

// ReconcileLegacyReferences observes only the primary mailbox visible through
// /me/messages. Missing references remain history, not fabricated remote proof.
func (p *Provider) ReconcileLegacyReferences(ctx context.Context, refs []LegacyReference) (out LegacyEvidence, err error) {
	return p.reconcileLegacyReferences(ctx, refs, legacyMessageLimit, legacyPageLimit)
}
func (p *Provider) reconcileLegacyReferences(ctx context.Context, refs []LegacyReference, messageLimit, pageLimit int) (out LegacyEvidence, err error) {
	out = LegacyEvidence{States: map[int64]mailbox.State{}, EnumeratedStates: map[int64]mailbox.State{}, Unavailable: map[int64]bool{}, Started: time.Now().UTC()}
	// Errors never return a partial evidence capability.
	defer func() {
		if err != nil {
			out = LegacyEvidence{}
		}
	}()
	targets := map[string]LegacyReference{}
	numbers := map[int64]bool{}
	for _, r := range refs {
		if r.N <= 0 || numbers[r.N] || r.Ref.Validate(p.account.ID) != nil || r.Ref.MessageID != mailbox.MessageID(r.Ref.Locator) || r.Collection.Validate(p.account.ID) != nil {
			return out, reconciliationBlock("invalid original identity or account; preserve the mirror and request owner review")
		}
		id := string(r.Ref.MessageID)
		if _, ok := targets[id]; ok {
			return out, reconciliationBlock("ambiguous original immutable identities; preserve the mirror and request owner review")
		}
		targets[id] = r
		numbers[r.N] = true
	}
	seen := map[string]bool{}
	links := map[string]bool{}
	listed := map[string]mailbox.State{}
	path := "/me/messages"
	query := url.Values{"$select": {selectFields}, "$top": {"200"}}
	for path != "" {
		if e := ctx.Err(); e != nil {
			return out, incompleteEvidence("enumeration", "", e)
		}
		if out.Pages >= pageLimit {
			return out, &ReconciliationError{Structural: true, Reason: "primary-mailbox enumeration page limit reached; preserve the mirror and request a reviewed resource plan"}
		}
		if links[path] {
			return out, incompleteEvidence("enumeration", "pagination-cycle", errors.New("cyclic enumeration"))
		}
		links[path] = true
		var page struct {
			Value json.RawMessage `json:"value"`
			Next  json.RawMessage `json:"@odata.nextLink"`
			Delta json.RawMessage `json:"@odata.deltaLink"`
		}
		if e := p.g.Get(ctx, path, query, &page); e != nil {
			return out, incompleteEvidence("enumeration", "", e)
		}
		if len(page.Value) == 0 || string(page.Value) == "null" || len(page.Delta) != 0 {
			return out, incompleteEvidence("enumeration", "invalid-page", errors.New("invalid enumeration page"))
		}
		var records []json.RawMessage
		if e := json.Unmarshal(page.Value, &records); e != nil || records == nil {
			return out, incompleteEvidence("enumeration", "invalid-records", errors.New("invalid enumeration records"))
		}
		out.Pages++
		for _, raw := range records {
			var w wireMessage
			fields := map[string]json.RawMessage{}
			if e := json.Unmarshal(raw, &w); e != nil {
				return out, incompleteEvidence("enumeration", "malformed-response", e)
			}
			if e := json.Unmarshal(raw, &fields); e != nil {
				return out, incompleteEvidence("enumeration", "malformed-response", e)
			}
			for _, key := range []string{"id", "parentFolderId", "internetMessageId", "subject", "toRecipients", "ccRecipients", "receivedDateTime", "hasAttachments", "isRead", "bodyPreview"} {
				if len(fields[key]) == 0 || string(fields[key]) == "null" {
					return out, incompleteEvidence("enumeration", "incomplete-metadata", errors.New("missing or null selected metadata"))
				}
			}

			if e := ctx.Err(); e != nil {
				return out, incompleteEvidence("enumeration", "", e)
			}
			if out.Observed >= messageLimit {
				return out, &ReconciliationError{Structural: true, Reason: "primary-mailbox enumeration message limit reached; preserve the mirror and request a reviewed resource plan"}
			}
			if len(w.Removed) != 0 || seen[w.ID] {
				return out, incompleteEvidence("enumeration", "duplicate-or-removed-record", errors.New("duplicate, removed or oversized enumeration"))
			}
			state, e := p.state(w)
			if e != nil {
				return out, incompleteEvidence("enumeration", "invalid-message-state", e)
			}
			seen[w.ID] = true
			out.Observed++
			if _, ok := targets[w.ID]; ok {
				listed[w.ID] = state
			}
		}
		path = ""
		query = nil
		if len(page.Next) != 0 {
			if e := json.Unmarshal(page.Next, &path); e != nil || path == "" {
				return out, incompleteEvidence("enumeration", "invalid-pagination", errors.New("invalid nextLink"))
			}
		}
	}
	inspectRefs := make([]mailbox.MessageRef, len(refs))
	for i, r := range refs {
		inspectRefs[i] = r.Ref
	}
	states, failures, e := p.InspectBatch(ctx, inspectRefs)
	if e != nil {
		return out, incompleteEvidence("inspection", "", e)
	}
	if len(states) != len(refs) || len(failures) != len(refs) {
		return out, incompleteEvidence("inspection", "incomplete-results", errors.New("incomplete original inspection"))
	}
	for i, r := range refs {
		if e := ctx.Err(); e != nil {
			return out, incompleteEvidence("inspection", "", e)
		}
		state, e := states[i], failures[i]
		observed, found := listed[string(r.Ref.MessageID)]
		if e != nil {
			var api *graph.APIError
			if !found && errors.As(e, &api) && api.Status == 404 && api.Code == "ErrorItemNotFound" {
				out.Unavailable[r.N] = true
				continue
			}
			return out, incompleteEvidence("inspection", "", e)
		}
		if !found {
			return out, incompleteEvidence("consistency", "reference-appeared", errors.New("reference appeared after enumeration"))
		}
		casingChanged := !reflect.DeepEqual(observed.Metadata, state.Metadata)
		if casingChanged && !reconciliationRecipientCasing(observed.Metadata, state.Metadata) {
			return out, incompleteEvidence("consistency", "metadata-changed", errors.New("metadata changed after enumeration"))
		}
		if e = mailbox.CheckPrecondition(p.account.ID, observed, state); e != nil {
			return out, incompleteEvidence("consistency", "precondition-changed", e)
		}
		expected := mailbox.State{Ref: r.Ref, Metadata: mailbox.Metadata{InternetMessageID: r.InternetMessageID, Sender: mailbox.Address{Address: r.Sender}}, Memberships: state.Memberships}
		if e = mailbox.CheckPrecondition(p.account.ID, expected, state); e != nil {
			return out, reconciliationBlock("original identity, sender or header conflicts with current evidence; preserve the mirror and request owner review")
		}
		if casingChanged {
			out.EnumeratedStates[r.N] = reconciliationObservation(observed)
		}
		out.States[r.N] = reconciliationObservation(state)
	}
	if len(out.Unavailable) > 0 && len(out.States) == 0 {
		return out, reconciliationBlock("insufficient same-account live evidence; preserve the mirror and restore access to an original immutable reference or ask the owner for an approved evidence policy")
	}
	if e := ctx.Err(); e != nil {
		return out, incompleteEvidence("completion", "", e)
	}
	out.Finished = time.Now().UTC()
	return out, nil
}

// This recovery-only comparison is positional and preserves duplicate entries.
// EqualFold is a string relationship, not RFC mailbox equivalence. The normal
// provider/action comparator and all input strings remain unchanged.
func reconciliationRecipientCasing(a, b mailbox.Metadata) bool {
	if (a.Recipients == nil) != (b.Recipients == nil) || len(a.Recipients) != len(b.Recipients) {
		return false
	}
	for i, left := range a.Recipients {
		right := b.Recipients[i]
		if left.Name != right.Name || !utf8.ValidString(left.Address) || !utf8.ValidString(right.Address) || !strings.EqualFold(left.Address, right.Address) {
			return false
		}
	}
	a.Recipients, b.Recipients = nil, nil // value copies only; never slice mutation
	return reflect.DeepEqual(a, b)
}

func reconciliationObservation(s mailbox.State) mailbox.State {
	if s.Metadata.Recipients != nil {
		s.Metadata.Recipients = append([]mailbox.Address{}, s.Metadata.Recipients...)
	}
	if s.Memberships != nil {
		s.Memberships = append([]mailbox.Membership{}, s.Memberships...)
	}
	return s
}
