package cli

import (
	"os"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/imapauth"
	"github.com/spf13/cobra"
)

func newConfigCommand(deps Deps) *cobra.Command {
	initCmd := newAgentCommand("init --provider <graph|yahoo>", "Write private provider configuration; fresh external processing is disabled", cobra.NoArgs, func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
		provider, _ := cmd.Flags().GetString("provider")
		client, _ := cmd.Flags().GetString("client-id")
		tenant, _ := cmd.Flags().GetString("tenant-id")
		endpoint, _ := cmd.Flags().GetString("endpoint")
		login, _ := cmd.Flags().GetString("login")
		path := config.DefaultPath()
		_, statErr := os.Stat(path)
		exists := statErr == nil
		if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
		var existing config.Config
		if exists {
			var e error
			existing, e = config.Load(path)
			if e != nil {
				return e
			}
		}
		write := func() error {
			c := existing
			if exists {
				var e error
				c, e = config.Load(path)
				if e != nil {
					return e
				}
			}
			if provider == "" {
				provider = "graph"
			}
			if exists && c.Mode() != provider {
				return agentio.NewUsageError("select a separate configuration and database for another provider", cmd.CommandPath()+" --help")
			}
			switch provider {
			case "yahoo":
				if client != "" || tenant != "" {
					return agentio.NewUsageError("Graph registration flags do not apply to Yahoo", cmd.CommandPath()+" --help")
				}
				if endpoint == "" {
					endpoint = "imap.mail.yahoo.com"
				}
				p, e := imapauth.NewPrincipal("yahoo", endpoint, login)
				if e != nil {
					return e
				}
				if exists && c.AccountID != string(p.Account().ID) {
					return agentio.NewUsageError("select a separate configuration and database for a different principal", cmd.CommandPath()+" --help")
				}
				c.Provider = "yahoo"
				c.AccountID = string(p.Account().ID)
				if c.IMAP == nil {
					c.IMAP = &config.IMAPConfig{IdentityVersion: 1}
				}
				c.IMAP.Endpoint = p.Endpoint
				c.IMAP.Login = p.Login
			case "graph":
				if cmd.Flags().Changed("endpoint") || cmd.Flags().Changed("login") {
					return agentio.NewUsageError("Yahoo endpoint/login flags require --provider yahoo", cmd.CommandPath()+" --help")
				}
				if client == "" || tenant == "" {
					return agentio.NewUsageError("--client-id and --tenant-id are required", cmd.CommandPath()+" --help")
				}
				if exists && (c.ClientID != client || c.TenantID != tenant) {
					return agentio.NewUsageError("select a separate configuration and database for another registration", cmd.CommandPath()+" --help")
				}
				c.ClientID = client
				c.TenantID = tenant
				if cmd.Flags().Changed("provider") {
					c.Provider = "graph"
				}
			default:
				return agentio.NewUsageError("unsupported provider", cmd.CommandPath()+" --help")
			}
			if !exists {
				c.Processing = &config.Processing{Version: 1, AccountID: c.AccountID}
			}
			if e := saveConfig(deps, c); e != nil {
				return agentio.NewUsageError(e.Error(), cmd.CommandPath()+" --help")
			}
			return nil
		}
		var e error
		if provider == "yahoo" {
			e = setupSession(cmd.Context(), deps, func(imapauth.Items) error { return write() })
		} else {
			e = write()
		}
		if e != nil {
			return e
		}
		return agentio.Tabled(agent, []string{"path"}, []string{path}, func(p string) []string { return []string{p} }, func(p string) map[string]any { return map[string]any{"path": p} })
	})
	initCmd.Flags().String("provider", "graph", "graph (default) or yahoo (read-only pilot)")
	initCmd.Flags().String("client-id", "", "Graph application client UUID")
	initCmd.Flags().String("tenant-id", "", "Graph directory tenant UUID")
	initCmd.Flags().String("endpoint", "", "Yahoo implicit-TLS host[:port]; omitted port is 993")
	initCmd.Flags().String("login", "", "Exact opaque Yahoo authentication login (case and spaces preserved)")
	show := newAgentCommand("show", "Show private configuration routing without credentials", cobra.NoArgs, func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
		c, e := routing()
		if e != nil {
			return e
		}
		path := config.DefaultPath()
		if c.Mode() == "graph" {
			return agentio.Tabled(agent, []string{"path", "client_id", "tenant_id"}, []config.Config{c}, func(c config.Config) []string { return []string{path, c.ClientID, c.TenantID} }, func(c config.Config) map[string]any {
				return map[string]any{"path": path, "client_id": c.ClientID, "tenant_id": c.TenantID}
			})
		}
		return accountConfiguration(cmd, agent, c)
	})
	processing := newAgentCommand("processing --account <id>", "Explicitly authorize model and external-stage data transfers for one bound account", cobra.NoArgs, func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
		c, e := routing()
		if e != nil {
			return e
		}
		id, _ := cmd.Flags().GetString("account")
		if !cmd.Flags().Changed("models") && !cmd.Flags().Changed("external-stages") {
			return agentio.NewUsageError("supply --models or --external-stages", cmd.CommandPath()+" --help")
		}
		if c.AccountID == "" || id != c.AccountID {
			return agentio.NewUsageError("exact bound account required; sync first", "mailpipe account show")
		}
		db, e := openBound(cmd.Context(), deps, dbPath(cmd), c.Account())
		if e != nil {
			return e
		}
		if e = db.Close(); e != nil {
			return e
		}
		write := func() error {
			fresh, e := routing()
			if e != nil {
				return e
			}
			if e = samePrincipal(fresh, c); e != nil {
				return e
			}
			if fresh.Processing == nil {
				fresh.Processing = &config.Processing{Version: 1, Models: fresh.Mode() == "graph", ExternalStages: fresh.Mode() == "graph"}
			}
			if fresh.Processing.AccountID != "" && fresh.Processing.AccountID != id {
				fresh.Processing.Models = false
				fresh.Processing.ExternalStages = false
			}
			fresh.Processing.AccountID = id
			if cmd.Flags().Changed("models") {
				fresh.Processing.Models, _ = cmd.Flags().GetBool("models")
			}
			if cmd.Flags().Changed("external-stages") {
				fresh.Processing.ExternalStages, _ = cmd.Flags().GetBool("external-stages")
			}
			return saveConfig(deps, fresh)
		}
		if c.Mode() == "yahoo" {
			e = setupSession(cmd.Context(), deps, func(imapauth.Items) error { return write() })
		} else {
			e = write()
		}
		if e != nil {
			return e
		}
		return agentio.Tabled(agent, []string{"account"}, []string{id}, func(s string) []string { return []string{s} }, func(s string) map[string]any { return map[string]any{"account": s} })
	})
	processing.Flags().String("account", "", "exact account ID reported by account show")
	processing.Flags().Bool("models", false, "authorize configured model transfers for this account")
	processing.Flags().Bool("external-stages", false, "authorize trusted exec/hook subprocess transfers for this account")
	return newGroup("config", "Create/show configuration and account processing consent", initCmd, show, processing)
}
