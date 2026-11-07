package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
)

const senderDiagnosticLimit = 1 << 20

// This is deliberately separate from proposals and automatic output overflow.
// Every variable string is private, untrusted data, not a rendering instruction.
type senderDiagnosticDocument struct {
	Format                 int    `json:"format"`
	Status                 string `json:"status"`
	Kind                   string `json:"diagnostic_kind"`
	Scope                  string `json:"diagnostic_scope"`
	Provider               string `json:"provider"`
	EnumerationComplete    bool   `json:"enumeration_complete"`
	ReconciliationComplete bool   `json:"reconciliation_complete"`
	Account                string `json:"account"`
	N                      int64  `json:"n"`
	ImmutableID            string `json:"requested_immutable_id"`
	Enumerated             string `json:"enumerated_sender_address"`
	Inspected              string `json:"inspected_sender_address"`
}

func encodeSenderDiagnostic(d *graphmail.ReconciliationSenderAddressDiagnostic) ([]byte, error) {
	if d == nil || d.N <= 0 {
		return nil, errors.New("invalid diagnostic")
	}
	size := 0
	for _, s := range []string{string(d.AccountID), string(d.ImmutableID), d.EnumeratedSenderAddress, d.InspectedSenderAddress} {
		if !utf8.ValidString(s) || len(s) > senderDiagnosticLimit-size {
			return nil, errors.New("diagnostic exceeds encoding boundary")
		}
		size += len(s)
	}
	doc := senderDiagnosticDocument{Format: 1, Status: "blocked", Kind: "sender_address_disagreement", Scope: "incomplete_observation_pair", Provider: "graph", EnumerationComplete: true, ReconciliationComplete: false, Account: string(d.AccountID), N: d.N, ImmutableID: string(d.ImmutableID), Enumerated: d.EnumeratedSenderAddress, Inspected: d.InspectedSenderAddress}
	raw, e := json.Marshal(doc)
	if e != nil {
		return nil, e
	}
	if len(raw) > senderDiagnosticLimit {
		return nil, errors.New("diagnostic exceeds encoding boundary")
	}
	return raw, nil
}

type senderDiagnosticOutput struct {
	root           *attachmentRoot
	name           string
	parent         os.FileInfo
	contextParents []string
	closeRoot      func() error
	readBack       func(*os.File) ([]byte, error)
}

func diagnosticCheckout(path string) bool {
	for {
		if _, e := os.Lstat(filepath.Join(path, ".git")); !errors.Is(e, os.ErrNotExist) {
			return true
		}
		parent := filepath.Dir(path)
		if parent == path {
			return false
		}
		path = parent
	}
}

func openSenderDiagnostic(path string, contexts ...string) (*senderDiagnosticOutput, error) {
	if !filepath.IsAbs(path) || path != filepath.Clean(path) || filepath.Base(path) == ".git" || filepath.Base(path) == "." || filepath.Base(path) == string(filepath.Separator) {
		return nil, errors.New("unsafe diagnostic destination")
	}
	r, e := openAttachmentRoot(filepath.Dir(path))
	if e != nil {
		return nil, e
	}
	d := &senderDiagnosticOutput{root: r, name: filepath.Base(path), contextParents: contexts, closeRoot: r.Close}
	d.readBack = func(f *os.File) ([]byte, error) {
		if _, e := f.Seek(0, io.SeekStart); e != nil {
			return nil, e
		}
		return io.ReadAll(io.LimitReader(f, senderDiagnosticLimit+1))
	}
	f, e := r.root.Open(".")
	if e == nil {
		d.parent, e = f.Stat()
		e = errors.Join(e, f.Close())
	}
	if e == nil {
		e = d.check()
	}
	if e == nil {
		_, e = r.root.Lstat(d.name)
		if errors.Is(e, os.ErrNotExist) {
			return d, nil
		}
		if e == nil {
			e = os.ErrExist
		}
	}
	return nil, errors.Join(e, r.Close())
}

// Recheck the named directory against its pinned identity as well as privacy
// and context isolation. Only directory metadata is read for DB/config paths.
func (d *senderDiagnosticOutput) check() error {
	info, e := os.Lstat(d.root.path)
	if e != nil {
		return e
	}
	if !attachmentOwned(info, true) || !os.SameFile(info, d.parent) {
		return errors.New("diagnostic directory changed")
	}
	canonical, e := filepath.EvalSymlinks(d.root.path)
	if e != nil {
		return e
	}
	if diagnosticCheckout(canonical) {
		return errors.New("diagnostic destination is a checkout")
	}
	for _, path := range d.contextParents {
		absolute, e := filepath.Abs(path)
		if e != nil {
			return e
		}
		parent, e := os.Stat(filepath.Dir(absolute))
		if e != nil {
			return e
		}
		if os.SameFile(parent, d.parent) {
			return errors.New("diagnostic destination overlaps context")
		}
	}
	return nil
}
func (d *senderDiagnosticOutput) close() error {
	if d == nil || d.closeRoot == nil {
		return nil
	}
	close := d.closeRoot
	d.closeRoot = nil
	return close()
}

func (d *senderDiagnosticOutput) write(ctx context.Context, raw []byte) (result error) {
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := d.check(); e != nil {
		return e
	}
	f, e := d.root.stageOpen(d.name)
	if e != nil {
		return e
	}
	info, e := f.Stat()
	var pin *os.File
	if e == nil {
		pin, _, e = pinAttachmentStage(d.root, d.name, info)
	}
	// A pinned descriptor prevents inode reuse during cleanup. If pinning failed,
	// retain the original descriptor and never remove an unproven replacement.
	closed := false
	defer func() {
		if result != nil {
			current, statErr := d.root.root.Lstat(d.name)
			if statErr == nil && info != nil && os.SameFile(current, info) {
				result = errors.Join(result, d.root.remove(d.name), d.root.sync())
			} else if !errors.Is(statErr, os.ErrNotExist) {
				result = errors.Join(result, errors.New("diagnostic cleanup uncertain"))
			}
		}
		if !closed {
			result = errors.Join(result, f.Close())
		}
		if pin != nil {
			result = errors.Join(result, pin.Close())
		}
	}()
	if e != nil {
		return e
	}
	if !attachmentOwned(info, false) {
		return errors.New("unsafe diagnostic file")
	}
	if _, e = attachmentCopy(ctx, f, bytes.NewReader(raw), senderDiagnosticLimit); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	e = f.Close()
	closed = true
	if e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	read, e := d.readBack(pin)
	if e != nil {
		return e
	}
	if !bytes.Equal(read, raw) {
		return errors.New("diagnostic readback differs")
	}
	current, e := d.root.root.Lstat(d.name)
	if e != nil {
		return e
	}
	if !attachmentOwned(current, false) || !os.SameFile(current, info) {
		return errors.New("diagnostic file changed")
	}
	if e = d.check(); e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	return d.root.sync()
}

// Closure failure must not downgrade the originating command's failure class.
// Only its class is used; raw collaborator messages are never rendered here.
func senderDiagnosticClosureFailure(cause, output error) error {
	const message = "sender-address diagnostic resource closure incomplete"
	const hint = "inspect the private destination before retrying"
	mapped := agentio.NewError(message, hint)
	if cause != nil {
		switch agentio.ExitCode(classify(reconciliationError(cause))) {
		case agentio.ExitUsage:
			mapped = agentio.NewUsageError(message, hint)
		case agentio.ExitStale:
			mapped = agentio.NewStaleError(message, hint)
		}
	}
	return &reconciliationFailure{mapped: mapped, cause: cause, output: output}
}

func (d *senderDiagnosticOutput) failure(ctx context.Context, agent *agentio.AgentIO, cause error) error {
	var evidence *graphmail.ReconciliationError
	var output error
	saved := false
	if errors.As(cause, &evidence) && evidence.SenderAddressDiagnostic != nil {
		var raw []byte
		raw, output = encodeSenderDiagnostic(evidence.SenderAddressDiagnostic)
		if output == nil {
			output = d.write(ctx, raw)
			saved = output == nil
		}
	}
	output = errors.Join(output, d.close())
	hint := "[hint] sender-address diagnostic not produced\n"
	if output != nil {
		hint = "[hint] sender-address diagnostic incomplete; inspect the private destination before retrying\n"
	} else if saved {
		hint = "[hint] sender-address diagnostic saved privately; observations are untrusted and not reconciliation proof\n"
	}
	n, e := io.WriteString(agent.Stderr(), hint)
	if e == nil && n != len(hint) {
		e = io.ErrShortWrite
	}
	output = errors.Join(output, e)
	return &reconciliationFailure{mapped: reconciliationError(cause), cause: cause, output: output}
}
