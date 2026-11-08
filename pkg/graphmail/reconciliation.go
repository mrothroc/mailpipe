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
	// address casing, sender-name or noncase sender-address differences. States
	// retains the exact inspection result.
	EnumeratedStates map[int64]mailbox.State
	// SenderSourceIntegrity is present only for an admitted noncase sender pair.
	// Both decode seams validated source-to-mapping integrity for this exact ref.
	SenderSourceIntegrity map[int64]bool
	Unavailable           map[int64]bool
	Started, Finished     time.Time
	Pages, Observed       int
}

// ReconciliationError distinguishes an ownership/identity block from incomplete
// observation that may be retried after its cause is resolved.
type ReconciliationError struct {
	Structural bool
	Reason     string
	Cause      error
	Conflict   *ReconciliationConflict
	// Only explicit private diagnostic callers may inspect this untrusted payload.
	SenderAddressDiagnostic *ReconciliationSenderAddressDiagnostic `json:"-"`
}

// ReconciliationSenderAddressDiagnostic is an incomplete observation pair, never
// identity proof or reconciliation evidence. Default collection does not retain it.
type ReconciliationSenderAddressDiagnostic struct {
	// RequestedOriginal distinguishes a selected, unvalidated observation from
	// a sender disagreement attached to the first admission failure.
	RequestedOriginal       bool
	AccountID               mailbox.AccountID
	N                       int64
	ImmutableID             mailbox.MessageID
	EnumeratedSenderAddress string
	InspectedSenderAddress  string
	EnumeratedOrigin        *ReconciliationSenderAddressOrigin
	InspectedOrigin         *ReconciliationSenderAddressOrigin
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

// ReconcileLegacyReferencesWithSenderAddressDiagnostics opts into private raw
// error data using the identical read protocol and acceptance rules.
func (p *Provider) ReconcileLegacyReferencesWithSenderAddressDiagnostics(ctx context.Context, refs []LegacyReference) (LegacyEvidence, error) {
	return p.collectLegacyReferences(ctx, refs, legacyMessageLimit, legacyPageLimit, true)
}
func (p *Provider) reconcileLegacyReferences(ctx context.Context, refs []LegacyReference, messageLimit, pageLimit int) (LegacyEvidence, error) {
	return p.collectLegacyReferences(ctx, refs, messageLimit, pageLimit, false)
}

// legacyAcquisition contains reads, not admitted reconciliation evidence.
type legacyAcquisition struct {
	Started                                 time.Time
	Pages, Observed                         int
	listed                                  map[string]mailbox.State
	states                                  []mailbox.State
	failures                                []error
	enumeratedOrigins, inspectedOrigins     map[string]ReconciliationSenderAddressOrigin
	enumeratedIntegrity, inspectedIntegrity map[string]bool
}

func (p *Provider) acquireLegacyReferences(ctx context.Context, refs []LegacyReference, messageLimit, pageLimit int, capture bool, selectedN int64) (out legacyAcquisition, err error) {
	out.Started = time.Now().UTC()
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
	if selectedN != 0 && !numbers[selectedN] {
		return out, reconciliationBlock("requested original is not a live historical reference")
	}
	seen := map[string]bool{}
	links := map[string]bool{}
	listed := map[string]mailbox.State{}
	var enumeratedOrigins, inspectedOrigins map[string]ReconciliationSenderAddressOrigin
	var originBudget reconciliationOriginBudget
	enumeratedIntegrity, inspectedIntegrity := map[string]bool{}, map[string]bool{}
	if capture {
		enumeratedOrigins = map[string]ReconciliationSenderAddressOrigin{}
		inspectedOrigins = map[string]ReconciliationSenderAddressOrigin{}
		originBudget.limit = reconciliationOriginLimit
	}

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
			if target, ok := targets[w.ID]; ok {
				if selectedN == 0 || (capture && target.N == selectedN) {
					origin, err := reconciliationOrigin(ctx, raw, state)
					if selectedN == 0 {
						enumeratedIntegrity[w.ID] = err == nil
					}
					if capture {
						if err == nil {
							err = originBudget.retain(ctx, w.ID, origin, state.Metadata.Sender.Address)
						}
						if err != nil {
							return out, incompleteEvidence("enumeration", originCategory(err), err)
						}
						enumeratedOrigins[w.ID] = origin
					}
				}
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
	var observe inspectionObserver
	if capture || selectedN == 0 {
		observe = func(ref mailbox.MessageRef, raw json.RawMessage, state mailbox.State) error {
			if selectedN != 0 && targets[string(ref.MessageID)].N != selectedN {
				return nil
			}
			origin, err := reconciliationOrigin(ctx, raw, state)
			if selectedN == 0 {
				inspectedIntegrity[string(ref.MessageID)] = err == nil
			}
			if !capture {
				return nil
			}
			if err != nil {
				return err
			}
			if err = originBudget.retain(ctx, string(ref.MessageID), origin, state.Metadata.Sender.Address); err != nil {
				return err
			}
			inspectedOrigins[string(ref.MessageID)] = origin
			return nil
		}
	}
	states, failures, e := p.inspectBatch(ctx, inspectRefs, observe)
	if e != nil {
		return out, incompleteEvidence("inspection", originCategory(e), e)
	}
	if len(states) != len(refs) || len(failures) != len(refs) {
		return out, incompleteEvidence("inspection", "incomplete-results", errors.New("incomplete original inspection"))
	}
	out.listed, out.states, out.failures = listed, states, failures
	out.enumeratedOrigins, out.inspectedOrigins = enumeratedOrigins, inspectedOrigins
	out.enumeratedIntegrity, out.inspectedIntegrity = enumeratedIntegrity, inspectedIntegrity
	return out, nil
}

func (p *Provider) collectLegacyReferences(ctx context.Context, refs []LegacyReference, messageLimit, pageLimit int, capture bool) (out LegacyEvidence, err error) {
	// Errors never return a partial evidence capability.
	defer func() {
		if err != nil {
			out = LegacyEvidence{}
		}
	}()
	reads, err := p.acquireLegacyReferences(ctx, refs, messageLimit, pageLimit, capture, 0)
	if err != nil {
		return out, err
	}
	out = LegacyEvidence{States: map[int64]mailbox.State{}, EnumeratedStates: map[int64]mailbox.State{}, SenderSourceIntegrity: map[int64]bool{}, Unavailable: map[int64]bool{}, Started: reads.Started, Pages: reads.Pages, Observed: reads.Observed}
	listed, states, failures := reads.listed, reads.states, reads.failures
	enumeratedOrigins, inspectedOrigins := reads.enumeratedOrigins, reads.inspectedOrigins
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
		metadataChanged := !reflect.DeepEqual(observed.Metadata, state.Metadata)
		left, right := observed.Metadata.Sender.Address, state.Metadata.Sender.Address
		senderDisagrees := !strings.EqualFold(left, right)
		expected := mailbox.State{Ref: r.Ref, Metadata: mailbox.Metadata{InternetMessageID: r.InternetMessageID, Sender: mailbox.Address{Address: r.Sender}}, Memberships: state.Memberships}
		admitSender := senderDisagrees && reads.enumeratedIntegrity[string(r.Ref.MessageID)] && reads.inspectedIntegrity[string(r.Ref.MessageID)] && mailbox.CheckPrecondition(p.account.ID, expected, state) == nil
		matches := !metadataChanged || reconciliationMetadataMatches(observed.Metadata, state.Metadata)
		if !matches && admitSender {
			matches = reconciliationMetadataMatchesSender(observed.Metadata, state.Metadata, true)
		}
		if !matches {
			failure := incompleteEvidence("consistency", "metadata-changed"+metadataDifference(observed.Metadata, state.Metadata), errors.New("metadata changed after enumeration"))
			left, right := observed.Metadata.Sender.Address, state.Metadata.Sender.Address
			if capture && observed.Ref == r.Ref && state.Ref == r.Ref && left != right && (!utf8.ValidString(left) || !utf8.ValidString(right) || !strings.EqualFold(left, right)) {
				enumeratedOrigin, enumOK := enumeratedOrigins[string(r.Ref.MessageID)]
				inspectedOrigin, inspectOK := inspectedOrigins[string(r.Ref.MessageID)]
				if !enumOK || !inspectOK || !enumeratedOrigin.MatchesMappedSender(left) || !inspectedOrigin.MatchesMappedSender(right) {
					return out, incompleteEvidence("consistency", "provenance-state-changed", errOriginMismatch)
				}
				failure.(*ReconciliationError).SenderAddressDiagnostic = &ReconciliationSenderAddressDiagnostic{AccountID: p.account.ID, N: r.N, ImmutableID: r.Ref.MessageID, EnumeratedSenderAddress: left, InspectedSenderAddress: right, EnumeratedOrigin: &enumeratedOrigin, InspectedOrigin: &inspectedOrigin}
			}
			return out, failure
		}
		precondition := observed
		if admitSender {
			precondition.Metadata.Sender.Address = right
		}
		if e = mailbox.CheckPrecondition(p.account.ID, precondition, state); e != nil {
			return out, incompleteEvidence("consistency", "precondition-changed", e)
		}
		if e = mailbox.CheckPrecondition(p.account.ID, expected, state); e != nil {
			return out, reconciliationBlock("original identity, sender or header conflicts with current evidence; preserve the mirror and request owner review")
		}
		if metadataChanged {
			out.EnumeratedStates[r.N] = reconciliationObservation(observed)
		}
		if admitSender {
			out.SenderSourceIntegrity[r.N] = true
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

func reconciliationObservation(s mailbox.State) mailbox.State {
	if s.Metadata.Recipients != nil {
		s.Metadata.Recipients = append([]mailbox.Address{}, s.Metadata.Recipients...)
	}
	if s.Memberships != nil {
		s.Memberships = append([]mailbox.Membership{}, s.Memberships...)
	}
	return s
}
