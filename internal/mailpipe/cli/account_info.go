package cli

import (
	"errors"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/spf13/cobra"
)

func accountConfiguration(cmd *cobra.Command, agent *agentio.AgentIO, c config.Config) error {
	endpoint, login := "", ""
	if c.IMAP != nil && c.Mode() == "yahoo" {
		endpoint, login = c.IMAP.Endpoint, c.IMAP.Login
	}
	return agentio.Tabled(agent, []string{"provider", "account", "database", "endpoint", "login"}, []config.Config{c}, func(c config.Config) []string { return []string{c.Mode(), c.AccountID, dbPath(cmd), endpoint, login} }, func(c config.Config) map[string]any {
		return map[string]any{"provider": c.Mode(), "account": c.AccountID, "database": dbPath(cmd), "endpoint": endpoint, "login": login, "processing": c.Processing, "configured_principal": true}
	})
}
func newAccountShowCommand() *cobra.Command {
	return newAgentCommand("show", "Show configured account/database identity (no authentication claim)", cobra.NoArgs, func(cmd *cobra.Command, a *agentio.AgentIO, _ []string) error {
		c, e := routing()
		if e != nil {
			return e
		}
		return accountConfiguration(cmd, a, c)
	})
}
func newCapabilitiesCommand(deps Deps) *cobra.Command {
	return newAgentCommand("capabilities", "Report actual configured provider method capabilities; availability is not approval", cobra.NoArgs, func(cmd *cobra.Command, a *agentio.AgentIO, _ []string) (result error) {
		get, close := lazyProvider(cmd.Context(), deps)
		defer func() { result = errors.Join(result, close()) }()
		p, e := get()
		if e != nil {
			return e
		}
		caps := mailbox.CapabilitiesOf(p)
		return agentio.Tabled(a, []string{"provider", "read", "sync", "trash", "archive", "native_rules", "save_execute"}, []mailbox.Capabilities{caps}, func(c mailbox.Capabilities) []string {
			return []string{string(p.Account().ProviderID), "true", "true", boolText(c.Trash), boolText(c.Archive), boolText(c.NativeRules), boolText(p.Account().ProviderID == "graph")}
		}, func(c mailbox.Capabilities) map[string]any {
			return map[string]any{"provider": p.Account().ProviderID, "read": true, "sync": true, "trash": c.Trash, "archive": c.Archive, "native_rules": c.NativeRules, "save_execute": p.Account().ProviderID == "graph"}
		})
	})
}
func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
