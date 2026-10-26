package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/stage"
	"github.com/mrothroc/mailpipe/pkg/store"
)

// importRecord is one line of an import-verdicts file.
type importRecord struct {
	InternetMessageID string             `json:"internet_message_id"`
	Sender            string             `json:"sender"`
	Stage             string             `json:"stage"`
	Labels            []string           `json:"labels"`
	Scores            map[string]float64 `json:"scores"`
	Action            string             `json:"action"`
	Reason            string             `json:"reason"`
	CostUSD           float64            `json:"cost_usd"`
	Final             bool               `json:"final"`
	Provenance        string             `json:"provenance"`
	At                string             `json:"at"`
}

var importStageName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func readImport(path string) ([]importRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, agentio.NewUsageError(err.Error(), "mailpipe import-verdicts --help")
	}
	defer f.Close()
	var out []importRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<24)
	for line := 1; sc.Scan(); line++ {
		text := bytes.TrimSpace(sc.Bytes())
		if len(text) == 0 {
			continue
		}
		var r importRecord
		dec := json.NewDecoder(bytes.NewReader(text))
		dec.DisallowUnknownFields()
		bad := func(why string) error {
			return agentio.NewUsageError(fmt.Sprintf("%s line %d: %s (nothing was imported)", path, line, why), "mailpipe import-verdicts --help")
		}
		if err := dec.Decode(&r); err != nil {
			return nil, bad(err.Error())
		}
		v := stage.Verdict{Labels: r.Labels, Scores: r.Scores, Action: r.Action, CostUSD: r.CostUSD}
		switch {
		case strings.TrimSpace(r.InternetMessageID) == "" || strings.TrimSpace(r.Sender) == "":
			return nil, bad("internet_message_id and sender are required")
		case !importStageName.MatchString(r.Stage):
			return nil, bad(fmt.Sprintf("stage %q must be lower-case letters, digits, '-' or '_'", r.Stage))
		case strings.TrimSpace(r.Provenance) == "" || strings.TrimSpace(r.At) == "":
			return nil, bad("provenance and at are required, so an import can be traced and re-run safely")
		}
		if err := v.Validate(); err != nil {
			return nil, bad(err.Error())
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, agentio.NewError(fmt.Sprintf("read %s: %v", path, err), "")
	}
	return out, nil
}

type importSummary struct{ records, added, present, unmatched, mismatch int }

func newImportCommand(deps Deps) *cobra.Command {
	cmd := newAgentCommand("import-verdicts <file.jsonl> [--dry-run]",
		"Import verdicts keyed by Message-ID and sender (the whole file is validated before anything is written)", cobra.ExactArgs(1),
		func(cmd *cobra.Command, agent *agentio.AgentIO, args []string) error {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			records, err := readImport(args[0])
			if err != nil {
				return err
			}
			s, err := openStore(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			ctx := cmd.Context()
			sum := importSummary{records: len(records)}
			var add []store.Verdict
			pending := map[string]bool{} // the same record twice in one file is added once
			for _, r := range records {
				ms, err := s.ByMessageID(ctx, r.InternetMessageID)
				if err != nil {
					return err
				}
				if len(ms) == 0 {
					sum.unmatched++
					continue
				}
				attached := false
				for _, m := range ms {
					if !strings.EqualFold(strings.TrimSpace(m.Sender), strings.TrimSpace(r.Sender)) {
						continue
					}
					attached = true
					key := fmt.Sprintf("%d\x00%s\x00%s\x00%s", m.N, r.Stage, r.Provenance, r.At)
					if have, err := s.HasVerdict(ctx, m.N, r.Stage, r.Provenance, r.At); err != nil {
						return err
					} else if have || pending[key] {
						sum.present++
						continue
					}
					pending[key] = true
					sum.added++
					add = append(add, store.Verdict{N: m.N, Stage: r.Stage, Labels: r.Labels, Scores: r.Scores, Action: r.Action,
						Reason: r.Reason, CostUSD: r.CostUSD, Final: r.Final, Provenance: r.Provenance, At: r.At})
				}
				if !attached {
					sum.mismatch++
				}
			}
			if dryRun {
				fmt.Fprintln(agent.Stderr(), "[hint] dry run: nothing was written")
			} else if err := s.AddVerdicts(ctx, add); err != nil { // one transaction: all or nothing
				return err
			}
			return agentio.Tabled(agent, []string{"records", "added", "already_present", "unmatched", "sender_mismatch"}, []importSummary{sum},
				func(x importSummary) []string {
					return []string{itoa(x.records), itoa(x.added), itoa(x.present), itoa(x.unmatched), itoa(x.mismatch)}
				},
				func(x importSummary) map[string]any {
					return map[string]any{"records": x.records, "added": x.added, "already_present": x.present, "unmatched": x.unmatched, "sender_mismatch": x.mismatch}
				})
		})
	cmd.Flags().Bool("dry-run", false, "count what would be imported without writing")
	return cmd
}

var planHeader = []string{"n", "folder", "sender", "subject", "action", "reasons"}

func planTable(agent *agentio.AgentIO, items []store.PlanItem) error {
	return agentio.Tabled(agent, planHeader, items,
		func(it store.PlanItem) []string {
			return []string{strconv.FormatInt(it.N, 10), it.Folder, it.Sender, oneLine(it.Subject), it.Action, strings.Join(it.Reasons, ",")}
		},
		func(it store.PlanItem) map[string]any {
			return map[string]any{"n": it.N, "folder": it.Folder, "sender": it.Sender, "subject": it.Subject, "action": it.Action,
				"reasons": it.Reasons, "no_block": it.NoBlock, "graph_id": it.GraphID, "internet_message_id": it.InternetMessageID}
		})
}

func newPlanCommand(deps Deps) *cobra.Command {
	create := newAgentCommand("create [--folder <name>] [--action <action>]",
		"Decide actions from the latest verdicts and the policy, and store them as a numbered plan", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			folder, _ := cmd.Flags().GetString("folder")
			only, _ := cmd.Flags().GetString("action")
			switch only {
			case "", stage.Delete, stage.Quarantine, stage.Digest:
			default:
				return agentio.NewUsageError(fmt.Sprintf("--action must be delete, quarantine or digest, got %q", only), cmd.CommandPath()+" --help")
			}
			c, err := loadStages()
			if err != nil {
				return err
			}
			s, err := openStore(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			ctx := cmd.Context()
			filter := store.Filter{Folder: folder}
			msgs, err := s.Messages(ctx, filter)
			if err != nil {
				return err
			}
			latest, err := s.LatestVerdicts(ctx, filter)
			if err != nil {
				return err
			}
			var items []store.PlanItem
			kept := 0
			now := time.Now()
			for _, m := range msgs {
				d := c.Policy.Decide(policyMessage(m), latest[m.N], now)
				action, reasons := d.Action, d.Reasons
				switch {
				case action == "":
					continue
				case action == stage.Keep:
					kept++
					continue
				case only != "" && action != only:
					continue
				}
				items = append(items, store.PlanItem{N: m.N, Action: action, Reasons: reasons, NoBlock: action == stage.Delete && !d.Block})
			}
			scope := "folder=" + folder
			if only != "" {
				scope += " action=" + only
			}
			id, err := s.CreatePlan(ctx, scope, items)
			if err != nil {
				return err
			}
			p, stored, err := s.Plan(ctx, id)
			if err != nil {
				return err
			}
			if err := planTable(agent, stored); err != nil {
				return err
			}
			fmt.Fprintf(agent.Stderr(), "[hint] plan %d: %d delete, %d quarantine, %d digest; %d kept by policy or allowlist. Try: mailpipe plan show %d\n",
				id, p.Counts[stage.Delete], p.Counts[stage.Quarantine], p.Counts[stage.Digest], kept, id)
			return nil
		})
	create.Flags().String("folder", "inbox", "only messages in this folder")
	create.Flags().String("action", "", "only plan this action (delete, quarantine or digest)")

	show := newAgentCommand("show <id>", "Show a stored plan exactly as it was created", cobra.ExactArgs(1),
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
			_, items, err := s.Plan(cmd.Context(), id)
			if err != nil {
				return agentio.NewError(err.Error(), "mailpipe plan list")
			}
			return planTable(agent, items)
		})

	list := newAgentCommand("list", "List stored plans, newest first", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			s, err := openStore(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			plans, err := s.Plans(cmd.Context())
			if err != nil {
				return err
			}
			return agentio.Tabled(agent, []string{"id", "created_at", "scope", "delete", "quarantine", "digest"}, plans,
				func(p store.Plan) []string {
					return []string{strconv.FormatInt(p.ID, 10), p.CreatedAt, p.Scope, itoa(p.Counts[stage.Delete]), itoa(p.Counts[stage.Quarantine]), itoa(p.Counts[stage.Digest])}
				},
				func(p store.Plan) map[string]any {
					return map[string]any{"id": p.ID, "created_at": p.CreatedAt, "scope": p.Scope, "counts": p.Counts}
				})
		})
	return newGroup("plan", "Create, show and list action plans", create, show, list)
}
