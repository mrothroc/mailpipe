package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/imapmail"
	"github.com/spf13/cobra"
)

func syncYahoo(cmd *cobra.Command, agent *agentio.AgentIO, deps Deps, c config.Config, names []string, full bool) (commandErr error) {
	ctx := cmd.Context()
	db, e := openBound(ctx, deps, dbPath(cmd), c.Account())
	if e != nil {
		return yahooBindingError(e)
	}
	defer func() { commandErr = errors.Join(commandErr, db.Close()) }()
	client, e := newYahooClient(deps, c, func(ctx context.Context) (imapmail.Credentials, error) { return credentials(ctx, deps, c) })
	if e != nil {
		return e
	}
	defer func() { commandErr = errors.Join(commandErr, client.Close()) }()
	d, e := client.Discover(ctx)
	if e != nil {
		return e
	}
	aliases := map[string]imapmail.Role{"inbox": imapmail.Inbox, "sentitems": imapmail.Sent, "deleteditems": imapmail.Trash, "junkemail": imapmail.Junk}
	routes := map[string]string{}
	selected := map[string]imapmail.Folder{}
	for _, f := range d.Folders {
		selected[f.Name] = f
		routes[f.Name] = string(f.Collection.Ref.CollectionID)
	}
	for _, f := range d.Folders {
		for alias, role := range aliases {
			if f.Role == role {
				if existing, ok := selected[alias]; ok && existing.Collection.Ref != f.Collection.Ref {
					return errors.New("IMAP selectable path conflicts with a role alias; reconcile explicit folder routing")
				}
				selected[alias] = f
				routes[alias] = string(f.Collection.Ref.CollectionID)
			}
		}
	}
	if _, e = saveRoutingConfig(ctx, deps, c, client.Account(), routes); e != nil {
		return fmt.Errorf("database bound but routing save failed; retry the same principal sync: %w", e)
	}
	if !cmd.Flags().Changed("folder") {
		names = []string{"inbox", "deleteditems", "sentitems", "junkemail"}
	}
	type row struct {
		Name string
		imapmail.SyncResult
	}
	var results []row
	for _, name := range names {
		f, ok := selected[name]
		if !ok {
			return errors.New("IMAP collection must name a discovered role or exact selectable path")
		}
		r, e := client.SyncCollection(ctx, db, f, imapmail.ContentMetadataLoader{}, full)
		if e != nil {
			return fmt.Errorf("sync stopped after %d committed collections: %w", len(results), e)
		}
		results = append(results, row{name, r})
	}
	return agentio.Tabled(agent, []string{"folder", "added", "updated", "removed", "local", "remote"}, results, func(r row) []string {
		return []string{r.Name, itoa(r.Added), itoa(r.Updated), itoa(r.Removed), itoa(r.Local), itoa(r.Remote)}
	}, func(r row) map[string]any {
		return map[string]any{"folder": r.Name, "added": r.Added, "updated": r.Updated, "removed": r.Removed, "local": r.Local, "remote": r.Remote}
	})
}
