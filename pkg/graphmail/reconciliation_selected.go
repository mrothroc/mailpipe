package graphmail

import (
	"context"
	"errors"

	"github.com/mrothroc/mailpipe/pkg/graph"
)

// ObserveLegacySenderAddress acquires the same complete enumeration and ordered
// inspections as reconciliation, but returns only one unvalidated observation.
// It does not evaluate admission, establish historical identity, or return a
// State/evidence capability, even when both sender addresses are equal.
func (p *Provider) ObserveLegacySenderAddress(ctx context.Context, refs []LegacyReference, n int64) (ReconciliationSenderAddressDiagnostic, error) {
	var empty ReconciliationSenderAddressDiagnostic
	if n <= 0 {
		return empty, reconciliationBlock("invalid requested original")
	}
	reads, err := p.acquireLegacyReferences(ctx, refs, legacyMessageLimit, legacyPageLimit, true, n)
	if err != nil {
		return empty, err
	}
	var selected ReconciliationSenderAddressDiagnostic
	for i, r := range refs {
		if err := ctx.Err(); err != nil {
			return empty, incompleteEvidence("inspection", "", err)
		}
		observed, found := reads.listed[string(r.Ref.MessageID)]
		state, err := reads.states[i], reads.failures[i]
		if err != nil {
			var api *graph.APIError
			// A non-target unavailable read is not an absence disposition or an
			// anchor. All other read failures prevent a complete diagnostic.
			if r.N != n && !found && errors.As(err, &api) && api.Status == 404 && api.Code == "ErrorItemNotFound" {
				continue
			}
			return empty, incompleteEvidence("inspection", "", err)
		}
		if r.N != n {
			continue
		}
		if !found {
			return empty, incompleteEvidence("consistency", "reference-appeared", errors.New("requested reference was not enumerated"))
		}
		left, right := observed.Metadata.Sender.Address, state.Metadata.Sender.Address
		before, beforeOK := reads.enumeratedOrigins[string(r.Ref.MessageID)]
		after, afterOK := reads.inspectedOrigins[string(r.Ref.MessageID)]
		if observed.Ref != r.Ref || state.Ref != r.Ref || !beforeOK || !afterOK || !before.MatchesMappedSender(left) || !after.MatchesMappedSender(right) {
			return empty, incompleteEvidence("consistency", "provenance-state-changed", errOriginMismatch)
		}
		selected = ReconciliationSenderAddressDiagnostic{RequestedOriginal: true, AccountID: p.account.ID, N: r.N, ImmutableID: r.Ref.MessageID, EnumeratedSenderAddress: left, InspectedSenderAddress: right, EnumeratedOrigin: &before, InspectedOrigin: &after}
	}
	if err := ctx.Err(); err != nil {
		return empty, incompleteEvidence("completion", "", err)
	}
	if selected.N == 0 {
		return empty, reconciliationBlock("requested original observation unavailable")
	}
	return selected, nil
}
