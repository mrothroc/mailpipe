package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/apply"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/stage"
	"github.com/mrothroc/mailpipe/pkg/store"
)

// deleteDestination is where deleted mail goes: recoverable, never purged.
const deleteDestination = "deleteditems"

// parsePlanID reads a plan id argument.
func parsePlanID(arg string) (int64, error) {
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || id < 1 {
		return 0, agentio.NewUsageError(fmt.Sprintf("plan id must be a positive integer, got %q", arg), "mailpipe plan list")
	}
	return id, nil
}

func newApplyCommand(deps Deps) *cobra.Command {
	cmd := newAgentCommand("apply <plan-id> [--execute] [--max-items N]",
		"Carry out a plan's deletes: check each message on the server, then (with --execute) move it to Deleted Items", cobra.ExactArgs(1),
		func(cmd *cobra.Command, agent *agentio.AgentIO, args []string) error {
			id, err := parsePlanID(args[0])
			if err != nil {
				return err
			}
			execute, _ := cmd.Flags().GetBool("execute")
			maxItems, _ := cmd.Flags().GetInt("max-items")
			if maxItems < 0 {
				return agentio.NewUsageError("--max-items must not be negative", cmd.CommandPath()+" --help")
			}
			s, err := openStore(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			ctx := cmd.Context()
			_, items, err := s.Plan(ctx, id)
			if err != nil {
				return agentio.NewError(err.Error(), "mailpipe plan list")
			}
			g, err := lazyProvider(cmd.Context(), deps)()
			if err != nil {
				return err
			}
			opts := apply.Options{Execute: execute, MaxItems: maxItems, Action: stage.Delete, Destination: deleteDestination}

			// The folders whose Graph totals bracket an executed run: where the
			// messages are, then where they go.
			var folders []string
			seen := map[string]bool{}
			for _, it := range items {
				if f := it.PlannedFolder; it.Action == opts.Action && f != "" && f != deleteDestination && !seen[f] {
					seen[f] = true
					folders = append(folders, f)
				}
			}
			folders = append(folders, deleteDestination)
			var before map[string]int
			if execute {
				if before, err = totals(cmd, g, folders); err != nil {
					return err
				}
			}

			var rows []apply.Outcome
			sum, runErr := apply.RunBound(ctx, s.bound, g, id, opts, func(o apply.Outcome) error {
				o.Item.Folder = s.alias(o.Item.Folder)
				o.Item.PlannedFolder = s.alias(o.Item.PlannedFolder)
				if o.Detail == "collection memberships changed" {
					o.Detail = "no longer in " + o.Item.PlannedFolder
				}
				rows = append(rows, o)
				return nil
			})
			// A retry repeats what was asked: a failed check is never answered
			// with a suggestion to execute.
			retry := fmt.Sprintf("mailpipe apply %d", id)
			if execute {
				retry += " --execute"
			}
			if errors.Is(runErr, apply.ErrStalePlan) {
				return agentio.NewStaleError(fmt.Sprintf("plan %d: %v", id, runErr), "mailpipe plan create")
			}
			if runErr != nil && len(rows) == 0 {
				return agentio.NewError(runErr.Error()+"; nothing was moved", retry)
			}
			if len(rows) == 0 && runErr == nil {
				return agentio.NewDataError(fmt.Sprintf("nothing left to apply in plan %d (%d already moved)", id, sum.AlreadyMoved), "mailpipe sync")
			}
			if err := agentio.Tabled(agent, []string{"n", "sender", "subject", "status", "detail"}, rows,
				func(o apply.Outcome) []string {
					return []string{strconv.FormatInt(o.Item.N, 10), o.Item.Sender, oneLine(o.Item.Subject), o.Status, o.Detail}
				},
				func(o apply.Outcome) map[string]any {
					return map[string]any{"n": o.Item.N, "sender": o.Item.Sender, "subject": o.Item.Subject, "status": o.Status,
						"detail": o.Detail, "graph_id": o.Item.GraphID, "internet_message_id": o.Item.InternetMessageID}
				}); err != nil {
				return errors.Join(runErr, err)
			}

			if !execute {
				next := "" // executing is suggested only after a clean check
				if runErr == nil && sum.Failed == 0 && sum.WouldMove > 0 {
					next = fmt.Sprintf(". Try: mailpipe apply %d --execute", id)
				}
				fmt.Fprintf(agent.Stderr(), "[hint] check only, nothing moved: %d would move, %d skipped, %d failed (%d already moved)%s\n",
					sum.WouldMove, sum.Skipped, sum.Failed, sum.AlreadyMoved, next)
			} else {
				fmt.Fprintf(agent.Stderr(), "[hint] moved %d to %s, skipped %d, failed %d (%d already moved earlier)\n",
					sum.Moved, deleteDestination, sum.Skipped, sum.Failed, sum.AlreadyMoved)
				if after, err := totals(cmd, g, folders); err == nil {
					parts := make([]string, len(folders))
					for i, f := range folders {
						parts[i] = fmt.Sprintf("%s %d -> %d", f, before[f], after[f])
					}
					fmt.Fprintf(agent.Stderr(), "[hint] Graph totals: %s. Try: mailpipe sync\n", strings.Join(parts, ", "))
				} else {
					fmt.Fprintf(agent.Stderr(), "[hint] could not read Graph totals after the run: %v. Try: mailpipe sync\n", err)
				}
			}
			switch {
			case runErr != nil:
				return agentio.NewError(runErr.Error()+"; items not moved stay pending", retry)
			case sum.Failed > 0:
				return agentio.NewError(fmt.Sprintf("%d failed; failed items stay pending", sum.Failed), retry)
			}
			return nil
		})
	cmd.Flags().Bool("execute", false, "move the messages (without it, only check them)")
	cmd.Flags().Int("max-items", 0, "act on at most N pending items, in plan order (0 = all)")
	return cmd
}

func totals(cmd *cobra.Command, g mailbox.Provider, folders []string) (map[string]int, error) {
	out := make(map[string]int, len(folders))
	for _, f := range folders {
		collection, err := resolveCollection(cmd.Context(), g, f)
		if err != nil {
			return nil, err
		}
		counter, ok := g.(interface {
			Total(context.Context, mailbox.CollectionRef) (int, error)
		})
		if !ok {
			return nil, &mailbox.UnsupportedError{Operation: "remote counts"}
		}
		n, err := counter.Total(cmd.Context(), collection.Ref)
		if err != nil {
			return nil, err
		}
		out[f] = n
	}
	return out, nil
}

func newActionsCommand(deps Deps) *cobra.Command {
	return newAgentCommand("actions <plan-id>", "Show a plan's action log: every attempt apply made, oldest first", cobra.ExactArgs(1),
		func(cmd *cobra.Command, agent *agentio.AgentIO, args []string) error {
			id, err := parsePlanID(args[0])
			if err != nil {
				return err
			}
			s, err := openStore(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			if _, _, err := s.Plan(cmd.Context(), id); err != nil {
				return agentio.NewError(err.Error(), "mailpipe plan list")
			}
			log, err := s.Actions(cmd.Context(), id)
			if err != nil {
				return err
			}
			if len(log) == 0 {
				return agentio.NewDataError(fmt.Sprintf("plan %d has no actions logged", id), fmt.Sprintf("mailpipe apply %d", id))
			}
			return agentio.Tabled(agent, []string{"n", "sender", "subject", "action", "destination", "status", "detail", "at"}, log,
				func(a store.Action) []string {
					return []string{strconv.FormatInt(a.N, 10), a.Sender, oneLine(a.Subject), a.Action, a.Destination, a.Status, a.Detail, a.At}
				},
				func(a store.Action) map[string]any {
					return map[string]any{"n": a.N, "sender": a.Sender, "subject": a.Subject, "action": a.Action, "destination": a.Destination,
						"status": a.Status, "detail": a.Detail, "at": a.At, "graph_id": a.GraphID}
				})
		})
}
