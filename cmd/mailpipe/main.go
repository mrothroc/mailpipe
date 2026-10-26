// Command mailpipe is the mailpipe CLI entry point. It builds the command tree
// and translates the returned error into the documented exit code. An error
// that bypassed the command layer is reported through the same contract, so no
// failure is silent.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := cli.NewRootCommand(cli.DefaultDeps()).ExecuteContext(ctx)
	if err != nil && !agentio.Reported(err) {
		agentio.Fail("mailpipe", os.Stderr, err)
	}
	os.Exit(cli.ExitCode(err))
}
