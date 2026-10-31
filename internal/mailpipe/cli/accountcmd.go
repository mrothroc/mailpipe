package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
	"github.com/spf13/cobra"
)

type legacyVerifier struct {
	provider  mailbox.Provider
	confirmed bool
	folders   map[string]mailbox.CollectionRef
}

func (v *legacyVerifier) VerifyLegacy(ctx context.Context, a mailbox.Account, prep store.LegacyPreparation) (store.LegacyVerification, error) {
	if a != v.provider.Account() {
		return store.LegacyVerification{}, &mailbox.AccountMismatchError{Operation: "legacy attribution", Expected: v.provider.Account().ID, Actual: a.ID}
	}
	proof := store.LegacyVerification{Account: a, Nonce: prep.Challenge(), States: map[int64]mailbox.State{}, Folders: map[string]mailbox.CollectionRef{}, HistoricalAttributionConfirmed: v.confirmed}
	for _, m := range prep.Live {
		scope, ok := proof.Folders[m.Folder]
		if !ok {
			c, err := resolveCollection(ctx, v.provider, m.Folder)
			if err != nil {
				return proof, err
			}
			scope = c.Ref
			proof.Folders[m.Folder] = scope
		}
		ref := mailbox.MessageRef{AccountID: a.ID, MessageID: mailbox.MessageID(m.GraphID), Locator: mailbox.Locator(m.GraphID)}
		state, err := v.provider.Inspect(ctx, ref)
		if err != nil {
			return proof, err
		}
		expected := mailbox.State{Ref: ref, Metadata: mailbox.Metadata{InternetMessageID: m.InternetMessageID, Sender: mailbox.Address{Address: m.Sender}}, Memberships: []mailbox.Membership{{Collection: scope}}}
		if err = mailbox.CheckPrecondition(a.ID, expected, state); err != nil {
			return proof, err
		}
		proof.States[m.N] = state
	}
	v.folders = proof.Folders
	return proof, nil
}

func newAccountCommand(deps Deps) *cobra.Command {
	bind := newAgentCommand("bind [--execute --confirm-history <digest>]", "Verify and explicitly attribute an unbound historical database to the authenticated mailbox", cobra.NoArgs, func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
		ctx := cmd.Context()
		execute, _ := cmd.Flags().GetBool("execute")
		confirmation, _ := cmd.Flags().GetString("confirm-history")
		p, err := lazyProvider(ctx, deps)()
		if err != nil {
			return err
		}
		cfg, err := routing()
		if err != nil {
			return err
		}
		if cfg.AccountID != "" && cfg.AccountID != string(p.Account().ID) {
			return &mailbox.AccountMismatchError{Operation: "legacy attribution", Expected: mailbox.AccountID(cfg.AccountID), Actual: p.Account().ID}
		}
		// The normal opener performs backed-up schema migration; close its shared
		// lifecycle session before exclusive inventory or binding.
		legacy, openErr := deps.Store(dbPath(cmd))
		if openErr != nil {
			if !execute {
				return agentio.NewError(openErr.Error(), "mailpipe account bind --execute")
			}
			bound, e := openBound(ctx, deps, dbPath(cmd), p.Account())
			if e != nil {
				return errors.Join(openErr, e)
			}
			if e = bound.Close(); e != nil {
				return e
			}
			cfg.AccountID = string(p.Account().ID)
			if cfg.Collections == nil {
				cfg.Collections = map[string]string{}
			}
			for _, name := range []string{"inbox", "deleteditems", "sentitems", "junkemail"} {
				collection, e := resolveCollection(ctx, p, name)
				if e != nil {
					return e
				}
				cfg.Collections[name] = string(collection.Ref.CollectionID)
			}
			if e = config.Save(config.DefaultPath(), cfg); e != nil {
				return fmt.Errorf("database binding verified but routing save failed; retry account bind: %w", e)
			}
			return agentio.Tabled(agent, []string{"account", "provider", "status"}, []string{"bound"}, func(status string) []string { return []string{cfg.AccountID, "graph", status} }, func(status string) map[string]any {
				return map[string]any{"account": cfg.AccountID, "provider": "graph", "status": status}
			})
		}
		if err = legacy.Close(); err != nil {
			return err
		}
		prep, err := store.PrepareLegacyBinding(ctx, dbPath(cmd))
		if err != nil {
			return err
		}
		verifier := &legacyVerifier{provider: p, confirmed: execute && confirmation == prep.Digest()}
		if _, err = verifier.VerifyLegacy(ctx, p.Account(), prep); err != nil {
			return err
		}
		status := "verified"
		if execute {
			if !verifier.confirmed {
				return agentio.NewUsageError("historical attribution requires the exact reviewed digest, including removed records and history", "mailpipe account bind")
			}
			bound, e := store.BindLegacy(ctx, dbPath(cmd), p.Account(), prep, verifier)
			if e != nil {
				return e
			}
			if e = bound.Close(); e != nil {
				return e
			}
			cfg.AccountID = string(p.Account().ID)
			if cfg.Collections == nil {
				cfg.Collections = map[string]string{}
			}
			for name, ref := range verifier.folders {
				cfg.Collections[name] = string(ref.CollectionID)
			}
			if e = config.Save(config.DefaultPath(), cfg); e != nil {
				return fmt.Errorf("database bound but routing save failed; retry account bind --execute: %w", e)
			}
			status = "bound"
		}
		row := struct {
			Status, Digest string
			Live           int
		}{status, prep.Digest(), len(prep.Live)}
		if !execute {
			fmt.Fprintln(agent.Stderr(), "[hint] verification only; confirm that all removed records and history belong to this account before binding. Try: mailpipe account bind --execute --confirm-history "+prep.Digest())
		}
		return agentio.Tabled(agent, []string{"account", "provider", "status", "live", "digest"}, []struct {
			Status, Digest string
			Live           int
		}{row}, func(r struct {
			Status, Digest string
			Live           int
		}) []string {
			return []string{string(p.Account().ID), "graph", r.Status, itoa(r.Live), r.Digest}
		}, func(r struct {
			Status, Digest string
			Live           int
		}) map[string]any {
			return map[string]any{"account": p.Account().ID, "provider": "graph", "status": r.Status, "live": r.Live, "digest": r.Digest}
		})
	})
	bind.Flags().Bool("execute", false, "permanently bind verified history and save account routing")
	bind.Flags().String("confirm-history", "", "exact reviewed inventory digest confirming attribution of all history")
	return newGroup("account", "Verify and bind local account history", bind)
}
