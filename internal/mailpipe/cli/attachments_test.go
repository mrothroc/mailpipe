package cli_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/graphmail"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func attachmentFixture(t *testing.T) (*env, []byte) {
	t.Helper()
	e := newEnv(t)
	if code, _, stderr := e.run("sync"); code != 0 {
		t.Fatal(stderr)
	}
	payload := []byte{0, 255, 1, 2, '\r', '\n'}
	e.graph.mime = map[string]string{"g1": "From: sender@example.org\r\nSubject: Image\r\nContent-Type: image/png\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString(payload)}
	return e, payload
}

type attachmentSourceObserver struct {
	readOnlyMailbox
	after func() error
}

func (p attachmentSourceObserver) WriteMIME(ctx context.Context, ref mailbox.MessageRef, w io.Writer) error {
	if e := p.source.WriteMIME(ctx, ref, w); e != nil {
		return e
	}
	return p.after()
}

func TestAttachmentsRecheckConfigurationBeforePublication(t *testing.T) {
	e, _ := attachmentFixture(t)
	part := attachmentList(t, e, "1")[0]["part"].(string)
	subject, err := e.auth.Subject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, err := graphmail.New(e.graph, subject.Account())
	if err != nil {
		t.Fatal(err)
	}
	e.provider = attachmentSourceObserver{readOnlyMailbox{p}, func() error {
		cfg, e := config.Load(config.DefaultPath())
		if e != nil {
			return e
		}
		cfg.AccountID = "another-account"
		return config.Save(config.DefaultPath(), cfg)
	}}
	root := attachmentPrivateDir(t)
	code, out, stderr := e.run("attachments", "extract", "1", "--part", part, "--out-dir", root, "--execute", "--json")
	if code != 3 || !strings.Contains(out, `"error_code":"stale_identity"`) {
		t.Fatalf("changed config %d %s %s", code, out, stderr)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("changed config published or leaked", entries, err)
	}
}

type attachmentWrongAccount struct {
	readOnlyMailbox
	account mailbox.Account
}

func (p attachmentWrongAccount) Account() mailbox.Account { return p.account }

func TestAttachmentsRejectProviderAccountMismatchBeforeContent(t *testing.T) {
	e, _ := attachmentFixture(t)
	subject, err := e.auth.Subject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, err := graphmail.New(e.graph, subject.Account())
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"account", "provider"} {
		a := subject.Account()
		if change == "account" {
			a.ID = "another-account"
		} else {
			a.ProviderID = "another-provider"
		}
		e.provider = attachmentWrongAccount{readOnlyMailbox{p}, a}
		before := e.graph.rawCalls.Load()
		code, out, stderr := e.run("attachments", "list", "1", "--json")
		if code != 3 || !strings.Contains(out, `"error_code":"stale_identity"`) || before != e.graph.rawCalls.Load() {
			t.Fatalf("mismatch %d %s %s", code, out, stderr)
		}
	}
}

func attachmentPrivateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	return dir
}

func attachmentList(t *testing.T, e *env, n string) []map[string]any {
	t.Helper()
	code, out, stderr := e.run("attachments", "list", n, "--json")
	if code != 0 {
		t.Fatalf("list %d %s %s", code, out, stderr)
	}
	return attachmentRows(t, out)
}

func TestAttachmentsImageKindFiltersFollowLegacyPrecedence(t *testing.T) {
	e, _ := attachmentFixture(t)
	for _, tc := range []struct{ name, headers, kind string }{
		{"named-no-cid", "Content-Disposition: inline; filename=picture.png\r\n", "attachment"},
		{"unnamed-no-cid", "", "inline_image"},
		{"named-cid", "Content-Disposition: inline; filename=picture.png\r\nContent-ID: <picture@example.org>\r\n", "inline_image"},
		{"explicit-attachment", "Content-Disposition: attachment; filename=picture.png\r\nContent-ID: <picture@example.org>\r\n", "attachment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.graph.mime["g1"] = "From: sender@example.org\r\nContent-Type: image/png\r\n" + tc.headers + "\r\nimage-bytes"
			for _, filter := range []string{"all", "attachment", "inline"} {
				code, out, stderr := e.run("attachments", "list", "1", "--kind", filter, "--json")
				matches := filter == "all" || filter == "attachment" && tc.kind == "attachment" || filter == "inline" && tc.kind == "inline_image"
				if matches {
					rows := attachmentRows(t, out)
					if code != 0 || len(rows) != 1 || rows[0]["kind"] != tc.kind {
						t.Fatalf("filter %s: %d %s %s; want %s", filter, code, out, stderr, tc.kind)
					}
				} else if code != 4 || !strings.Contains(out, `"error_code":"no_data"`) {
					t.Fatalf("excluded filter %s: %d %s %s", filter, code, out, stderr)
				}
			}
		})
	}
}

func TestAttachmentsDestinationAndUsageMatrix(t *testing.T) {
	e, _ := attachmentFixture(t)
	part := attachmentList(t, e, "1")[0]["part"].(string)
	root := attachmentPrivateDir(t)
	for name, args := range map[string][]string{
		"missing-destination": {"extract", "1", "--part", part},
		"empty-destination":   {"extract", "1", "--part", part, "--out-dir", ""},
		"missing-part":        {"extract", "1", "--out-dir", root},
		"duplicate":           {"extract", "1", "--part", part, "--part", part, "--out-dir", root},
		"bad-token":           {"extract", "1", "--part", "1", "--out-dir", root},
		"nonhex-token":        {"extract", "1", "--part", "v1." + strings.Repeat("z", 64), "--out-dir", root},
		"bad-n":               {"list", "0"}, "bad-kind": {"list", "1", "--kind", "other"},
	} {
		t.Run(name, func(t *testing.T) {
			before := e.graph.rawCalls.Load()
			code, out, stderr := e.run(append([]string{"attachments"}, args...)...)
			if code != 2 || !strings.Contains(out, `"error_code"`) && !strings.Contains(out, "usage") || !strings.Contains(stderr, "Try:") {
				t.Fatalf("usage %d %s %s", code, out, stderr)
			}
			if e.graph.rawCalls.Load() != before {
				t.Fatal("usage read content")
			}
		})
	}
	for _, args := range [][]string{{"attachments"}, {"attachments", "list", "--help"}, {"attachments", "extract", "--help"}} {
		before := e.graph.rawCalls.Load()
		code, out, stderr := e.run(args...)
		if code != 0 || !strings.Contains(out, "Usage:") || e.graph.rawCalls.Load() != before {
			t.Fatalf("help %d %s %s", code, out, stderr)
		}
	}
	for _, args := range [][]string{{"attachments", "unknown"}, {"attachments", "list"}, {"attachments", "list", "1", "--bad"}} {
		code, out, stderr := e.run(args...)
		if code != 2 || out != "" || !strings.Contains(stderr, "[error]") || !strings.Contains(stderr, "Try:") {
			t.Fatalf("wrapper %d %s %s", code, out, stderr)
		}
	}
	for name, path := range map[string]string{"missing": filepath.Join(root, "missing"), "file": filepath.Join(root, "regular"), "broad": t.TempDir(), "symlink": filepath.Join(root, "symlink")} {
		if name == "file" {
			if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if name == "broad" {
			if err := os.Chmod(path, 0755); err != nil {
				t.Fatal(err)
			}
		}
		if name == "symlink" {
			if err := os.Symlink(root, path); err != nil {
				t.Fatal(err)
			}
		}
		code, out, stderr := e.run("attachments", "extract", "1", "--part", part, "--out-dir", path, "--execute", "--json")
		if code != 1 || !strings.Contains(out, `"error_code":"unsafe_path"`) {
			t.Fatalf("root %s: %d %s %s", name, code, out, stderr)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, attachmentPrivateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr := e.run("attachments", "extract", "1", "--part", part, "--out-dir", rel, "--execute", "--json")
	if code != 0 {
		t.Fatalf("relative root %d %s %s", code, out, stderr)
	}
	row := attachmentRows(t, out)[0]
	abs, _ := filepath.Abs(rel)
	if row["saved_path"] != filepath.Join(abs, row["output_name"].(string)) {
		t.Fatal(row)
	}
}

func TestAttachmentsSnapshotStalenessAndFiltering(t *testing.T) {
	e, _ := attachmentFixture(t)
	first := attachmentList(t, e, "1")[0]
	second := attachmentList(t, e, "1")[0]
	if first["part"] != second["part"] {
		t.Fatal("unstable identical snapshot")
	}
	code, out, stderr := e.run("attachments", "list", "1", "--kind", "attachment", "--json")
	if code != 4 || !strings.Contains(out, `"error_code":"no_data"`) {
		t.Fatalf("filter %d %s %s", code, out, stderr)
	}
	code, out, stderr = e.run("attachments", "list", "1", "--kind", "inline", "--json")
	if code != 0 || attachmentRows(t, out)[0]["part"] != first["part"] {
		t.Fatalf("inline filter %d %s %s", code, out, stderr)
	}
	e.graph.mime["g2"] = e.graph.mime["g1"]
	if attachmentList(t, e, "2")[0]["part"] == first["part"] {
		t.Fatal("content equality collapsed messages")
	}
	root := attachmentPrivateDir(t)
	for _, n := range []string{"2", "999"} {
		code, out, stderr = e.run("attachments", "extract", n, "--part", first["part"].(string), "--out-dir", root, "--execute", "--json")
		if code != 3 || !strings.Contains(out, `"error_code":"stale_identity"`) {
			t.Fatalf("wrong message %d %s %s", code, out, stderr)
		}
	}
	e.graph.mime["g1"] = "X-Reserialized: yes\r\n" + e.graph.mime["g1"]
	code, out, stderr = e.run("attachments", "extract", "1", "--part", first["part"].(string), "--out-dir", root, "--execute", "--json")
	if code != 3 || !strings.Contains(out, `"error_code":"stale_identity"`) {
		t.Fatalf("reserialized %d %s %s", code, out, stderr)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("stale wrote files")
	}
	e.auth.signedIn = false
	code, out, stderr = e.run("attachments", "list", "1", "--json")
	if code != 3 {
		t.Fatalf("signed out %d %s %s", code, out, stderr)
	}
}

func TestAttachmentsNamesCollisionsAndSanitizedOutput(t *testing.T) {
	for _, name := range []string{"../outside", "/absolute", `C:\outside`, "one/two", "control\x1bname", "bidi\u202ename"} {
		t.Run(name, func(t *testing.T) {
			e, _ := attachmentFixture(t)
			e.graph.mime["g1"] = "From: a@example.org\r\nContent-Type: application/octet-stream\r\nContent-Disposition: " + mime.FormatMediaType("attachment", map[string]string{"filename": name}) + "\r\n\r\nprivate-payload-canary"
			row := attachmentList(t, e, "1")[0]
			if row["extractable"] != false || row["error_code"] != "unsafe_path" {
				t.Fatal(row)
			}
			code, out, stderr := e.run("attachments", "extract", "1", "--part", row["part"].(string), "--out-dir", attachmentPrivateDir(t), "--execute", "--json")
			if code != 1 || !strings.Contains(out, `"error_code":"unsafe_path"`) || strings.Contains(out+stderr, "\x1b") || strings.Contains(out+stderr, "private-payload-canary") {
				t.Fatalf("unsafe %d %s %s", code, out, stderr)
			}
		})
	}
	for _, kind := range []string{"file", "directory", "symlink-contained", "symlink-escape", "symlink-dangling", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			e, _ := attachmentFixture(t)
			row := attachmentList(t, e, "1")[0]
			root := attachmentPrivateDir(t)
			target := filepath.Join(root, row["output_name"].(string))
			outside := filepath.Join(t.TempDir(), "canary")
			if err := os.WriteFile(outside, []byte("unmodified"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "file":
				err = os.WriteFile(target, []byte("unmodified"), 0600)
			case "directory":
				err = os.Mkdir(target, 0700)
			case "symlink-contained":
				err = os.Symlink("other", target)
			case "symlink-escape":
				err = os.Symlink(outside, target)
			case "symlink-dangling":
				err = os.Symlink("/nonexistent/example.org", target)
			case "hardlink":
				err = os.Link(outside, target)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, execute := range []bool{false, true} {
				args := []string{"attachments", "extract", "1", "--part", row["part"].(string), "--out-dir", root, "--json"}
				if execute {
					args = append(args, "--execute")
				}
				code, out, stderr := e.run(args...)
				if code != 1 || !strings.Contains(out, `"error_code":"collision"`) {
					t.Fatalf("collision %d %s %s", code, out, stderr)
				}
			}
			b, _ := os.ReadFile(outside)
			if string(b) != "unmodified" {
				t.Fatal("overwrote canary")
			}
		})
	}
}

func TestAttachmentsDuplicateNamesLimitAndNoFilingEffects(t *testing.T) {
	e, _ := attachmentFixture(t)
	e.graph.mime["g1"] = "From: a@example.org\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n"
	for _, name := range []string{"same.bin", "same.bin", ""} {
		e.graph.mime["g1"] += "--x\r\nContent-Type: application/octet-stream\r\nContent-Disposition: " + mime.FormatMediaType("attachment", map[string]string{"filename": name}) + "\r\n\r\nexact\r\n"
	}
	e.graph.mime["g1"] += "--x--\r\n"
	rows := attachmentList(t, e, "1")
	if len(rows) != 3 {
		t.Fatal(rows)
	}
	seen := map[string]bool{}
	args := []string{"attachments", "extract", "1", "--out-dir", attachmentPrivateDir(t), "--execute", "--json"}
	for _, row := range rows {
		if seen[row["output_name"].(string)] {
			t.Fatal("duplicate output")
		}
		seen[row["output_name"].(string)] = true
		args = append(args, "--part", row["part"].(string))
	}
	if code, out, stderr := e.run(args...); code != 0 || len(attachmentRows(t, out)) != 3 {
		t.Fatalf("batch %d %s %s", code, out, stderr)
	}
	code, out, stderr := e.run("attachments", "list", "1", "--limit", "1")
	if code != 0 || strings.Count(out, "\n") != 2 || !strings.Contains(out, "provider_id\taccount_id\tn\tpart\t") || !strings.Contains(stderr, "full result:") || !strings.Contains(stderr, "rows=3") {
		t.Fatalf("table %d %s %s", code, out, stderr)
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.OpenAccount(context.Background(), e.db, cfg.Account())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, err := s.GetMessage(context.Background(), mailbox.LocalRef{AccountID: cfg.Account().ID, N: 1})
	if err != nil {
		t.Fatal(err)
	}
	vs, err := s.Verdicts(context.Background(), m.Local)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 || m.State.Metadata.IsRead || m.Body != nil || m.State.Metadata.HasAttachments {
		t.Fatalf("content extraction mutated local state: %+v %v", m, vs)
	}
}

func TestAttachmentsContentErrorsDoNotLeakOrPublish(t *testing.T) {
	for _, fault := range []string{"malformed", "transport", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			e, _ := attachmentFixture(t)
			row := attachmentList(t, e, "1")[0]
			root := attachmentPrivateDir(t)
			switch fault {
			case "malformed":
				e.graph.mime["g1"] = "From: a@example.org\r\nContent-Transfer-Encoding: base64\r\n\r\n!!!!"
			case "transport":
				e.graph.rawErr = map[string]error{"g1": errors.New("private-payload-canary\x1b[2J")}
			case "cancel":
				e.graph.rawErr = map[string]error{"g1": context.Canceled}
			}
			code, out, stderr := e.run("attachments", "extract", "1", "--part", row["part"].(string), "--out-dir", root, "--execute", "--json")
			if code != 1 || strings.Contains(out+stderr, "private-payload-canary") || strings.Contains(out+stderr, "\x1b") || strings.Contains(out, `"status":"saved"`) {
				t.Fatalf("error %d %s %s", code, out, stderr)
			}
			want := map[string]string{"malformed": "incomplete_content", "transport": "incomplete_content", "cancel": "cancelled"}[fault]
			if attachmentRows(t, out)[0]["error_code"] != want {
				t.Fatal(out)
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 0 {
				t.Fatal("failed content published")
			}
		})
	}
}

func attachmentRows(t *testing.T, out string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("JSON %q: %v", out, err)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestAttachmentsListAndSelectedExtraction(t *testing.T) {
	e, payload := attachmentFixture(t)
	code, out, stderr := e.run("attachments", "list", "1", "--json")
	if code != 0 {
		t.Fatalf("list: %d %s %s", code, out, stderr)
	}
	rows := attachmentRows(t, out)
	digest := sha256.Sum256(payload)
	if len(rows) != 1 || rows[0]["kind"] != "inline_image" || rows[0]["sha256"] != hex.EncodeToString(digest[:]) {
		t.Fatalf("rows: %v", rows)
	}
	part := rows[0]["part"].(string)
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	args := []string{"attachments", "extract", "1", "--part", part, "--out-dir", root, "--json"}
	if code, out, stderr = e.run(args...); code != 0 || !strings.Contains(out, `"status":"ready"`) {
		t.Fatalf("dry: %d %s %s", code, out, stderr)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("dry wrote outputs")
	}
	if code, out, stderr = e.run(append(args, "--execute")...); code != 0 {
		t.Fatalf("extract: %d %s %s", code, out, stderr)
	}
	row := attachmentRows(t, out)[0]
	b, err := os.ReadFile(row["saved_path"].(string))
	if err != nil || sha256.Sum256(b) != digest {
		t.Fatalf("saved bytes %v", err)
	}
	info, err := os.Stat(filepath.Join(root, rows[0]["output_name"].(string)))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v %v", info, err)
	}
	if code, out, stderr = e.run(append(args, "--execute")...); code != 1 || !strings.Contains(out, `"error_code":"collision"`) {
		t.Fatalf("collision: %d %s %s", code, out, stderr)
	}
	if len(e.rendered) != 0 || len(e.graph.moved) != 0 || e.graph.bodyCalls.Load() != 0 {
		t.Fatal("unexpected non-content effects")
	}
	if code, out, stderr = e.run("verdicts", "1"); strings.Contains(out, "saved") {
		t.Fatalf("extraction filed message: %d %s %s", code, out, stderr)
	}
}
