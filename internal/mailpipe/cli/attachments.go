package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/document"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/spf13/cobra"
)

type attachmentRow struct {
	ProviderID         string `json:"provider_id"`
	AccountID          string `json:"account_id"`
	N                  int64  `json:"n"`
	Part               string `json:"part"`
	MIMEPath           string `json:"mime_path"`
	Kind               string `json:"kind"`
	Filename           string `json:"filename"`
	ContentType        string `json:"content_type"`
	Disposition        string `json:"disposition"`
	CID                string `json:"cid"`
	DecodedBytes       int64  `json:"decoded_bytes"`
	SHA256             string `json:"sha256"`
	OutputName         string `json:"output_name"`
	Extractable        bool   `json:"extractable"`
	Status             string `json:"status"`
	ErrorCode          string `json:"error_code"`
	Detail             string `json:"detail"`
	SavedPath          string `json:"saved_path"`
	PlannedPath        string `json:"planned_path"`
	data               *document.Blob
	artifactUnverified bool
}

type attachmentFailure struct {
	code  string
	cause error
}

func (e *attachmentFailure) Error() string          { return "attachment operation: " + e.code }
func (e *attachmentFailure) Unwrap() error          { return e.cause }
func attachmentFail(code string, cause error) error { return &attachmentFailure{code, cause} }

// Keep causes available for programmatic inspection without printing their
// potentially private server/parser text through the command wrapper.
type attachmentCommandError struct{ coded, cause error }

func (e *attachmentCommandError) Error() string   { return e.coded.Error() }
func (e *attachmentCommandError) Unwrap() []error { return []error{e.coded, e.cause} }

func attachmentError(err error) (string, error) {
	code := "io_error"
	var f *attachmentFailure
	if errors.As(err, &f) {
		code = f.code
	}
	if code == "cleanup" || errors.Is(err, document.ErrCleanup) {
		code = "cleanup"
	} else if interrupted(err) {
		code = "cancelled"
	}
	if code == "io_error" {
		if agentio.ExitCode(classify(err)) == agentio.ExitStale {
			code = "stale_identity"
		} else if errors.Is(err, document.ErrIncomplete) {
			code = "incomplete_content"
		}
	}
	message := "attachment operation failed (" + code + ")"
	hint := "mailpipe attachments --help"
	switch code {
	case "usage":
		return code, agentio.NewUsageError(message, hint)
	case "no_data":
		return code, agentio.NewDataError(message, "mailpipe attachments list <n>")
	case "stale_identity":
		return code, agentio.NewStaleError(message, "mailpipe account show; mailpipe auth status; mailpipe attachments list <n>")
	default:
		return code, agentio.NewError(message, hint)
	}
}

func attachmentTable(a *agentio.AgentIO, rows []*attachmentRow) error {
	return agentio.Tabled(a, []string{"provider_id", "account_id", "n", "part", "mime_path", "kind", "filename", "content_type", "disposition", "cid", "decoded_bytes", "sha256", "output_name", "extractable", "status", "error_code", "detail", "saved_path", "planned_path"}, rows,
		func(r *attachmentRow) []string {
			return []string{oneLine(r.ProviderID), oneLine(r.AccountID), strconv.FormatInt(r.N, 10), r.Part, r.MIMEPath, r.Kind, oneLine(r.Filename), oneLine(r.ContentType), oneLine(r.Disposition), oneLine(r.CID), strconv.FormatInt(r.DecodedBytes, 10), r.SHA256, oneLine(r.OutputName), strconv.FormatBool(r.Extractable), r.Status, r.ErrorCode, r.Detail, oneLine(r.SavedPath), oneLine(r.PlannedPath)}
		},
		func(r *attachmentRow) map[string]any {
			return map[string]any{"provider_id": r.ProviderID, "account_id": r.AccountID, "n": r.N, "part": r.Part, "mime_path": r.MIMEPath, "kind": r.Kind, "filename": r.Filename, "content_type": r.ContentType, "disposition": r.Disposition, "cid": r.CID, "decoded_bytes": r.DecodedBytes, "sha256": r.SHA256, "output_name": r.OutputName, "extractable": r.Extractable, "status": r.Status, "error_code": r.ErrorCode, "detail": r.Detail, "saved_path": r.SavedPath, "planned_path": r.PlannedPath}
		})
}

func attachmentFailureRows(n int64, rows []*attachmentRow, code string) []*attachmentRow {
	if len(rows) == 0 {
		rows = []*attachmentRow{{N: n, Status: "failed", ErrorCode: code, Detail: "attachment operation failed"}}
	}
	marked := false
	for _, r := range rows {
		if r.artifactUnverified {
			r.Status = "failed"
			r.SavedPath = ""
			if r.ErrorCode == "" {
				r.ErrorCode = code
			}
			marked = true
		} else if r.SavedPath != "" {
			r.Status, r.ErrorCode, r.Detail = "published", code, "complete file published; operation failed"
			marked = true
		} else if r.Status != "not_saved" {
			r.Status, r.ErrorCode, r.Detail = "failed", code, "attachment operation failed"
			marked = true
		}
	}
	if !marked {
		rows[0].Status, rows[0].ErrorCode, rows[0].Detail = "failed", code, "attachment operation failed"
	}
	return rows
}

func newAttachmentsCommand(deps Deps) *cobra.Command {
	leaf := func(extract bool) *cobra.Command {
		use, short := "list <n>", "List complete MIME attachments and inline images, including unnamed images"
		if extract {
			use, short = "extract <n> --part <selector> --out-dir <dir> [--execute]", "Check or extract explicitly selected MIME parts as exact decoded bytes"
		}
		cmd := newAgentCommand(use, short, cobra.ExactArgs(1), func(cmd *cobra.Command, a *agentio.AgentIO, args []string) error {
			n, e := strconv.ParseInt(args[0], 10, 64)
			var rows []*attachmentRow
			if e != nil || n < 1 {
				e = attachmentFail("usage", nil)
			} else {
				rows, e = runAttachments(cmd, deps, n, extract)
			}
			if e != nil {
				code, coded := attachmentError(e)
				rows = attachmentFailureRows(n, rows, code)
				return errors.Join(&attachmentCommandError{coded: coded, cause: e}, attachmentTable(a, rows))
			}
			return attachmentTable(a, rows)
		})
		cmd.Long = short + ".\n\nSelectors bind the account, message and complete MIME snapshot. Re-list if MIME is\nreserialized or changes. Default listing includes all attachments and inline images,\neven unnamed/CID-less images; no message attachment flag is trusted. Image kind\nfollows existing precedence: attachment disposition, then CID inline, then named\nattachment, then unnamed inline. Attached\ncontainers/messages are opaque files; ordinary text/HTML bodies are not files.\nRows include MIME path, kind, filename/CID, decoded size, SHA-256 and output name.\nOutput names use a MIME-path prefix and the safe original name, or a deterministic\nmedia-type extension (.bin for unknown types). No rendering, implicit opens or filing\nverdicts. Data: stdout TSV or --json JSON Lines; hints/errors/footer: stderr.\nExit codes: 0 success, 1 runtime/unsafe path/collision/incomplete, 2 usage,\n3 stale identity/configuration/sign-in, 4 no matching parts."
		if extract {
			cmd.Flags().StringArray("part", nil, "full selector from attachments list; repeat for each selected part")
			cmd.Flags().String("out-dir", "", "required existing user-owned 0700 output directory; no default; relative to invocation cwd")
			cmd.Flags().Bool("execute", false, "authorize selected local 0600 file writes; existing entries always fail collision")
			cmd.Long += "\n\n--out-dir is mandatory, absolute or cwd-relative; no ~ expansion. The directory\nmust already exist, be owned by you with mode 0700, and not be a symlink.\nCheck mode fetches/validates content using private bounded spools but writes no\noutput-root files. --execute writes flat, validated names under the pinned root.\nNo overwrites, even identical files; unsafe MIME names are rejected. Multi-part\npublication is not atomic: a late failure reports earlier complete published files\nand exits nonzero. Extraction never marks a message filed or authorizes deletion."
		} else {
			cmd.Flags().String("kind", "all", "all, attachment or inline (default includes inline images)")
		}
		return cmd
	}
	return newGroup("attachments", "Inspect MIME files and safely extract selected decoded parts", leaf(false), leaf(true))
}

func attachmentDigest(ctx context.Context, open func() (io.ReadCloser, error), max int64) (string, int64, error) {
	r, e := open()
	if e != nil {
		return "", 0, e
	}
	h := sha256.New()
	n, e := attachmentCopy(ctx, h, r, max)
	e = errors.Join(e, r.Close())
	if e != nil {
		return "", 0, e
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func attachmentSelector(account mailbox.Account, n int64, ref mailbox.MessageRef, raw, path, decoded string) string {
	b, _ := json.Marshal([]string{"mailpipe-attachment-selector-v1", string(account.ProviderID), string(account.ID), strconv.FormatInt(n, 10), string(ref.MessageID), string(ref.Locator), raw, path, decoded})
	h := sha256.Sum256(b)
	return "v1." + hex.EncodeToString(h[:])
}

func runAttachments(cmd *cobra.Command, deps Deps, n int64, extract bool) (rows []*attachmentRow, result error) {
	var selectors []string
	var destination string
	kind := "all"
	execute := false
	if extract {
		selectors, _ = cmd.Flags().GetStringArray("part")
		destination, _ = cmd.Flags().GetString("out-dir")
		execute, _ = cmd.Flags().GetBool("execute")
		if destination == "" || len(selectors) == 0 || len(selectors) > document.DefaultLimits().Parts {
			return nil, attachmentFail("usage", nil)
		}
		seen := map[string]bool{}
		for _, p := range selectors {
			if len(p) != 67 || !strings.HasPrefix(p, "v1.") || strings.ToLower(p) != p || seen[p] {
				return nil, attachmentFail("usage", nil)
			}
			if _, e := hex.DecodeString(p[3:]); e != nil {
				return nil, attachmentFail("usage", nil)
			}
			seen[p] = true
		}
	} else {
		kind, _ = cmd.Flags().GetString("kind")
		if kind != "all" && kind != "attachment" && kind != "inline" {
			return nil, attachmentFail("usage", nil)
		}
	}
	var output *attachmentRoot
	if extract {
		var e error
		output, e = openAttachmentRoot(destination)
		if e != nil {
			return nil, e
		}
		defer func() {
			if e := output.Close(); e != nil {
				result = attachmentFail("cleanup", errors.Join(result, e))
			}
		}()
	}
	limits := document.DefaultLimits()
	ctx, cancel := context.WithTimeout(cmd.Context(), limits.Timeout)
	defer cancel()
	var scope *document.Scope
	var source *document.Source
	var doc *document.FileDoc
	var s *commandStore
	get, closeProvider := lazyProvider(ctx, deps)
	closed := false
	closeContent := func() error {
		if closed {
			return nil
		}
		closed = true
		var e error
		if doc != nil {
			e = errors.Join(e, doc.Close())
		}
		if source != nil {
			e = errors.Join(e, source.Close())
		}
		if scope != nil {
			e = errors.Join(e, scope.CloseContext(ctx))
		}
		e = errors.Join(e, closeProvider())
		if s != nil {
			e = errors.Join(e, s.Close())
		}
		return e
	}
	defer func() {
		if e := closeContent(); e != nil {
			result = attachmentFail("cleanup", errors.Join(result, e))
		}
	}()
	var e error
	s, e = openStore(cmd, deps)
	if e != nil {
		return nil, e
	}
	if s.bound == nil {
		return nil, attachmentFail("stale_identity", nil)
	}
	account := s.bound.Account()
	ref, e := s.remoteRef(ctx, n)
	if e != nil {
		return nil, e
	}
	g, e := get()
	if e != nil {
		return nil, e
	}
	if g.Account() != account {
		return nil, attachmentFail("stale_identity", nil)
	}
	scope, e = document.NewScope(limits)
	if e != nil {
		return nil, e
	}
	ctx = document.WithScope(ctx, scope)
	source, e = document.OpenSource(ctx, g, ref, scope)
	if e != nil {
		return nil, e
	}
	raw, _, e := attachmentDigest(ctx, source.Open, limits.RawBytes)
	if e != nil {
		return nil, e
	}
	r, e := source.Open()
	if e != nil {
		return nil, e
	}
	doc, e = document.ParsePartsReader(ctx, r, scope)
	e = errors.Join(e, r.Close())
	if e != nil {
		return nil, e
	}
	all := make(map[string]*attachmentRow, len(doc.Parts))
	for _, p := range doc.Parts {
		h, size, e := attachmentDigest(ctx, p.Data.Open, limits.PartBytes)
		if e != nil {
			return nil, e
		}
		row := &attachmentRow{ProviderID: string(account.ProviderID), AccountID: string(account.ID), N: n, MIMEPath: p.Path, Kind: p.Kind, Filename: p.Filename, ContentType: p.ContentType, Disposition: p.Disposition, CID: p.CID, DecodedBytes: size, SHA256: h, Status: "available", Extractable: true, data: p.Data}
		row.Part = attachmentSelector(account, n, ref, raw, p.Path, h)
		row.OutputName, e = attachmentName(p)
		if e != nil {
			row.Extractable, row.Status, row.ErrorCode = false, "unsafe", "unsafe_path"
		}
		all[row.Part] = row
		if !extract && (kind == "all" || kind == "attachment" && row.Kind == "attachment" || kind == "inline" && row.Kind == "inline_image") {
			rows = append(rows, row)
		}
	}
	if extract {
		for _, p := range selectors {
			row, ok := all[p]
			if !ok {
				return nil, attachmentFail("stale_identity", nil)
			}
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil, attachmentFail("no_data", nil)
	}
	finishContent := func() error {
		// Execute invokes this after staging, immediately before publication.
		// A concurrent sync/config change must not silently reuse the selection.
		current, err := s.remoteRef(ctx, n)
		if err != nil {
			return err
		}
		cfg, err := routing()
		if err != nil {
			return err
		}
		if current != ref || cfg.Account() != account || g.Account() != account {
			return attachmentFail("stale_identity", nil)
		}
		if err := closeContent(); err != nil {
			return attachmentFail("cleanup", err)
		}
		return nil
	}
	if extract {
		return rows, extractAttachments(ctx, output, rows, execute, limits, finishContent)
	}
	return rows, finishContent()
}
