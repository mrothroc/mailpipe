package cli

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/rules"
	"github.com/mrothroc/mailpipe/pkg/stage"
	"github.com/mrothroc/mailpipe/pkg/store"
)

// assessSenders decides which senders the blocklist holds: those any applied
// plan moved mail from, with none of their mail kept anywhere in the mailbox,
// and the policy's block senders whatever is kept; never the owner, a
// contact, a keep, never_block or junk sender. No candidates is no error.
func assessSenders(ctx context.Context, s *commandStore, srv rules.ManagedService, c config.Stages) ([]rules.Candidate, error) {
	moved, err := s.MovedSenders(ctx)
	if err != nil {
		return nil, err
	}
	// Block senders are candidates even when no blocking plan moved their mail.
	seen := map[string]bool{}
	for _, m := range moved {
		seen[m.Sender] = true
	}
	for _, b := range c.Policy.BlockSenders {
		if b = strings.ToLower(strings.TrimSpace(b)); b != "" && !seen[b] {
			seen[b] = true
			moved = append(moved, store.SenderMoves{Sender: b})
		}
	}
	sort.Slice(moved, func(i, j int) bool { return moved[i].Sender < moved[j].Sender })
	if len(moved) == 0 {
		return nil, nil
	}
	contacts, err := s.SentRecipients(ctx)
	if err != nil {
		return nil, err
	}
	known := stage.NewAllowlist("rules", c.SelfAddresses, c.SelfDomains, contacts, nil) // automated senders stay protected from the blocklist
	protect := func(sender string) string {
		if c.Policy.Keeps(sender) {
			return "keep_sender"
		}
		if c.Policy.NeverBlocks(sender) {
			return "never_block"
		}
		if c.Policy.Junks(sender) {
			return "junk_sender"
		}
		// The allowlist cannot fail; a keep without a label still protects.
		if v, _ := known.Evaluate(ctx, stage.Message{Sender: sender}); v.Action == stage.Keep {
			if len(v.Labels) > 0 {
				return v.Labels[0]
			}
			return "allowlist"
		}
		return ""
	}
	// Only senders who could be blocked are searched for on the server.
	var search []string
	for _, m := range moved {
		if strings.TrimSpace(m.Sender) != "" && protect(m.Sender) == "" {
			search = append(search, m.Sender)
		}
	}
	kept, err := srv.Kept(ctx, search)
	if err != nil {
		return nil, agentio.NewError(fmt.Sprintf("check where each sender's mail is: %v; nothing was written", err),
			"mailpipe rules senders")
	}
	expired, err := s.ExpiredSenders(ctx)
	if err != nil {
		return nil, err
	}
	return rules.Assess(moved, kept, expired, protect, c.Policy.Blocks), nil
}

// openRules opens the store and Graph; the caller closes the store.
func openRules(cmd *cobra.Command, deps Deps) (*commandStore, mailbox.Provider, error) {
	g, err := lazyProvider(cmd.Context(), deps)()
	if err != nil {
		return nil, nil, err
	}
	s, err := openStore(cmd, deps)
	if err != nil {
		return nil, nil, err
	}
	return s, g, nil
}

// blocklistDiff assesses the senders and diffs the managed rules (the
// blocklist, then the junk rules) against the server's.
func blocklistDiff(ctx context.Context, s *commandStore, g mailbox.Provider, srv rules.ManagedService) ([]rules.Change, error) {
	c, err := loadStages()
	if err != nil {
		return nil, err
	}
	cs, err := assessSenders(ctx, s, srv, c)
	if err != nil {
		return nil, err
	}
	server, err := srv.List(ctx)
	if err != nil {
		return nil, err
	}
	desired := append(rules.Desired(rules.Blocked(cs)), rules.DesiredJunk(c.Policy.JunkSenders)...)
	changes := rules.Diff(desired, server)
	if len(changes) == 0 {
		return nil, agentio.NewDataError("no sender is blocked or junked and no managed rules are on the server", "mailpipe rules senders")
	}
	return changes, nil
}

func changesTable(agent *agentio.AgentIO, changes []rules.Change) error {
	return agentio.Tabled(agent, []string{"rule", "op", "senders", "added", "removed"}, changes,
		func(c rules.Change) []string {
			return []string{c.Rule.Name, string(c.Op), strconv.Itoa(len(c.Rule.Senders)), strconv.Itoa(len(c.Added)), strconv.Itoa(len(c.Removed))}
		},
		func(c rules.Change) map[string]any {
			return map[string]any{"rule": c.Rule.Name, "op": c.Op, "id": c.Rule.ID, "senders": c.Rule.Senders, "added": c.Added, "removed": c.Removed}
		})
}

func countPending(changes []rules.Change) int {
	n := 0
	for _, c := range changes {
		if c.Op != rules.Same {
			n++
		}
	}
	return n
}

func newRulesCommand(deps Deps) *cobra.Command {
	senders := newAgentCommand("senders",
		"List every sender an applied plan moved mail from, and whether the blocklist blocks them (only if none of their mail is kept anywhere)",
		cobra.NoArgs, func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			s, g, err := openRules(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			c, err := loadStages()
			if err != nil {
				return err
			}
			srv, err := rules.ManagedFor(g)
			if err != nil {
				return err
			}
			cs, err := assessSenders(cmd.Context(), s, srv, c)
			if err != nil {
				return err
			}
			if len(cs) == 0 {
				return agentio.NewDataError("no applied plan has moved mail yet", "mailpipe apply <plan-id> --execute")
			}
			if err := agentio.Tabled(agent, []string{"sender", "moved", "kept", "expired", "status", "reason"}, cs,
				func(c rules.Candidate) []string {
					return []string{c.Sender, strconv.Itoa(c.Moved), strconv.Itoa(c.Kept), strconv.Itoa(c.Expired), status(c), c.Reason}
				},
				func(c rules.Candidate) map[string]any {
					return map[string]any{"sender": c.Sender, "moved": c.Moved, "kept": c.Kept, "expired": c.Expired, "status": status(c), "reason": c.Reason}
				}); err != nil {
				return err
			}
			fmt.Fprintf(agent.Stderr(), "[hint] %d of %d senders blocked. Try: mailpipe rules diff\n", len(rules.Blocked(cs)), len(cs))
			return nil
		})

	diff := newAgentCommand("diff",
		"Compare the blocklist with the Inbox rules on the server (create, update, replace, delete or same; retired per-plan rules are deleted)",
		cobra.NoArgs, func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			s, g, err := openRules(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			srv, err := rules.ManagedFor(g)
			if err != nil {
				return err
			}
			changes, err := blocklistDiff(cmd.Context(), s, g, srv)
			if err != nil {
				return err
			}
			if err := changesTable(agent, changes); err != nil {
				return err
			}
			if n := countPending(changes); n > 0 {
				fmt.Fprintf(agent.Stderr(), "[hint] %d to change. Try: mailpipe rules apply --execute\n", n)
			} else {
				fmt.Fprintln(agent.Stderr(), "[hint] nothing to change: the server matches the managed rules")
			}
			return nil
		})

	apply := newAgentCommand("apply [--execute]",
		"Write the blocklist to the server: show the diff, and with --execute carry it out and read it back",
		cobra.NoArgs, func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			execute, _ := cmd.Flags().GetBool("execute")
			s, g, err := openRules(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			ctx := cmd.Context()
			srv, err := rules.ManagedFor(g)
			if err != nil {
				return err
			}
			changes, err := blocklistDiff(ctx, s, g, srv)
			if err != nil {
				return err
			}
			n := countPending(changes)
			if !execute {
				if err := changesTable(agent, changes); err != nil {
					return err
				}
				next := ""
				if n > 0 {
					next = ". Try: mailpipe rules apply --execute"
				}
				fmt.Fprintf(agent.Stderr(), "[hint] check only, nothing written: %d to change%s\n", n, next)
				return nil
			}
			const retry = "mailpipe rules apply --execute"
			done, runErr := srv.ExecuteReviewed(ctx, changes)
			tableErr := changesTable(agent, changes)
			if runErr != nil { // the failed write matters more than the table
				what := fmt.Sprintf("made %d of %d changes, then %v", done, n, runErr)
				if tableErr != nil {
					what += fmt.Sprintf(" (printing the changes failed too: %v)", tableErr)
				}
				return agentio.NewError(what, retry)
			}
			if tableErr != nil {
				return tableErr
			}
			// Read the server back: what was written must now match the blocklist.
			server, err := srv.List(ctx)
			if err != nil {
				return agentio.NewError(fmt.Sprintf("made %d changes, but could not read the rules back: %v", done, err), "mailpipe rules diff")
			}
			if left := countPending(rules.Diff(desiredFrom(changes), server)); left > 0 {
				return agentio.NewError(fmt.Sprintf("made %d changes, but %d still differ on the server", done, left), "mailpipe rules diff")
			}
			fmt.Fprintf(agent.Stderr(), "[hint] made %d %s; the server now matches the managed rules. Try: mailpipe rules list\n", done, plural(done, "change", "changes"))
			return nil
		})
	apply.Flags().Bool("execute", false, "write the rules (without it, only show the diff)")

	list := newAgentCommand("list", "List every Inbox rule on the server; kind is managed (the blocklist), retired (an old per-plan rule) or blank (yours)", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			g, err := lazyProvider(cmd.Context(), deps)()
			if err != nil {
				return err
			}
			srv, err := rules.ManagedFor(g)
			if err != nil {
				return err
			}
			server, err := srv.List(cmd.Context())
			if err != nil {
				return err
			}
			if len(server) == 0 {
				return agentio.NewDataError("the Inbox has no rules", "mailpipe rules diff")
			}
			return agentio.Tabled(agent, []string{"name", "sequence", "enabled", "kind", "senders", "managed_shape"}, server,
				func(r rules.Rule) []string {
					return []string{r.Name, strconv.Itoa(r.Sequence), strconv.FormatBool(r.Enabled), string(rules.KindOf(r.Name)), strconv.Itoa(len(r.Senders)), strconv.FormatBool(r.Exact)}
				},
				func(r rules.Rule) map[string]any {
					return map[string]any{"name": r.Name, "id": r.ID, "sequence": r.Sequence, "enabled": r.Enabled, "kind": string(rules.KindOf(r.Name)),
						"senders": r.Senders, "managed_shape": r.Exact}
				})
		})
	return newGroup("rules", "Manage server-side Inbox rules: a blocklist of senders whose mail applied plans deleted (to Deleted Items), and junk_senders (to Junk)", senders, diff, apply, list)
}

func status(c rules.Candidate) string {
	if c.Blocked {
		return "block"
	}
	return "exclude"
}

// desiredFrom recovers the rules a diff wants on the server.
func desiredFrom(changes []rules.Change) []rules.Rule {
	var out []rules.Rule
	for _, c := range changes {
		if c.Op != rules.Delete {
			out = append(out, c.Rule)
		}
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
