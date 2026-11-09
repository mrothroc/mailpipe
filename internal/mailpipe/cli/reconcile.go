package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
	"github.com/spf13/cobra"
)

type legacyEvidenceProvider interface {
	ReconcileLegacyReferences(context.Context, []graphmail.LegacyReference) (graphmail.LegacyEvidence, error)
}

type senderAddressEvidenceProvider interface {
	ReconcileLegacyReferencesWithSenderAddressDiagnostics(context.Context, []graphmail.LegacyReference) (graphmail.LegacyEvidence, error)
}

type senderAddressObserver interface {
	ObserveLegacySenderAddress(context.Context, []graphmail.LegacyReference, int64) (graphmail.ReconciliationSenderAddressDiagnostic, error)
}

type reconciliationVerifier struct {
	senderDiagnostics bool
	selectedOriginal  int64
	provider          mailbox.Provider
	confirmed         bool
	report            store.LegacyReconciliationReceipt
}

func (v *reconciliationVerifier) ReconcileLegacy(ctx context.Context, a mailbox.Account, p store.LegacyPreparation) (proof store.LegacyReconciliation, err error) {
	v.report = store.LegacyReconciliationReceipt{}
	defer func() {
		if err != nil {
			proof = store.LegacyReconciliation{}
			v.report = store.LegacyReconciliationReceipt{}
		}
	}()
	proof = store.LegacyReconciliation{Verification: store.LegacyVerification{Account: a, Nonce: p.Challenge(), States: map[int64]mailbox.State{}, Folders: map[string]mailbox.CollectionRef{}, HistoricalAttributionConfirmed: v.confirmed}, Unavailable: map[int64]string{}}
	if v.provider.Account() != a {
		return proof, &mailbox.AccountMismatchError{Operation: "legacy reconciliation", Expected: a.ID, Actual: v.provider.Account().ID}
	}
	collector, ok := v.provider.(legacyEvidenceProvider)
	if !ok && v.selectedOriginal == 0 {
		return proof, &mailbox.UnsupportedError{Operation: "Graph legacy reconciliation evidence"}
	}
	if v.selectedOriginal != 0 {
		if _, ok := v.provider.(senderAddressObserver); !ok {
			return proof, &graphmail.ReconciliationError{Reason: "selected-original sender-address diagnostics unsupported by this provider"}
		}
		matches := 0
		for _, m := range p.Live {
			if m.N == v.selectedOriginal {
				matches++
			}
		}
		if matches != 1 {
			return proof, &graphmail.ReconciliationError{Reason: "requested original is not a unique live historical reference"}
		}
	} else if v.senderDiagnostics {
		if _, ok := v.provider.(senderAddressEvidenceProvider); !ok {
			return proof, &graphmail.ReconciliationError{Reason: "sender-address diagnostics unsupported by this provider"}
		}
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
	var evidence graphmail.LegacyEvidence
	var e error
	if v.selectedOriginal != 0 {
		d, e := v.provider.(senderAddressObserver).ObserveLegacySenderAddress(ctx, refs, v.selectedOriginal)
		if e != nil {
			return proof, &graphmail.ReconciliationError{Reason: "selected-original observation could not be acquired; no reconciliation proposal was generated", Cause: e}
		}
		for _, r := range refs {
			if r.N == v.selectedOriginal && d.RequestedOriginal && d.AccountID == a.ID && d.N == r.N && d.ImmutableID == r.Ref.MessageID && d.EnumeratedOrigin.MatchesMappedSender(d.EnumeratedSenderAddress) && d.InspectedOrigin.MatchesMappedSender(d.InspectedSenderAddress) {
				// No store review or admitted State is produced on this path.
				return proof, &graphmail.ReconciliationError{Reason: "selected-original observation only; admission and historical identity were not evaluated", SenderAddressDiagnostic: &d}
			}
		}
		return proof, &graphmail.ReconciliationError{Reason: "selected-original observation association could not be verified"}
	} else if v.senderDiagnostics {
		diagnosticCollector := v.provider.(senderAddressEvidenceProvider)
		evidence, e = diagnosticCollector.ReconcileLegacyReferencesWithSenderAddressDiagnostics(ctx, refs)
	} else {
		evidence, e = collector.ReconcileLegacyReferences(ctx, refs)
	}
	if e != nil {
		return proof, e
	}
	proof.Verification.States = evidence.States
	proof.EnumeratedStates = evidence.EnumeratedStates
	proof.SenderSourceIntegrity = evidence.SenderSourceIntegrity
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
		if evidence.SenderAddressDiagnostic != nil && evidence.SenderAddressDiagnostic.RequestedOriginal {
			return agentio.NewError("Graph reconciliation: selected-original diagnostic only; admission and historical identity were not evaluated", "review the private diagnostic; no reconciliation proposal was generated")
		}
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

// reconciliationFailure retains error identity without rendering raw joined
// causes (including untrusted writer errors) into the command's error line.
type reconciliationFailure struct {
	mapped, cause, output error
}

func (e *reconciliationFailure) Error() string { return e.mapped.Error() }
func (e *reconciliationFailure) Code() int     { return agentio.ExitCode(e.mapped) }
func (e *reconciliationFailure) Unwrap() []error {
	errs := []error{e.mapped, e.cause}
	if e.output != nil {
		errs = append(errs, e.output)
	}
	return errs
}

func reconciliationFailureOutput(agent *agentio.AgentIO, cause error) error {
	mapped := reconciliationError(cause)
	var evidence *graphmail.ReconciliationError
	var output error
	if errors.As(cause, &evidence) && evidence.Structural && evidence.Conflict != nil {
		output = reconciliationConflictOutput(agent, evidence.Conflict)
		if output != nil {
			mapped = agentio.NewStaleError(evidence.Error()+"; conflict diagnostic output incomplete", "preserve the mirror and request owner review of the original identity evidence")
		}
	}
	return &reconciliationFailure{mapped: mapped, cause: cause, output: output}
}

func reconciliationConflictOutput(agent *agentio.AgentIO, c *graphmail.ReconciliationConflict) error {
	header := []string{"status", "diagnostic_kind", "diagnostic_scope", "enumeration_complete", "original_inspection_complete", "provider", "account", "n", "original_id", "original_header", "original_sender", "original_collection", "observed_id", "observed_header", "observed_sender", "observed_collection", "matching_originals", "retained_originals", "omitted_originals", "fields_omitted_bytes", "started", "observed_at", "pages", "observed"}
	rows := make([]map[string]any, 0, len(c.Originals))
	for _, original := range c.Originals {
		// Only primitive containers reach Tabled: agentio does not recursively
		// redact typed structs or pointers. Keys are fixed application labels.
		omitted := map[string]any{}
		row := map[string]any{
			"status": "blocked", "diagnostic_kind": "historical_header_sender_collision", "diagnostic_scope": "first_conflicting_observation",
			"enumeration_complete": false, "original_inspection_complete": false, "provider": "graph", "n": original.N,
			"matching_originals": c.MatchingOriginals, "retained_originals": len(c.Originals), "omitted_originals": c.MatchingOriginals - len(c.Originals),
			"fields_omitted_bytes": omitted, "started": c.Started.Format(time.RFC3339Nano), "observed_at": c.ObservedAt.Format(time.RFC3339Nano), "pages": c.Pages, "observed": c.Observed,
		}
		fields := map[string]graphmail.ReconciliationDiagnosticField{
			"account": c.Account, "original_id": original.Message.ID, "original_header": original.Message.InternetMessageID,
			"original_sender": original.Message.Sender, "original_collection": original.Message.CollectionID,
			"observed_id": c.ObservedMessage.ID, "observed_header": c.ObservedMessage.InternetMessageID,
			"observed_sender": c.ObservedMessage.Sender, "observed_collection": c.ObservedMessage.CollectionID,
		}
		for name, field := range fields {
			row[name] = field.Value
			if field.OmittedBytes != 0 {
				omitted[name] = field.OmittedBytes
			}
		}
		rows = append(rows, row)
	}
	if err := agentio.Tabled(agent, header, rows, func(row map[string]any) []string {
		values := make([]string, len(header))
		for i, key := range header {
			if key == "fields_omitted_bytes" {
				// This map has only fixed keys and byte counts, no metadata.
				raw, _ := json.Marshal(row[key])
				values[i] = string(raw)
			} else {
				values[i] = fmt.Sprint(row[key])
			}
		}
		return values
	}, func(row map[string]any) map[string]any { return row }); err != nil {
		return err
	}
	_, err := fmt.Fprintln(agent.Stderr(), "[hint] blocked: incomplete first-conflict correlation diagnostics only; retain this untrusted report privately, preserve the mirror and request owner review; no identity, historical attribution or action authority is granted")
	return err
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
	rows := make([]map[string]any, 0, len(items))
	for _, i := range items {
		// Convert nested DTOs before Tabled so shared recursive redaction reaches
		// every metadata, identity and collection string, including overflow JSON.
		raw, err := json.Marshal(i)
		if err != nil {
			return err
		}
		var nested map[string]any
		if err = json.Unmarshal(raw, &nested); err != nil {
			return err
		}
		rows = append(rows, map[string]any{
			"verified": counts["verified"], "unavailable": counts["unavailable"], "historical": counts["historical"], "status": status,
			"version": r.Version, "account": string(r.Account.ID), "provider": string(r.Account.ProviderID), "source_digest": r.SourceDigest, "reconciliation_digest": r.Digest, "audit_digest": r.AuditDigest,
			"coverage": r.Coverage, "started": r.Started.Format(time.RFC3339Nano), "finished": r.Finished.Format(time.RFC3339Nano), "pages": r.Pages, "observed": r.Observed,
			"n": i.N, "disposition": i.Disposition, "original_id": i.OriginalID, "reason": i.Reason, "original_removed": i.Removed,
			"original_folder": i.OriginalFolder, "original_memberships": nested["original_memberships"], "membership_changed": i.MembershipChanged, "state": nested["state"],
			"enumerated_state": nested["enumerated_state"], "recipient_name_changed": i.RecipientNameChanged, "recipient_casing_changed": i.RecipientCasingChanged, "sender_casing_changed": i.SenderCasingChanged, "sender_name_changed": i.SenderNameChanged, "sender_address_disagrees": i.SenderAddressDisagrees,
			"history_confirmed": r.HistoricalAttributionConfirmed, "backup": r.Backup, "backup_digest": r.BackupDigest,
		})
	}
	header := []string{"status", "account", "provider", "source_digest", "reconciliation_digest", "audit_digest", "n", "disposition", "original_id", "reason", "original_folder", "original_memberships", "membership_changed", "state", "recipient_name_changed", "recipient_casing_changed", "sender_casing_changed", "sender_name_changed", "sender_address_disagrees", "enumerated_state", "verified", "unavailable", "historical"}
	return agentio.Tabled(agent, header, rows, func(row map[string]any) []string {
		values := make([]string, len(header))
		for j, key := range header {
			if key == "state" || key == "original_memberships" || key == "enumerated_state" {
				raw, _ := json.Marshal(row[key])
				values[j] = string(raw)
			} else {
				values[j] = fmt.Sprint(row[key])
			}
		}
		return values
	}, func(row map[string]any) map[string]any { return row })
}
func newReconcileCommand(deps Deps) *cobra.Command {
	cmd := newAgentCommand("reconcile", "Review unavailable Graph references and explicitly attribute preserved history", cobra.NoArgs, func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) (result error) {
		if e := pilotGuard(cmd, "Graph historical reconciliation"); e != nil {
			return e
		}
		ctx := cmd.Context()
		execute, _ := cmd.Flags().GetBool("execute")
		confirmA, _ := cmd.Flags().GetString("confirm-account")
		confirmD, _ := cmd.Flags().GetString("confirm-history")
		confirmR, _ := cmd.Flags().GetString("confirm-reconciliation")
		diagnosticPath, _ := cmd.Flags().GetString("sender-address-diagnostic-out")
		selectedText, _ := cmd.Flags().GetString("sender-address-diagnostic-n")
		var selectedN int64
		if cmd.Flags().Changed("sender-address-diagnostic-n") {
			var parseErr error
			selectedN, parseErr = strconv.ParseInt(selectedText, 10, 64)
			if parseErr != nil || selectedN <= 0 || strconv.FormatInt(selectedN, 10) != selectedText || !cmd.Flags().Changed("sender-address-diagnostic-out") {
				return agentio.NewUsageError("sender-address diagnostic selection requires a canonical positive original number and an output destination", "mailpipe account reconcile --help")
			}
		}
		var diagnostic *senderDiagnosticOutput
		if cmd.Flags().Changed("sender-address-diagnostic-out") {
			if cmd.Flags().Changed("execute") || cmd.Flags().Changed("confirm-account") || cmd.Flags().Changed("confirm-history") || cmd.Flags().Changed("confirm-reconciliation") {
				return agentio.NewUsageError("sender-address diagnostics require an unconfirmed preview", "mailpipe account reconcile --help")
			}
			var err error
			diagnostic, err = openSenderDiagnostic(diagnosticPath, dbPath(cmd), config.DefaultPath())
			if err != nil {
				return &reconciliationFailure{mapped: agentio.NewUsageError("unsafe sender-address diagnostic destination", "use a new absolute file in a separate existing owner-only directory"), cause: err}
			}
			defer func() {
				if err := diagnostic.close(); err != nil {
					result = senderDiagnosticClosureFailure(result, err)
				}
			}()
		}
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
				if diagnostic != nil {
					return agentio.NewUsageError("sender-address diagnostics require a fresh unbound preview", "mailpipe account reconcile")
				}
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
		verifier := &reconciliationVerifier{provider: p, senderDiagnostics: diagnostic != nil, selectedOriginal: selectedN, confirmed: execute && confirmD == prep.Digest()}
		if execute && !verifier.confirmed {
			return agentio.NewUsageError("history changed or was not reviewed", "mailpipe account reconcile")
		}
		fmt.Fprintln(agent.Stderr(), "[hint] observing complete primary-mailbox metadata and inspecting original references")
		status := "review"
		if execute {
			bound, e := store.ReconcileLegacy(ctx, dbPath(cmd), p.Account(), prep, confirmR, verifier)
			if e != nil {
				return reconciliationFailureOutput(agent, e)
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
			fmt.Fprintln(agent.Stderr(), "[hint] reviewed approval matched; audit_digest records the fresh execution observations and may differ from preview")
			status = "bound"
		} else {
			if _, e = verifier.ReconcileLegacy(ctx, p.Account(), prep); e != nil {
				if diagnostic != nil {
					return diagnostic.failure(ctx, agent, e)
				}
				return reconciliationFailureOutput(agent, e)
			}
			if diagnostic != nil {
				if err := diagnostic.close(); err != nil {
					return senderDiagnosticClosureFailure(nil, err)
				}
			}
			fmt.Fprintln(agent.Stderr(), "[hint] reconciliation_digest identifies the inspected-state/history proposal to approve; audit_digest identifies this run's raw observations only. A fresh execution audit may differ from preview; it cannot authorize attribution")
			fmt.Fprintf(agent.Stderr(), "[hint] review every disposition, old-to-current membership change, sender-address disagreement, address casing pair and sender/recipient display-name pair, and confirm ALL history belongs to this account, including unavailable references; no deletion or historical ownership is proved by absence. With owner approval: mailpipe account reconcile --execute --confirm-account %s --confirm-history %s --confirm-reconciliation %s\n", p.Account().ID, prep.Digest(), verifier.report.Digest)
		}
		return reconciliationOutput(agent, status, verifier.report)
	})
	cmd.Flags().String("sender-address-diagnostic-out", "", "write a sender-address observation to a new owner-private file (preview only; no proof)")
	cmd.Flags().String("sender-address-diagnostic-n", "", "select one original observation, without admission or historical validation; requires diagnostic-out; always exits runtime 1, even when saved")
	cmd.Flags().Bool("execute", false, "bind only fresh validated evidence matching the reviewed approval; audit is regenerated")
	cmd.Flags().String("confirm-account", "", "exact reviewed immutable account")
	cmd.Flags().String("confirm-history", "", "exact reviewed complete historical inventory digest")
	cmd.Flags().String("confirm-reconciliation", "", "exact reviewed reconciliation approval digest (not audit_digest)")
	return cmd
}
