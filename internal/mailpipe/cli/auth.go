package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrothroc/agentio"
)

func newAuthCommand(deps Deps) *cobra.Command {
	login := newAgentCommand("login", "Sign in with a device code (the URL and code are printed on stderr)", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			a, err := deps.Auth()
			if err != nil {
				return err
			}
			who, err := a.Login(cmd.Context(), func(msg string) { fmt.Fprintln(agent.Stderr(), msg) })
			if interrupted(err) {
				return err // classify gives an interruption its rerun hint
			}
			if err != nil {
				return agentio.NewStaleError(err.Error(), "mailpipe auth login")
			}
			return agentio.Tabled(agent, []string{"account"}, []string{who},
				func(s string) []string { return []string{s} },
				func(s string) map[string]any { return map[string]any{"account": s} })
		})
	status := newAgentCommand("status", "Show the signed-in account and the scopes it was granted", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			a, err := deps.Auth()
			if err != nil {
				return err
			}
			s, err := a.Status(cmd.Context())
			if err != nil {
				return err
			}
			return agentio.Tabled(agent, []string{"account", "scopes"}, []struct{}{{}},
				func(struct{}) []string { return []string{s.Account, strings.Join(s.Scopes, " ")} },
				func(struct{}) map[string]any { return map[string]any{"account": s.Account, "scopes": s.Scopes} })
		})
	logout := newAgentCommand("logout", "Forget the stored sign-in", cobra.NoArgs,
		func(cmd *cobra.Command, _ *agentio.AgentIO, _ []string) error {
			a, err := deps.Auth()
			if err != nil {
				return err
			}
			return a.Logout(cmd.Context())
		})
	return newGroup("auth", "Sign in to Microsoft 365 and check access", login, status, logout)
}
