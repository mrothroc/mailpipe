package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/keychain"
	"github.com/mrothroc/mailpipe/pkg/msauth"
	"github.com/mrothroc/mailpipe/pkg/stage"
	"github.com/mrothroc/mailpipe/pkg/store"
	"github.com/mrothroc/mailpipe/pkg/template"
)

const (
	defaultOpenRouterService = "mailpipe-openrouter"
	defaultOpenRouterAccount = "default"
)

func loadStages() (config.Stages, error) {
	c, err := config.LoadStages(config.DefaultStagesPath())
	return c, stagesError(err)
}

// stagesError gives a stage configuration error its exit code and hint.
func stagesError(err error) error {
	if errors.Is(err, config.ErrNotConfigured) {
		return agentio.NewStaleError(err.Error(), "cp examples/stages.example.json ~/mail/stages.json && chmod 600 ~/mail/stages.json")
	}
	if err != nil {
		return agentio.NewUsageError(err.Error(), "mailpipe stages")
	}
	return nil
}

func needsBody(d config.StageDef) bool {
	switch d.Type {
	case "jev", "template":
		return true
	case "exec", "hook":
		return d.NeedsBody
	}
	return false
}

func newStagesCommand() *cobra.Command {
	return newAgentCommand("stages", "List the configured stages (~/mail/stages.json, or $MAILPIPE_STAGES)", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			c, err := loadStages()
			if err != nil {
				return err
			}
			return agentio.Tabled(agent, []string{"name", "type", "needs_body"}, c.Stages,
				func(d config.StageDef) []string { return []string{d.Name, d.Type, strconv.FormatBool(needsBody(d))} },
				func(d config.StageDef) map[string]any {
					return map[string]any{"name": d.Name, "type": d.Type, "needs_body": needsBody(d)}
				})
		})
}

// openRouterItem names the Keychain item that holds the OpenRouter key.
func openRouterItem(c config.Stages) (service, account string) {
	service, account = c.OpenRouter.KeychainService, c.OpenRouter.KeychainAccount
	if service == "" {
		service = defaultOpenRouterService
	}
	if account == "" {
		account = defaultOpenRouterAccount
	}
	return service, account
}

// buildStage turns a definition into a runnable stage.
func buildStage(ctx context.Context, deps Deps, c config.Stages, d config.StageDef, s *store.Store, maxCost float64) (stage.Stage, error) {
	switch d.Type {
	case "allowlist":
		contacts, err := s.SentRecipients(ctx)
		if err != nil {
			return nil, err
		}
		return stage.NewAllowlist(d.Name, c.SelfAddresses, c.SelfDomains, contacts, c.AutomatedSenders), nil
	case "honeypot":
		owner := stage.NewAllowlist(d.Name, c.SelfAddresses, c.SelfDomains, nil, c.AutomatedSenders)
		return stage.NewHoneypot(d.Name, c.Honeypots, owner), nil
	case "exec":
		return stage.NewExec(d.Name, d.Command, time.Duration(d.TimeoutSeconds)*time.Second, d.NeedsBody), nil
	case "hook":
		var source func(context.Context, stage.Message) ([]byte, error)
		if d.NeedsSource {
			graphFn := lazyGraph(deps)
			source = func(ctx context.Context, m stage.Message) ([]byte, error) {
				g, err := graphFn()
				if err != nil {
					return nil, fatalIfAccess(err)
				}
				raw, err := g.Raw(ctx, "/me/messages/"+url.PathEscape(m.GraphID)+"/$value")
				return raw, fatalIfAccess(err)
			}
		}
		return stage.NewHook(d.Name, d.Command, time.Duration(d.TimeoutSeconds)*time.Second, d.NeedsBody, d.Senders, source, stage.NewBudget(maxCost)), nil
	case "sender_cache":
		// The source stage's history is loaded once for the whole run; each
		// lookup is then in memory (one query instead of one per message).
		bySender, err := s.LatestBySender(ctx, d.Source)
		if err != nil {
			return nil, err
		}
		lookup := func(_ context.Context, sender string, exclude int64) ([]stage.Prior, error) {
			var prior []stage.Prior
			for _, v := range bySender[strings.ToLower(strings.TrimSpace(sender))] {
				if v.N == exclude {
					continue
				}
				p := stage.Prior{N: v.N, Score: v.Scores[d.Label]}
				for _, l := range v.Labels {
					p.Labelled = p.Labelled || l == d.Label
				}
				prior = append(prior, p)
			}
			return prior, nil
		}
		return stage.NewSenderCache(d.Name, d.Source, d.Label, d.MinVerdicts, lookup), nil
	case "jev":
		client, err := jevClient(ctx, deps, c, d)
		if err != nil {
			return nil, err
		}
		return stage.NewJev(d.Name, client, d.Questions, d.Threshold, stage.NewBudget(maxCost)), nil
	case "template":
		set, err := loadTemplates(d.Templates)
		if err != nil {
			return nil, err
		}
		client, err := jevClient(ctx, deps, c, d)
		if err != nil {
			return nil, err
		}
		return template.NewStage(d.Name, set, client, d.Threshold, stage.NewBudget(maxCost), time.Local), nil
	}
	return nil, fmt.Errorf("unknown stage type %q", d.Type)
}

// jevClient builds a Decisions API client for a stage, with the OpenRouter
// key from the Keychain.
func jevClient(ctx context.Context, deps Deps, c config.Stages, d config.StageDef) (*stage.JevClient, error) {
	service, account := openRouterItem(c)
	key, err := deps.Secret(ctx, service, account)
	if errors.Is(err, keychain.ErrNotFound) || (err == nil && strings.TrimSpace(key) == "") {
		return nil, agentio.NewStaleError(fmt.Sprintf("the OpenRouter API key is not in the Keychain (service %s, account %s)", service, account),
			fmt.Sprintf("security add-generic-password -s %s -a %s -w", service, account))
	}
	if err != nil {
		return nil, err
	}
	agentio.RegisterSecret(key)
	client := stage.NewJevClient(key, d.Model)
	if d.URL != "" {
		client.URL = d.URL
	}
	return client, nil
}

// stageLabelFlag reads a "stage:label" flag, trimmed; empty when unset. A
// malformed value suggests the command try (with its arguments) again.
func stageLabelFlag(cmd *cobra.Command, flag, try string) (string, error) {
	v, _ := cmd.Flags().GetString(flag)
	if v == "" {
		return "", nil
	}
	st, l, ok := strings.Cut(v, ":")
	st, l = strings.TrimSpace(st), strings.TrimSpace(l)
	if !ok || st == "" || l == "" {
		return "", agentio.NewUsageError(fmt.Sprintf("--%s %q must be stage:label", flag, v), fmt.Sprintf("%s --%s category:event", try, flag))
	}
	return st + ":" + l, nil
}

func toStageMessage(m store.Message) stage.Message {
	return stage.Message{N: m.N, GraphID: m.GraphID, InternetMessageID: m.InternetMessageID, Folder: m.Folder,
		Received: m.Received, Sender: m.Sender, SenderName: m.SenderName, Recipients: m.Recipients,
		Subject: m.Subject, Preview: m.Preview, HasAttachments: m.HasAttachments}
}

// fatalIfAccess marks sign-in and permission failures as fatal: every later
// message would fail the same way.
func fatalIfAccess(err error) error {
	var apiErr *graph.APIError
	if errors.Is(err, msauth.ErrNotSignedIn) || errors.Is(err, config.ErrNotConfigured) ||
		(errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden)) {
		return fmt.Errorf("%w: %w", stage.ErrFatal, err)
	}
	return err
}

type runRow struct {
	pos int
	stage.Result
}

func joinScores(scores map[string]float64) string {
	keys := make([]string, 0, len(scores))
	for k := range scores {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%.2f", k, scores[k])
	}
	return strings.Join(parts, ",")
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func newRunCommand(deps Deps) *cobra.Command {
	cmd := newAgentCommand("run <stage> [--folder <name>] [--from <text>] [--since <when>] [--labelled <stage:label>] [--max-messages N] [--max-cost-usd X] [--workers N] [--rerun]",
		"Run a stage over stored messages that it has not judged yet, recording its verdicts", cobra.ExactArgs(1),
		func(cmd *cobra.Command, agent *agentio.AgentIO, args []string) error {
			ctx := cmd.Context()
			folder, _ := cmd.Flags().GetString("folder")
			from, _ := cmd.Flags().GetString("from")
			maxMessages, _ := cmd.Flags().GetInt("max-messages")
			maxCost, _ := cmd.Flags().GetFloat64("max-cost-usd")
			workers, _ := cmd.Flags().GetInt("workers")
			rerun, _ := cmd.Flags().GetBool("rerun")
			if maxCost <= 0 || workers < 1 || maxMessages < 0 {
				return agentio.NewUsageError("--max-cost-usd must be positive, --workers at least 1, --max-messages not negative", cmd.CommandPath()+" --help")
			}
			c, err := loadStages()
			if err != nil {
				return err
			}
			name := args[0]
			sinceFlag, _ := cmd.Flags().GetString("since")
			var since time.Time
			if sinceFlag != "" {
				if since, err = parseSince(sinceFlag, time.Now()); err != nil {
					return agentio.NewUsageError(err.Error(), fmt.Sprintf("mailpipe run %s --since 24h", name))
				}
			}
			labelled, err := stageLabelFlag(cmd, "labelled", "mailpipe run "+name)
			if err != nil {
				return err
			}
			def, ok := c.Stage(name)
			if !ok {
				names := make([]string, len(c.Stages))
				for i, d := range c.Stages {
					names[i] = d.Name
				}
				return agentio.NewUsageError(fmt.Sprintf("unknown stage %q (configured: %s)", name, strings.Join(names, ", ")), "mailpipe stages")
			}
			s, err := openStore(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			st, err := buildStage(ctx, deps, c, def, s, maxCost)
			if err != nil {
				return err
			}

			filter := store.Filter{Folder: folder, SenderLike: from, Since: since, Labelled: labelled}
			var pending []store.Message
			if rerun {
				pending, err = s.Messages(ctx, filter)
			} else {
				pending, err = s.Pending(ctx, filter, name)
			}
			if err != nil {
				return err
			}
			if maxMessages > 0 && len(pending) > maxMessages {
				pending = pending[:maxMessages]
			}
			if len(pending) == 0 {
				fmt.Fprintf(agent.Stderr(), "[hint] no messages pending for stage %s; use --rerun to evaluate again\n", name)
			}

			msgs := make([]stage.Message, len(pending))
			stored := make(map[int64]store.Message, len(pending))
			pos := make(map[int64]int, len(pending))
			for i, m := range pending {
				msgs[i], stored[m.N], pos[m.N] = toStageMessage(m), m, i
			}
			graphFn := lazyGraph(deps)
			body := func(ctx context.Context, m *stage.Message) error {
				text, err := messageBody(ctx, s, stored[m.N], graphFn)
				if err != nil {
					return fatalIfAccess(err)
				}
				m.Body = text
				return nil
			}
			var rows []runRow
			sum, runErr := stage.Run(ctx, st, msgs, body, func(r stage.Result) error {
				v := store.Verdict{N: r.Message.N, Stage: name, Error: errText(r.Err), CostUSD: r.Verdict.CostUSD}
				if r.Err == nil {
					v.Labels, v.Scores, v.Action, v.Reason = r.Verdict.Labels, r.Verdict.Scores, r.Verdict.Action, r.Verdict.Reason
					v.CostUSD, v.Final, v.Provenance = r.Verdict.CostUSD, r.Verdict.Final, r.Verdict.Provenance
				}
				if errors.Is(r.Err, stage.ErrFatal) {
					return nil // a run-level failure is reported once, not stored against this message
				}
				rows = append(rows, runRow{pos: pos[r.Message.N], Result: r})
				return s.AddVerdict(ctx, v)
			}, workers)
			sort.Slice(rows, func(i, j int) bool { return rows[i].pos < rows[j].pos })

			if err := agentio.Tabled(agent, []string{"n", "sender", "subject", "labels", "scores", "action", "cost_usd", "error"}, rows,
				func(r runRow) []string {
					return []string{strconv.FormatInt(r.Message.N, 10), r.Message.Sender, oneLine(r.Message.Subject),
						strings.Join(r.Verdict.Labels, ","), joinScores(r.Verdict.Scores), r.Verdict.Action,
						fmt.Sprintf("%.4f", r.Verdict.CostUSD), errText(r.Err)}
				},
				func(r runRow) map[string]any {
					return map[string]any{"n": r.Message.N, "sender": r.Message.Sender, "subject": r.Message.Subject,
						"labels": r.Verdict.Labels, "scores": r.Verdict.Scores, "action": r.Verdict.Action,
						"final": r.Verdict.Final, "cost_usd": r.Verdict.CostUSD, "error": errText(r.Err)}
				}); err != nil {
				return err
			}
			fmt.Fprintf(agent.Stderr(), "[hint] stage %s: evaluated %d, failed %d, cost $%.4f\n", name, sum.Evaluated, sum.Failed, sum.CostUSD)
			switch {
			case runErr == nil:
				return nil
			case errors.Is(runErr, stage.ErrBudget):
				return agentio.NewError(runErr.Error(), fmt.Sprintf("mailpipe run %s --max-cost-usd %.2f", name, maxCost*2))
			case errors.Is(runErr, msauth.ErrNotSignedIn), errors.Is(runErr, config.ErrNotConfigured):
				return runErr // classify gives these (and Graph access errors) their sign-in hints
			}
			var de *stage.DecisionsError
			if errors.As(runErr, &de) {
				switch de.Status {
				case http.StatusUnauthorized, http.StatusForbidden:
					service, account := openRouterItem(c)
					return agentio.NewStaleError(runErr.Error(), fmt.Sprintf("security add-generic-password -U -s %s -a %s -w", service, account))
				case http.StatusPaymentRequired:
					return agentio.NewStaleError(runErr.Error()+" (the OpenRouter account has no credits left)", "add credits at https://openrouter.ai/settings/credits, then rerun")
				default: // 400, 404: the request itself (model or question) is wrong
					return agentio.NewError(runErr.Error()+" (check the stage's model and question in stages.json)", "mailpipe stages")
				}
			}
			return runErr
		})
	cmd.Flags().String("folder", "inbox", "only this folder")
	cmd.Flags().String("from", "", "only senders whose address contains <text>")
	cmd.Flags().String("labelled", "", "only messages whose latest verdict from a stage carries a label, as stage:label (e.g. category:event)")
	cmd.Flags().String("since", "", "only mail received since <when>: a duration back from now (36h, 2d), a date (2026-10-23, from local midnight) or a time (2026-10-23T09:00:00Z)")
	cmd.Flags().Int("max-messages", 0, "evaluate at most N messages (0 = all pending)")
	cmd.Flags().Float64("max-cost-usd", 1.00, "stop paid stages once this many dollars are spent (checked before each call)")
	cmd.Flags().Int("workers", 4, "messages evaluated in parallel")
	cmd.Flags().Bool("rerun", false, "evaluate messages again even if the stage already judged them")
	return cmd
}

func newVerdictsCommand(deps Deps) *cobra.Command {
	return newAgentCommand("verdicts <n>", "Show the verdict history of one message", cobra.ExactArgs(1),
		func(cmd *cobra.Command, agent *agentio.AgentIO, args []string) error {
			n, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || n < 1 {
				return agentio.NewUsageError(fmt.Sprintf("message number must be a positive integer, got %q", args[0]), "mailpipe ls")
			}
			s, err := openStore(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			if _, ok, err := s.Get(cmd.Context(), n); err != nil {
				return err
			} else if !ok {
				return agentio.NewError(fmt.Sprintf("no message %d", n), "mailpipe ls")
			}
			vs, err := s.Verdicts(cmd.Context(), n)
			if err != nil {
				return err
			}
			if len(vs) == 0 {
				fmt.Fprintln(agent.Stderr(), "[hint] no verdicts yet; run a stage first: mailpipe stages")
			}
			return agentio.Tabled(agent, []string{"stage", "at", "labels", "scores", "action", "reason", "cost_usd", "provenance", "error"}, vs,
				func(v store.Verdict) []string {
					return []string{v.Stage, v.At, strings.Join(v.Labels, ","), joinScores(v.Scores), v.Action, oneLine(v.Reason),
						fmt.Sprintf("%.4f", v.CostUSD), v.Provenance, v.Error}
				},
				func(v store.Verdict) map[string]any {
					return map[string]any{"stage": v.Stage, "at": v.At, "labels": v.Labels, "scores": v.Scores, "action": v.Action,
						"reason": v.Reason, "cost_usd": v.CostUSD, "final": v.Final, "provenance": v.Provenance, "error": v.Error}
				})
		})
}
