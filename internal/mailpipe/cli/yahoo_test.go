package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrothroc/mailpipe/internal/mailpipe/cli"
	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
	"github.com/mrothroc/mailpipe/pkg/imapauth"
	"github.com/mrothroc/mailpipe/pkg/imapmail"
	"github.com/mrothroc/mailpipe/pkg/keychain"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/store"
)

type setupRunner struct {
	mu      sync.Mutex
	items   map[string]string
	mode    string
	calls   int
	writes  int
	maxLine int
}

func (r *setupRunner) Run(ctx context.Context, in []byte, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if name != "security" {
		return nil, errors.New("unexpected tool")
	}
	for _, a := range args {
		if strings.Contains(a, "credential-canary") || strings.Contains(a, hex.EncodeToString([]byte("credential-canary"))) {
			return nil, errors.New("credential in argv")
		}
	}
	missing := func() error { return exec.Command("sh", "-c", "exit 44").Run() }
	if len(in) > 0 {
		r.writes++
		r.maxLine = max(r.maxLine, len(in))
		f := strings.Fields(string(in))
		if len(f) != 8 {
			return nil, errors.New("invalid stdin command")
		}
		v, e := hex.DecodeString(f[7])
		if e != nil {
			return nil, errors.New("invalid stdin encoding")
		}
		k := f[3] + "/" + f[5]
		switch r.mode {
		case "write":
			return nil, errors.New("credential-canary rejected")
		case "partial":
			r.items[k] = string(v[:len(v)/2])
			return nil, io.ErrShortWrite
		case "complete-error":
			r.items[k] = string(v)
			return nil, errors.New("credential-canary acknowledgement lost")
		}
		r.items[k] = string(v)
		if r.mode == "readback" {
			r.mode = "read"
		}
		return nil, nil
	}
	if len(args) < 5 {
		return nil, errors.New("invalid read")
	}
	k := args[2] + "/" + args[4]
	if args[0] == "delete-generic-password" {
		delete(r.items, k)
		return nil, nil
	}
	if r.mode == "read" {
		return nil, errors.New("credential-canary read failed")
	}
	v, ok := r.items[k]
	if !ok {
		return nil, missing()
	}
	return []byte(v + "\n"), nil
}

type yahooEnv struct {
	t                              *testing.T
	dir, db                        string
	deps                           cli.Deps
	runner                         *setupRunner
	server                         *scriptServer
	inputErr                       error
	input                          []byte
	inputs, models, clients, opens int
}

func newYahooEnv(t *testing.T) *yahooEnv {
	t.Helper()
	srv, ic := localScript(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("MAILPIPE_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("MAILPIPE_DB", filepath.Join(dir, "mail.db"))
	t.Setenv("MAILPIPE_STAGES", filepath.Join(dir, "stages.json"))
	t.Setenv("MAILPIPE_TEMPLATES", filepath.Join(dir, "templates.json"))
	e := &yahooEnv{t: t, dir: dir, db: filepath.Join(dir, "mail.db"), runner: &setupRunner{items: map[string]string{}}, server: srv}
	e.deps = cli.Deps{Store: func(path string) (*store.Store, error) { e.opens++; return store.Open(path) }, AccountStore: func(ctx context.Context, path string, a mailbox.Account) (*store.AccountStore, error) {
		e.opens++
		return store.OpenAccount(ctx, path, a)
	},
		Credentials: func() (imapauth.Store, error) {
			return imapauth.NewKeychain(keychain.New(imapauth.Service, e.runner).WithLock(filepath.Join(dir, "credential.lock"))), nil
		},
		CredentialInput: func(ctx context.Context, _ io.Reader, _ io.Writer, _ bool) ([]byte, error) {
			e.inputs++
			if e.inputErr != nil {
				return nil, e.inputErr
			}
			if e.input != nil {
				return append([]byte(nil), e.input...), ctx.Err()
			}
			return []byte("credential-canary"), ctx.Err()
		},
		NewIMAP: func(c imapmail.Config, f imapmail.CredentialFunc) (*imapmail.Client, error) {
			e.clients++
			c.Roots = ic.Roots
			return imapmail.New(c, f)
		},
		Secret: func(context.Context, string, string) (string, error) { e.models++; return "invented-model-key", nil },
		Auth:   func() (cli.Authenticator, error) { t.Fatal("Yahoo selected Graph auth"); return nil, nil },
		Graph:  func(cli.Authenticator) cli.Graph { t.Fatal("Yahoo selected Graph"); return nil },
		Render: func(context.Context, string) ([]byte, error) { t.Fatal("Yahoo rendered document"); return nil, nil },
	}
	if code, _, _ := e.run("config", "init", "--provider", "yahoo", "--endpoint", ic.Address, "--login", " Owner@Example.org "); code != 0 {
		t.Fatal("Yahoo setup failed")
	}
	return e
}
func (e *yahooEnv) run(args ...string) (int, string, string) {
	return e.runContext(context.Background(), args...)
}
func (e *yahooEnv) runContext(ctx context.Context, args ...string) (int, string, string) {
	e.t.Helper()
	e.t.Setenv("HOME", e.dir)
	e.t.Setenv("MAILPIPE_CONFIG", filepath.Join(e.dir, "config.json"))
	e.t.Setenv("MAILPIPE_STAGES", filepath.Join(e.dir, "stages.json"))
	e.t.Setenv("MAILPIPE_TEMPLATES", filepath.Join(e.dir, "templates.json"))
	r := cli.NewRootCommand(e.deps)
	var out, err bytes.Buffer
	r.SetOut(&out)
	r.SetErr(&err)
	r.SetArgs(append([]string{"--db", e.db}, args...))
	failure := r.ExecuteContext(ctx)
	if strings.Contains(out.String()+err.String(), "credential-canary") {
		e.t.Fatal("credential leaked")
	}
	return cli.ExitCode(failure), out.String(), err.String()
}
func (e *yahooEnv) ok(args ...string) string {
	e.t.Helper()
	code, out, err := e.run(args...)
	if code != 0 {
		e.t.Fatalf("%v: exit %d, %s", args, code, err)
	}
	return out
}

func TestYahooReadOnlyPilot(t *testing.T) {
	e := newYahooEnv(t)
	e.ok("account", "show")
	e.ok("account", "capabilities")
	e.ok("auth", "login")
	e.ok("auth", "status")
	e.ok("sync", "--folder", "inbox")
	e.ok("ls")
	e.ok("show", "1")
	e.ok("show", "1", "--headers")
	e.ok("sync", "--folder", "inbox")
	os.WriteFile(filepath.Join(e.dir, "stages.json"), []byte(`{"stages":[{"name":"local","type":"honeypot"}],"policy":{}}`), 0600)
	e.ok("run", "local")
	e.ok("digest")
	e.ok("plan", "create")
	e.ok("plan", "show", "1")
	cfg, _ := config.Load(config.DefaultPath())
	db, err := store.OpenAccount(context.Background(), e.db, cfg.Account())
	if err != nil {
		t.Fatal(err)
	}
	before, _ := db.Plans(context.Background())
	db.Close()
	for _, args := range [][]string{{"apply", "1", "--execute"}, {"rules", "apply", "--execute"}, {"save", "--manifest", filepath.Join(e.dir, "missing.tsv"), "--root", filepath.Join(e.dir, "forbidden"), "--execute"}} {
		calls, clients, opens := e.runner.calls, e.clients, e.opens
		if c, _, _ := e.run(args...); c == 0 {
			t.Fatal("execute permitted")
		}
		if e.runner.calls != calls || e.clients != clients || e.opens != opens {
			t.Fatal("execute touched store/credentials/provider")
		}
	}
	if _, err = os.Stat(filepath.Join(e.dir, "forbidden")); !os.IsNotExist(err) {
		t.Fatal("execute touched destination")
	}
	db, err = store.OpenAccount(context.Background(), e.db, cfg.Account())
	if err != nil {
		t.Fatal(err)
	}
	after, _ := db.Plans(context.Background())
	db.Close()
	if len(before) != len(after) {
		t.Fatal("execute changed plans")
	}
	// Reopened command trees preserve binding; an independently valid new principal cannot adopt the DB.
	old := cfg
	cfg.IMAP = &config.IMAPConfig{IdentityVersion: 1, Endpoint: old.IMAP.Endpoint, Login: "different@example.org"}
	p, _ := cfg.Principal()
	cfg.AccountID = string(p.Account().ID)
	config.Save(config.DefaultPath(), cfg)
	clients := e.clients
	if c, _, _ := e.run("sync", "--folder", "inbox"); c == 0 {
		t.Fatal("changed login reused DB")
	}
	if e.clients != clients {
		t.Fatal("mismatch contacted provider")
	}
	config.Save(config.DefaultPath(), old)
	e.ok("ls")
	if e.models != 0 {
		t.Fatal("default setup transferred model data")
	}
}

func TestYahooDefaultRolesAndFullPass(t *testing.T) {
	e := newYahooEnv(t)
	e.ok("auth", "login")
	out := e.ok("sync", "--json")
	if strings.Count(out, "\"remote\":2") != 4 {
		t.Fatal("default sync did not run all proven roles")
	}
	c, _ := config.Load(config.DefaultPath())
	for _, alias := range []string{"inbox", "sentitems", "deleteditems", "junkemail"} {
		if c.Collections[alias] == "" {
			t.Fatal("proven role routing missing")
		}
	}
	before := e.ok("ls", "--json")
	e.ok("sync", "--full")
	after := e.ok("ls", "--json")
	if before != after {
		t.Fatal("full repeat renumbered or changed complete metadata")
	}
	e.ok("show", "1", "--headers")
	e.server.mu.Lock()
	defer e.server.mu.Unlock()
	if e.server.sourceFetches == 0 || e.server.metadataFetches == 0 {
		t.Fatal("real metadata/content protocol was not exercised")
	}
}

func TestProviderCredentialIsolation(t *testing.T) {
	e := newYahooEnv(t)
	e.runner.items["mailpipe/token-cache"] = "invented-existing-cache"
	e.runner.items["mailpipe-openrouter/default"] = "invented-existing-model-item"
	e.ok("auth", "login")
	cfg, _ := config.Load(config.DefaultPath())
	p, _ := cfg.Principal()
	v, ok := e.runner.items[imapauth.Service+"/"+p.Item()]
	if !ok || v == "" {
		t.Fatal("missing exact tuple item")
	}
	other := &setupRunner{items: map[string]string{}}
	k := keychain.New(imapauth.Service, other)
	if _, err := k.Get(context.Background(), p.Item()); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatal("fake OS user crossed isolation")
	}
	if e.runner.maxLine >= 4000 {
		t.Fatal("stdin command exceeded single-item bound")
	}
	b, _ := os.ReadFile(config.DefaultPath())
	if bytes.Contains(b, []byte("credential-canary")) {
		t.Fatal("credential persisted in config")
	}
	// Two homes model two OS-user Keychain namespaces through independent
	// runners. HOME text alone is deliberately not treated as OS isolation.
	otherHome := newYahooEnv(t)
	same := cfg
	same.Credential = nil
	if err := config.Save(filepath.Join(otherHome.dir, "config.json"), same); err != nil {
		t.Fatal(err)
	}
	otherHome.deps.NewIMAP = e.deps.NewIMAP // same verified endpoint/CA and tuple
	if code, _, _ := otherHome.run("auth", "status"); code == 0 {
		t.Fatal("second home adopted first user's credential")
	}
	otherHome.ok("auth", "login")
	if otherHome.runner.items[imapauth.Service+"/"+p.Item()] == v {
		t.Fatal("independent user generation not established")
	}
	otherHome.ok("auth", "logout")
	e.ok("auth", "status")
	if e.runner.items[imapauth.Service+"/"+p.Item()] != v {
		t.Fatal("second user changed first user's item")
	}
	if e.runner.items["mailpipe/token-cache"] != "invented-existing-cache" || e.runner.items["mailpipe-openrouter/default"] != "invented-existing-model-item" {
		t.Fatal("Graph/OpenRouter items changed")
	}
	e.ok("auth", "logout")
	if c, _, _ := e.run("auth", "status"); c == 0 {
		t.Fatal("signed out status succeeded")
	}
}

func TestYahooLazyCleanupAndOfflineReads(t *testing.T) {
	e := newYahooEnv(t)
	e.ok("auth", "login")
	e.ok("sync", "--folder", "inbox")
	spools := t.TempDir()
	t.Setenv("TMPDIR", spools)
	e.ok("show", "1") // no explicit Scope: Client owns and closes its fallback
	if entries, err := os.ReadDir(spools); err != nil || len(entries) != 0 {
		t.Fatal("fallback scope leaked")
	}
	// An active source read must cancel, join protocol work and close the scope.
	e.server.mu.Lock()
	e.server.stall = make(chan struct{}, 1)
	e.server.stallSource = true
	stall := e.server.stall
	e.server.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { code, _, _ := e.runContext(ctx, "show", "2"); done <- code }()
	select {
	case <-stall:
		cancel()
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("source read never reached local protocol control")
	}
	select {
	case code := <-done:
		if code == 0 {
			t.Fatal("canceled source succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled command did not join")
	}
	if entries, err := os.ReadDir(spools); err != nil || len(entries) != 0 {
		t.Fatal("canceled source scope leaked")
	}
	e.ok("auth", "logout")
	clients, inputs, calls := e.clients, e.inputs, e.runner.calls
	e.ok("show", "1")
	e.ok("ls")
	if e.clients != clients || e.inputs != inputs || e.runner.calls != calls {
		t.Fatal("cached reads initialized credentials/client")
	}
	for _, args := range [][]string{{"--help"}, {"auth", "--help"}, {"config", "init", "--help"}, {"sync", "--help"}, {"run", "--help"}} {
		e.ok(args...)
	}
	if e.clients != clients || e.inputs != inputs || e.runner.calls != calls {
		t.Fatal("help initialized credentials/client")
	}
}

func TestProviderCredentialInputFailures(t *testing.T) {
	for _, mode := range []string{"write", "partial", "complete-error", "readback", "read", "input"} {
		t.Run(mode, func(t *testing.T) {
			e := newYahooEnv(t)
			e.runner.mode = mode
			if mode == "input" {
				e.inputErr = io.EOF
			}
			if c, _, _ := e.run("auth", "login"); c == 0 {
				t.Fatal("failed credential input/persistence succeeded")
			}
			if c, _, _ := e.run("auth", "status"); c == 0 {
				t.Fatal("failed persistence reported stored")
			}
			e.runner.mode = ""
			e.inputErr = nil
			e.ok("auth", "login")
			e.ok("auth", "status")
		})
	}
}

func TestYahooProcessingConsent(t *testing.T) {
	e := newYahooEnv(t)
	e.ok("auth", "login")
	e.ok("sync", "--folder", "inbox")
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var in struct {
			State     string
			Questions map[string]struct{ Criteria map[string]string }
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			t.Error("invalid model request")
		}
		if !strings.Contains(in.State, "Complete tail 2099-12-31") {
			t.Error("model input incomplete")
		}
		if q, ok := in.Questions["template"]; ok {
			probs := map[string]float64{}
			for k := range q.Criteria {
				probs[k] = 0
			}
			probs["none"] = 1
			json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"template": map[string]any{"type": "choice", "choice": "none", "probabilities": probs}}, "usage": map[string]any{"cost": 0.001}})
		} else {
			json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"check": map[string]any{"noul": 0.9}}, "usage": map[string]any{"cost": 0.001}})
		}
	}))
	defer srv.Close()
	marker := filepath.Join(e.dir, "child")
	sourceMarker := filepath.Join(e.dir, "source-path")
	program := filepath.Join(e.dir, "classifier.sh")
	script := `#!/bin/sh
input=$(cat)
printf '%s' "$input" > "$1"
path=$(printf '%s' "$input" | sed -n 's/.*"source_path":"\([^"]*\)".*/\1/p')
if test -n "$path"; then
 test -f "$path" || exit 3
 grep -q 'Complete tail 2099-12-31' "$path" || exit 4
 printf '%s' "$path" > "$2"
fi
printf '{"labels":["review"]}'
`
	os.WriteFile(program, []byte(script), 0700)
	defs := []map[string]any{
		{"name": "model", "type": "jev", "url": srv.URL, "question": map[string]string{"key": "check", "instructions": "Check complete message", "criteria_true": "yes", "criteria_false": "no"}},
		{"name": "template", "type": "template", "url": srv.URL},
		{"name": "external", "type": "exec", "command": []string{program, marker, sourceMarker}},
		{"name": "hook", "type": "hook", "command": []string{program, marker, sourceMarker}, "needs_body": true, "needs_source": true},
	}
	b, _ := json.Marshal(map[string]any{"stages": defs, "policy": map[string]any{}})
	os.WriteFile(filepath.Join(e.dir, "stages.json"), b, 0600)
	os.WriteFile(filepath.Join(e.dir, "templates.json"), []byte(`{"templates":[{"id":"note","senders":["sender@example.org"],"describe":"An invented note","subject":"^invented$","anchors":["invented source"]}]}`), 0600)
	clients := e.clients
	for _, name := range []string{"model", "template", "external", "hook"} {
		if c, _, _ := e.run("run", name, "--max-messages", "1"); c == 0 {
			t.Fatal("external stage admitted without consent")
		}
	}
	if e.models != 0 || requests.Load() != 0 || e.clients != clients {
		t.Fatal("unconsented stage read key/data/provider")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("unconsented child started")
	}
	cfg, _ := config.Load(config.DefaultPath())
	e.ok("config", "processing", "--account", cfg.AccountID, "--models=true")
	e.ok("run", "model", "--max-messages", "1")
	e.ok("run", "template", "--max-messages", "1")
	if requests.Load() != 2 || e.models != 2 {
		t.Fatal("authorized models did not use real local endpoint")
	}
	if c, _, _ := e.run("run", "external", "--max-messages", "1"); c == 0 {
		t.Fatal("models consent also granted subprocess consent")
	}
	e.ok("config", "processing", "--account", cfg.AccountID, "--external-stages=true")
	e.ok("run", "external", "--max-messages", "1")
	// The second message is uncached: source/body callbacks must share one client.
	clients = e.clients
	e.ok("run", "hook", "--max-messages", "2")
	if e.clients-clients != 1 {
		t.Fatal("hook/body did not share one lazy owned client")
	}
	source, err := os.ReadFile(sourceMarker)
	if err != nil {
		t.Fatal("hook did not receive MIME source")
	}
	if _, err = os.Stat(string(source)); !os.IsNotExist(err) {
		t.Fatal("hook spool not cleaned")
	}
	e.ok("config", "processing", "--account", cfg.AccountID, "--models=false", "--external-stages=false")
	if c, _, _ := e.run("run", "model", "--rerun"); c == 0 {
		t.Fatal("opt-out ignored")
	}
	cfg, _ = config.Load(config.DefaultPath())
	cfg.Processing.Models = true
	cfg.Processing.AccountID = "other-account"
	config.Save(config.DefaultPath(), cfg)
	if c, _, _ := e.run("run", "model", "--rerun"); c == 0 {
		t.Fatal("copied consent authorized wrong account")
	}
	e.ok("config", "processing", "--account", cfg.AccountID, "--external-stages=true")
	if c, _, _ := e.run("run", "model", "--rerun"); c == 0 {
		t.Fatal("external-only consent inherited another account model authorization")
	}
}

func yahooHistory(t *testing.T, e *yahooEnv, c config.Config) [32]byte {
	t.Helper()
	db, err := store.OpenAccount(context.Background(), e.db, c.Account())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ms, err := db.Messages(context.Background(), store.AccountFilter{})
	if err != nil {
		t.Fatal(err)
	}
	plans, err := db.Plans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var verdicts []store.Verdict
	for _, m := range ms {
		v, e := db.Verdicts(context.Background(), m.Local)
		if e != nil {
			t.Fatal(e)
		}
		verdicts = append(verdicts, v...)
	}
	b, _ := json.Marshal([]any{ms, plans, verdicts})
	return sha256.Sum256(b)
}
func TestYahooPermanentBindingReopen(t *testing.T) {
	e := newYahooEnv(t)
	e.ok("auth", "login")
	e.ok("sync", "--folder", "inbox")
	e.ok("show", "1")
	os.WriteFile(filepath.Join(e.dir, "stages.json"), []byte(`{"stages":[{"name":"local","type":"honeypot"}],"policy":{}}`), 0600)
	e.ok("run", "local")
	e.ok("plan", "create")
	original, _ := config.Load(config.DefaultPath())
	before := yahooHistory(t, e, original)
	for _, kind := range []string{"login-case", "login-space", "endpoint", "port", "account"} {
		t.Run(kind, func(t *testing.T) {
			c := original
			im := *original.IMAP
			c.IMAP = &im
			switch kind {
			case "login-case":
				c.IMAP.Login = strings.ToLower(c.IMAP.Login)
			case "login-space":
				c.IMAP.Login = strings.TrimSpace(c.IMAP.Login)
			case "endpoint":
				c.IMAP.Endpoint = "mail.example.org:993"
			case "port":
				c.IMAP.Endpoint = "127.0.0.1:65534"
			}
			p, err := c.Principal()
			if err != nil {
				t.Fatal(err)
			}
			c.AccountID = string(p.Account().ID)
			if kind == "account" {
				b, _ := json.Marshal(c)
				var raw map[string]any
				json.Unmarshal(b, &raw)
				raw["account_id"] = "arbitrary"
				b, _ = json.Marshal(raw)
				os.WriteFile(config.DefaultPath(), b, 0600)
			} else if err = config.Save(config.DefaultPath(), c); err != nil {
				t.Fatal(err)
			}
			calls, clients, models := e.runner.calls, e.clients, e.models
			if code, _, _ := e.run("sync", "--folder", "inbox"); code == 0 {
				t.Fatal("principal change reused database")
			}
			if e.runner.calls != calls || e.clients != clients || e.models != models {
				t.Fatal("binding mismatch caused external effects")
			}
			if err = config.Save(config.DefaultPath(), original); err != nil {
				t.Fatal(err)
			}
			if before != yahooHistory(t, e, original) {
				t.Fatal("binding failure changed history")
			}
		})
	}
	e.ok("sync", "--folder", "inbox")
	e.ok("show", "1")
	// An explicit distinct database accepts another configured principal without rebinding history.
	c := original
	im := *c.IMAP
	im.Login = "another@example.org"
	c.IMAP = &im
	p, _ := c.Principal()
	c.AccountID = string(p.Account().ID)
	c.Credential = nil
	config.Save(config.DefaultPath(), c)
	oldDB := e.db
	e.db = filepath.Join(e.dir, "other.db")
	e.ok("auth", "login")
	e.ok("sync", "--folder", "inbox")
	e.db = oldDB
	config.Save(config.DefaultPath(), original)
	if before != yahooHistory(t, e, original) {
		t.Fatal("recovery reassigned original history")
	}
}

func TestYahooFinalReadySaveFailure(t *testing.T) {
	e := newYahooEnv(t)
	e.deps.SaveConfig = func(path string, c config.Config) error {
		if c.Credential != nil && c.Credential.Ready {
			return errors.New("credential-canary save failed")
		}
		return config.Save(path, c)
	}
	if c, _, _ := e.run("auth", "login"); c == 0 {
		t.Fatal("failed ready commit succeeded")
	}
	if c, _, _ := e.run("auth", "status"); c == 0 {
		t.Fatal("failed ready commit reported stored")
	}
	e.deps.SaveConfig = nil
	e.ok("auth", "login")
	e.ok("auth", "status")
}

func TestYahooExecuteEmptyAndAlreadySaved(t *testing.T) {
	e := newYahooEnv(t)
	e.ok("auth", "login")
	e.ok("sync", "--folder", "inbox")
	cfg, _ := config.Load(config.DefaultPath())
	db, err := store.OpenAccount(context.Background(), e.db, cfg.Account())
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AddVerdict(context.Background(), mailbox.LocalRef{AccountID: cfg.Account().ID, N: 1}, store.Verdict{Stage: "filing", Labels: []string{"saved"}, Reason: "Record.pdf"}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	root := filepath.Join(e.dir, "documents")
	os.Mkdir(root, 0700)
	os.WriteFile(filepath.Join(root, "Record.pdf"), []byte("%PDF-invented"), 0600)
	manifest := filepath.Join(e.dir, "manifest.tsv")
	os.WriteFile(manifest, []byte("n\tfolder\tname\n1\t.\tRecord.pdf\n"), 0600)
	opens, clients, calls := e.opens, e.clients, e.runner.calls
	if code, _, _ := e.run("save", "--manifest", manifest, "--root", root, "--execute"); code == 0 {
		t.Fatal("already-saved execution bypassed pilot guard")
	}
	if e.opens != opens || e.clients != clients || e.runner.calls != calls {
		t.Fatal("already-saved guard performed effects")
	}
	b, _ := os.ReadFile(filepath.Join(root, "Record.pdf"))
	if string(b) != "%PDF-invented" {
		t.Fatal("saved output changed")
	}
	if out := e.ok("save", "--manifest", manifest, "--root", root); strings.Contains(out, "--execute") {
		t.Fatal("dry save suggested execution")
	}
	for _, args := range [][]string{{"apply", "1"}, {"rules", "list"}, {"rules", "diff"}, {"rules", "senders"}} {
		code, _, stderr := e.run(args...)
		if code == 0 || strings.Contains(stderr, "--execute") {
			t.Fatal("unsupported effect gave success/execute hint")
		}
	}
}

func TestYahooCandidateAndInputNegativeControls(t *testing.T) {
	for _, kind := range []string{"empty", "control", "oversize", "canceled", "rejected-auth"} {
		t.Run(kind, func(t *testing.T) {
			e := newYahooEnv(t)
			switch kind {
			case "empty":
				e.input = []byte{}
			case "control":
				e.input = []byte("line\n")
			case "oversize":
				e.input = bytes.Repeat([]byte{'x'}, 257)
			case "canceled":
				e.inputErr = context.Canceled
			case "rejected-auth":
				e.server.mu.Lock()
				e.server.authFail = true
				e.server.mu.Unlock()
			}
			if code, _, _ := e.run("auth", "login"); code == 0 {
				t.Fatal("negative authentication/input control succeeded")
			}
			if e.runner.writes != 0 {
				t.Fatal("failed input/probe persisted secret")
			}
			if code, _, _ := e.run("auth", "status"); code == 0 {
				t.Fatal("failed input/probe reported stored")
			}
		})
	}
}

func TestYahooWrongEnvelopeNeverSelectsAnotherCredential(t *testing.T) {
	for _, kind := range []string{"tuple", "version"} {
		t.Run(kind, func(t *testing.T) {
			e := newYahooEnv(t)
			e.ok("auth", "login")
			c, _ := config.Load(config.DefaultPath())
			p, _ := c.Principal()
			key := imapauth.Service + "/" + p.Item()
			original := e.runner.items[key]
			var fields []string
			json.Unmarshal([]byte(original), &fields)
			if kind == "tuple" {
				fields[1] += "x"
			} else {
				fields[0] = "imap-credential-v2"
			}
			b, _ := json.Marshal(fields)
			e.runner.items[key] = string(b)
			e.server.mu.Lock()
			connections := e.server.connections
			e.server.mu.Unlock()
			if code, _, _ := e.run("sync", "--folder", "inbox"); code == 0 {
				t.Fatal("wrong envelope selected")
			}
			e.server.mu.Lock()
			after := e.server.connections
			e.server.mu.Unlock()
			if connections != after {
				t.Fatal("corrupt stored credential reached endpoint")
			}
			writes := e.runner.writes
			if code, _, _ := e.run("auth", "login"); code == 0 {
				t.Fatal("wrong tuple/version overwritten")
			}
			if e.runner.writes != writes || e.runner.items[key] != string(b) {
				t.Fatal("wrong tuple/version item changed")
			}
		})
	}
}
