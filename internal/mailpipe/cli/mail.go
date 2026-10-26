package cli

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/mailsync"
	"github.com/mrothroc/mailpipe/pkg/stage"
	"github.com/mrothroc/mailpipe/pkg/store"
)

func openStore(cmd *cobra.Command, deps Deps) (*store.Store, error) {
	s, err := deps.Store(dbPath(cmd))
	if err != nil {
		return nil, agentio.NewError("open database: "+err.Error(), "mailpipe --help")
	}
	return s, nil
}

func newSyncCommand(deps Deps) *cobra.Command {
	cmd := newAgentCommand("sync [--folder <name>]... [--full]",
		"Mirror mailbox folders locally (a full pass the first time, only changes after)", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			folders, _ := cmd.Flags().GetStringSlice("folder")
			full, _ := cmd.Flags().GetBool("full")
			if len(folders) == 0 {
				folders = mailsync.DefaultFolders
			}
			a, err := deps.Auth()
			if err != nil {
				return err
			}
			s, err := openStore(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			g := deps.Graph(a)
			var results []mailsync.Result
			var mismatch []string
			for _, f := range folders {
				r, err := mailsync.SyncFolder(cmd.Context(), s, g, f, full)
				if err != nil {
					return err
				}
				results = append(results, r)
				if r.Local != r.Remote {
					mismatch = append(mismatch, f)
				}
			}
			if err := agentio.Tabled(agent, []string{"folder", "added", "updated", "removed", "local", "graph"}, results,
				func(r mailsync.Result) []string {
					return []string{r.Folder, itoa(r.Added), itoa(r.Updated), itoa(r.Removed), itoa(r.Local), itoa(r.Remote)}
				},
				func(r mailsync.Result) map[string]any {
					return map[string]any{"folder": r.Folder, "added": r.Added, "updated": r.Updated, "removed": r.Removed, "local": r.Local, "graph": r.Remote}
				}); err != nil {
				return err
			}
			if len(mismatch) > 0 {
				return agentio.NewStaleError(fmt.Sprintf("local count differs from Graph for %s (mail may have arrived during the sync)", strings.Join(mismatch, ", ")),
					"mailpipe sync --full --folder "+mismatch[0])
			}
			return nil
		})
	cmd.Flags().StringSlice("folder", nil, "well-known folder to sync, repeatable (default: "+strings.Join(mailsync.DefaultFolders, ", ")+")")
	cmd.Flags().Bool("full", false, "ignore saved progress and re-read the whole folder")
	return cmd
}

func itoa(n int) string { return strconv.Itoa(n) }

var lsHeader = []string{"n", "received", "folder", "sender", "subject"}

func newLsCommand(deps Deps) *cobra.Command {
	cmd := newAgentCommand("ls [--folder <name>] [--from <text>]",
		"List stored messages, newest first: n, received, folder, sender, subject", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			folder, _ := cmd.Flags().GetString("folder")
			from, _ := cmd.Flags().GetString("from")
			s, err := openStore(cmd, deps)
			if err != nil {
				return err
			}
			defer s.Close()
			total, err := s.Count(cmd.Context(), store.Filter{})
			if err != nil {
				return err
			}
			if total == 0 {
				return agentio.NewDataError("no messages stored yet", "mailpipe sync")
			}
			ms, err := s.Messages(cmd.Context(), store.Filter{Folder: folder, SenderLike: from})
			if err != nil {
				return err
			}
			received := receivedFormat(cmd)
			return agentio.Tabled(agent, lsHeader, ms,
				func(m store.Message) []string {
					return []string{strconv.FormatInt(m.N, 10), received(m.Received), m.Folder, m.Sender, oneLine(m.Subject)}
				},
				func(m store.Message) map[string]any {
					return map[string]any{"n": m.N, "received": received(m.Received), "folder": m.Folder, "sender": m.Sender,
						"sender_name": m.SenderName, "recipients": m.Recipients, "subject": m.Subject,
						"internet_message_id": m.InternetMessageID, "graph_id": m.GraphID,
						"has_attachments": m.HasAttachments, "is_read": m.IsRead}
				})
		})
	cmd.Flags().String("folder", "", "only this folder (inbox, deleteditems, ...)")
	cmd.Flags().String("from", "", "sender address contains <text> (case-insensitive)")
	cmd.Long = cmd.Short + ".\n\n" + nOrderNote
	return cmd
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func newShowCommand(deps Deps) *cobra.Command {
	cmd := newAgentCommand("show <n> [--no-quotes]", "Show one message: headers, then the text body (bounded by --limit lines)",
		cobra.ExactArgs(1),
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
			m, ok, err := s.Get(cmd.Context(), n)
			if err != nil {
				return err
			}
			if !ok {
				return agentio.NewError(fmt.Sprintf("no message %d", n), "mailpipe ls")
			}
			if headers, _ := cmd.Flags().GetBool("headers"); headers {
				g, err := lazyGraph(deps)()
				if err != nil {
					return err
				}
				raw, err := g.Raw(cmd.Context(), "/me/messages/"+url.PathEscape(m.GraphID)+"/$value")
				if err != nil {
					return err
				}
				return agent.Text(headerBlock(raw), agent.Limit())
			}
			body, err := messageBody(cmd.Context(), s, m, lazyGraph(deps))
			if err != nil {
				return err
			}
			noQuotes, _ := cmd.Flags().GetBool("no-quotes")
			removed := 0
			if noQuotes {
				body, removed = stage.WithoutQuoted(body)
			}
			received := receivedFormat(cmd)(m.Received)
			sender := m.Sender
			if m.SenderName != "" {
				sender = m.SenderName + " <" + m.Sender + ">"
			}
			if agent.JSON() {
				return agentio.Tabled(agent, nil, []store.Message{m}, nil, func(m store.Message) map[string]any {
					obj := map[string]any{"n": m.N, "from": sender, "to": m.Recipients, "received": received,
						"folder": m.Folder, "subject": m.Subject, "internet_message_id": m.InternetMessageID,
						"has_attachments": m.HasAttachments, "body": body}
					if noQuotes {
						obj["quoted_lines_removed"] = removed
					}
					return obj
				})
			}
			attachments := "no"
			if m.HasAttachments {
				attachments = "yes"
			}
			var head strings.Builder
			for _, kv := range [][2]string{{"n", strconv.FormatInt(m.N, 10)}, {"from", sender}, {"to", strings.Join(m.Recipients, ", ")},
				{"received", received}, {"folder", m.Folder}, {"subject", oneLine(m.Subject)},
				{"message-id", m.InternetMessageID}, {"attachments", attachments}} {
				fmt.Fprintf(&head, "%s: %s\n", kv[0], kv[1])
			}
			head.WriteString("\n")
			if body != "" && !strings.HasSuffix(body, "\n") {
				body += "\n"
			}
			if removed > 0 {
				body += fmt.Sprintf("[quoted history removed: %d %s. Try: mailpipe show %d]\n", removed, plural(removed, "line", "lines"), m.N)
			}
			return agent.Text(head.String()+body, agent.Limit())
		})
	cmd.Flags().Bool("headers", false, "print the message's own header block from the server (Authentication-Results, Received, ...) instead of the body")
	cmd.Flags().Bool("no-quotes", false, "cut the body at its quoted reply history (an earlier message's header or > lines)")
	cmd.Flags().Lookup("json").Usage = "emit the message as one JSON object, body included"
	cmd.Flags().Lookup("limit").Usage = "maximum body lines on stdout before the full text overflows to a file"
	return cmd
}

// headerBlock returns a MIME message's header block with LF line ends.
func headerBlock(raw []byte) string {
	h := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if i := strings.Index(h, "\n\n"); i >= 0 {
		h = h[:i+1]
	}
	return h
}

// lazyGraph signs in and builds the Graph client on first use only, so
// commands that may not need Graph (cached bodies, local stages) never prompt.
func lazyGraph(deps Deps) func() (Graph, error) {
	var once sync.Once
	var g Graph
	var gErr error
	return func() (Graph, error) {
		once.Do(func() {
			a, err := deps.Auth()
			if err != nil {
				gErr = err
				return
			}
			g = deps.Graph(a)
		})
		return g, gErr
	}
}

// messageBody returns m's text body, from the store's cache or from Graph
// (caching it). It is safe for concurrent use.
func messageBody(ctx context.Context, s *store.Store, m store.Message, graphFn func() (Graph, error)) (string, error) {
	if m.Body != nil {
		return *m.Body, nil
	}
	g, err := graphFn()
	if err != nil {
		return "", err
	}
	var resp struct {
		Body struct {
			Content string `json:"content"`
		} `json:"body"`
	}
	if err := g.Get(ctx, "/me/messages/"+url.PathEscape(m.GraphID), url.Values{"$select": {"body"}}, &resp); err != nil {
		return "", err
	}
	if err := s.SetBody(ctx, m.N, resp.Body.Content); err != nil {
		return "", err
	}
	return resp.Body.Content, nil
}
