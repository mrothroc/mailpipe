package cli_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mrothroc/agentio"
)

var attachedPDF = "%PDF-1.4 the invoice\n%%EOF\n"

// savable gives g1 (Chip in) a PDF attachment and g2 (Dinner) none.
func savable(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t)
	e.stagesConfig("http://unused")
	e.run("sync")
	e.graph.mime = map[string]string{
		"g1": "From: info@campaign.example.org\r\nSubject: Chip in\r\nDate: Mon, 20 Oct 2026 10:00:00 +0000\r\n" +
			"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nSee attached.\r\n" +
			"--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=inv.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
			base64.StdEncoding.EncodeToString([]byte(attachedPDF)) + "\r\n--b--\r\n",
		"g2": "From: friend@example.org\r\nSubject: Dinner Tuesday?\r\nDate: Tue, 21 Oct 2026 09:30:00 +0000\r\n" +
			"Content-Type: text/html\r\n\r\n<p>See you at 7</p>\r\n",
	}
	root := filepath.Join(t.TempDir(), "To File")
	return e, root
}

func manifest(t *testing.T, e *env, rows ...string) string {
	t.Helper()
	_, ls, _ := e.run("ls", "--json")
	n := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(ls), "\n") {
		var m struct {
			N       int64  `json:"n"`
			GraphID string `json:"graph_id"`
		}
		_ = jsonUnmarshal(line, &m)
		n[m.GraphID] = itoa64(m.N)
	}
	body := "n\tfolder\tname\n"
	for _, r := range rows {
		for id, num := range n {
			r = strings.ReplaceAll(r, "{"+id+"}", num)
		}
		body += r + "\n"
	}
	return writeFile(t, body)
}

func TestSaveChecksThenSavesAttachmentsAndRendersTheRest(t *testing.T) {
	e, root := savable(t)
	m := manifest(t, e, "{g1}\tArchive/Records/2026\t2026-10-20_Campaign_Invoice.pdf", "{g2}\tArchive/Notes\t2026-10-21_Friend_Note")
	code, out, errOut := e.run("save", "--manifest", m, "--root", root)
	if code != 0 || !strings.Contains(out, "\twould_save\t") || !strings.Contains(errOut, "check only, nothing written: 2 to save") {
		t.Fatalf("check: code %d out %q err %q", code, out, errOut)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("check mode wrote to the root")
	}
	code, out, errOut = e.run("save", "--manifest", m, "--root", root, "--execute")
	if code != 0 || strings.Count(out, "\tsaved\t") != 2 {
		t.Fatalf("execute: code %d out %q err %q", code, out, errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "Archive/Records/2026/2026-10-20_Campaign_Invoice.pdf")); string(b) != attachedPDF {
		t.Fatalf("the attachment was not saved as is: %q", b)
	}
	note := filepath.Join(root, "Archive/Notes/2026-10-21_Friend_Note.pdf")
	if b, _ := os.ReadFile(note); !strings.HasPrefix(string(b), "%PDF-") || len(e.rendered) != 1 || !strings.Contains(e.rendered[0], "See you at 7") {
		t.Fatalf("rendered %q from pages %v", b, e.rendered)
	}
	if fi, _ := os.Stat(note); fi.Mode().Perm() != 0o600 {
		t.Fatalf("saved file mode %v", fi.Mode().Perm())
	}
	_, vs, _ := e.run("verdicts", firstN(t, out))
	if !strings.Contains(vs, "filing\t") || !strings.Contains(vs, "\tsaved\t") {
		t.Fatalf("no filing:saved verdict: %q", vs)
	}
	// Saving again finds both saved where the manifest says, fetches
	// nothing and leaves them saved.
	calls := e.graph.rawCalls.Load()
	code, out, _ = e.run("save", "--manifest", m, "--root", root, "--execute")
	if code != 0 || strings.Count(out, "\texists\t") != 2 || e.graph.rawCalls.Load() != calls {
		t.Fatalf("rerun: code %d out %q, %d fetches", code, out, e.graph.rawCalls.Load()-calls)
	}
	if _, vs, _ := e.run("verdicts", firstN(t, out)); strings.Contains(vs[strings.LastIndex(vs, "filing\t"):], "\tpending\t") {
		t.Fatalf("a rerun put a saved message back to pending: %q", vs)
	}
}

func TestSaveNeverOverwritesADifferentFile(t *testing.T) {
	e, root := savable(t)
	m := manifest(t, e, "{g2}\tArchive/Notes\tnote.pdf")
	dest := filepath.Join(root, "Archive/Notes/note.pdf")
	_ = os.MkdirAll(filepath.Dir(dest), 0o700)
	_ = os.WriteFile(dest, []byte("the owner's own file"), 0o600)
	code, out, errOut := e.run("save", "--manifest", m, "--root", root, "--execute")
	if code != agentio.ExitRuntime || !strings.Contains(out, "\tfailed\t") || !strings.Contains(out, "already exists") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	if b, _ := os.ReadFile(dest); string(b) != "the owner's own file" {
		t.Fatalf("overwritten: %q", b)
	}
}

func TestAFailedSaveLeavesTheMessagePending(t *testing.T) {
	e, root := savable(t)
	e.graph.rawErr = map[string]error{"g2": errors.New("graph 503")}
	m := manifest(t, e, "{g1}\tA\ta.pdf", "{g2}\tB\tb.pdf")
	code, out, _ := e.run("save", "--manifest", m, "--root", root, "--execute")
	if code != agentio.ExitRuntime || strings.Count(out, "\tsaved\t") != 1 || strings.Count(out, "\tfailed\t") != 1 {
		t.Fatalf("code %d out %q", code, out)
	}
	var pending string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "\tfailed\t") {
			pending = strings.Split(line, "\t")[0]
		}
	}
	if _, vs, _ := e.run("verdicts", pending); !strings.Contains(vs, "\tpending\t") {
		t.Fatalf("a failed save is not pending: %q", vs)
	}
}

func TestSaveRejectsABadManifestBeforeWritingAnything(t *testing.T) {
	e, root := savable(t)
	for name, row := range map[string]string{
		"escapes the root": "{g1}\t../outside\ta.pdf",
		"absolute folder":  "{g1}\t/etc\ta.pdf",
		"name with slash":  "{g1}\tA\tsub/a.pdf",
		"name with colon":  "{g1}\tA\tre: a.pdf",
		"name with ;":      "{g1}\tA\ta; b.pdf",
		"unknown message":  "99999\tA\ta.pdf",
		"missing column":   "{g1}\tA",
	} {
		m := manifest(t, e, "{g2}\tOK\tok.pdf", row)
		code, _, errOut := e.run("save", "--manifest", m, "--root", root, "--execute")
		if code != agentio.ExitUsage {
			t.Errorf("%s: code %d err %q", name, code, errOut)
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("%s: something was written", name)
		}
	}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// show --headers prints the message's own header block, fetched from the
// server: Authentication-Results and Received tell real mail from spoofs.
func TestShowHeadersPrintsTheServersHeaderBlock(t *testing.T) {
	e, _ := savable(t)
	e.graph.mime["g2"] = "Authentication-Results: spf=pass smtp.mailfrom=example.org; dkim=pass header.d=example.org\r\n" +
		"From: friend@example.org\r\nSubject: Dinner Tuesday?\r\n\r\n<p>See you at 7</p>\r\n"
	_, ls, _ := e.run("ls", "--from", "friend")
	code, out, errOut := e.run("show", firstN(t, ls), "--headers")
	if code != 0 || out != "Authentication-Results: spf=pass smtp.mailfrom=example.org; dkim=pass header.d=example.org\nFrom: friend@example.org\nSubject: Dinner Tuesday?\n" {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

// Several PDF attachments become distinct files, even when they share a
// name or have none.
func TestSaveGivesEveryAttachmentItsOwnFile(t *testing.T) {
	e, root := savable(t)
	part := func(name string, body string) string {
		disp := "attachment"
		if name != "" {
			disp += "; filename=" + name
		}
		return "--b\r\nContent-Type: application/pdf\r\nContent-Disposition: " + disp + "\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
			base64.StdEncoding.EncodeToString([]byte(body)) + "\r\n"
	}
	e.graph.mime["g1"] = "From: info@campaign.example.org\r\nSubject: Chip in\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		part("inv.pdf", "%PDF-1 one\n%%EOF\n") + part("inv.pdf", "%PDF-1 two\n%%EOF\n") + part("", "%PDF-1 three\n%%EOF\n") +
		part("..pdf", "%PDF-1 four\n%%EOF\n") + "--b--\r\n"
	m := manifest(t, e, "{g1}\tA\tdoc.pdf")
	code, out, errOut := e.run("save", "--manifest", m, "--root", root, "--execute")
	if code != 0 || !strings.Contains(out, "A/doc - inv.pdf; A/doc - inv (2).pdf; A/doc - attachment 3.pdf; A/doc - attachment 4.pdf") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

func TestSaveRejectsDuplicateRows(t *testing.T) {
	e, root := savable(t)
	for name, rows := range map[string][]string{
		"same message": {"{g1}\tA\ta.pdf", "{g1}\tB\tb.pdf"},
		"same file":    {"{g1}\tA\ta.pdf", "{g2}\tA/\ta"},
	} {
		code, _, errOut := e.run("save", "--manifest", manifest(t, e, rows...), "--root", root, "--execute")
		if code != agentio.ExitUsage {
			t.Errorf("%s: code %d err %q", name, code, errOut)
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("something was written")
	}
}

// A rendered file moved to where an edited manifest now points counts as
// saved: renders differ only in their creation time.
func TestARenderedFileAtANewPathCountsAsSaved(t *testing.T) {
	e, root := savable(t)
	e.run("save", "--manifest", manifest(t, e, "{g2}\tOld\tnote.pdf"), "--root", root, "--execute")
	_ = os.MkdirAll(filepath.Join(root, "New"), 0o700)
	if err := os.Rename(filepath.Join(root, "Old/note.pdf"), filepath.Join(root, "New/note.pdf")); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := e.run("save", "--manifest", manifest(t, e, "{g2}\tNew\tnote.pdf"), "--root", root, "--execute")
	if code != 0 || !strings.Contains(out, "\texists\tNew/note.pdf\t") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}
