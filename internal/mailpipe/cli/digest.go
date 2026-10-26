package cli

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/policy"
	"github.com/mrothroc/mailpipe/pkg/stage"
	"github.com/mrothroc/mailpipe/pkg/store"
)

// previewRunes bounds a digest preview, so each record stays one short line.
const previewRunes = 140

// previewLine renders a body preview as one line: invisible formatting
// characters (which bulk mail pads previews with) are dropped, whitespace is
// collapsed, and the text is cut to previewRunes.
func previewLine(s string) string {
	s = oneLine(strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) || r == '͏' { // U+034F is a combining mark used as padding
			return -1
		}
		return r
	}, s))
	if utf8.RuneCountInString(s) > previewRunes {
		s = string([]rune(s)[:previewRunes-1]) + "…"
	}
	return s
}

type digestRow struct {
	store.Message
	Decision string
	Reasons  []string
	Labels   []string
}

func newDigestCommand(deps Deps) *cobra.Command {
	cmd := newAgentCommand("digest [--folder <name>] [--from <text>] [--since <when>] [--unread]",
		"One line per message for triage, newest first: n, received, sender, subject, the policy's decision, verdict labels and a preview (full text: mailpipe show <n>)",
		cobra.NoArgs, func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			ctx := cmd.Context()
			folder, _ := cmd.Flags().GetString("folder")
			from, _ := cmd.Flags().GetString("from")
			sinceFlag, _ := cmd.Flags().GetString("since")
			unread, _ := cmd.Flags().GetBool("unread")
			filter := store.Filter{Folder: folder, SenderLike: from, Unread: unread}
			if sinceFlag != "" {
				var err error
				if filter.Since, err = parseSince(sinceFlag, time.Now()); err != nil {
					return agentio.NewUsageError(err.Error(), "mailpipe digest --since 24h")
				}
			}
			// The digest only reads, so it works before stages are configured:
			// the decisions are then blank.
			var pol policy.Policy
			c, err := config.LoadStages(config.DefaultStagesPath())
			switch {
			case err == nil:
				pol = c.Policy
			case errors.Is(err, config.ErrNotConfigured):
				fmt.Fprintln(agent.Stderr(), "[hint] no stage configuration, so decisions are blank. Try: mailpipe stages")
			default:
				return stagesError(err)
			}
			s, err := openStore(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			if total, err := s.Count(ctx, store.Filter{}); err != nil {
				return err
			} else if total == 0 {
				return agentio.NewDataError("no messages stored yet", "mailpipe sync")
			}
			ms, err := s.Messages(ctx, filter)
			if err != nil {
				return err
			}
			if len(ms) == 0 {
				return agentio.NewDataError("no messages match", "mailpipe digest --help")
			}
			latest, err := s.LatestVerdicts(ctx, filter)
			if err != nil {
				return err
			}
			received := receivedFormat(cmd)
			now := time.Now()
			rows := make([]digestRow, len(ms))
			counts := map[string]int{}
			for i, m := range ms {
				r := digestRow{Message: m, Labels: []string{}}
				d := pol.Decide(policyMessage(m), latest[m.N], now)
				r.Decision, r.Reasons = d.Action, d.Reasons
				for _, v := range latest[m.N] {
					for _, l := range v.Labels {
						r.Labels = append(r.Labels, v.Stage+":"+l)
					}
				}
				sort.Strings(r.Labels)
				if r.Reasons == nil {
					r.Reasons = []string{}
				}
				counts[r.Decision]++
				rows[i] = r
			}
			if err := agentio.Tabled(agent, []string{"n", "received", "sender", "subject", "decision", "labels", "preview"}, rows,
				func(r digestRow) []string {
					return []string{strconv.FormatInt(r.N, 10), received(r.Received), r.Sender, oneLine(r.Subject), r.Decision,
						strings.Join(r.Labels, ","), previewLine(r.Preview)}
				},
				func(r digestRow) map[string]any {
					return map[string]any{"n": r.N, "received": received(r.Received), "folder": r.Folder, "sender": r.Sender,
						"sender_name": r.SenderName, "subject": r.Subject, "read": r.IsRead, "attachments": r.HasAttachments,
						"decision": r.Decision, "reasons": r.Reasons, "labels": r.Labels, "preview": previewLine(r.Preview)}
				}); err != nil {
				return err
			}
			var parts []string
			for _, a := range []string{stage.Delete, stage.Quarantine, stage.Digest, stage.Keep} {
				if counts[a] > 0 {
					parts = append(parts, fmt.Sprintf("%d %s", counts[a], a))
				}
			}
			parts = append(parts, fmt.Sprintf("%d with no policy rule", counts[""]))
			// Point at the newest message that stays: one being removed needs no reading.
			next := rows[0].N
			for _, r := range rows {
				if r.Decision != stage.Delete && r.Decision != stage.Quarantine {
					next = r.N
					break
				}
			}
			fmt.Fprintf(agent.Stderr(), "[hint] %d %s: %s. Try: mailpipe show %d\n", len(rows), plural(len(rows), "message", "messages"),
				strings.Join(parts, ", "), next)
			return nil
		})
	cmd.Long = cmd.Short + `.

decision is what the policy in stages.json would do with the message now: keep,
delete, quarantine or digest, and blank when no policy rule applies. Stages only
judge; the policy decides from their labels, so a stage's own action column in
mailpipe verdicts <n> can be empty while the decision is not (--json lists the
reasons). labels are every stage's current labels, as stage:label.

` + nOrderNote
	cmd.Flags().String("folder", "inbox", "only this folder")
	cmd.Flags().String("from", "", "only senders whose address contains <text>")
	cmd.Flags().String("since", "", "only mail received since <when>: a duration back from now (36h, 2d), a date (2026-10-23, from local midnight) or a time (2026-10-23T09:00:00Z)")
	cmd.Flags().Bool("unread", false, "only unread messages")
	return cmd
}
