package cli

import (
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"

	"github.com/mrothroc/agentio"
)

// newAgentCommand builds a leaf command with the agent-first contract applied
// (the shared agent CLI contract):
//
//   - argument and flag failures print the usage plus one
//     '[error] <cmd>: <what>. Try: <command>' line and exit 2;
//   - a runtime failure prints the same line with its documented code;
//   - success writes the one-line footer to stderr;
//   - data goes to stdout through the command's AgentIO.
func newAgentCommand(use, short string, args cobra.PositionalArgs, run func(cmd *cobra.Command, agent *agentio.AgentIO, args []string) error) *cobra.Command {
	cmd := &cobra.Command{Use: use, Short: short, Args: args}
	agent := agentio.New(cmd.Name(), agentio.WithStdout(cmd.OutOrStdout()), agentio.WithStderr(cmd.ErrOrStderr()))
	agent.SetUsage(func(w io.Writer) { fmt.Fprint(w, cmd.UsageString()) })
	cmd.Flags().Bool("json", false, "emit one JSON object per row instead of TSV")
	cmd.Flags().Int("limit", agentio.DefaultLimit, "maximum data rows on stdout before the rest overflows to a file")

	bind := func(c *cobra.Command) {
		if _, ok := c.OutOrStdout().(*terminalWriter); !ok {
			c.SetOut(&terminalWriter{out: c.OutOrStdout()})
		}
		if _, ok := c.ErrOrStderr().(*terminalWriter); !ok {
			c.SetErr(&terminalWriter{out: c.ErrOrStderr()})
		}
		agent.SetStdout(c.OutOrStdout())
		agent.SetStderr(c.ErrOrStderr())
		agent.SetCommand(c.CommandPath())
	}
	usageFail := func(c *cobra.Command, err error) error {
		bind(c)
		defer c.OutOrStdout().(*terminalWriter).flush()
		defer c.ErrOrStderr().(*terminalWriter).flush()
		code := agent.Fail(agentio.NewUsageError(err.Error(), c.CommandPath()+" --help"))
		return newExitErr(code, agentio.NewReported(code))
	}
	cmd.SetFlagErrorFunc(usageFail)
	if args != nil {
		cmd.Args = func(c *cobra.Command, argv []string) error {
			agent.Reset()
			if err := args(c, argv); err != nil {
				return usageFail(c, err)
			}
			return nil
		}
	}
	cmd.RunE = func(c *cobra.Command, argv []string) error {
		agent.Reset()
		bind(c)
		defer c.OutOrStdout().(*terminalWriter).flush()
		defer c.ErrOrStderr().(*terminalWriter).flush()
		if enabled, err := c.Flags().GetBool("json"); err == nil {
			agent.SetJSON(enabled)
		}
		if limit, err := c.Flags().GetInt("limit"); err == nil {
			agent.SetLimit(limit)
		}
		if err := run(c, agent, argv); err != nil {
			code := agent.Fail(classify(err))
			return newExitErr(code, agentio.NewReported(code))
		}
		agent.Footer(agentio.ExitOK)
		return nil
	}
	return cmd
}

// newGroup builds a command that only holds subcommands: bare, it prints its
// help; with an unknown subcommand it is a usage error listing the real ones.
func newGroup(use, short string, subs ...*cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args: func(c *cobra.Command, args []string) error {
			if len(args) > 0 {
				return unknownCommand(c, args[0])
			}
			return nil
		},
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(subs...)
	return cmd
}

// unknownCommand lists the available subcommands on stderr and fails with a
// usage error, so stdout stays empty and the agent learns what exists.
func unknownCommand(c *cobra.Command, name string) error {
	stderr := c.ErrOrStderr()
	fmt.Fprintln(stderr, "Available commands:")
	subs := c.Commands()
	sort.Slice(subs, func(i, j int) bool { return subs[i].Name() < subs[j].Name() })
	width := 0
	for _, sub := range subs {
		if sub.IsAvailableCommand() && len(sub.Name()) > width {
			width = len(sub.Name())
		}
	}
	for _, sub := range subs {
		if sub.IsAvailableCommand() {
			fmt.Fprintf(stderr, "  %-*s  %s\n", width, sub.Name(), sub.Short)
		}
	}
	code := agentio.Fail(c.CommandPath(), stderr, agentio.NewUsageError(fmt.Sprintf("unknown command %q", name), c.CommandPath()+" --help"))
	return newExitErr(code, agentio.NewReported(code))
}
