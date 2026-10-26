package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mrothroc/agentio"
	"github.com/mrothroc/mailpipe/internal/mailpipe/cli"
	"github.com/mrothroc/mailpipe/pkg/graph"
	"github.com/mrothroc/mailpipe/pkg/keychain"
	"github.com/mrothroc/mailpipe/pkg/msauth"
	"github.com/mrothroc/mailpipe/pkg/store"
)

func TestMain(m *testing.M) {
	// The default overflow directory is real private data (~/mail/out); no test may reach it.
	dir, err := os.MkdirTemp("", "mailpipe-cli-overflow-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("MAILPIPE_OUT_DIR", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// commandCtx marks the context a test runs a command with, so collaborators
// can check they were handed it (and so would see Ctrl-C).
type commandCtx struct{}

type fakeAuth struct {
	signedIn  bool
	loggedOut bool
	err       error // returned by Token, Status and Login when set
}

func (f *fakeAuth) Token(context.Context) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if !f.signedIn {
		return "", msauth.ErrNotSignedIn
	}
	return "TOKEN", nil
}

func (f *fakeAuth) Status(context.Context) (msauth.Status, error) {
	if f.err != nil {
		return msauth.Status{}, f.err
	}
	if !f.signedIn {
		return msauth.Status{}, msauth.ErrNotSignedIn
	}
	return msauth.Status{Account: "alex@example.com", Scopes: []string{"Mail.ReadWrite", "openid"}}, nil
}

func (f *fakeAuth) Login(_ context.Context, notify func(string)) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	notify("To sign in, enter ABC123 at https://microsoft.com/devicelogin")
	return "alex@example.com", nil
}

func (f *fakeAuth) Logout(context.Context) error { f.loggedOut = true; return nil }

type fakeGraph struct {
	pagesErr  error
	pages     map[string][]graph.Page
	totals    map[string]int
	body      string
	bodyCalls atomic.Int32
	getErr    error
	// server is the mailbox as Graph sees it now, for lookups and moves:
	// graph id -> its folder, Message-ID and sender.
	server     map[string]serverMsg
	moveFail   int   // status every move answers with, when set
	batchErr   error // every $batch call fails as a whole, when set
	lookupFail int   // status every lookup answers with, when set
	moved      []string
	// rules are the server's Inbox rules, as Graph returns them.
	rules     []map[string]any
	ruleSeq   int
	ruleFail  string // "METHOD" every rule write of that method fails with, when set
	searchErr int    // status every sender search answers with, when set
	// mime is each message's MIME source by graph id, for Raw; rawErr fails
	// the ids it names.
	mime     map[string]string
	rawErr   map[string]error
	rawCalls atomic.Int32
}

func (g *fakeGraph) Raw(_ context.Context, path string) ([]byte, error) {
	g.rawCalls.Add(1)
	id, _ := strings.CutSuffix(strings.TrimPrefix(path, "/me/messages/"), "/$value")
	if err := g.rawErr[id]; err != nil {
		return nil, err
	}
	m, ok := g.mime[id]
	if !ok {
		return nil, &graph.APIError{Status: 404, Code: "ErrorItemNotFound", Message: "not found"}
	}
	return []byte(m), nil
}

type serverMsg struct{ folder, imid, sender string }

func (g *fakeGraph) Pages(_ context.Context, start string, _ url.Values, fn func(graph.Page) error) error {
	if g.pagesErr != nil {
		return g.pagesErr
	}
	for _, p := range g.pages[start] {
		if err := fn(p); err != nil {
			return err
		}
	}
	return nil
}

func (g *fakeGraph) FolderTotal(_ context.Context, folder string) (int, error) {
	return g.totals[folder], nil
}

func (g *fakeGraph) Get(_ context.Context, path string, _ url.Values, out any) error {
	if g.getErr != nil {
		return g.getErr
	}
	if folder, ok := strings.CutPrefix(path, "/me/mailFolders/"); ok {
		return json.Unmarshal([]byte(`{"id":"F-`+folder+`"}`), out)
	}
	g.bodyCalls.Add(1)
	data, _ := json.Marshal(map[string]any{"body": map[string]any{"contentType": "text", "content": g.body}})
	return json.Unmarshal(data, out)
}

const rulesPath = "/me/mailFolders/inbox/messageRules"

func (g *fakeGraph) Do(_ context.Context, method, path string, _ url.Values, _ string, body, out any) error {
	if g.getErr != nil {
		return g.getErr
	}
	if method != http.MethodGet && method == g.ruleFail {
		return &graph.APIError{Status: 500, Code: "ErrorInternalServerError", Message: "rule write failed"}
	}
	var in map[string]any
	if body != nil {
		b, _ := json.Marshal(body)
		_ = json.Unmarshal(b, &in)
	}
	id, _ := strings.CutPrefix(path, rulesPath+"/")
	switch {
	case method == http.MethodGet && path == "/me/mailFolders/deleteditems":
		return json.Unmarshal([]byte(`{"id":"F-deleteditems"}`), out)
	case method == http.MethodGet && path == "/me/mailFolders/junkemail":
		return json.Unmarshal([]byte(`{"id":"F-junkemail"}`), out)
	case method == http.MethodGet && path == rulesPath:
		b, _ := json.Marshal(map[string]any{"value": g.rules})
		return json.Unmarshal(b, out)
	case method == http.MethodPost && path == rulesPath:
		g.ruleSeq++
		in["id"] = fmt.Sprintf("R%d", g.ruleSeq)
		g.rules = append(g.rules, in)
		return nil
	case method == http.MethodPatch:
		for _, r := range g.rules {
			if r["id"] == id {
				for k, v := range in {
					r[k] = v
				}
				return nil
			}
		}
	case method == http.MethodDelete:
		for i, r := range g.rules {
			if r["id"] == id {
				g.rules = append(g.rules[:i], g.rules[i+1:]...)
				return nil
			}
		}
	}
	return &graph.APIError{Status: 404, Code: "ErrorItemNotFound", Message: method + " " + path}
}

// search answers a sender search with every server message from that sender.
func (g *fakeGraph) search(rawURL string) graph.BatchResponse {
	if g.searchErr != 0 {
		return graph.BatchResponse{Status: g.searchErr}
	}
	u, _ := url.Parse(rawURL)
	f := u.Query().Get("$filter")
	sender, _ := strings.CutPrefix(f, "from/emailAddress/address eq '")
	sender = strings.ReplaceAll(strings.TrimSuffix(sender, "'"), "''", "'")
	var value []map[string]string
	for _, m := range g.server {
		if strings.EqualFold(m.sender, sender) {
			value = append(value, map[string]string{"parentFolderId": "F-" + m.folder})
		}
	}
	b, _ := json.Marshal(map[string]any{"value": value})
	return graph.BatchResponse{Status: 200, Body: b}
}

func (g *fakeGraph) Batch(_ context.Context, reqs []graph.BatchRequest) ([]graph.BatchResponse, error) {
	if g.batchErr != nil {
		return nil, g.batchErr
	}
	out := make([]graph.BatchResponse, len(reqs))
	for i, r := range reqs {
		if strings.HasPrefix(r.URL, "/me/messages?") {
			out[i] = g.search(r.URL)
			out[i].ID = r.ID
			continue
		}
		out[i].ID = r.ID
		rest, _ := strings.CutPrefix(r.URL, "/me/messages/")
		id, move := strings.CutSuffix(rest, "/move")
		id, _, _ = strings.Cut(id, "?")
		m, ok := g.server[id]
		switch {
		case !ok:
			out[i].Status = 404
		case !move && g.lookupFail != 0:
			out[i].Status = g.lookupFail
		case move && g.moveFail != 0:
			out[i].Status = g.moveFail
		case move:
			dest := r.Body.(map[string]string)["destinationId"]
			g.totals[m.folder]--
			g.totals[dest]++
			m.folder = dest
			g.server[id] = m
			g.moved = append(g.moved, id)
			out[i].Status = 201
		default:
			out[i].Status = 200
			out[i].Body, _ = json.Marshal(map[string]any{"internetMessageId": m.imid, "parentFolderId": "F-" + m.folder,
				"from": map[string]any{"emailAddress": map[string]any{"address": m.sender}}})
		}
	}
	return out, nil
}

func msg(id, sender, subject, received string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"id": id, "internetMessageId": "<" + id + "@example.org>", "subject": subject,
		"from":             map[string]any{"emailAddress": map[string]any{"address": sender, "name": "A Sender"}},
		"toRecipients":     []any{map[string]any{"emailAddress": map[string]any{"address": "alex@example.com"}}},
		"receivedDateTime": received})
	return b
}

type env struct {
	t       *testing.T
	auth    *fakeAuth
	graph   *fakeGraph
	db      string
	secrets map[string]string // "service/account" -> value
	// brokenStdout makes every write to stdout fail, as a closed pipe does.
	brokenStdout bool
	rendered     []string // pages passed to Render
}

func newEnv(t *testing.T) *env {
	return &env{t: t, auth: &fakeAuth{signedIn: true}, db: filepath.Join(t.TempDir(), "mail.db"), graph: &fakeGraph{
		pages: map[string][]graph.Page{
			"/me/mailFolders/inbox/messages/delta": {{Value: []json.RawMessage{
				msg("g1", "info@campaign.example.org", "Chip in", "2026-10-20T10:00:00Z"),
				msg("g2", "friend@example.org", "Dinner\tTuesday?", "2026-10-21T09:30:00Z"),
			}, DeltaLink: "d"}},
			"/me/mailFolders/deleteditems/messages/delta": {{DeltaLink: "dd"}},
			"/me/mailFolders/sentitems/messages/delta": {{Value: []json.RawMessage{
				sent("s1", "friend@example.org", "Re: Dinner"),
			}, DeltaLink: "ds"}},
		},
		totals: map[string]int{"inbox": 2, "deleteditems": 0, "sentitems": 1},
		body:   "line\nline\n",
		server: map[string]serverMsg{
			"g1": {"inbox", "<g1@example.org>", "info@campaign.example.org"},
			"g2": {"inbox", "<g2@example.org>", "friend@example.org"},
		},
	}}
}

func (e *env) run(args ...string) (int, string, string) {
	e.t.Helper()
	deps := cli.Deps{
		Auth:  func() (cli.Authenticator, error) { return e.auth, nil },
		Store: store.Open,
		Graph: func(cli.Authenticator) cli.Graph { return e.graph },
		Render: func(_ context.Context, page string) ([]byte, error) {
			e.rendered = append(e.rendered, page)
			// Like Chrome, every render carries its own creation time.
			return []byte(fmt.Sprintf("%%PDF-1.7 rendered\n/CreationDate (D:2026102518%04d+00'00')\n%%%%EOF\n", len(e.rendered))), nil
		},
		Secret: func(ctx context.Context, service, account string) (string, error) {
			if ctx.Value(commandCtx{}) == nil {
				return "", errors.New("secret read without the command's context")
			}
			if v, ok := e.secrets[service+"/"+account]; ok {
				return v, nil
			}
			return "", keychain.ErrNotFound
		},
	}
	root := cli.NewRootCommand(deps)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	if e.brokenStdout {
		root.SetOut(brokenWriter{})
	}
	root.SetErr(&stderr)
	root.SetArgs(append([]string{"--db", e.db}, args...))
	err := root.ExecuteContext(context.WithValue(context.Background(), commandCtx{}, true))
	if err != nil && !agentio.Reported(err) {
		agentio.Fail("mailpipe", &stderr, err)
	}
	return cli.ExitCode(err), stdout.String(), stderr.String()
}

func sent(id, to, subject string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"id": id, "internetMessageId": "<" + id + "@example.com>", "subject": subject,
		"from":             map[string]any{"emailAddress": map[string]any{"address": "alex@example.com"}},
		"toRecipients":     []any{map[string]any{"emailAddress": map[string]any{"address": to}}},
		"receivedDateTime": "2026-10-19T08:00:00Z"})
	return b
}

func firstN(t *testing.T, tsv string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(tsv), "\n")
	if len(lines) < 2 {
		t.Fatalf("no data rows in %q", tsv)
	}
	return strings.SplitN(lines[1], "\t", 2)[0]
}

func TestBareCommandPrintsHelpListingCommands(t *testing.T) {
	code, out, _ := newEnv(t).run()
	if code != 0 || !strings.Contains(out, "auth") || !strings.Contains(out, "sync") || !strings.Contains(out, "Usage:") {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestUnknownCommandListsAvailableOnesAndIsAUsageError(t *testing.T) {
	code, out, errOut := newEnv(t).run("fetch")
	if code != agentio.ExitUsage || out != "" {
		t.Fatalf("code %d out %q", code, out)
	}
	for _, want := range []string{"Available commands:", "sync", `[error] mailpipe: unknown command "fetch". Try: mailpipe --help`} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("stderr %q lacks %q", errOut, want)
		}
	}
}

func TestGroupBareShowsHelpAndUnknownSubcommandIsUsageError(t *testing.T) {
	e := newEnv(t)
	if code, out, _ := e.run("auth"); code != 0 || !strings.Contains(out, "login") || !strings.Contains(out, "status") {
		t.Fatalf("bare auth: %d %q", code, out)
	}
	code, _, errOut := e.run("auth", "refresh")
	if code != agentio.ExitUsage || !strings.Contains(errOut, `unknown command "refresh". Try: mailpipe auth --help`) {
		t.Fatalf("auth refresh: %d %q", code, errOut)
	}
}

func TestAuthStatusSignedInIsTSV(t *testing.T) {
	code, out, errOut := newEnv(t).run("auth", "status")
	if code != 0 || out != "account\tscopes\nalex@example.com\tMail.ReadWrite openid\n" {
		t.Fatalf("code %d out %q", code, out)
	}
	if !strings.Contains(errOut, "[mailpipe auth status | rows=1 | exit=0 |") {
		t.Fatalf("footer missing: %q", errOut)
	}
}

func TestAuthStatusSignedOutIsStaleWithLoginHint(t *testing.T) {
	e := newEnv(t)
	e.auth.signedIn = false
	code, out, errOut := e.run("auth", "status")
	if code != agentio.ExitStale || out != "" || !strings.Contains(errOut, "[error] mailpipe auth status: not signed in. Try: mailpipe auth login") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

func TestAuthLoginInstructionsOnStderrAccountOnStdout(t *testing.T) {
	code, out, errOut := newEnv(t).run("auth", "login")
	if code != 0 || out != "account\nalex@example.com\n" || !strings.Contains(errOut, "ABC123") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

func TestAuthLogout(t *testing.T) {
	e := newEnv(t)
	if code, out, _ := e.run("auth", "logout"); code != 0 || out != "" || !e.auth.loggedOut {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestSyncReportsCountsPerFolder(t *testing.T) {
	code, out, errOut := newEnv(t).run("sync")
	want := "folder\tadded\tupdated\tremoved\tlocal\tgraph\ninbox\t2\t0\t0\t2\t2\ndeleteditems\t0\t0\t0\t0\t0\nsentitems\t1\t0\t0\t1\t1\n"
	if code != 0 || out != want {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

func TestSyncMismatchIsStaleWithRebuildHint(t *testing.T) {
	e := newEnv(t)
	e.graph.totals["inbox"] = 5
	code, out, errOut := e.run("sync", "--folder", "inbox")
	if code != agentio.ExitStale || !strings.Contains(out, "inbox\t2\t0\t0\t2\t5") {
		t.Fatalf("code %d out %q", code, out)
	}
	if !strings.Contains(errOut, "local count differs from Graph for inbox") || !strings.Contains(errOut, "Try: mailpipe sync --full --folder inbox") {
		t.Fatalf("stderr %q", errOut)
	}
}

func TestSyncSignedOutIsStaleWithLoginHint(t *testing.T) {
	e := newEnv(t)
	// The real Graph client fails with the token source's error before any request.
	e.graph.pagesErr = msauth.ErrNotSignedIn
	code, out, errOut := e.run("sync")
	if code != agentio.ExitStale || out != "" || !strings.Contains(errOut, "not signed in. Try: mailpipe auth login") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

func TestLsIsTSVWithHeaderNewestFirst(t *testing.T) {
	e := newEnv(t)
	e.run("sync")
	code, out, _ := e.run("ls")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if code != 0 || len(lines) != 4 || lines[0] != "n\treceived\tfolder\tsender\tsubject" {
		t.Fatalf("code %d out %q", code, out)
	}
	fields := strings.Split(lines[1], "\t")
	if fields[1] != "2026-10-21T09:30:00Z" || fields[3] != "friend@example.org" || fields[4] != "Dinner Tuesday?" {
		t.Fatalf("row %q", lines[1])
	}
}

func TestLsFiltersAndJSON(t *testing.T) {
	e := newEnv(t)
	e.run("sync")
	code, out, _ := e.run("ls", "--from", "CAMPAIGN.example.org", "--json")
	var row map[string]any
	if code != 0 || json.Unmarshal([]byte(strings.TrimSpace(out)), &row) != nil {
		t.Fatalf("code %d out %q", code, out)
	}
	if row["sender"] != "info@campaign.example.org" || row["internet_message_id"] != "<g1@example.org>" || row["graph_id"] != "g1" {
		t.Fatalf("row %v", row)
	}
}

func TestLsLimitSpillsToOverflow(t *testing.T) {
	e := newEnv(t)
	e.run("sync")
	code, out, errOut := e.run("ls", "--limit", "1")
	if code != 0 || strings.Count(out, "\n") != 2 || !strings.Contains(errOut, "3 rows exceed --limit 1; full result:") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

func TestLsOnEmptyStoreIsNoDataWithSyncHint(t *testing.T) {
	code, out, errOut := newEnv(t).run("ls")
	if code != agentio.ExitNoData || out != "" || !strings.Contains(errOut, "no messages stored yet. Try: mailpipe sync") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
}

func TestShowPrintsHeadersAndBodyAndCachesIt(t *testing.T) {
	e := newEnv(t)
	e.run("sync")
	_, ls, _ := e.run("ls", "--from", "campaign.example.org")
	n := firstN(t, ls)
	code, out, _ := e.run("show", n)
	if code != 0 || !strings.HasPrefix(out, "n: "+n+"\nfrom: A Sender <info@campaign.example.org>\n") || !strings.HasSuffix(out, "\n\nline\nline\n") {
		t.Fatalf("code %d out %q", code, out)
	}
	e.run("show", n)
	if e.graph.bodyCalls.Load() != 1 {
		t.Fatalf("body fetched %d times, want 1 (cached)", e.graph.bodyCalls.Load())
	}
}

func TestShowLongBodyOverflowsToAFile(t *testing.T) {
	e := newEnv(t)
	var b strings.Builder
	for i := 0; i < 500; i++ {
		b.WriteString("row\n")
	}
	e.graph.body = b.String()
	e.run("sync")
	_, ls, _ := e.run("ls")
	code, out, errOut := e.run("show", firstN(t, ls), "--limit", "50")
	if code != 0 || strings.Count(out, "\n") != 50 || !strings.Contains(errOut, "full text:") {
		t.Fatalf("code %d lines %d err %q", code, strings.Count(out, "\n"), errOut)
	}
}

func TestShowArgumentErrors(t *testing.T) {
	e := newEnv(t)
	e.run("sync")
	cases := map[string]struct {
		args []string
		code int
		want string
	}{
		"missing": {[]string{"show"}, agentio.ExitUsage, "accepts 1 arg(s), received 0. Try: mailpipe show --help"},
		"not int": {[]string{"show", "abc"}, agentio.ExitUsage, `positive integer, got "abc". Try: mailpipe ls`},
		"unknown": {[]string{"show", "999"}, agentio.ExitRuntime, "no message 999. Try: mailpipe ls"},
	}
	for name, c := range cases {
		code, out, errOut := e.run(c.args...)
		if code != c.code || out != "" || !strings.Contains(errOut, c.want) {
			t.Fatalf("%s: code %d out %q err %q", name, code, out, errOut)
		}
	}
}

func TestGraphAuthFailuresAreStaleWithStatusHint(t *testing.T) {
	e := newEnv(t)
	e.run("sync")
	e.graph.getErr = &graph.APIError{Status: 403, Code: "ErrorAccessDenied", Message: "Access is denied."}
	_, ls, _ := e.run("ls")
	code, _, errOut := e.run("show", firstN(t, ls))
	if code != agentio.ExitStale || !strings.Contains(errOut, "ErrorAccessDenied") || !strings.Contains(errOut, "Try: mailpipe auth status") {
		t.Fatalf("code %d err %q", code, errOut)
	}
}

// --- config -------------------------------------------------------------------------------------

func TestConfigInitThenShow(t *testing.T) {
	e := newEnv(t)
	path := filepath.Join(t.TempDir(), "mail", "config.json")
	t.Setenv("MAILPIPE_CONFIG", path)
	code, out, errOut := e.run("config", "init", "--client-id", "11111111-1111-4111-8111-111111111111",
		"--tenant-id", "22222222-2222-4222-8222-222222222222")
	if code != 0 || out != "path\n"+path+"\n" {
		t.Fatalf("init: code %d out %q err %q", code, out, errOut)
	}
	code, out, _ = e.run("config", "show")
	want := "path\tclient_id\ttenant_id\n" + path + "\t11111111-1111-4111-8111-111111111111\t22222222-2222-4222-8222-222222222222\n"
	if code != 0 || out != want {
		t.Fatalf("show: code %d out %q", code, out)
	}
}

func TestConfigInitValidatesItsFlags(t *testing.T) {
	e := newEnv(t)
	t.Setenv("MAILPIPE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	code, _, errOut := e.run("config", "init", "--client-id", "nope", "--tenant-id", "22222222-2222-4222-8222-222222222222")
	if code != agentio.ExitUsage || !strings.Contains(errOut, "client_id must be a UUID") {
		t.Fatalf("code %d err %q", code, errOut)
	}
	code, _, errOut = e.run("config", "init")
	if code != agentio.ExitUsage || !strings.Contains(errOut, "--client-id and --tenant-id are required") {
		t.Fatalf("code %d err %q", code, errOut)
	}
}

func TestUnconfiguredSignInIsStaleWithInitHint(t *testing.T) {
	t.Setenv("MAILPIPE_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
	t.Setenv("MAILPIPE_CLIENT_ID", "")
	t.Setenv("MAILPIPE_TENANT_ID", "")
	root := cli.NewRootCommand(cli.DefaultDeps())
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"auth", "status"})
	err := root.ExecuteContext(context.Background())
	if code := cli.ExitCode(err); code != agentio.ExitStale || !strings.Contains(stderr.String(), "Try: mailpipe config init --client-id <uuid> --tenant-id <uuid>") {
		t.Fatalf("code %d stderr %q", code, stderr.String())
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
