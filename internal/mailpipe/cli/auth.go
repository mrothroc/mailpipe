package cli

import (
	"fmt"
	"strings"

	"github.com/mrothroc/agentio"
	"github.com/spf13/cobra"
)

func newAuthCommand(deps Deps) *cobra.Command {
	login := newAgentCommand("login", "Sign in: Graph device code or Yahoo app password on hidden stdin", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			c, err := routing()
			if err != nil {
				return err
			}
			if c.Mode() == "yahoo" {
				return yahooLogin(cmd, agent, deps, c)
			}
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
	login.Flags().Bool("password-stdin", false, "read one app-password line from stdin (never argv or environment)")
	status := newAgentCommand("status", "Show Graph sign-in or Yahoo credential readiness (not a remote authentication proof)", cobra.NoArgs,
		func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			c, err := routing()
			if err != nil {
				return err
			}
			if c.Mode() == "yahoo" {
				return yahooStatus(cmd.Context(), deps, c, agent)
			}
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
			c, err := routing()
			if err != nil {
				return err
			}
			if c.Mode() == "yahoo" {
				return yahooLogout(cmd.Context(), deps, c)
			}
			a, err := deps.Auth()
			if err != nil {
				return err
			}
			return a.Logout(cmd.Context())
		})
	return newGroup("auth", "Sign in to the configured Graph or read-only Yahoo account", login, status, logout)
}
