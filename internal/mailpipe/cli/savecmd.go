package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/pkg/document"
	"github.com/mrothroc/mailpipe/pkg/store"
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
		cobra.NoArgs, func(cmd *cobra.Command, agent *agentio.AgentIO, _ []string) error {
			ctx := cmd.Context()
			manifestPath, _ := cmd.Flags().GetString("manifest")
			root, _ := cmd.Flags().GetString("root")
			execute, _ := cmd.Flags().GetBool("execute")
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
			defer s.Close()
			rows, err := readManifest(ctx, s, manifestPath)
			if err != nil {
				return err
			}
			retry := fmt.Sprintf("mailpipe save --manifest %s --root %s --execute", manifestPath, root)
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
			var g mailbox.Provider
			if len(todo) > 0 {
				if g, err = lazyProvider(ctx, deps)(); err != nil {
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
func saveOne(ctx context.Context, s *commandStore, g mailbox.Provider, renderFn func(context.Context, string) ([]byte, error), root *os.Root, r *saveRow) {
	fail := func(err error) { r.status, r.detail = "failed", err.Error() }
	ref, err := s.remoteRef(ctx, r.msg.N)
	if err != nil {
		r.status, r.detail = "failed", err.Error()
		return
	}
	raw, err := document.ReadSource(ctx, g, ref)
	if err != nil {
		fail(fmt.Errorf("fetch: %w", err))
		return
	}
	doc, err := document.Parse(raw)
	if err != nil {
		fail(err)
		return
	}
	outs, err := outputs(ctx, doc, r.name, renderFn)
	if err != nil {
		fail(err)
		return
	}
	wrote := false
	for _, o := range outs {
		rel := filepath.Join(r.folder, o.name)
		created, err := writeNew(root, rel, o.data)
		if err != nil {
			fail(err)
			return
		}
		wrote = wrote || created
		r.files = append(r.files, rel)
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
