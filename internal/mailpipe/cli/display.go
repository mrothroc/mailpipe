package cli

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/mrothroc/mailpipe/pkg/policy"
	"github.com/mrothroc/mailpipe/pkg/store"
)

// policyMessage is what the policy needs from a stored message; a received
// time that does not parse is unknown, so no age rule can apply to it.
func policyMessage(m store.Message) policy.Message {
	t, _ := time.Parse(time.RFC3339, m.Received)
	return policy.Message{Sender: m.Sender, Received: t}
}

// nOrderNote explains message numbers in listing help.
const nOrderNote = "n numbers messages in the order they were first synced, not by time; rows are sorted by received time, newest first."

// receivedFormat returns how a command renders stored received times (UTC, as
// Graph writes them): unchanged, or with --local-time in the local zone with
// its offset. A time that does not parse is shown as stored.
func receivedFormat(cmd *cobra.Command) func(string) string {
	if local, _ := cmd.Flags().GetBool("local-time"); !local {
		return func(s string) string { return s }
	}
	return func(s string) string {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return s
		}
		return t.Local().Format(time.RFC3339)
	}
}
