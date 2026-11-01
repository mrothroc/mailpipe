package cli

import "github.com/mrothroc/agentio"

func authorizeProcessing(s *commandStore, kind string) error {
	model := kind == "jev" || kind == "template"
	external := kind == "exec" || kind == "hook"
	if !model && !external {
		return nil
	}
	c := s.cfg
	// Compatibility is restricted to pre-existing explicitly configured Graph
	// stages. Fresh setup always persists an explicit default-off record.
	if c.Mode() == "graph" && c.Processing == nil {
		return nil
	}
	p := c.Processing
	if p == nil || p.Version != 1 || s.bound == nil || p.AccountID != string(s.bound.Account().ID) || p.AccountID != c.AccountID || model && !p.Models || external && !p.ExternalStages {
		return agentio.NewStaleError("external processing is not authorized for this exact account", "mailpipe config processing --help")
	}
	return nil
}
