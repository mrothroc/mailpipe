package cli

import "github.com/mrothroc/agentio"

// exitErr carries the documented process exit code out of a cobra RunE, which
// itself only knows success or failure (the shared agent CLI contract).
type exitErr struct {
	code int
	err  error
}

func (e *exitErr) Error() string {
	if e.err == nil {
		return "command failed"
	}
	return e.err.Error()
}

func (e *exitErr) Unwrap() error { return e.err }

// Code satisfies agentio's coded-error interface, so agentio.ExitCode reads it.
func (e *exitErr) Code() int { return e.code }

func newExitErr(code int, err error) error { return &exitErr{code: code, err: err} }

// ExitCode maps an error from the command tree to the process exit code.
func ExitCode(err error) int { return agentio.ExitCode(err) }
