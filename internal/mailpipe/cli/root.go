// Package cli implements the mailpipe command line. It is agent-first: stdout
// carries data (TSV with a header, or JSON Lines with --json), stderr carries
// usage, '[error] <cmd>: <what>. Try: <command>' lines and the footer, and exit
// codes are fixed (0 ok, 1 runtime, 2 usage, 3 stale or signed out, 4 no data).
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mrothroc/agentio"
)

// NewRootCommand assembles the command tree over deps.
func NewRootCommand(deps Deps) *cobra.Command {
	agentio.SetTool("mailpipe")
	agentio.SetDefaultOutDir(dataDir + "/out")
	root := &cobra.Command{
		Use:   "mailpipe",
		Short: "Process Microsoft 365 mail through pluggable stages",
		Long: "mailpipe mirrors a Microsoft 365 mailbox into a local SQLite database\n" +
			"(~/mail/mailpipe.db), runs messages through stages, and applies policy actions.\n\n" +
			"Sign-in is a device code flow; the token cache lives only in the macOS Keychain\n" +
			"(service \"" + keychainService + "\"), never in a file, environment variable or log.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: func(c *cobra.Command, args []string) error {
			if len(args) > 0 {
				return unknownCommand(c, args[0])
			}
			return nil
		},
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().String("db", "", "SQLite database path (default: $"+dbEnv+", else ~/mail/mailpipe.db)")
	root.PersistentFlags().Bool("local-time", false, "show received times in the local time zone (default: UTC, as Graph reports them)")
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		fmt.Fprint(c.ErrOrStderr(), c.UsageString())
		code := agentio.Fail(c.CommandPath(), c.ErrOrStderr(), agentio.NewUsageError(err.Error(), c.CommandPath()+" --help"))
		return newExitErr(code, agentio.NewReported(code))
	})
	root.AddCommand(newAccountCommand(deps), newConfigCommand(), newAuthCommand(deps), newSyncCommand(deps), newLsCommand(deps), newShowCommand(deps), newStagesCommand(), newRunCommand(deps), newVerdictsCommand(deps), newImportCommand(deps), newPlanCommand(deps), newApplyCommand(deps), newActionsCommand(deps), newRulesCommand(deps), newDigestCommand(deps), newSaveCommand(deps), newTemplatesCommand(deps), newFileCommand(deps))
	return root
}
