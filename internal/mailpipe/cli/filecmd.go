package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/store"
	"github.com/mrothroc/mailpipe/pkg/template"
)

const defaultTemplateStage = "template"

func newFileCommand(deps Deps) *cobra.Command {
	return newGroup("file", "Draft save manifests from template matches, and list the emails waiting for the owner",
		newFileDraftCommand(deps), newFileQueueCommand(deps), newFileDismissCommand(deps))
}

// latestOf returns the stage's verdict among a message's latest verdicts.
func latestOf(vs []store.Verdict, stageName string) (store.Verdict, bool) {
	for _, v := range vs {
		if v.Stage == stageName {
			return v, true
		}
	}
	return store.Verdict{}, false
}

// matchedID is the template id a matched verdict names.
func matchedID(v store.Verdict) string {
	for _, l := range v.Labels {
		if l != template.Matched && l != template.Filed {
			return l
		}
	}
	return ""
}

type draftRow struct {
	msg                          store.Message
	status, folder, name, detail string
}

func newFileDraftCommand(deps Deps) *cobra.Command {
	cmd := newAgentCommand("draft --out <manifest.tsv> [--stage <name>]",
		"Write a save manifest for template matches that are filed but not yet saved, marking each filing:pending", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			ctx := cmd.Context()
			out, _ := cmd.Flags().GetString("out")
			stageName, _ := cmd.Flags().GetString("stage")
			if strings.TrimSpace(out) == "" {
				return agentio.NewUsageError("--out <manifest.tsv> is required", cmd.CommandPath()+" --out ~/mail/out/draft.tsv")
			}
			if _, err := os.Lstat(out); err == nil {
				return agentio.NewUsageError(out+" exists; a draft never replaces a file", cmd.CommandPath()+" --out <a new file>")
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			// The stage's own template file: matches are re-derived from the
			// templates that made them.
			path := config.DefaultTemplatesPath()
			c, err := config.LoadStages(config.DefaultStagesPath())
			if err != nil && !errors.Is(err, config.ErrNotConfigured) {
				return stagesError(err)
			}
			if d, ok := c.Stage(stageName); ok && d.Type == "template" {
				path = d.Templates
			}
			set, err := loadTemplates(path)
			if err != nil {
				return err
			}
			s, err := openStore(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			filter := store.Filter{Labelled: stageName + ":" + template.Filed}
			msgs, err := s.Messages(ctx, filter)
			if err != nil {
				return err
			}
			latest, err := s.LatestVerdicts(ctx, filter)
			if err != nil {
				return err
			}
			graphFn := lazyGraph(deps)
			var rows []draftRow
			seen := map[string]int64{}
			saved, stale := 0, 0
			for _, m := range msgs {
				if f, ok := latestOf(latest[m.N], filingStage); ok && slices.Contains(f.Labels, "saved") {
					saved++
					continue
				}
				v, _ := latestOf(latest[m.N], stageName)
				r := draftRow{msg: m, status: "drafted"}
				t := set.Get(matchedID(v))
				if t == nil || !t.Files() {
					r.status, r.detail = "stale", fmt.Sprintf("template %q is gone or no longer filed", matchedID(v))
				} else {
					body, err := messageBody(ctx, s, m, graphFn)
					if err != nil {
						return err
					}
					sm := toStageMessage(m)
					sm.Body = body
					if got := t.Match(sm, time.Local); !got.OK {
						r.status, r.detail = "stale", t.ID+" no longer fits: "+got.Why
					} else if r.folder, r.name, err = t.Destination(got.Fields); err != nil {
						r.status, r.detail = "stale", err.Error()
					}
				}
				if r.status == "drafted" {
					key := strings.ToLower(filepath.Join(r.folder, r.name))
					if other, dup := seen[key]; dup {
						r.status, r.detail = "collision", fmt.Sprintf("same file as message %d: give template %s a field that tells them apart", other, t.ID)
					} else {
						seen[key] = m.N
					}
				}
				if r.status == "stale" {
					stale++
				}
				rows = append(rows, r)
			}
			var manifest strings.Builder
			manifest.WriteString("n\tfolder\tname\n")
			var pending []store.Verdict
			for _, r := range rows {
				if r.status == "drafted" {
					fmt.Fprintf(&manifest, "%d\t%s\t%s\n", r.msg.N, r.folder, r.name)
					pending = append(pending, store.Verdict{N: r.msg.N, Stage: filingStage, Labels: []string{"pending"},
						Reason: filepath.Join(r.folder, r.name), Provenance: "mailpipe file draft"})
				}
			}
			if err := agentio.Tabled(agent, []string{"n", "sender", "subject", "status", "folder", "name", "detail"}, rows,
				func(r draftRow) []string {
					return []string{strconv.FormatInt(r.msg.N, 10), r.msg.Sender, oneLine(r.msg.Subject), r.status, r.folder, r.name, r.detail}
				},
				func(r draftRow) map[string]any {
					return map[string]any{"n": r.msg.N, "sender": r.msg.Sender, "subject": r.msg.Subject, "status": r.status,
						"folder": r.folder, "name": r.name, "detail": r.detail}
				}); err != nil {
				return err
			}
			if len(pending) == 0 {
				hint := "mailpipe file queue"
				if stale > 0 {
					hint = fmt.Sprintf("fix the templates, or mailpipe run %s --rerun --folder '' --labelled %s:%s", stageName, stageName, template.Filed)
				}
				return agentio.NewDataError(fmt.Sprintf("nothing to draft (%d saved already, %d stale)", saved, stale), hint)
			}
			if err := writeNewPrivate(out, manifest.String()); err != nil {
				return err
			}
			if err := s.AddVerdicts(ctx, pending); err != nil {
				return err
			}
			fmt.Fprintf(agent.Stderr(), "[hint] drafted %d to %s (%d saved already, %d not drafted). Review it, then Try: mailpipe save --manifest %s --root ~/Documents\n",
				len(pending), out, saved, len(rows)-len(pending), out)
			return nil
		})
	cmd.Flags().String("out", "", "the manifest file to write (must not exist)")
	cmd.Flags().String("stage", defaultTemplateStage, "the template stage whose matches are drafted")
	return cmd
}

// writeNewPrivate creates path (mode 0600) with data; it never replaces a
// file, and removes what it created if the write fails.
func writeNewPrivate(path, data string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path) // never leave half a manifest behind
	}
	return err
}

type queueRow struct {
	msg     store.Message
	outcome string
	reason  string
}

func newFileQueueCommand(deps Deps) *cobra.Command {
	cmd := newAgentCommand("queue [--records <stage:label>] [--folder <name>] [--stage <name>]",
		"List emails waiting for the owner: review (fingerprint and Jev disagree), new (a kind the sender has no template for) and, with --records, records from senders with no template", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			ctx := cmd.Context()
			stageName, _ := cmd.Flags().GetString("stage")
			records, err := stageLabelFlag(cmd, "records", cmd.CommandPath())
			if err != nil {
				return err
			}
			s, err := openStore(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			folder, _ := cmd.Flags().GetString("folder")
			// Each pass lists the messages carrying a label, keeping those whose
			// verdicts call for the owner and that are not saved yet. With
			// --records, new kinds and senders with no template count only
			// when the message is a record.
			isRecord := func([]store.Verdict) bool { return true }
			if records != "" {
				st, label, _ := strings.Cut(records, ":")
				isRecord = func(vs []store.Verdict) bool {
					v, ok := latestOf(vs, st)
					return ok && slices.Contains(v.Labels, label)
				}
			}
			type pass struct {
				labelled, outcome string
				waiting           func(template store.Verdict, all []store.Verdict) bool
			}
			passes := []pass{
				{stageName + ":" + template.Review, template.Review, func(store.Verdict, []store.Verdict) bool { return true }},
				{stageName + ":" + template.New, template.New, func(_ store.Verdict, all []store.Verdict) bool { return isRecord(all) }},
			}
			if records != "" {
				passes = append(passes, pass{records, "no template", func(v store.Verdict, _ []store.Verdict) bool { return v.Stage != "" && len(v.Labels) == 0 }})
			}
			var rows []queueRow
			for _, p := range passes {
				filter := store.Filter{Folder: folder, Labelled: p.labelled}
				msgs, err := s.Messages(ctx, filter)
				if err != nil {
					return err
				}
				latest, err := s.LatestVerdicts(ctx, filter)
				if err != nil {
					return err
				}
				for _, m := range msgs {
					if f, ok := latestOf(latest[m.N], filingStage); ok && (slices.Contains(f.Labels, "saved") || slices.Contains(f.Labels, dismissed)) {
						continue
					}
					if v, _ := latestOf(latest[m.N], stageName); p.waiting(v, latest[m.N]) {
						rows = append(rows, queueRow{msg: m, outcome: p.outcome, reason: v.Reason})
					}
				}
			}
			if len(rows) == 0 {
				return agentio.NewDataError("nothing is waiting for the owner", "mailpipe file draft --help")
			}
			sort.SliceStable(rows, func(i, j int) bool { return rows[i].msg.Received < rows[j].msg.Received })
			received := receivedFormat(cmd)
			if err := agentio.Tabled(agent, []string{"n", "received", "sender", "subject", "outcome", "reason"}, rows,
				func(r queueRow) []string {
					return []string{strconv.FormatInt(r.msg.N, 10), received(r.msg.Received), r.msg.Sender, oneLine(r.msg.Subject), r.outcome, oneLine(r.reason)}
				},
				func(r queueRow) map[string]any {
					return map[string]any{"n": r.msg.N, "received": received(r.msg.Received), "sender": r.msg.Sender,
						"subject": r.msg.Subject, "outcome": r.outcome, "reason": r.reason}
				}); err != nil {
				return err
			}
			fmt.Fprintf(agent.Stderr(), "[hint] %d waiting. File one with a manifest (mailpipe save --help), or let it go: Try: mailpipe file dismiss <n>... --reason <why>\n", len(rows))
			return nil
		})
	cmd.Flags().String("stage", defaultTemplateStage, "the template stage whose outcomes are listed")
	cmd.Flags().String("folder", "inbox", "only this folder (empty for every folder)")
	cmd.Flags().String("records", "", "also list records (messages carrying this stage:label, e.g. tx-kind:record) whose sender has no template")
	return cmd
}

// dismissed marks a message the owner decided not to file.
const dismissed = "dismissed"

func newFileDismissCommand(deps Deps) *cobra.Command {
	cmd := newAgentCommand("dismiss <n>... [--reason <why>] [--stage <name>]",
		"Record that the owner decided not to file these messages: they leave the queue for good, and the record shelf life applies to them again", cobra.MinimumNArgs(1),
		func(cmd *cobra.Command, agent *agentio.AgentIO, args []string) error {
			ctx := cmd.Context()
			reason, _ := cmd.Flags().GetString("reason")
			stageName, _ := cmd.Flags().GetString("stage")
			if strings.TrimSpace(reason) == "" {
				reason = "the owner decided not to file it"
			}
			s, err := openStore(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			var msgs []store.Message
			for _, a := range args {
				n, err := strconv.ParseInt(a, 10, 64)
				if err != nil || n < 1 {
					return agentio.NewUsageError(fmt.Sprintf("%q is not a message number", a), "mailpipe file queue")
				}
				m, ok, err := s.Get(ctx, n)
				if err != nil {
					return err
				}
				if !ok {
					return agentio.NewUsageError(fmt.Sprintf("no message %d in the mirror", n), "mailpipe file queue")
				}
				msgs = append(msgs, m)
			}
			// The filing verdict is the decision; a later run of the stage
			// cannot outrank it. The stage verdict ends the shelf-life rule's
			// review/new exemption.
			var vs []store.Verdict
			for _, m := range msgs {
				for _, st := range []string{filingStage, stageName} {
					vs = append(vs, store.Verdict{N: m.N, Stage: st, Labels: []string{dismissed}, Reason: reason, Provenance: "mailpipe file dismiss"})
				}
			}
			if err := s.AddVerdicts(ctx, vs); err != nil {
				return err
			}
			return agentio.Tabled(agent, []string{"n", "status", "sender", "subject"}, msgs,
				func(m store.Message) []string {
					return []string{strconv.FormatInt(m.N, 10), dismissed, m.Sender, oneLine(m.Subject)}
				},
				func(m store.Message) map[string]any {
					return map[string]any{"n": m.N, "status": dismissed, "sender": m.Sender, "subject": m.Subject}
				})
		})
	cmd.Flags().String("reason", "", "why it is not filed (kept with the decision)")
	cmd.Flags().String("stage", defaultTemplateStage, "the template stage whose queue it leaves")
	return cmd
}
