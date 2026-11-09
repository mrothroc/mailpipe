package graphmail

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

const (
	ReferenceMethod            = "graph-exact-original-inspections-v1"
	ReferenceCoverage          = "graph-original-references-v1"
	ReferenceFound             = "found"
	ReferenceNotFound          = "item-not-found"
	ReferenceLimit             = 1000000
	ReferenceStateLimit  int64 = 256 << 20
	ReferenceReadTimeout       = 30 * time.Minute
)

type ReferenceContext struct {
	RequestedReferences int   `json:"requested_references"`
	CompletedReferences int   `json:"completed_references"`
	InspectionBatches   int   `json:"inspection_batches"`
	StateBytes          int64 `json:"state_bytes"`
}
type ReferenceOutcome struct {
	Ref             mailbox.MessageRef
	Kind            string
	State           *mailbox.State
	SourceValidated bool
}
type ReferenceEvidence struct {
	Account           mailbox.Account
	Method, Coverage  string
	Started, Finished time.Time
	Context           ReferenceContext
	Outcomes          map[int64]ReferenceOutcome
}

// ValidateReferenceRequests is new-recovery preflight, also composed before
// historical collection mapping. It does not change generic/legacy validators.
func ValidateReferenceRequests(account mailbox.AccountID, refs []mailbox.MessageRef) error {
	if len(refs) > ReferenceLimit {
		return reconciliationBlock("original-reference count limit reached; request a reviewed resource plan")
	}
	for _, r := range refs {
		if !utf8.ValidString(string(r.AccountID)) || !utf8.ValidString(string(r.MessageID)) || !utf8.ValidString(string(r.Locator)) {
			return reconciliationBlock("original reference has unsupported encoding")
		}
		if err := r.Validate(account); err != nil {
			return err
		}
		if string(r.MessageID) != string(r.Locator) {
			return reconciliationBlock("original reference locator differs")
		}
	}
	return nil
}
func referenceFailure(category string, err error) error {
	if category == "" {
		category = reconciliationReadCategory(err)
	}
	return &ReconciliationError{Reason: "incomplete original-reference evidence; phase=inspection; category=" + category + "; resolve the read failure and retry a reconciliation preview", Cause: err}
}

// validateReferenceResponse sees the actual ordered envelope before an outcome
// can be admitted. No raw error/message/origin trace escapes this operation.
func validateReferenceResponse(ctx context.Context, expectedID string, ref mailbox.MessageRef, response graph.BatchResponse, state mailbox.State, readErr error) error {
	bad := func() error {
		return referenceFailure("invalid-reference-response", errors.New("unsupported exact-reference response"))
	}
	if err := ctx.Err(); err != nil {
		return referenceFailure("", err)
	}
	if response.ID != expectedID {
		return bad()
	}
	if response.Status == 404 {
		fields, err := originObject(response.Body, "error")
		if err != nil {
			return bad()
		}
		fields, err = originObject(fields["error"], "code")
		if err != nil {
			return bad()
		}
		code, err := originString(fields["code"])
		if err != nil || code != "ErrorItemNotFound" {
			return bad()
		}
		var api *graph.APIError
		if !errors.As(readErr, &api) || api == nil || api.Status != 404 || api.Code != code {
			return bad()
		}
		return nil
	}
	if response.Status != 200 {
		if response.Status >= 200 && response.Status < 300 {
			return bad()
		}
		return nil // Other read failures remain fatal per-item errors, never absence.
	}
	if readErr != nil {
		return nil
	}
	fields, err := originObject(response.Body, "id", "parentFolderId", "internetMessageId", "subject", "from", "sender", "toRecipients", "ccRecipients", "receivedDateTime", "hasAttachments", "isRead", "bodyPreview", "changeKey", "@removed", "error")
	if err != nil {
		return bad()
	}
	if fields["@removed"] != nil || fields["error"] != nil {
		return bad()
	}
	for _, key := range []string{"id", "parentFolderId", "internetMessageId", "subject", "toRecipients", "ccRecipients", "receivedDateTime", "hasAttachments", "isRead", "bodyPreview"} {
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			return bad()
		}
	}
	if state.Ref != ref {
		return bad()
	}
	if _, err = reconciliationOrigin(ctx, response.Body, state); err != nil {
		return bad()
	}
	return nil
}

// ReconcileOriginalReferences inspects only the complete supplied historical
// inventory. Unavailable is a scoped exact lookup outcome, not deletion proof.
func (p *Provider) ReconcileOriginalReferences(ctx context.Context, refs []LegacyReference) (ReferenceEvidence, error) {
	return p.reconcileOriginalReferences(ctx, refs, ReferenceLimit, ReferenceStateLimit, ReferenceReadTimeout)
}
func (p *Provider) reconcileOriginalReferences(ctx context.Context, refs []LegacyReference, maxRefs int, maxBytes int64, readTimeout time.Duration) (out ReferenceEvidence, err error) {
	defer func() {
		if err != nil {
			out = ReferenceEvidence{}
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	if err = ctx.Err(); err != nil {
		return out, referenceFailure("", err)
	}
	if len(refs) > maxRefs {
		return out, reconciliationBlock("original-reference count limit reached; request a reviewed resource plan")
	}
	exact := make([]mailbox.MessageRef, len(refs))
	positions := make(map[string]int, len(refs))
	for i, r := range refs {
		exact[i] = r.Ref
		positions[string(r.Ref.MessageID)] = i
	}
	if err = ValidateReferenceRequests(p.account.ID, exact); err != nil {
		return out, err
	}
	if _, err = legacyReferenceTargets(p.account.ID, refs, 0); err != nil {
		return out, err
	}
	out = ReferenceEvidence{Account: p.account, Method: ReferenceMethod, Coverage: ReferenceCoverage, Started: time.Now().UTC(), Outcomes: map[int64]ReferenceOutcome{}}
	out.Context.RequestedReferences = len(refs)
	var stateBytes int64
	observe := func(ref mailbox.MessageRef, response graph.BatchResponse, state mailbox.State, readErr error) error {
		expected := strconv.Itoa(positions[string(ref.MessageID)]%graph.MaxBatch + 1)
		if e := validateReferenceResponse(ctx, expected, ref, response, state, readErr); e != nil {
			return e
		}
		if readErr == nil {
			raw, e := json.Marshal(state)
			if e != nil {
				return referenceFailure("invalid-reference-state", e)
			}
			if int64(len(raw)) > maxBytes-stateBytes {
				return referenceFailure("state-byte-limit", errors.New("reference state budget exhausted"))
			}
			stateBytes += int64(len(raw))
		}
		return nil
	}
	states, failures, e := p.inspectBatch(ctx, exact, observe)
	if e != nil {
		var safe *ReconciliationError
		if errors.As(e, &safe) {
			return out, e
		}
		return out, referenceFailure("", e)
	}
	if len(states) != len(refs) || len(failures) != len(refs) {
		return out, referenceFailure("incomplete-results", errors.New("incomplete exact reference results"))
	}
	found, missing := 0, 0
	for i, r := range refs {
		if e = ctx.Err(); e != nil {
			return out, referenceFailure("", e)
		}
		outcome := ReferenceOutcome{Ref: r.Ref}
		if failures[i] != nil {
			var api *graph.APIError
			if !errors.As(failures[i], &api) || api.Status != 404 || api.Code != "ErrorItemNotFound" {
				return out, referenceFailure("", failures[i])
			}
			// The all-response observer has already validated the raw404 envelope.
			outcome.Kind = ReferenceNotFound
			missing++
		} else {
			state := reconciliationObservation(states[i])
			expected := mailbox.State{Ref: r.Ref, Metadata: mailbox.Metadata{InternetMessageID: r.InternetMessageID, Sender: mailbox.Address{Address: r.Sender}}, Memberships: state.Memberships}
			if e = mailbox.CheckPrecondition(p.account.ID, expected, state); e != nil {
				return out, reconciliationBlock("original identity, sender or header conflicts with current evidence; preserve the mirror and request owner review")
			}
			outcome.Kind, outcome.State, outcome.SourceValidated = ReferenceFound, &state, true
			found++
		}
		out.Outcomes[r.N] = outcome
	}
	if missing > 0 && found == 0 {
		return out, reconciliationBlock("insufficient same-account live evidence; preserve the mirror and restore access to an original immutable reference")
	}
	out.Context.CompletedReferences = len(out.Outcomes)
	out.Context.InspectionBatches = (len(refs) + graph.MaxBatch - 1) / graph.MaxBatch
	out.Context.StateBytes = stateBytes
	out.Finished = time.Now().UTC()
	if e = ctx.Err(); e != nil {
		return out, referenceFailure("", e)
	}
	return out, nil
}
