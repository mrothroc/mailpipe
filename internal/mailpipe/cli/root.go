// Package cli implements the mailpipe command line. It is agent-first: stdout
// carries data (TSV with a header, or JSON Lines with --json), stderr carries
// usage, '[error] <cmd>: <what>. Try: <command>' lines and the footer, and exit
// codes are fixed (0 ok, 1 runtime, 2 usage, 3 stale or signed out, 4 no data).
package cli

import (
	"fmt"

	"github.com/mrothroc/agentio"
	"github.com/spf13/cobra"
)

// NewRootCommand assembles the command tree over deps.
func NewRootCommand(deps Deps) *cobra.Command {
	agentio.SetTool("mailpipe")
	agentio.SetDefaultOutDir(dataDir + "/out")
	root := &cobra.Command{
		Use:   "mailpipe",
		Short: "Mirror Microsoft 365 or read-only Yahoo mail and run reviewed processing stages",
		Long: "mailpipe mirrors a configured mailbox into local SQLite (~/mail/mailpipe.db).\n" +
			"Microsoft 365 (Graph) uses device-code sign-in; Yahoo uses an app password entered on hidden stdin.\n" +
			"Credentials live only in the current macOS user's Keychain. Fresh setup disables\n" +
			"model and external-stage transfers until explicit per-account authorization.\n\n" +
			"The Yahoo pilot supports real read-only sync, content, classification and local plans.\n" +
			"Yahoo apply/rules apply/save --execute and native rule installation are unavailable.\n" +
			"Start with config init --help; inspect account show and account capabilities.",
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
	root.AddCommand(newAttachmentsCommand(deps), newAccountCommand(deps), newConfigCommand(deps), newAuthCommand(deps), newSyncCommand(deps), newLsCommand(deps), newShowCommand(deps), newStagesCommand(), newRunCommand(deps), newVerdictsCommand(deps), newImportCommand(deps), newPlanCommand(deps), newApplyCommand(deps), newActionsCommand(deps), newRulesCommand(deps), newDigestCommand(deps), newSaveCommand(deps), newTemplatesCommand(deps), newFileCommand(deps))
	return root
}
