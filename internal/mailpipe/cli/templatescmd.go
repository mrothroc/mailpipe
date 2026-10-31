package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
	"github.com/mrothroc/mailpipe/pkg/template"
)

// loadTemplates reads the template file, giving a missing file its exit code
// and the way to create it.
func loadTemplates(path string) (*template.Set, error) {
	set, err := template.Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, agentio.NewStaleError(path+" does not exist",
			fmt.Sprintf("cp examples/templates.example.json %s && chmod 600 %s", path, path))
	}
	if err != nil {
		return nil, agentio.NewUsageError(err.Error(), "mailpipe templates --help")
	}
	return set, nil
}

func newTemplatesCommand(deps Deps) *cobra.Command {
	cmd := newAgentCommand("templates", "List the templates that recognise each sender's kinds of email ($MAILPIPE_TEMPLATES, else ~/mail/templates.json)", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			set, err := loadTemplates(config.DefaultTemplatesPath())
			if err != nil {
				return err
			}
			return agentio.Tabled(agent, []string{"id", "senders", "files", "folder", "name"}, set.Templates,
				func(t *template.Template) []string {
					folder, name := "", ""
					if t.File != nil {
						folder, name = t.File.Folder, t.File.Name
					}
					return []string{t.ID, strings.Join(t.Senders, ","), strconv.FormatBool(t.Files()), folder, name}
				},
				func(t *template.Template) map[string]any {
					return map[string]any{"id": t.ID, "senders": t.Senders, "describe": t.Describe, "files": t.Files(), "file": t.File}
				})
		})
	cmd.AddCommand(newTemplatesCheckCommand(deps), newTemplatesAnchorsCommand(deps))
	return cmd
}

type checkRow struct {
	msg                    store.Message
	template, folder, name string
	expected, detail       string
}

// fingerprint matches m against its sender's templates: the id and
// destination of the one that fits, "a,b" when several do, or "-" with every
// template's reason.
func fingerprint(set *template.Set, m store.Message, body string) checkRow {
	sm := toStageMessage(m)
	sm.Body = body
	r := checkRow{msg: m, template: "-"}
	var fits, misses []string
	for _, t := range set.For(m.Sender) {
		got := t.Match(sm, time.Local)
		if !got.OK {
			misses = append(misses, t.ID+": "+got.Why)
			continue
		}
		fits = append(fits, t.ID)
		if t.Files() {
			folder, name, err := t.Destination(got.Fields)
			if err != nil {
				r.detail = err.Error()
			}
			r.folder, r.name = folder, name
		}
	}
	switch len(fits) {
	case 0:
		r.detail = strings.Join(misses, "; ")
	case 1:
		r.template = fits[0]
	default:
		r.template, r.folder, r.name, r.detail = strings.Join(fits, ","), "", "", "several templates fit"
	}
	return r
}

func newTemplatesCheckCommand(deps Deps) *cobra.Command {
	cmd := newAgentCommand("check [--folder <name>] [--from <text>] [--labelled <stage:label>] [--expect <manifest.tsv>]",
		"Fingerprint stored mail against the templates (free: no model is asked) and show where each match would be filed", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			ctx := cmd.Context()
			set, err := loadTemplates(config.DefaultTemplatesPath())
			if err != nil {
				return err
			}
			folder, _ := cmd.Flags().GetString("folder")
			from, _ := cmd.Flags().GetString("from")
			labelled, err := stageLabelFlag(cmd, "labelled", cmd.CommandPath())
			if err != nil {
				return err
			}
			expectPath, _ := cmd.Flags().GetString("expect")
			s, err := openStore(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			expect := map[int64]string{}
			if expectPath != "" {
				rows, err := readManifest(ctx, s, expectPath)
				if err != nil {
					return err
				}
				for _, r := range rows {
					expect[r.msg.N] = filepath.Join(r.folder, r.name)
				}
			}
			msgs, err := s.Messages(ctx, store.Filter{Folder: folder, SenderLike: from, Labelled: labelled})
			if err != nil {
				return err
			}
			graphFn := lazyProvider(ctx, deps)
			var rows []checkRow
			matched, same, differ := 0, 0, 0
			for _, m := range msgs {
				_, inManifest := expect[m.N]
				if len(set.For(m.Sender)) == 0 && !inManifest {
					continue
				}
				body, err := messageBody(ctx, s, m, graphFn)
				if err != nil {
					return err
				}
				r := fingerprint(set, m, body)
				if r.template != "-" && !strings.Contains(r.template, ",") {
					matched++
				}
				if expectPath != "" {
					r.expected = compareExpected(r, expect[m.N], inManifest)
					switch {
					case r.expected == "same":
						same++
					case r.expected != "":
						differ++
					}
				}
				rows = append(rows, r)
			}
			received := receivedFormat(cmd)
			if err := agentio.Tabled(agent, []string{"n", "received", "sender", "subject", "template", "folder", "name", "expected", "detail"}, rows,
				func(r checkRow) []string {
					return []string{strconv.FormatInt(r.msg.N, 10), received(r.msg.Received), r.msg.Sender, oneLine(r.msg.Subject),
						r.template, r.folder, r.name, r.expected, r.detail}
				},
				func(r checkRow) map[string]any {
					return map[string]any{"n": r.msg.N, "received": received(r.msg.Received), "sender": r.msg.Sender, "subject": r.msg.Subject,
						"template": r.template, "folder": r.folder, "name": r.name, "expected": r.expected, "detail": r.detail}
				}); err != nil {
				return err
			}
			hint := fmt.Sprintf("[hint] %d messages, %d matched", len(rows), matched)
			if expectPath != "" {
				hint += fmt.Sprintf("; against %s: %d same, %d differ", expectPath, same, differ)
			}
			fmt.Fprintln(agent.Stderr(), hint)
			return nil
		})
	cmd.Flags().String("folder", "inbox", "only this folder (empty for every folder)")
	cmd.Flags().String("from", "", "only senders whose address contains <text>")
	cmd.Flags().String("labelled", "", "only messages whose latest verdict from a stage carries a label, as stage:label (e.g. tx-kind:record)")
	cmd.Flags().String("expect", "", "compare with an earlier save manifest (n, folder, name)")
	return cmd
}

// compareExpected says how a fingerprint compares with a manifest row: same,
// a different destination, filed only by the manifest, or filed only by the
// templates. Agreeing not to file is empty.
func compareExpected(r checkRow, want string, inManifest bool) string {
	got := ""
	if r.name != "" {
		got = filepath.Join(r.folder, r.name) + ".pdf"
	}
	switch {
	case inManifest && got == "":
		return "filed: " + want
	case inManifest && got == want:
		return "same"
	case inManifest:
		return "differs: " + want
	case got != "":
		return "not filed"
	}
	return ""
}

func newTemplatesAnchorsCommand(deps Deps) *cobra.Command {
	cmd := newAgentCommand("anchors --from <sender> [--subject <regexp>] [--folder <name>]",
		"List the lines common to every stored email from a sender: candidate anchors for its template", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			ctx := cmd.Context()
			from, _ := cmd.Flags().GetString("from")
			folder, _ := cmd.Flags().GetString("folder")
			subjectFlag, _ := cmd.Flags().GetString("subject")
			from = strings.ToLower(strings.TrimSpace(from))
			if from == "" {
				return agentio.NewUsageError("--from <sender address> is required", cmd.CommandPath()+" --from billing@example.com")
			}
			subject, err := regexp.Compile(subjectFlag)
			if err != nil {
				return agentio.NewUsageError("--subject: "+err.Error(), cmd.CommandPath()+" --help")
			}
			s, err := openStore(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			texts, err := exampleBodies(ctx, s, lazyProvider(ctx, deps), store.Filter{Folder: folder, SenderLike: from}, from, subject)
			if err != nil {
				return err
			}
			if len(texts) == 0 {
				return agentio.NewDataError(fmt.Sprintf("no stored email from %s matches", from), "mailpipe ls --from "+from)
			}
			lines := template.CommonLines(texts)
			if err := agentio.Tabled(agent, []string{"line"}, lines,
				func(l string) []string { return []string{l} },
				func(l string) map[string]any { return map[string]any{"line": l} }); err != nil {
				return err
			}
			fmt.Fprintf(agent.Stderr(), "[hint] %d lines common to %d example(s)\n", len(lines), len(texts))
			return nil
		})
	cmd.Flags().String("from", "", "the sender's exact address")
	cmd.Flags().String("subject", "", "only emails whose subject matches this regular expression")
	cmd.Flags().String("folder", "", "only this folder (default: every folder)")
	return cmd
}

// exampleBodies returns the bodies of the stored emails exactly from sender
// whose subjects match.
func exampleBodies(ctx context.Context, s *commandStore, graphFn func() (mailbox.Provider, error), f store.Filter, sender string, subject *regexp.Regexp) ([]string, error) {
	msgs, err := s.Messages(ctx, f)
	if err != nil {
		return nil, err
	}
	var texts []string
	for _, m := range msgs {
		if m.Sender != sender || !subject.MatchString(m.Subject) {
			continue
		}
		body, err := messageBody(ctx, s, m, graphFn)
		if err != nil {
			return nil, err
		}
		texts = append(texts, body)
	}
	return texts, nil
}
