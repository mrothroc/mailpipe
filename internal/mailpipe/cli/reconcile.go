package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/graph"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
	"github.com/spf13/cobra"
)

type legacyEvidenceProvider interface {
	ReconcileLegacyReferences(context.Context, []graphmail.LegacyReference) (graphmail.LegacyEvidence, error)
}

type reconciliationVerifier struct {
	provider  mailbox.Provider
	confirmed bool
	report    store.LegacyReconciliationReceipt
}

func (v *reconciliationVerifier) ReconcileLegacy(ctx context.Context, a mailbox.Account, p store.LegacyPreparation) (store.LegacyReconciliation, error) {
	proof := store.LegacyReconciliation{Verification: store.LegacyVerification{Account: a, Nonce: p.Challenge(), States: map[int64]mailbox.State{}, Folders: map[string]mailbox.CollectionRef{}, HistoricalAttributionConfirmed: v.confirmed}, Unavailable: map[int64]string{}}
	if v.provider.Account() != a {
		return proof, &mailbox.AccountMismatchError{Operation: "legacy reconciliation", Expected: a.ID, Actual: v.provider.Account().ID}
	}
	collector, ok := v.provider.(legacyEvidenceProvider)
	if !ok {
		return proof, &mailbox.UnsupportedError{Operation: "Graph legacy reconciliation evidence"}
	}
	refs := make([]graphmail.LegacyReference, 0, len(p.Live))
	for _, m := range p.Live {
		scope, ok := proof.Verification.Folders[m.Folder]
		if !ok {
			c, e := resolveCollection(ctx, v.provider, m.Folder)
			if e != nil {
				var api *graph.APIError
				if errors.As(e, &api) && api.Status == 404 {
					return proof, &graphmail.ReconciliationError{Structural: true, Reason: "historical collection is unavailable; preserve the mirror and request owner review of collection mapping", Cause: e}
				}
				return proof, &graphmail.ReconciliationError{Reason: "historical collection mapping could not be read; resolve the read failure and retry a reconciliation preview", Cause: e}
			}
			scope = c.Ref
			proof.Verification.Folders[m.Folder] = scope
		}
		refs = append(refs, graphmail.LegacyReference{N: m.N, Ref: mailbox.MessageRef{AccountID: a.ID, MessageID: mailbox.MessageID(m.GraphID), Locator: mailbox.Locator(m.GraphID)}, InternetMessageID: m.InternetMessageID, Sender: m.Sender, Collection: scope})
	}
	evidence, e := collector.ReconcileLegacyReferences(ctx, refs)
	if e != nil {
		return proof, e
	}
	proof.Verification.States = evidence.States
	for n, unavailable := range evidence.Unavailable {
		if unavailable {
			proof.Unavailable[n] = store.LegacyUnavailable
		}
	}
	proof.Coverage = store.LegacyPrimaryMailbox
	proof.Started = evidence.Started
	proof.Finished = evidence.Finished
	proof.Pages = evidence.Pages
	proof.Observed = evidence.Observed
	v.report, e = store.ReviewLegacyReconciliation(a, p, proof)
	return proof, e
}
func reconciliationError(e error) error {
	if e == nil {
		return nil
	}
	var evidence *graphmail.ReconciliationError
	if errors.As(e, &evidence) {
		if evidence.Structural {
			return agentio.NewStaleError(evidence.Error(), "preserve the mirror and request owner review of the original identity evidence")
		}
		return agentio.NewError(evidence.Error(), "resolve the read failure, then mailpipe account reconcile")
	}
	if errors.Is(e, mailbox.ErrStaleIdentity) {
		return agentio.NewStaleError("legacy reconciliation evidence changed or conflicts with the reviewed proposal", "mailpipe account reconcile")
	}
	return e
}
func legacyAttributionError(e error) error {
	if errors.Is(e, mailbox.ErrStaleIdentity) {
		return agentio.NewStaleError("legacy live verification did not complete; preserve the mirror and reconcile the original references", "mailpipe account reconcile")
	}
	return e
}

func receiptError(e error) error {
	if errors.Is(e, sql.ErrNoRows) {
		return agentio.NewError("this binding has no reconciliation receipt", "mailpipe account show")
	}
	return e
}
func saveReconciliationRouting(ctx context.Context, deps Deps, cfg config.Config, p mailbox.Provider, r store.LegacyReconciliationReceipt) error {
	collections := map[string]string{}
	for name, ref := range r.Folders {
		collections[name] = string(ref.CollectionID)
	}
	for _, name := range []string{"inbox", "deleteditems", "sentitems", "junkemail"} {
		c, e := resolveCollection(ctx, p, name)
		if e != nil {
			return fmt.Errorf("database reconciled but routing resolution failed; retry account reconcile with the same confirmations: %w", e)
		}
		collections[name] = string(c.Ref.CollectionID)
	}
	if _, e := saveRoutingConfig(ctx, deps, cfg, p.Account(), collections); e != nil {
		return fmt.Errorf("database reconciled but routing save failed; retry account reconcile with the same confirmations: %w", e)
	}
	return nil
}
func reconciliationOutput(agent *agentio.AgentIO, status string, r store.LegacyReconciliationReceipt) error {
	counts := map[string]int{}
	for _, i := range r.Items {
		counts[i.Disposition]++
	}
	items := append([]store.LegacyReconciliationItem{{Disposition: "summary"}}, r.Items...)
	return agentio.Tabled(agent, []string{"status", "account", "provider", "source_digest", "reconciliation_digest", "n", "disposition", "original_id", "reason", "state", "verified", "unavailable", "historical"}, items, func(i store.LegacyReconciliationItem) []string {
		state, _ := json.Marshal(i.State)
		return []string{status, string(r.Account.ID), string(r.Account.ProviderID), r.SourceDigest, r.Digest, itoa(int(i.N)), i.Disposition, i.OriginalID, i.Reason, string(state), itoa(counts["verified"]), itoa(counts["unavailable"]), itoa(counts["historical"])}
	}, func(i store.LegacyReconciliationItem) map[string]any {
		return map[string]any{"verified": counts["verified"], "unavailable": counts["unavailable"], "historical": counts["historical"], "status": status, "account": r.Account.ID, "provider": r.Account.ProviderID, "source_digest": r.SourceDigest, "reconciliation_digest": r.Digest, "coverage": r.Coverage, "started": r.Started, "finished": r.Finished, "pages": r.Pages, "observed": r.Observed, "n": i.N, "disposition": i.Disposition, "original_id": i.OriginalID, "reason": i.Reason, "original_removed": i.Removed, "state": i.State, "history_confirmed": r.HistoricalAttributionConfirmed, "backup": r.Backup, "backup_digest": r.BackupDigest}
	})
}
func newReconcileCommand(deps Deps) *cobra.Command {
	cmd := newAgentCommand("reconcile", "Review unavailable Graph references and explicitly attribute preserved history", cobra.NoArgs, func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
		if e := pilotGuard(cmd, "Graph historical reconciliation"); e != nil {
			return e
		}
		ctx := cmd.Context()
		execute, _ := cmd.Flags().GetBool("execute")
		confirmA, _ := cmd.Flags().GetString("confirm-account")
		confirmD, _ := cmd.Flags().GetString("confirm-history")
		confirmR, _ := cmd.Flags().GetString("confirm-reconciliation")
		cfg, e := routing()
		if e != nil {
			return e
		}
		// Receipt display is offline and never creates a first binding.
		if !execute && cfg.AccountID != "" {
			receipt, found, e := store.ReadLegacyReconciliationReceipt(ctx, dbPath(cmd), mailbox.Account{ID: mailbox.AccountID(cfg.AccountID), ProviderID: store.GraphProvider})
			if e != nil {
				return receiptError(e)
			}
			if found {
				return reconciliationOutput(agent, "bound", receipt)
			}
		}
		p, e := lazyGraphProvider(ctx, deps)()
		if e != nil {
			return e
		}
		if cfg.AccountID != "" && cfg.AccountID != string(p.Account().ID) {
			return &mailbox.AccountMismatchError{Operation: "legacy reconciliation", Expected: mailbox.AccountID(cfg.AccountID), Actual: p.Account().ID}
		}
		if execute && (confirmA != string(p.Account().ID) || confirmD == "" || confirmR == "") {
			return agentio.NewUsageError("reconciliation requires exact reviewed account, history and reconciliation confirmations", "mailpipe account reconcile")
		}
		legacy, openErr := deps.Store(dbPath(cmd))
		if openErr != nil {
			if !execute {
				return openErr
			}
			bound, e := openBound(ctx, deps, dbPath(cmd), p.Account())
			if e != nil {
				return errors.Join(openErr, e)
			}
			receipt, e := bound.LegacyReconciliationReceipt(ctx)
			e = errors.Join(e, bound.Close())
			if e != nil {
				return e
			}
			if receipt.Account != p.Account() || receipt.SourceDigest != confirmD || receipt.Digest != confirmR {
				return agentio.NewUsageError("confirmations do not match the committed reconciliation receipt", "mailpipe account reconcile")
			}
			if e = saveReconciliationRouting(ctx, deps, cfg, p, receipt); e != nil {
				return e
			}
			return reconciliationOutput(agent, "bound", receipt)
		}
		if e = legacy.Close(); e != nil {
			return e
		}
		fmt.Fprintln(agent.Stderr(), "[hint] preparing exact historical inventory; keep other database sessions closed")
		prep, e := store.PrepareLegacyBinding(ctx, dbPath(cmd))
		if e != nil {
			return e
		}
		verifier := &reconciliationVerifier{provider: p, confirmed: execute && confirmD == prep.Digest()}
		if execute && !verifier.confirmed {
			return agentio.NewUsageError("history changed or was not reviewed", "mailpipe account reconcile")
		}
		fmt.Fprintln(agent.Stderr(), "[hint] observing complete primary-mailbox metadata and inspecting original references")
		status := "review"
		if execute {
			bound, e := store.ReconcileLegacy(ctx, dbPath(cmd), p.Account(), prep, confirmR, verifier)
			if e != nil {
				return reconciliationError(e)
			}
			receipt, e := bound.LegacyReconciliationReceipt(ctx)
			e = errors.Join(e, bound.Close())
			if e != nil {
				return e
			}
			verifier.report = receipt
			if e = saveReconciliationRouting(ctx, deps, cfg, p, receipt); e != nil {
				return e
			}
			status = "bound"
		} else {
			if _, e = verifier.ReconcileLegacy(ctx, p.Account(), prep); e != nil {
				return reconciliationError(e)
			}
			fmt.Fprintf(agent.Stderr(), "[hint] review every disposition and confirm ALL history belongs to this account, including unavailable references; no deletion or historical ownership is proved by absence. With owner approval: mailpipe account reconcile --execute --confirm-account %s --confirm-history %s --confirm-reconciliation %s\n", p.Account().ID, prep.Digest(), verifier.report.Digest)
		}
		return reconciliationOutput(agent, status, verifier.report)
	})
	cmd.Flags().Bool("execute", false, "bind only the freshly regenerated owner-reviewed reconciliation")
	cmd.Flags().String("confirm-account", "", "exact reviewed immutable account")
	cmd.Flags().String("confirm-history", "", "exact reviewed complete historical inventory digest")
	cmd.Flags().String("confirm-reconciliation", "", "exact reviewed reconciliation proposal digest")
	return cmd
}
