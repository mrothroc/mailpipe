package imapmail

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	imap "github.com/emersion/go-imap"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"github.com/mrothroc/mailpipe/pkg/mailsync"
	"github.com/mrothroc/mailpipe/pkg/store"
)

func TestIMAPWireLimits(t *testing.T) {
	for _, reply := range []string{"* 1 FETCH (UID 1 BODY[]<0> {4294967295}\r\n", "* 1 FETCH (UID 1 BODY[]<0> {65537}\r\n", "* OK " + strings.Repeat("x", 129<<10) + "\r\n"} {
		t.Run("bounded response", func(t *testing.T) {
			b := &budgetReader{r: bufio.NewReaderSize(strings.NewReader(reply), 128)}
			r := imap.NewReader(b)
			r.MaxLiteralSize = 64 << 10
			if _, e := imap.ReadResp(r); e == nil {
				t.Fatal("oversized response accepted")
			}
			if b.response > 128<<10+4 {
				t.Fatalf("budget bypass %d", b.response)
			}
		})
	}
	b := &budgetReader{r: bufio.NewReader(strings.NewReader(strings.Repeat("x", 129<<10)))}
	if _, e := b.ReadString('\n'); !errors.Is(e, errWireLimit) {
		t.Fatal("ReadString bypass", e)
	}
}
func TestIMAPIncompleteResponses(t *testing.T) {
	replies := map[string]string{
		"duplicate":     "* 1 FETCH (UID 1 UID 1 FLAGS () RFC822.SIZE 0)\r\n${tag} OK done\r\n",
		"missing UID":   "* 1 FETCH (FLAGS ())\r\n${tag} OK done\r\n",
		"unknown":       "* 1 FETCH (UID 1 UNKNOWN 4)\r\n${tag} OK done\r\n",
		"short success": "${tag} OK incomplete\r\n",
		"bad prefix":    "* 0 UIDFETCH (UID 1 FLAGS () RFC822.SIZE 0)\r\n${tag} OK done\r\n",
		"uidnotsticky":  "* OK [UIDNOTSTICKY] nonpersistent\r\n${tag} OK done\r\n",
		"limited":       "* 1 FETCH (UID 1 FLAGS ())\r\n${tag} NO [MESSAGELIMIT 1] incomplete\r\n",
		"malformed":     "* 1 FETCH (UID nope)\r\n${tag} OK done\r\n",
	}
	for name, reply := range replies {
		t.Run(name, func(t *testing.T) {
			s, cfg := localScript(t)
			cfg.MaxAttempts = 1
			c := openClient(t, cfg)
			f := inbox(t, c)
			db := openMirror(t, cfg)
			if _, e := c.SyncCollection(context.Background(), db, f, &testLoader{}, false); e != nil {
				t.Fatal(e)
			}
			before, _ := db.Checkpoint(context.Background(), f.Collection.Ref)
			s.mu.Lock()
			s.fetchReply = reply
			s.mu.Unlock()
			if _, e := c.SyncCollection(context.Background(), db, f, &testLoader{}, false); e == nil {
				t.Fatal("partial response succeeded")
			}
			after, _ := db.Checkpoint(context.Background(), f.Collection.Ref)
			if before != after {
				t.Fatal("failed response published")
			}
		})
	}
}
func TestIMAPCancellationAndClose(t *testing.T) {
	for _, scenario := range []string{"cancel", "close", "source cancellation"} {
		t.Run(scenario, func(t *testing.T) {
			s, cfg := localScript(t)
			c := openClient(t, cfg)
			f := inbox(t, c)
			db := openMirror(t, cfg)
			if _, e := c.SyncCollection(context.Background(), db, f, &testLoader{}, false); e != nil {
				t.Fatal(e)
			}
			before, _ := db.Checkpoint(context.Background(), f.Collection.Ref)
			stalled := make(chan struct{}, 1)
			s.mu.Lock()
			s.stall = stalled
			s.stallSource = scenario == "source cancellation"
			s.mu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, e := c.SyncCollection(ctx, db, f, &testLoader{}, false); done <- e }()
			select {
			case <-stalled:
			case <-time.After(5 * time.Second):
				t.Fatal("script not reached")
			}
			if scenario == "close" {
				_ = c.Close()
			} else {
				cancel()
			}
			select {
			case e := <-done:
				if e == nil {
					t.Fatal("canceled scan succeeded")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("owned transport did not terminate")
			}
			after, _ := db.Checkpoint(context.Background(), f.Collection.Ref)
			if before != after {
				t.Fatal("cancellation published")
			}
		})
	}
}
func TestIMAPRetryProvenance(t *testing.T) {
	for _, phase := range []string{"protocol", "begin", "stage", "commit", "loader", "local", "cleanup", "lost marker", "old marker"} {
		t.Run(phase, func(t *testing.T) {
			marker := &protocolAttemptFailure{&protocolFault{code: "invented disconnect", transient: true}}
			e := &attemptEvidence{entered: true, returned: true, clean: true, marker: marker, errorReturned: marker}
			runErr := error(marker)
			switch phase {
			case "begin":
				e.entered = false
			case "stage":
				e.emitError = io.EOF
			case "commit":
				e.errorReturned = nil
			case "loader":
				e.loaderError = io.EOF
			case "local":
				e.localError = io.EOF
			case "cleanup":
				runErr = errors.Join(marker, mailsync.ErrCleanup)
			case "lost marker":
				runErr = io.EOF
			case "old marker":
				runErr = &protocolAttemptFailure{marker.reason}
			}
			if e.mayRetry(runErr) != (phase == "protocol") {
				t.Fatal("unsafe retry classification")
			}
		})
	}
}
func TestIMAPPermanentAuthAndRedaction(t *testing.T) {
	s, cfg := localScript(t)
	s.mu.Lock()
	s.authFail = true
	s.mu.Unlock()
	c := openClient(t, cfg)
	_, e := c.Discover(context.Background())
	if e == nil || strings.Contains(e.Error(), "invented-private-token") || strings.Contains(e.Error(), "owner@example.com") || strings.Contains(e.Error(), "invented-test-password") {
		t.Fatal("unsafe authentication error", e)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connections != 1 {
		t.Fatal("permanent auth retried")
	}
}
func TestIMAPGenericNoExtensionsAndYahooFullView(t *testing.T) {
	for _, profile := range []Profile{Generic, Yahoo} {
		t.Run(string(profile), func(t *testing.T) {
			s, cfg := localScript(t)
			s.mu.Lock()
			s.caps = "IMAP4rev1"
			s.special = false
			s.mu.Unlock()
			cfg.Profile = profile
			cfg.RoleMappings = map[Role]string{Sent: "Folder A", Trash: "Folder B", Junk: "Folder C"}
			c := openClient(t, cfg)
			d, e := c.Discover(context.Background())
			if profile == Yahoo {
				if e == nil {
					t.Fatal("Yahoo limited view accepted")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			db := openMirror(t, cfg)
			if _, e = c.SyncCollection(context.Background(), db, d.Folders[0], &testLoader{}, false); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestIMAPIndependentFolderOccurrences(t *testing.T) {
	_, cfg := localScript(t)
	c := openClient(t, cfg)
	d, e := c.Discover(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	db := openMirror(t, cfg)
	for _, f := range d.Folders[:2] {
		if _, e = c.SyncCollection(context.Background(), db, f, &testLoader{}, false); e != nil {
			t.Fatal(e)
		}
	}
	n, e := db.Count(context.Background(), store.AccountFilter{})
	if e != nil || n != 4 {
		t.Fatalf("folder occurrences collapsed %d %v", n, e)
	}
}

func TestIMAPPartialOKIsNeverCompletion(t *testing.T) {
	s, cfg := localScript(t)
	cfg.MaxAttempts = 1
	c := openClient(t, cfg)
	f := inbox(t, c)
	db := openMirror(t, cfg)
	s.mu.Lock()
	s.fetchReply = "* 1 FETCH (UID 1 FLAGS () INTERNALDATE \"01-Jan-2026 00:00:00 +0000\" RFC822.SIZE 0 ENVELOPE (NIL NIL NIL NIL NIL NIL NIL NIL NIL NIL))\r\n* 3 FETCH (UID 3 FLAGS () INTERNALDATE \"01-Jan-2026 00:00:00 +0000\" RFC822.SIZE 0 ENVELOPE (NIL NIL NIL NIL NIL NIL NIL NIL NIL NIL))\r\n${tag} OK [MESSAGELIMIT 1 1] partial\r\n"
	s.mu.Unlock()
	if _, e := c.SyncCollection(context.Background(), db, f, &testLoader{}, false); e == nil {
		t.Fatal("partial OK succeeded")
	}
	cp, _ := db.Checkpoint(context.Background(), f.Collection.Ref)
	if cp.Token != "" {
		t.Fatal("partial OK checkpoint")
	}
}

func TestIMAPRetryBudgetAndIdentity(t *testing.T) {
	t.Run("exhausted churn", func(t *testing.T) {
		s, cfg := localScript(t)
		cfg.MaxAttempts = 4
		c := openClient(t, cfg)
		f := inbox(t, c)
		db := openMirror(t, cfg)
		s.mu.Lock()
		s.churn = true
		s.mu.Unlock()
		var waits []time.Duration
		c.clock = &fakeClock{&waits}
		c.random = func() float64 { return 1 }
		if _, e := c.SyncCollection(context.Background(), db, f, &testLoader{}, false); e == nil {
			t.Fatal("churn succeeded")
		}
		if len(waits) != 3 || waits[0] != time.Second || waits[1] != 2*time.Second || waits[2] != 4*time.Second {
			t.Fatalf("retry budget %v", waits)
		}
		cp, _ := db.Checkpoint(context.Background(), f.Collection.Ref)
		if cp.Token != "" {
			t.Fatal("churn checkpoint")
		}
	})
	t.Run("changed reconnect epoch", func(t *testing.T) {
		s, cfg := localScript(t)
		c := openClient(t, cfg)
		f := inbox(t, c)
		db := openMirror(t, cfg)
		s.mu.Lock()
		s.byeOnce = true
		s.mu.Unlock()
		var waits []time.Duration
		c.clock = waitFunc(func(ctx context.Context, d time.Duration) error {
			waits = append(waits, d)
			s.mu.Lock()
			s.validity++
			s.mu.Unlock()
			return nil
		})
		if _, e := c.SyncCollection(context.Background(), db, f, &testLoader{}, false); !errors.Is(e, mailbox.ErrStaleIdentity) {
			t.Fatalf("reconnect reset %v", e)
		}
		if len(waits) != 1 {
			t.Fatal(waits)
		}
		cp, _ := db.Checkpoint(context.Background(), f.Collection.Ref)
		if cp.Token != "" {
			t.Fatal("changed epoch checkpoint")
		}
	})
	t.Run("trusted hint and deadline", func(t *testing.T) {
		_, cfg := localScript(t)
		calls := 0
		c, e := New(cfg, func(ctx context.Context) (Credentials, error) {
			calls++
			if calls == 1 {
				return Credentials{}, &TemporaryCredentialError{RetryAfter: 3 * time.Second}
			}
			return inventedCredentials(cfg.Account)(ctx)
		})
		if e != nil {
			t.Fatal(e)
		}
		defer c.Close()
		var waits []time.Duration
		c.clock = &fakeClock{&waits}
		f := openClient(t, cfg)
		folder := inbox(t, f)
		db := openMirror(t, cfg)
		if _, e = c.SyncCollection(context.Background(), db, folder, &testLoader{}, false); e != nil || len(waits) != 1 || waits[0] != 3*time.Second {
			t.Fatalf("hint %v %v", waits, e)
		}
		c.credentials = func(context.Context) (Credentials, error) {
			return Credentials{}, &TemporaryCredentialError{RetryAfter: 30 * time.Second}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		waits = nil
		if _, e = c.SyncCollection(ctx, db, folder, &testLoader{}, false); e == nil || len(waits) != 0 {
			t.Fatal("deadline hint waited", e)
		}
	})
}

func TestIMAPSyncLimitedWindowsAndLargeInventory(t *testing.T) {
	s, cfg := localScript(t)
	cfg.Profile = Yahoo
	s.mu.Lock()
	s.caps += " MESSAGELIMIT=100"
	s.messages = nil
	for i := uint32(1); i <= 1005; i++ {
		s.messages = append(s.messages, scriptMessage{i, "From: sender@example.org\r\n\r\ninvented", `\Deleted`})
	}
	s.mu.Unlock()
	c := openClient(t, cfg)
	f := inbox(t, c)
	db := openMirror(t, cfg)
	result, e := c.SyncCollection(context.Background(), db, f, &testLoader{}, false)
	if e != nil || result.Remote != 1005 {
		t.Fatalf("limited window scan %+v %v", result, e)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.messages {
		if m.flags != `\Deleted` {
			t.Fatal("read changed flags")
		}
	}
}

type loaderFunc func(context.Context, Occurrence, WireMetadata, ReadAccess) (mailbox.Metadata, error)

func (f loaderFunc) Load(ctx context.Context, o Occurrence, w WireMetadata, a ReadAccess) (mailbox.Metadata, error) {
	return f(ctx, o, w, a)
}
func TestIMAPLoaderFailuresAndLease(t *testing.T) {
	for _, scenario := range []string{"ignored access failure", "spool cleanup", "recursive access"} {
		t.Run(scenario, func(t *testing.T) {
			_, cfg := localScript(t)
			c := openClient(t, cfg)
			f := inbox(t, c)
			db := openMirror(t, cfg)
			if _, e := c.SyncCollection(context.Background(), db, f, &testLoader{}, false); e != nil {
				t.Fatal(e)
			}
			cp, _ := db.Checkpoint(context.Background(), f.Collection.Ref)
			dir := t.TempDir()
			loader := loaderFunc(func(ctx context.Context, o Occurrence, w WireMetadata, a ReadAccess) (mailbox.Metadata, error) {
				switch scenario {
				case "ignored access failure":
					_ = a.ReadSource(ctx, writerFunc(func([]byte) (int, error) { return 0, io.ErrShortWrite }))
					return mailbox.Metadata{Sender: mailbox.Address{Address: "sender@example.org"}}, nil
				case "recursive access":
					e := a.ReadSource(ctx, writerFunc(func(p []byte) (int, error) {
						if e := a.ReadSource(ctx, io.Discard); e == nil {
							return 0, errors.New("recursive read accepted")
						}
						return len(p), nil
					}))
					return mailbox.Metadata{}, e
				default:
					path := filepath.Join(dir, "invented-spool")
					if e := os.Mkdir(path, 0700); e != nil {
						return mailbox.Metadata{}, e
					}
					file, e := os.CreateTemp(path, "source-")
					if e != nil {
						return mailbox.Metadata{}, e
					}
					defer os.RemoveAll(path)
					readErr := a.ReadSource(ctx, file)
					closeErr := file.Close()
					cleanupErr := os.Remove(path)
					return mailbox.Metadata{}, errors.Join(readErr, closeErr, cleanupErr)
				}
			})
			var waits []time.Duration
			c.clock = &fakeClock{&waits}
			if _, e := c.SyncCollection(context.Background(), db, f, loader, false); e == nil || len(waits) != 0 {
				t.Fatal("opaque/ignored loader failure retried/succeeded", e)
			}
			after, _ := db.Checkpoint(context.Background(), f.Collection.Ref)
			if cp != after {
				t.Fatal("loader failure checkpoint")
			}
			entries, e := os.ReadDir(dir)
			if e != nil || len(entries) != 0 {
				t.Fatal("test-owned spool cleanup", e)
			}
		})
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestIMAPActualCoreUncertainOutcomeNeverRetries(t *testing.T) {
	for _, phase := range []string{"admission", "stage acknowledgment", "commit acknowledgment", "postcommit report"} {
		t.Run(phase, func(t *testing.T) {
			_, cfg := localScript(t)
			c := openClient(t, cfg)
			f := inbox(t, c)
			db := openMirror(t, cfg)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var waits []time.Duration
			c.clock = &fakeClock{&waits}
			calls := 0
			c.runPass = func(ctx context.Context, s *store.AccountStore, spec mailsync.PassSpec, scan mailsync.Scan) error {
				calls++
				if phase == "admission" {
					return io.EOF
				}
				if phase == "stage acknowledgment" {
					return mailsync.RunPass(ctx, s, spec, func(ctx context.Context, emit mailsync.EmitPage) error {
						return scan(ctx, func(input mailbox.Cursor, page mailbox.SyncPage, records []store.MessageRecord) error {
							if e := emit(input, page, records); e != nil {
								return e
							}
							return io.EOF
						})
					})
				}
				e := mailsync.RunPass(ctx, s, spec, scan)
				if e != nil {
					return e
				}
				if phase == "postcommit report" {
					cancel()
					return nil
				}
				return io.EOF
			}
			if _, e := c.SyncCollection(ctx, db, f, &testLoader{}, false); e == nil || len(waits) != 0 || calls != 1 {
				t.Fatalf("uncertain %s replayed %d %v %v", phase, calls, waits, e)
			}
			cp, e := db.Checkpoint(context.Background(), f.Collection.Ref)
			if e != nil {
				t.Fatal(e)
			}
			committed := phase == "commit acknowledgment" || phase == "postcommit report"
			if committed != (cp.Token != "") {
				t.Fatalf("incorrect durable outcome %s %+v", phase, cp)
			}
		})
	}
}

func TestIMAPSourceCompleteOrError(t *testing.T) {
	for _, scenario := range []string{"large", "empty", "wrong UID", "wrong offset", "wrong size", "short literal", "known limit"} {
		t.Run(scenario, func(t *testing.T) {
			s, cfg := localScript(t)
			if scenario == "known limit" {
				cfg.SourceBytes = 1
			}
			s.mu.Lock()
			s.messages = s.messages[:1]
			if scenario == "large" {
				s.messages[0].source = strings.Repeat("invented source\r\n", 5000)
			}
			if scenario == "empty" {
				s.messages[0].source = ""
			}
			expected := s.messages[0].source
			switch scenario {
			case "wrong UID":
				s.sourceReply = "* 2 FETCH (UID 2 RFC822.SIZE 0 BODY[]<0> {0}\r\n)\r\n${tag} OK done\r\n"
			case "wrong offset":
				s.sourceReply = "* 1 FETCH (UID 1 RFC822.SIZE " + strconv.Itoa(len(expected)) + " BODY[]<1> {0}\r\n)\r\n${tag} OK done\r\n"
			case "wrong size":
				s.sourceReply = "* 1 FETCH (UID 1 RFC822.SIZE 999 BODY[]<0> {0}\r\n)\r\n${tag} OK done\r\n"
			case "short literal":
				s.sourceReply = "* 1 FETCH (UID 1 RFC822.SIZE " + strconv.Itoa(len(expected)) + " BODY[]<0> {32}\r\nx"
			}
			s.mu.Unlock()
			c := openClient(t, cfg)
			f := inbox(t, c)
			db := openMirror(t, cfg)
			var actual bytes.Buffer
			loader := loaderFunc(func(ctx context.Context, o Occurrence, w WireMetadata, a ReadAccess) (mailbox.Metadata, error) {
				if e := a.ReadSource(ctx, &actual); e != nil {
					return mailbox.Metadata{}, e
				}
				return mailbox.Metadata{Sender: mailbox.Address{Address: "sender@example.org"}}, nil
			})
			_, e := c.SyncCollection(context.Background(), db, f, loader, false)
			valid := scenario == "large" || scenario == "empty"
			if valid != (e == nil) {
				t.Fatalf("%s complete source %v", scenario, e)
			}
			if valid && actual.String() != expected {
				t.Fatal("source bytes changed")
			}
			if !valid {
				cp, _ := db.Checkpoint(context.Background(), f.Collection.Ref)
				if cp.Token != "" {
					t.Fatal("partial source checkpoint")
				}
			}
			if scenario == "known limit" && actual.Len() != 0 {
				t.Fatal("known oversized source wrote bytes")
			}
		})
	}
}
func TestIMAPSyncExternalMove(t *testing.T) {
	ctx := context.Background()
	s, cfg := localScript(t)
	s.mu.Lock()
	original := s.messages[0]
	s.folders = map[string][]scriptMessage{"INBOX": {original}, "Folder A": {}}
	s.mu.Unlock()
	c := openClient(t, cfg)
	d, e := c.Discover(ctx)
	if e != nil {
		t.Fatal(e)
	}
	db := openMirror(t, cfg)
	if _, e = c.SyncCollection(ctx, db, d.Folders[0], &testLoader{}, false); e != nil {
		t.Fatal(e)
	}
	old, e := db.Messages(ctx, store.AccountFilter{})
	if e != nil || len(old) != 1 {
		t.Fatal(e)
	}
	s.mu.Lock()
	s.folders["INBOX"] = nil
	s.folders["Folder A"] = []scriptMessage{original}
	s.mu.Unlock()
	for _, f := range d.Folders[:2] {
		if _, e = c.SyncCollection(ctx, db, f, &testLoader{}, false); e != nil {
			t.Fatal(e)
		}
	}
	live, e := db.Messages(ctx, store.AccountFilter{})
	if e != nil || len(live) != 1 || live[0].Local == old[0].Local {
		t.Fatal("external move merged occurrence", e)
	}
	history, e := db.GetMessage(ctx, old[0].Local)
	if e != nil || len(history.State.Memberships) != 0 {
		t.Fatal("source history lost", e)
	}
}

func TestIMAPNoMutationSurfaceAndRequiredLoader(t *testing.T) {
	s, cfg := localScript(t)
	c := openClient(t, cfg)
	if _, ok := any(c).(mailbox.Provider); ok {
		t.Fatal("unfinished full provider")
	}
	if _, ok := any(c).(mailbox.TrashCapability); ok {
		t.Fatal("writable trash surface")
	}
	if _, ok := any(c).(mailbox.ArchiveCapability); ok {
		t.Fatal("writable archive surface")
	}
	if _, ok := any(c).(mailbox.NativeRuleCapability); ok {
		t.Fatal("writable rule surface")
	}
	for _, cmd := range []string{"CREATE", "STORE", "UID STORE", "COPY", "UID COPY", "MOVE", "UID MOVE", "EXPUNGE", "UID EXPUNGE", "CLOSE", "DELETE", "RENAME", "APPEND", "SUBSCRIBE", "UNSUBSCRIBE", "SELECT"} {
		if e := (&session{}).command(cmd, nil, nil); e == nil {
			t.Fatalf("mutation %s accepted", cmd)
		}
	}
	db := openMirror(t, cfg)
	var nilPointer *testLoader
	var nilFunction loaderFunc
	for _, loader := range []MetadataLoader{nil, nilPointer, nilFunction} {
		if _, e := c.SyncCollection(context.Background(), db, c.folder("INBOX", "/"), loader, false); e == nil {
			t.Fatal("missing loader accepted")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connections != 0 {
		t.Fatal("missing loader contacted transport")
	}
}

func TestIMAPMidPassBYEAbortsBeforeBackoff(t *testing.T) {
	ctx := context.Background()
	server, cfg := localScript(t)
	cfg.UIDWindow = 1
	c := openClient(t, cfg)
	folder := inbox(t, c)
	path := filepath.Join(t.TempDir(), "mail.db")
	mirror, e := store.OpenAccount(ctx, path, cfg.Account)
	if e != nil {
		t.Fatal(e)
	}
	defer mirror.Close()
	observer, e := sql.Open("sqlite", "file:"+path+"?mode=rw")
	if e != nil {
		t.Fatal(e)
	}
	defer observer.Close()
	server.mu.Lock()
	server.byeAtMetadata = 2
	server.mu.Unlock()
	staged := 0
	c.runPass = func(ctx context.Context, s *store.AccountStore, spec mailsync.PassSpec, scan mailsync.Scan) error {
		return mailsync.RunPass(ctx, s, spec, func(ctx context.Context, emit mailsync.EmitPage) error {
			return scan(ctx, func(input mailbox.Cursor, page mailbox.SyncPage, records []store.MessageRecord) error {
				e := emit(input, page, records)
				if e == nil && len(page.Messages) > 0 {
					staged++
				}
				return e
			})
		})
	}
	waits := 0
	c.clock = waitFunc(func(ctx context.Context, d time.Duration) error {
		waits++
		if staged == 0 {
			t.Fatal("BYE did not follow a staged page")
		}
		for _, table := range []string{"sync_passes", "sync_pages", "sync_seen"} {
			var count int
			if e := observer.QueryRowContext(ctx, "select count(*) from "+table).Scan(&count); e != nil {
				t.Fatal(e)
			}
			if count != 0 {
				t.Fatalf("backoff before owned abort: %s=%d", table, count)
			}
		}
		cp, e := mirror.Checkpoint(ctx, folder.Collection.Ref)
		if e != nil || cp.Token != "" {
			t.Fatal("interrupted attempt published", e)
		}
		return nil
	})
	result, e := c.SyncCollection(ctx, mirror, folder, &testLoader{}, false)
	if e != nil || waits != 1 || result.Remote != 2 {
		t.Fatalf("fresh retry %+v %v waits=%d", result, e, waits)
	}
}

func TestIMAPSourceFieldGrammar(t *testing.T) {
	for _, fields := range [][]any{
		{"UID", uint32(1), "RFC822.SIZE", uint32(0), "BODY[]<0>junk>", bytes.NewReader(nil)},
		{"UID", uint32(1), "RFC822.SIZE", uint32(0), "BODY[]<1>", bytes.NewReader(nil), "BODY[]<0>", bytes.NewReader(nil)},
	} {
		_, _, e := parseFetch(&imap.DataResp{Tag: "*", Fields: []any{"1", "FETCH", fields}}, true)
		if e == nil {
			t.Fatal("malformed or extra source field accepted")
		}
	}
}

func TestIMAPPermanentBADNeverRetries(t *testing.T) {
	for _, code := range []string{"UNAVAILABLE", "THROTTLED", "MESSAGELIMIT 1 1"} {
		t.Run(code, func(t *testing.T) {
			s, cfg := localScript(t)
			c := openClient(t, cfg)
			f := inbox(t, c)
			db := openMirror(t, cfg)
			s.mu.Lock()
			s.fetchReply = "${tag} BAD [" + code + "] permanent protocol failure\r\n"
			s.mu.Unlock()
			var waits []time.Duration
			c.clock = &fakeClock{&waits}
			if _, e := c.SyncCollection(context.Background(), db, f, &testLoader{}, false); e == nil || len(waits) != 0 {
				t.Fatal("permanent BAD promoted by response code", e)
			}
		})
	}
}

func TestIMAPReadSourceIdentityAndWriter(t *testing.T) {
	ctx := context.Background()
	server, cfg := localScript(t)
	c := openClient(t, cfg)
	folder := inbox(t, c)
	mirror := openMirror(t, cfg)
	if _, e := c.SyncCollection(ctx, mirror, folder, &testLoader{}, false); e != nil {
		t.Fatal(e)
	}
	rows, e := mirror.Messages(ctx, store.AccountFilter{})
	if e != nil {
		t.Fatal(e)
	}
	ref := rows[0].State.Ref
	server.mu.Lock()
	expected := server.messages[0].source
	server.mu.Unlock()
	var output bytes.Buffer
	if e = c.ReadSource(ctx, ref, &output); e != nil || output.String() != expected {
		t.Fatalf("public source %v", e)
	}
	if e = c.ReadSource(ctx, ref, writerFunc(func(p []byte) (int, error) { return 0, nil })); !errors.Is(e, io.ErrShortWrite) {
		t.Fatalf("short writer error lost %v", e)
	}
	server.mu.Lock()
	server.messages = server.messages[1:]
	server.mu.Unlock()
	if e = c.ReadSource(ctx, ref, io.Discard); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("missing occurrence not stale %v", e)
	}
}

func TestIMAPInternalDateFixedWidth(t *testing.T) {
	for _, date := range []string{"01-Jan-2026 00:00:00 +0000", " 1-Jan-2026 00:00:00 +0000"} {
		fields := []any{"UID", uint32(1), "FLAGS", []any{}, "INTERNALDATE", date, "RFC822.SIZE", uint32(0), "ENVELOPE", make([]any, 10)}
		value, ok, e := parseFetch(&imap.DataResp{Tag: "*", Fields: []any{"1", "FETCH", fields}}, false)
		if e != nil || !ok || value.wire.InternalDate.Day() != 1 {
			t.Fatalf("valid fixed-width date rejected %q %v", date, e)
		}
	}
}

type firstCloseErrorConn struct {
	net.Conn
	calls   atomic.Int32
	failure error
}

func (c *firstCloseErrorConn) Close() error {
	if c.calls.Add(1) == 1 {
		_ = c.Conn.Close()
		return c.failure
	}
	return nil
}
func TestIMAPCanceledTransportCleanupFailureIsLatched(t *testing.T) {
	_, cfg := localScript(t)
	c := openClient(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	operation, release, e := c.operation(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	s, e := c.connect(operation)
	if e != nil {
		t.Fatal(e)
	}
	injected := errors.New("invented close acknowledgment failure")
	s.conn = &firstCloseErrorConn{Conn: s.conn, failure: injected}
	cancel()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not close transport")
	}
	if e = s.close(); !errors.Is(e, injected) {
		t.Fatalf("asynchronous cleanup uncertainty lost %v", e)
	}
}

func TestIMAPPreparationCleanupVetoesTransient(t *testing.T) {
	_, cfg := localScript(t)
	c := openClient(t, cfg)
	ctx, release, e := c.operation(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	s, e := c.connect(ctx)
	if e != nil {
		t.Fatal(e)
	}
	s.conn = &firstCloseErrorConn{Conn: s.conn, failure: errors.New("invented close uncertainty")}
	e = failedSession(s, fault("invented temporary disconnect", true))
	var transient *protocolFault
	if e == nil || errors.As(e, &transient) {
		t.Fatal("cleanup uncertainty retained preparation retry authority", e)
	}
}

func TestIMAPSyncRejectsUIDReuseWithinEpoch(t *testing.T) {
	ctx := context.Background()
	server, cfg := localScript(t)
	c := openClient(t, cfg)
	folder := inbox(t, c)
	mirror := openMirror(t, cfg)
	if _, e := c.SyncCollection(ctx, mirror, folder, &testLoader{}, false); e != nil {
		t.Fatal(e)
	}
	old, e := mirror.Messages(ctx, store.AccountFilter{})
	if e != nil {
		t.Fatal(e)
	}
	server.mu.Lock()
	expunged := server.messages[0]
	server.messages = server.messages[1:]
	server.mu.Unlock()
	if _, e = c.SyncCollection(ctx, mirror, folder, &testLoader{}, false); e != nil {
		t.Fatal(e)
	}
	cp, e := mirror.Checkpoint(ctx, folder.Collection.Ref)
	if e != nil {
		t.Fatal(e)
	}
	server.mu.Lock()
	server.messages = append([]scriptMessage{expunged}, server.messages...)
	server.mu.Unlock()
	if _, e = c.SyncCollection(ctx, mirror, folder, &testLoader{}, false); !errors.Is(e, mailbox.ErrStaleIdentity) {
		t.Fatalf("UID reused below committed high water was accepted: %v", e)
	}
	after, _ := mirror.Checkpoint(ctx, folder.Collection.Ref)
	history, e := mirror.GetMessage(ctx, old[0].Local)
	if e != nil || after != cp || len(history.State.Memberships) != 0 {
		t.Fatal("UID reuse resurrected old occurrence/history", e)
	}
}
