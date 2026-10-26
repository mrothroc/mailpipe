package cli

import (
	"github.com/spf13/cobra"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
)

func newConfigCommand() *cobra.Command {
	initCmd := newAgentCommand("init --client-id <uuid> --tenant-id <uuid>",
		"Write the private configuration (~/mail/config.json, or $MAILPIPE_CONFIG)", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			clientID, _ := cmd.Flags().GetString("client-id")
			tenantID, _ := cmd.Flags().GetString("tenant-id")
			if clientID == "" || tenantID == "" {
				return agentio.NewUsageError("--client-id and --tenant-id are required (the Entra app registration's Overview page)", cmd.CommandPath()+" --help")
			}
			path := config.DefaultPath()
			if err := config.Save(path, config.Config{ClientID: clientID, TenantID: tenantID}); err != nil {
				return agentio.NewUsageError(err.Error(), cmd.CommandPath()+" --help")
			}
			return agentio.Tabled(agent, []string{"path"}, []string{path},
				func(p string) []string { return []string{p} },
				func(p string) map[string]any { return map[string]any{"path": p} })
		})
	initCmd.Flags().String("client-id", "", "Application (client) ID of the Entra app registration")
	initCmd.Flags().String("tenant-id", "", "Directory (tenant) ID of the Entra app registration")

	show := newAgentCommand("show", "Show the configuration in use", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			path := config.DefaultPath()
			c, err := config.Load(path)
			if err != nil {
				return err
			}
			return agentio.Tabled(agent, []string{"path", "client_id", "tenant_id"}, []config.Config{c},
				func(c config.Config) []string { return []string{path, c.ClientID, c.TenantID} },
				func(c config.Config) map[string]any {
					return map[string]any{"path": path, "client_id": c.ClientID, "tenant_id": c.TenantID}
				})
		})
	return newGroup("config", "Create or show the private configuration", initCmd, show)
}
