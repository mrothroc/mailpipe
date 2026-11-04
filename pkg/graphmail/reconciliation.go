package graphmail

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"time"

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
	States            map[int64]mailbox.State
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

// ReconciliationConflict describes only the first conflicting observation.
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

const reconciliationConflictOriginalLimit = 32
const reconciliationDiagnosticFieldLimit = 1024

func reconciliationDiagnosticField(value string) ReconciliationDiagnosticField {
	if len(value) > reconciliationDiagnosticFieldLimit {
		return ReconciliationDiagnosticField{OmittedBytes: len(value)}
	}
	return ReconciliationDiagnosticField{Value: strings.Clone(value)}
}

func reconciliationConflictMessage(id, header, sender, collection string) ReconciliationConflictMessage {
	return ReconciliationConflictMessage{
		ID: reconciliationDiagnosticField(id), InternetMessageID: reconciliationDiagnosticField(header),
		Sender: reconciliationDiagnosticField(sender), CollectionID: reconciliationDiagnosticField(collection),
	}
}

func (p *Provider) reconciliationConflict(refs []LegacyReference, state mailbox.State, out LegacyEvidence) *ReconciliationConflict {
	c := &ReconciliationConflict{
		Account: reconciliationDiagnosticField(string(p.account.ID)),
		ObservedMessage: reconciliationConflictMessage(string(state.Ref.MessageID), state.Metadata.InternetMessageID,
			state.Metadata.Sender.Address, string(state.Memberships[0].Collection.CollectionID)),
		Started: out.Started, ObservedAt: time.Now().UTC(), Pages: out.Pages, Observed: out.Observed,
	}
	// refs is the existing exact-header bucket. Count all matching originals for
	// this record, without retaining an unbounded list or making another read.
	for _, r := range refs {
		if r.Ref.MessageID == state.Ref.MessageID || !strings.EqualFold(strings.TrimSpace(r.Sender), strings.TrimSpace(state.Metadata.Sender.Address)) {
			continue
		}
		c.MatchingOriginals++
		if len(c.Originals) < reconciliationConflictOriginalLimit {
			c.Originals = append(c.Originals, ReconciliationConflictOriginal{N: r.N,
				Message: reconciliationConflictMessage(string(r.Ref.MessageID), r.InternetMessageID, r.Sender, string(r.Collection.CollectionID))})
		}
	}
	return c
}

func (e *ReconciliationError) Error() string { return "Graph reconciliation: " + e.Reason }
func (e *ReconciliationError) Unwrap() error { return e.Cause }
func reconciliationBlock(reason string) error {
	return &ReconciliationError{Structural: true, Reason: reason}
}
func incompleteEvidence(err error) error {
	return &ReconciliationError{Reason: "incomplete primary-mailbox evidence; resolve the read failure and retry a reconciliation preview", Cause: err}
}

const legacyMessageLimit = 1000000
const legacyPageLimit = 100000

// ReconcileLegacyReferences observes only the primary mailbox visible through
// /me/messages. Missing references remain history, not fabricated remote proof.
func (p *Provider) ReconcileLegacyReferences(ctx context.Context, refs []LegacyReference) (out LegacyEvidence, err error) {
	return p.reconcileLegacyReferences(ctx, refs, legacyMessageLimit, legacyPageLimit)
}
func (p *Provider) reconcileLegacyReferences(ctx context.Context, refs []LegacyReference, messageLimit, pageLimit int) (out LegacyEvidence, err error) {
	out = LegacyEvidence{States: map[int64]mailbox.State{}, Unavailable: map[int64]bool{}, Started: time.Now().UTC()}
	// Errors never return a partial evidence capability.
	defer func() {
		if err != nil {
			out = LegacyEvidence{}
		}
	}()
	targets := map[string]LegacyReference{}
	numbers := map[int64]bool{}
	correlations := map[string][]LegacyReference{}
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
		if r.InternetMessageID != "" {
			correlations[r.InternetMessageID] = append(correlations[r.InternetMessageID], r)
		}
	}
	seen := map[string]bool{}
	links := map[string]bool{}
	listed := map[string]mailbox.State{}
	path := "/me/messages"
	query := url.Values{"$select": {selectFields}, "$top": {"200"}}
	for path != "" {
		if e := ctx.Err(); e != nil {
			return out, incompleteEvidence(e)
		}
		if out.Pages >= pageLimit {
			return out, &ReconciliationError{Structural: true, Reason: "primary-mailbox enumeration page limit reached; preserve the mirror and request a reviewed resource plan"}
		}
		if links[path] {
			return out, incompleteEvidence(errors.New("cyclic enumeration"))
		}
		links[path] = true
		var page struct {
			Value json.RawMessage `json:"value"`
			Next  json.RawMessage `json:"@odata.nextLink"`
			Delta json.RawMessage `json:"@odata.deltaLink"`
		}
		if e := p.g.Get(ctx, path, query, &page); e != nil {
			return out, incompleteEvidence(e)
		}
		if len(page.Value) == 0 || string(page.Value) == "null" || len(page.Delta) != 0 {
			return out, incompleteEvidence(errors.New("invalid enumeration page"))
		}
		var records []json.RawMessage
		if e := json.Unmarshal(page.Value, &records); e != nil || records == nil {
			return out, incompleteEvidence(errors.New("invalid enumeration records"))
		}
		out.Pages++
		for _, raw := range records {
			var w wireMessage
			fields := map[string]json.RawMessage{}
			if e := json.Unmarshal(raw, &w); e != nil {
				return out, incompleteEvidence(e)
			}
			if e := json.Unmarshal(raw, &fields); e != nil {
				return out, incompleteEvidence(e)
			}
			for _, key := range []string{"id", "parentFolderId", "internetMessageId", "subject", "toRecipients", "ccRecipients", "receivedDateTime", "hasAttachments", "isRead", "bodyPreview"} {
				if len(fields[key]) == 0 || string(fields[key]) == "null" {
					return out, incompleteEvidence(errors.New("missing or null selected metadata"))
				}
			}

			if e := ctx.Err(); e != nil {
				return out, incompleteEvidence(e)
			}
			if out.Observed >= messageLimit {
				return out, &ReconciliationError{Structural: true, Reason: "primary-mailbox enumeration message limit reached; preserve the mirror and request a reviewed resource plan"}
			}
			if len(w.Removed) != 0 || seen[w.ID] {
				return out, incompleteEvidence(errors.New("duplicate, removed or oversized enumeration"))
			}
			state, e := p.state(w)
			if e != nil {
				return out, incompleteEvidence(e)
			}
			seen[w.ID] = true
			out.Observed++
			for _, r := range correlations[state.Metadata.InternetMessageID] {
				if string(r.Ref.MessageID) != w.ID && strings.EqualFold(strings.TrimSpace(r.Sender), strings.TrimSpace(state.Metadata.Sender.Address)) {
					return out, &ReconciliationError{Structural: true,
						Reason:   "different immutable identities share historical header evidence; preserve the mirror and request owner review",
						Conflict: p.reconciliationConflict(correlations[state.Metadata.InternetMessageID], state, out)}
				}
			}
			if _, ok := targets[w.ID]; ok {
				listed[w.ID] = state
			}
		}
		path = ""
		query = nil
		if len(page.Next) != 0 {
			if e := json.Unmarshal(page.Next, &path); e != nil || path == "" {
				return out, incompleteEvidence(errors.New("invalid nextLink"))
			}
		}
	}
	for _, r := range refs {
		state, e := p.Inspect(ctx, r.Ref)
		observed, found := listed[string(r.Ref.MessageID)]
		if e != nil {
			var api *graph.APIError
			if !found && errors.As(e, &api) && api.Status == 404 && api.Code == "ErrorItemNotFound" {
				out.Unavailable[r.N] = true
				continue
			}
			return out, incompleteEvidence(e)
		}
		if !found {
			return out, incompleteEvidence(errors.New("reference appeared after enumeration"))
		}
		if !reflect.DeepEqual(observed.Metadata, state.Metadata) {
			return out, incompleteEvidence(errors.New("metadata changed after enumeration"))
		}
		if e = mailbox.CheckPrecondition(p.account.ID, observed, state); e != nil {
			return out, incompleteEvidence(e)
		}
		expected := mailbox.State{Ref: r.Ref, Metadata: mailbox.Metadata{InternetMessageID: r.InternetMessageID, Sender: mailbox.Address{Address: r.Sender}}, Memberships: []mailbox.Membership{{Collection: r.Collection}}}
		if e = mailbox.CheckPrecondition(p.account.ID, expected, state); e != nil {
			return out, reconciliationBlock("original identity, sender, header or collection conflicts with current evidence; preserve the mirror and request owner review")
		}
		out.States[r.N] = state
	}
	if len(out.Unavailable) > 0 && len(out.States) == 0 {
		return out, reconciliationBlock("insufficient same-account live evidence; preserve the mirror and restore access to an original immutable reference or ask the owner for an approved evidence policy")
	}
	if e := ctx.Err(); e != nil {
		return out, incompleteEvidence(e)
	}
	out.Finished = time.Now().UTC()
	return out, nil
}
