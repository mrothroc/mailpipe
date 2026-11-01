package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/document"
	"github.com/mrothroc/mailpipe/pkg/store"
	"github.com/spf13/cobra"
)

// filingStage records what save has done to a message: "pending" once it is
// in a manifest being saved, "saved" once its files are written. The record
// deletion rule spares filing:pending, so nothing to be filed is deleted first.
const filingStage = "filing"

type saveRow struct {
	line   int
	msg    store.Message
	folder string
	name   string
	status string
	files  []string
	detail string
}

// readManifest reads and checks every row before anything is written: a
// header "n<TAB>folder<TAB>name", then one row per message. Folders are
// relative to the root and may not leave it; names are plain file names.
func readManifest(ctx context.Context, s *commandStore, path string) ([]*saveRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, agentio.NewUsageError(err.Error(), "mailpipe save --help")
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var rows []*saveRow
	seenN, seenPath := map[int64]int{}, map[string]int{}
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimRight(sc.Text(), "\r")
		if line == 1 {
			if text != "n\tfolder\tname" {
				return nil, agentio.NewUsageError(fmt.Sprintf("%s: the first line must be the header n<TAB>folder<TAB>name", path), "mailpipe save --help")
			}
			continue
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		bad := func(why string) error {
			return agentio.NewUsageError(fmt.Sprintf("%s line %d: %s", path, line, why), "mailpipe save --help")
		}
		cols := strings.Split(text, "\t")
		if len(cols) != 3 {
			return nil, bad(fmt.Sprintf("want 3 columns, got %d", len(cols)))
		}
		n, err := strconv.ParseInt(strings.TrimSpace(cols[0]), 10, 64)
		if err != nil || n < 1 {
			return nil, bad(fmt.Sprintf("message number %q", cols[0]))
		}
		folder := filepath.Clean(strings.TrimSpace(cols[1]))
		if filepath.IsAbs(folder) || folder == ".." || strings.HasPrefix(folder, ".."+string(filepath.Separator)) {
			return nil, bad(fmt.Sprintf("folder %q must stay inside the root", cols[1]))
		}
		name := strings.TrimSpace(cols[2])
		// ';' separates the files recorded in a filing verdict.
		if name == "" || strings.ContainsAny(name, "/\\:;") || strings.Trim(name, ".") == "" {
			return nil, bad(fmt.Sprintf("name %q must be a plain file name (no / \\ : or ;)", cols[2]))
		}
		if !strings.HasSuffix(strings.ToLower(name), ".pdf") {
			name += ".pdf"
		}
		// Each message and each file once: two rows racing for one file, or one
		// message saved twice, is a manifest mistake.
		if prev, dup := seenN[n]; dup {
			return nil, bad(fmt.Sprintf("message %d is already on line %d", n, prev))
		}
		target := strings.ToLower(filepath.Join(folder, name))
		if prev, dup := seenPath[target]; dup {
			return nil, bad(fmt.Sprintf("%s is already the file on line %d", filepath.Join(folder, name), prev))
		}
		seenN[n], seenPath[target] = line, line
		m, ok, err := s.Get(ctx, n)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, bad(fmt.Sprintf("no message %d in the mirror", n))
		}
		rows = append(rows, &saveRow{line: line, msg: m, folder: folder, name: name})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, agentio.NewDataError(path+" lists no messages", "mailpipe save --help")
	}
	return rows, nil
}

// pdfTimes are the parts of a PDF that differ between two renders of the
// same page: Chrome stamps each with its creation time.
var pdfTimes = regexp.MustCompile(`/(?:CreationDate|ModDate) \(D:[^)]*\)`)

// samePDF reports two PDFs that are the same document apart from when they
// were made.
func samePDF(a, b []byte) bool {
	return bytes.HasPrefix(a, []byte("%PDF-")) && bytes.HasPrefix(b, []byte("%PDF-")) &&
		bytes.Equal(pdfTimes.ReplaceAll(a, nil), pdfTimes.ReplaceAll(b, nil))
}

// writeNew writes data to path unless something is there: the same document
// is reported as already there, anything else is an error. A hard link
// creates the name, so even a file that appears meanwhile is never
// overwritten.
func writeNew(root *os.Root, path string, data []byte) (bool, error) {
	if !filepath.IsLocal(path) {
		return false, errors.New("file must stay inside the filing root")
	}
	if err := root.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	// Pin the parent directory. Every operation stays relative to this
	// descriptor, including temp creation, link, read-back and cleanup.
	dir, err := root.OpenRoot(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	defer dir.Close()
	name := filepath.Base(path)
	if info, err := dir.Lstat(name); err == nil && !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s already exists and is not a regular file", path)
	}
	if old, err := dir.ReadFile(name); err == nil {
		if bytes.Equal(old, data) || samePDF(old, data) {
			return false, nil
		}
		return false, fmt.Errorf("%s already exists with different content", path)
	} else if !os.IsNotExist(err) {
		return false, err
	}
	tempName := ".mailpipe-save-" + rand.Text()
	tmp, err := dir.OpenFile(tempName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	defer dir.Remove(tempName)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, err
	}
	if err := dir.Link(tempName, name); err != nil {
		return false, fmt.Errorf("%s already exists, or could not be created: %w", path, err)
	}
	if got, err := dir.ReadFile(name); err != nil || !bytes.Equal(got, data) {
		return false, fmt.Errorf("%s did not read back as written", path)
	}
	return true, nil
}

type outputFile struct {
	name string
	data []byte
}

// outputs returns the files a message becomes: its PDF attachments as they
// are, or, with none, the message rendered to PDF. A single attachment takes
// name; several are suffixed with their own names (or "attachment <i>"), made
// unique.
func outputs(ctx context.Context, doc document.Doc, name string, renderFn func(context.Context, string) ([]byte, error)) ([]outputFile, error) {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	pdfs := doc.PDFs()
	if len(pdfs) == 0 {
		pdf, err := renderFn(ctx, doc.OfflineHTML())
		if err != nil {
			return nil, err
		}
		return []outputFile{{name, pdf}}, nil
	}
	if len(pdfs) == 1 {
		return []outputFile{{name, pdfs[0].Data}}, nil
	}
	used := map[string]bool{}
	out := make([]outputFile, len(pdfs))
	for i, p := range pdfs {
		own := strings.TrimSuffix(filepath.Base(p.Filename), filepath.Ext(p.Filename))
		own = strings.TrimSpace(strings.NewReplacer("/", "-", "\\", "-", ":", "-", ";", ",").Replace(own))
		if own == "" || strings.Trim(own, ".") == "" {
			own = fmt.Sprintf("attachment %d", i+1)
		}
		n := fmt.Sprintf("%s - %s.pdf", stem, own)
		for k := 2; used[strings.ToLower(n)]; k++ {
			n = fmt.Sprintf("%s - %s (%d).pdf", stem, own, k)
		}
		used[strings.ToLower(n)] = true
		out[i] = outputFile{n, p.Data}
	}
	return out, nil
}

func newSaveCommand(deps Deps) *cobra.Command {
	cmd := newAgentCommand("save --manifest <file> [--root <dir>] [--execute]",
		"Save messages as PDFs into a filing tree: PDF attachments as they are, otherwise the message rendered offline",
		cobra.NoArgs, func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) (commandErr error) {
			ctx := cmd.Context()
			manifestPath, _ := cmd.Flags().GetString("manifest")
			root, _ := cmd.Flags().GetString("root")
			execute, _ := cmd.Flags().GetBool("execute")
			if execute {
				if e := pilotGuard(cmd, "save execution"); e != nil {
					return e
				}
			}
			workers, _ := cmd.Flags().GetInt("workers")
			if manifestPath == "" || workers < 1 {
				return agentio.NewUsageError("--manifest is required and --workers must be at least 1", "mailpipe save --help")
			}
			if root == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				root = filepath.Join(home, "Documents", "To File")
			}
			s, err := openStore(cmd, deps)
			if err != nil {
				return err
			}
			defer func() { commandErr = errors.Join(commandErr, s.Close()) }()
			rows, err := readManifest(ctx, s, manifestPath)
			if err != nil {
				return err
			}
			retry := fmt.Sprintf("mailpipe save --manifest %s --root %s --execute", manifestPath, root)
			if s.cfg.Mode() == "yahoo" {
				retry = "mailpipe plan list"
			}
			if !execute {
				for _, r := range rows {
					r.status, r.files = "would_save", []string{filepath.Join(r.folder, r.name)}
				}
				if err := saveTable(agent, rows); err != nil {
					return err
				}
				fmt.Fprintf(agent.Stderr(), "[hint] check only, nothing written: %d to save under %s. Try: %s\n", len(rows), root, retry)
				return nil
			}
			scope, scopeErr := document.NewScope(document.Limits{})
			if scopeErr != nil {
				return scopeErr
			}
			defer func() { commandErr = errors.Join(commandErr, scope.CloseContext(ctx)) }()
			ctx = document.WithScope(ctx, scope)
			if err := os.MkdirAll(root, 0o700); err != nil {
				return err
			}
			filingRoot, err := os.OpenRoot(root)
			if err != nil {
				return err
			}
			defer filingRoot.Close()
			// Rows already saved where this manifest says need no fetch, and
			// must not be put back to pending.
			var todo []*saveRow
			for _, r := range rows {
				done, err := alreadySaved(ctx, s, filingRoot, r)
				if err != nil {
					return err
				}
				if !done {
					todo = append(todo, r)
				}
			}
			var pending []store.Verdict
			for _, r := range todo {
				pending = append(pending, store.Verdict{N: r.msg.N, Stage: filingStage, Labels: []string{"pending"},
					Reason: filepath.Join(r.folder, r.name), Provenance: "mailpipe save"})
			}
			if err := s.AddVerdicts(ctx, pending); err != nil {
				return err
			}
			var g readMailbox
			if len(todo) > 0 {
				getProvider, closeProvider := lazyProvider(ctx, deps)
				defer func() { commandErr = errors.Join(commandErr, closeProvider()) }()
				if g, err = getProvider(); err != nil {
					return err
				}
			}
			jobs := make(chan *saveRow)
			var wg sync.WaitGroup
			for range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for r := range jobs {
						saveOne(ctx, s, g, deps.Render, filingRoot, r)
					}
				}()
			}
			for _, r := range todo {
				if ctx.Err() != nil {
					break
				}
				jobs <- r
			}
			close(jobs)
			wg.Wait()
			if err := saveTable(agent, rows); err != nil {
				return err
			}
			counts := map[string]int{}
			files := 0
			for _, r := range rows {
				counts[r.status]++
				if r.status == "saved" {
					files += len(r.files)
				}
			}
			fmt.Fprintf(agent.Stderr(), "[hint] saved %d (%d %s), %d already there, %d failed, under %s\n",
				counts["saved"], files, plural(files, "file", "files"), counts["exists"], counts["failed"], root)
			if err := ctx.Err(); err != nil {
				return err
			}
			if counts["failed"] > 0 {
				return agentio.NewError(fmt.Sprintf("%d of %d messages were not saved; they stay filing:pending", counts["failed"], len(rows)), retry)
			}
			return nil
		})
	cmd.Flags().String("manifest", "", "TSV: header n<TAB>folder<TAB>name, then one row per message (folder relative to --root)")
	cmd.Flags().String("root", "", "the filing root (default ~/Documents/To File)")
	cmd.Flags().Bool("execute", false, "fetch, render and write the files (without it, only check the manifest)")
	cmd.Flags().Int("workers", 4, "messages saved in parallel")
	return cmd
}

// alreadySaved reports, and marks on r, a row whose message's latest filing
// verdict is "saved" to this row's target (its file, or its attachments
// named after it), with every one of those files still there.
func alreadySaved(ctx context.Context, s *commandStore, root *os.Root, r *saveRow) (bool, error) {
	vs, err := s.Verdicts(ctx, r.msg.N)
	if err != nil {
		return false, err
	}
	var last *store.Verdict
	for i := range vs {
		if vs[i].Stage == filingStage && vs[i].Error == "" {
			last = &vs[i]
		}
	}
	if last == nil || !slices.Contains(last.Labels, "saved") || last.Reason == "" {
		return false, nil
	}
	primary := filepath.Join(r.folder, r.name)
	stem := strings.TrimSuffix(primary, filepath.Ext(primary)) + " - "
	files := strings.Split(last.Reason, "; ")
	for _, f := range files {
		if f != primary && !strings.HasPrefix(f, stem) {
			return false, nil
		}
		if _, err := root.Stat(f); err != nil {
			return false, nil
		}
	}
	r.status, r.files = "exists", files
	return true, nil
}

// saveOne saves one row's files and records the outcome on the row and, once
// saved, as a filing:saved verdict.
func saveOne(ctx context.Context, s *commandStore, g readMailbox, renderFn func(context.Context, string) ([]byte, error), root *os.Root, r *saveRow) {
	fail := func(err error) { r.status, r.detail = "failed", err.Error() }
	ref, err := s.remoteRef(ctx, r.msg.N)
	if err != nil {
		r.status, r.detail = "failed", err.Error()
		return
	}

	scope := document.ScopeFrom(ctx)
	own := scope == nil
	if own {
		scope, err = document.NewScope(document.Limits{})
		if err != nil {
			fail(err)
			return
		}
	}
	source, err := document.OpenSource(ctx, g, ref, scope)
	if err != nil {
		if own {
			err = errors.Join(err, scope.CloseContext(ctx))
		}
		fail(err)
		return
	}
	rd, err := source.Open()
	if err != nil {
		err = errors.Join(err, source.Close())
		if own {
			err = errors.Join(err, scope.CloseContext(ctx))
		}
		fail(err)
		return
	}
	doc, err := document.ParseReader(ctx, rd, scope)
	err = errors.Join(err, rd.Close())
	if err != nil {
		if doc != nil {
			err = errors.Join(err, doc.Close())
		}
		err = errors.Join(err, source.Close())
		if own {
			err = errors.Join(err, scope.CloseContext(ctx))
		}
		fail(err)
		return
	}
	cleanup := func() error {
		e := errors.Join(doc.Close(), source.Close())
		if own {
			e = errors.Join(e, scope.CloseContext(ctx))
		}
		return e
	}
	pdfs := doc.PDFs()
	wrote := false
	if len(pdfs) > 0 {
		// Reuse the established attachment naming algorithm; no payload is copied.
		naming := document.Doc{}
		for _, p := range pdfs {
			naming.Attachments = append(naming.Attachments, document.Part{Filename: p.Filename, ContentType: p.ContentType})
		}
		names, e := outputs(ctx, naming, r.name, renderFn)
		if e != nil {
			fail(errors.Join(e, cleanup()))
			return
		}
		for i, p := range pdfs {
			created, e := writeNewReader(ctx, root, filepath.Join(r.folder, names[i].name), p.Data.Open, scope.Limits().PDFBytes)
			if e != nil {
				fail(errors.Join(e, cleanup()))
				return
			}
			wrote = wrote || created
			r.files = append(r.files, filepath.Join(r.folder, names[i].name))
		}
	} else {
		page, e := doc.OfflineHTML(ctx)
		if e != nil {
			fail(errors.Join(e, cleanup()))
			return
		}
		pdf, e := renderFn(ctx, page)
		if e != nil {
			fail(errors.Join(e, cleanup()))
			return
		}
		if int64(len(pdf)) > scope.Limits().PDFBytes {
			fail(errors.Join(document.ErrIncomplete, cleanup()))
			return
		}
		created, e := writeNewReader(ctx, root, filepath.Join(r.folder, r.name), func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(pdf)), nil }, scope.Limits().PDFBytes)
		if e != nil {
			fail(errors.Join(e, cleanup()))
			return
		}
		wrote = created
		r.files = append(r.files, filepath.Join(r.folder, r.name))
	}
	if err = cleanup(); err != nil {
		fail(err)
		return
	}
	r.status = "exists"
	if wrote {
		r.status = "saved"
	}
	if err := s.AddVerdict(ctx, store.Verdict{N: r.msg.N, Stage: filingStage, Labels: []string{"saved"},
		Reason: strings.Join(r.files, "; "), Provenance: "mailpipe save"}); err != nil {
		fail(errors.Join(errors.New("saved, but recording it failed"), err))
	}
}

func saveTable(agent *agentio.AgentIO, rows []*saveRow) error {
	return agentio.Tabled(agent, []string{"n", "sender", "subject", "status", "files", "detail"}, rows,
		func(r *saveRow) []string {
			return []string{strconv.FormatInt(r.msg.N, 10), r.msg.Sender, oneLine(r.msg.Subject), r.status, strings.Join(r.files, "; "), r.detail}
		},
		func(r *saveRow) map[string]any {
			return map[string]any{"n": r.msg.N, "sender": r.msg.Sender, "subject": r.msg.Subject, "status": r.status,
				"files": r.files, "detail": r.detail}
		})
}

// pdfDateReader preserves the existing PDF timestamp-insensitive comparison
// without loading an existing attachment. Unbounded date tokens fail closed.
type pdfDateReader struct{ r *bufio.Reader }

func (p pdfDateReader) Read(out []byte) (int, error) {
	n := 0
	for n < len(out) {
		prefix, _ := p.r.Peek(23)
		skip := 0
		for _, token := range []string{"/CreationDate (D:", "/ModDate (D:"} {
			if bytes.HasPrefix(prefix, []byte(token)) {
				skip = len(token)
				break
			}
		}
		if skip > 0 {
			_, _ = p.r.Discard(skip)
			for count := skip; ; count++ {
				if count > 1024 {
					return n, errors.New("PDF date comparison token exceeds limit")
				}
				b, e := p.r.ReadByte()
				if e != nil {
					return n, e
				}
				if b == ')' {
					break
				}
			}
			continue
		}
		b, e := p.r.ReadByte()
		if e != nil {
			return n, e
		}
		out[n] = b
		n++
	}
	return n, nil
}
func equalReaders(a, b io.Reader) (bool, error) {
	left, right := make([]byte, 32768), make([]byte, 32768)
	for {
		na, ea := io.ReadFull(a, left)
		nb, eb := io.ReadFull(b, right)
		if ea != nil && ea != io.EOF && ea != io.ErrUnexpectedEOF {
			return false, ea
		}
		if eb != nil && eb != io.EOF && eb != io.ErrUnexpectedEOF {
			return false, eb
		}
		if na != nb || !bytes.Equal(left[:na], right[:nb]) {
			return false, nil
		}
		if ea != nil || eb != nil {
			return ea == eb, nil
		}
	}
}
func writeNewReader(ctx context.Context, root *os.Root, path string, open func() (io.ReadCloser, error), maxBytes int64) (created bool, err error) {
	if !filepath.IsLocal(path) {
		return false, errors.New("file must stay inside filing root")
	}
	if err = root.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, err
	}
	dir, e := root.OpenRoot(filepath.Dir(path))
	if e != nil {
		return false, e
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	name := filepath.Base(path)
	if info, e := dir.Lstat(name); e == nil {
		if !info.Mode().IsRegular() || info.Size() > maxBytes {
			return false, errors.New("existing file is not a bounded regular output")
		}
		compare := func(normalize bool) (same bool, err error) {
			old, e := dir.Open(name)
			if e != nil {
				return false, e
			}
			defer func() { err = errors.Join(err, old.Close()) }()
			fresh, e := open()
			if e != nil {
				return false, e
			}
			defer func() { err = errors.Join(err, fresh.Close()) }()
			if normalize {
				a, b := bufio.NewReaderSize(contextFileReader{ctx, old}, 4096), bufio.NewReaderSize(contextFileReader{ctx, fresh}, 4096)
				ha, _ := a.Peek(5)
				hb, _ := b.Peek(5)
				if string(ha) != "%PDF-" || string(hb) != "%PDF-" {
					return false, nil
				}
				return equalReaders(pdfDateReader{a}, pdfDateReader{b})
			}
			return equalReaders(contextFileReader{ctx, old}, contextFileReader{ctx, fresh})
		}
		same, e := compare(false)
		if e != nil {
			return false, e
		}
		if !same {
			same, e = compare(true)
		}
		if e != nil {
			return false, e
		}
		if !same {
			return false, fmt.Errorf("%s already exists with different content", path)
		}
		return false, nil
	} else if !os.IsNotExist(e) {
		return false, e
	}
	tempName := ".mailpipe-save-" + rand.Text()
	f, e := dir.OpenFile(tempName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return false, e
	}
	defer func() { err = errors.Join(err, dir.Remove(tempName)) }()
	input, e := open()
	if e != nil {
		return false, errors.Join(e, f.Close())
	}
	buffer := make([]byte, 32768)
	var count int64
	for {
		if e = ctx.Err(); e != nil {
			break
		}
		n, re := input.Read(buffer)
		if int64(n) > maxBytes-count {
			e = document.ErrIncomplete
			break
		}
		if n > 0 {
			var written int
			written, e = f.Write(buffer[:n])
			count += int64(written)
			if e == nil && written != n {
				e = io.ErrShortWrite
			}
			if e != nil {
				break
			}
		}
		if re != nil {
			if re != io.EOF {
				e = re
			}
			break
		}
	}
	e = errors.Join(e, input.Close())
	if e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return false, e
	}
	if e = dir.Link(tempName, name); e != nil {
		return false, e
	}
	// Verify through the same pinned directory and bounded source reader.
	old, e := dir.Open(name)
	if e != nil {
		return false, e
	}
	fresh, e := open()
	if e != nil {
		return false, errors.Join(e, old.Close())
	}
	same, e := equalReaders(contextFileReader{ctx, old}, contextFileReader{ctx, fresh})
	e = errors.Join(e, old.Close(), fresh.Close())
	if e != nil {
		return false, e
	}
	if !same {
		return false, errors.New("output readback differs")
	}
	return true, nil
}

// contextFileReader checks cancellation between finite comparison/readback reads.
// A blocking local filesystem syscall still requires filesystem cooperation.
type contextFileReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextFileReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(b)
}
