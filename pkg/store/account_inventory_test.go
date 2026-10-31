package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"modernc.org/sqlite"
)

// Observe values from the real pinned engine before database/sql scans them.
// The positive GetMessage control proves this catches load-then-discard bugs.
type inventoryCapture struct {
	maxValue int
	sync.Mutex
	bytes   int
	body    bool
	columns []string
	observe func([]string, []driver.Value)
}
type inventoryDriver struct{ capture *inventoryCapture }

func (d inventoryDriver) Open(n string) (driver.Conn, error) {
	c, e := (&sqlite.Driver{}).Open(n)
	if e != nil {
		return nil, e
	}
	return &inventoryConn{Conn: c, capture: d.capture}, nil
}

type inventoryConn struct {
	driver.Conn
	capture *inventoryCapture
}

func (c *inventoryConn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	r, e := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, a)
	if e != nil {
		return nil, e
	}
	return &inventoryRows{Rows: r, capture: c.capture}, nil
}

type inventoryRows struct {
	driver.Rows
	capture *inventoryCapture
}

func (r *inventoryRows) Next(v []driver.Value) error {
	e := r.Rows.Next(v)
	if e != nil {
		return e
	}
	r.capture.Lock()
	defer r.capture.Unlock()
	r.capture.columns = append(r.capture.columns, r.Columns()...)
	if r.capture.observe != nil {
		r.capture.observe(r.Columns(), v)
	}
	for _, v := range v {
		switch v := v.(type) {
		case string:
			r.capture.bytes += len(v)
			r.capture.maxValue = max(r.capture.maxValue, len(v))
			r.capture.body = r.capture.body || strings.Contains(v, "invented-body-canary")
		case []byte:
			r.capture.bytes += len(v)
			r.capture.maxValue = max(r.capture.maxValue, len(v))
			r.capture.body = r.capture.body || strings.Contains(string(v), "invented-body-canary")
		}
	}
	return nil
}
func TestAccountIMAPInventory(t *testing.T) {
	ctx := context.Background()
	t.Run("projection positive control and identity", func(t *testing.T) {
		a, c, n := epochFixture(t)
		body := strings.Repeat("invented-body-canary", 1<<17)
		if e := a.SetBody(ctx, n, body); e != nil {
			t.Fatal(e)
		}
		if e := a.store.db.Close(); e != nil {
			t.Fatal(e)
		}
		cap := &inventoryCapture{}
		name := "invented-inventory-" + randomID()
		sql.Register(name, inventoryDriver{cap})
		db, e := sql.Open(name, sqliteURI(a.store.life.entry.path, "rw", false))
		if e != nil {
			t.Fatal(e)
		}
		db.SetMaxOpenConns(1)
		a.store.db = db
		rows, e := a.ReadIMAPInventory(ctx, c.Ref, IMAPEpoch{MailboxID: "mailbox-a", UIDValidity: 10, Generation: 1})
		if e != nil || len(rows) != 1 {
			t.Fatalf("%+v %v", rows, e)
		}
		if rows[0].Local != n || rows[0].Remote.MessageID != "epoch-occurrence" || rows[0].IMAP.UID != 4 || rows[0].IMAP.EpochGeneration != 1 {
			t.Fatalf("wrong identity %+v", rows)
		}
		if cap.body || cap.bytes > 8192 {
			t.Fatalf("body materialized: %d bytes", cap.bytes)
		}
		for _, col := range cap.columns {
			if col == "body" || col == "metadata" {
				t.Fatalf("content column %s", col)
			}
		}
		m, e := a.GetMessage(ctx, n)
		if e != nil || m.Body == nil || *m.Body != body || !cap.body || cap.bytes < len(body) {
			t.Fatal("positive full-read control did not detect content", e)
		}
		cp, e := a.ReadIMAPEpochState(ctx, c.Ref)
		if e != nil {
			t.Fatal(e)
		}
		bad := cp
		bad.UIDValidity++
		if out, e := a.ReadIMAPInventory(ctx, c.Ref, IMAPEpoch{MailboxID: bad.MailboxID, UIDValidity: bad.UIDValidity, Generation: bad.EpochGeneration}); !errors.Is(e, mailbox.ErrStaleIdentity) || out != nil {
			t.Fatal("stale inventory", e)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if out, e := a.ReadIMAPInventory(canceled, c.Ref, IMAPEpoch{MailboxID: "mailbox-a", UIDValidity: 10, Generation: 1}); !errors.Is(e, context.Canceled) || out != nil {
			t.Fatal("canceled inventory", e)
		}
	})
	t.Run("field byte bounds before projection", func(t *testing.T) {
		a, c, n := epochFixture(t)
		capture := &inventoryCapture{}
		attachInventoryDriver(t, a, capture)
		for _, length := range []int{IMAPInventoryMaxFieldBytes, IMAPInventoryMaxFieldBytes + 1} {
			if _, e := a.store.db.ExecContext(ctx, "update messages set locator=? where n=?", strings.Repeat("x", length), n.N); e != nil {
				t.Fatal(e)
			}
			capture.maxValue = 0
			rows, e := a.ReadIMAPInventory(ctx, c.Ref, IMAPEpoch{MailboxID: "mailbox-a", UIDValidity: 10, Generation: 1})
			if length == IMAPInventoryMaxFieldBytes {
				if e != nil || len(rows) != 1 {
					t.Fatal(e)
				}
			} else {
				var limit *IMAPInventoryLimitError
				if capture.maxValue > IMAPInventoryMaxFieldBytes {
					t.Fatal("oversized identity converted before refusal")
				}
				if !errors.As(e, &limit) || rows != nil {
					t.Fatal("oversized field accepted", e)
				}
			}
		}
	})
}

func TestAccountIMAPInventoryLimitsAndFailures(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range []string{"rows", "aggregate", "missing locator", "wrong tuple", "unknown collection", "Unicode bytes"} {
		t.Run(scenario, func(t *testing.T) {
			a, c, n := epochFixture(t)
			epoch := IMAPEpoch{MailboxID: "mailbox-a", UIDValidity: 10, Generation: 1}
			switch scenario {
			case "rows", "aggregate":
				count := IMAPInventoryMaxRows
				locator := "route"
				if scenario == "aggregate" {
					count = 5000
					locator = strings.Repeat("x", 8192)
				}
				// Real SQLite-generated rows; avoid test setup materializing cached payloads.
				tx, e := a.store.db.BeginTx(ctx, nil)
				if e != nil {
					t.Fatal(e)
				}
				defer tx.Rollback()
				for _, q := range []string{"delete from memberships", "delete from imap_locators", "delete from verdicts", "delete from messages"} {
					if _, e = tx.ExecContext(ctx, q); e != nil {
						t.Fatal(e)
					}
				}
				insert := func(count int) error {
					_, e := tx.ExecContext(ctx, `with recursive seq(x) as (select 1 union all select x+1 from seq where x<?) insert into messages(n,account_id,provider_id,provider_message_id,locator,identity_valid,metadata,folder) select x,?,?,printf('invented-%d',x),?,1,'{}','' from seq`, count, fixtureAccount.ID, fixtureAccount.ProviderID, locator)
					if e != nil {
						return e
					}
					_, e = tx.ExecContext(ctx, `insert into memberships select account_id,provider_id,n,? from messages`, c.Ref.CollectionID)
					if e != nil {
						return e
					}
					_, e = tx.ExecContext(ctx, `insert into imap_locators(n,account_id,provider_id,collection_id,mailbox_id,uidvalidity,uid,epoch_generation) select n,account_id,provider_id,?,'mailbox-a',10,n,1 from messages`, c.Ref.CollectionID)
					return e
				}
				if e = insert(count); e != nil {
					t.Fatal(e)
				}
				if e = tx.Commit(); e != nil {
					t.Fatal(e)
				}
				rows, e := a.ReadIMAPInventory(ctx, c.Ref, epoch)
				if scenario == "rows" {
					if e != nil || len(rows) != count {
						t.Fatalf("exact cap %d %v", len(rows), e)
					}
					r := fixtureRecord("overflow-row")
					r.IMAP = &IMAPLocator{CollectionID: c.Ref.CollectionID, MailboxID: "mailbox-a", UIDValidity: 10, UID: uint32(count + 1), EpochGeneration: 1}
					if _, e = a.UpsertMessage(ctx, c, r); e != nil {
						t.Fatal(e)
					}
				} else {
					if e != nil || len(rows) != count {
						t.Fatalf("below aggregate limit %d %v", len(rows), e)
					}
					if _, e = a.store.db.ExecContext(ctx, `update messages set provider_message_id=printf('%08192d',n)`); e != nil {
						t.Fatal(e)
					}
				}
			case "missing locator":
				if _, e := a.store.db.ExecContext(ctx, "update imap_locators set valid=0"); e != nil {
					t.Fatal(e)
				}
			case "wrong tuple":
				if _, e := a.store.db.ExecContext(ctx, "update imap_locators set epoch_generation=2"); e != nil {
					t.Fatal(e)
				}
			case "unknown collection":
				c.Ref.CollectionID = "absent"
			case "Unicode bytes":
				if _, e := a.store.db.ExecContext(ctx, "update messages set locator=? where n=?", strings.Repeat("界", 2731), n.N); e != nil {
					t.Fatal(e)
				}
			}
			before := inventoryStateDigest(t, a)
			out, e := a.ReadIMAPInventory(ctx, c.Ref, epoch)
			if e == nil || out != nil {
				t.Fatalf("unsafe inventory succeeded %d %v", len(out), e)
			}
			if inventoryStateDigest(t, a) != before {
				t.Fatal("read failure changed database")
			}
		})
	}
}

// Stream actual values with type/NULL boundaries, rather than issuing a query
// per cell. This test measurement still covers every row in the fixture tables.
func inventoryStateDigest(t *testing.T, a *AccountStore) string {
	t.Helper()
	h := sha256.New()
	for _, table := range []string{"store_binding", "collections", "messages", "memberships", "imap_locators", "provider_sync_state", "sync_passes", "sync_pages", "sync_seen", "verdicts", "plans", "plan_items", "actions"} {
		_, _ = fmt.Fprintln(h, table)
		rows, e := a.store.db.QueryContext(context.Background(), "select * from "+table+" order by rowid")
		if e != nil {
			t.Fatal(e)
		}
		cols, e := rows.Columns()
		if e != nil {
			t.Fatal(e)
		}
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		for rows.Next() {
			if e = rows.Scan(ptrs...); e != nil {
				t.Fatal(e)
			}
			for _, v := range values {
				b, e := json.Marshal(v)
				if e != nil {
					t.Fatal(e)
				}
				_, _ = fmt.Fprintf(h, "%T:%d:", v, len(b))
				_, _ = h.Write(b)
			}
			_, _ = h.Write([]byte("\n"))
		}
		if e = errors.Join(rows.Err(), rows.Close()); e != nil {
			t.Fatal(e)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (c *inventoryConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	var stmt driver.Stmt
	var e error
	if prepare, ok := c.Conn.(driver.ConnPrepareContext); ok {
		stmt, e = prepare.PrepareContext(ctx, q)
	} else {
		stmt, e = c.Conn.Prepare(q)
	}
	if e != nil {
		return nil, e
	}
	return &inventoryStmt{Stmt: stmt, capture: c.capture}, nil
}

type inventoryStmt struct {
	driver.Stmt
	capture *inventoryCapture
}

func (s *inventoryStmt) Query(args []driver.Value) (driver.Rows, error) {
	r, e := s.Stmt.Query(args)
	if e != nil {
		return nil, e
	}
	return &inventoryRows{Rows: r, capture: s.capture}, nil
}
func (s *inventoryStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if query, ok := s.Stmt.(driver.StmtQueryContext); ok {
		r, e := query.QueryContext(ctx, args)
		if e != nil {
			return nil, e
		}
		return &inventoryRows{Rows: r, capture: s.capture}, nil
	}
	values := make([]driver.Value, len(args))
	for i, a := range args {
		values[i] = a.Value
	}
	return s.Query(values)
}
func TestAccountIMAPInventoryCancellationAndAllocations(t *testing.T) {
	for _, phase := range []int{7, 4} {
		t.Run(fmt.Sprintf("cancel projection width %d", phase), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a, c, _ := epochFixture(t)
			before := inventoryStateDigest(t, a)
			if e := a.store.db.Close(); e != nil {
				t.Fatal(e)
			}
			observed := false
			cap := &inventoryCapture{observe: func(cols []string, v []driver.Value) {
				if len(v) == phase {
					observed = true
					cancel()
				}
			}}
			name := "invented-cancel-" + randomID()
			sql.Register(name, inventoryDriver{cap})
			db, e := sql.Open(name, sqliteURI(a.store.life.entry.path, "rw", false))
			if e != nil {
				t.Fatal(e)
			}
			db.SetMaxOpenConns(1)
			a.store.db = db
			out, e := a.ReadIMAPInventory(ctx, c.Ref, IMAPEpoch{MailboxID: "mailbox-a", UIDValidity: 10, Generation: 1})
			if !observed || !errors.Is(e, context.Canceled) || out != nil {
				t.Fatalf("cancellation %v %v", observed, e)
			}
			cap.observe = nil
			if inventoryStateDigest(t, a) != before {
				t.Fatal("canceled reader changed state")
			}
			if _, e = a.ReadIMAPInventory(context.Background(), c.Ref, IMAPEpoch{MailboxID: "mailbox-a", UIDValidity: 10, Generation: 1}); e != nil {
				t.Fatal("rows/transaction leaked", e)
			}
		})
	}
}

func TestAccountIMAPInventoryAllocationEvidence(t *testing.T) {
	a, c, n := epochFixture(t)
	ctx := context.Background()
	epoch := IMAPEpoch{MailboxID: "mailbox-a", UIDValidity: 10, Generation: 1}
	for _, bodyBytes := range []int{1024, 4 << 20} {
		if e := a.SetBody(ctx, n, strings.Repeat("b", bodyBytes)); e != nil {
			t.Fatal(e)
		}
		if _, e := a.ReadIMAPInventory(ctx, c.Ref, epoch); e != nil {
			t.Fatal(e)
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for i := 0; i < 4; i++ {
			rows, e := a.ReadIMAPInventory(ctx, c.Ref, epoch)
			if e != nil || len(rows) != 1 || cap(rows) != 1 {
				t.Fatal("unbounded result allocation", e)
			}
		}
		runtime.ReadMemStats(&after)
		t.Logf("cached_body_bytes=%d four_real_inventory_calls_allocated=%d (includes SQLite engine; supplementary, not an exact heap ceiling)", bodyBytes, after.TotalAlloc-before.TotalAlloc)
	}
}

func attachInventoryDriver(t *testing.T, a *AccountStore, capture *inventoryCapture) {
	t.Helper()
	if e := a.store.db.Close(); e != nil {
		t.Fatal(e)
	}
	name := "invented-projection-" + randomID()
	sql.Register(name, inventoryDriver{capture})
	db, e := sql.Open(name, sqliteURI(a.store.life.entry.path, "rw", false))
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	a.store.db = db
}
func TestAccountIMAPInventoryExactAggregateBoundary(t *testing.T) {
	a, c, _ := epochFixture(t)
	ctx := context.Background()
	base := 256 + 2*len(fixtureAccount.ID) + len(c.Ref.CollectionID) + len("mailbox-a")
	fullCharge := base + 2*IMAPInventoryMaxFieldBytes
	full := (IMAPInventoryMaxBytes - base - 2) / fullCharge
	lastLocator := IMAPInventoryMaxBytes - full*fullCharge - base - 1
	tx, e := a.store.db.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	for _, q := range []string{"delete from memberships", "delete from imap_locators", "delete from verdicts", "delete from messages"} {
		if _, e = tx.ExecContext(ctx, q); e != nil {
			t.Fatal(e)
		}
	}
	_, e = tx.ExecContext(ctx, `with recursive seq(x) as (select 1 union all select x+1 from seq where x<?) insert into messages(n,account_id,provider_id,provider_message_id,locator,identity_valid,metadata,folder) select x,?,?,case when x=? then 'z' else printf('%08192d',x) end,case when x=? then ? else ? end,1,'{}','' from seq`, full+1, fixtureAccount.ID, fixtureAccount.ProviderID, full+1, full+1, strings.Repeat("x", lastLocator), strings.Repeat("x", 8192))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.ExecContext(ctx, "insert into memberships select account_id,provider_id,n,? from messages", c.Ref.CollectionID); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.ExecContext(ctx, `insert into imap_locators(n,account_id,provider_id,collection_id,mailbox_id,uidvalidity,uid,epoch_generation) select n,account_id,provider_id,?,'mailbox-a',10,n,1 from messages`, c.Ref.CollectionID); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	epoch := IMAPEpoch{MailboxID: "mailbox-a", UIDValidity: 10, Generation: 1}
	rows, e := a.ReadIMAPInventory(ctx, c.Ref, epoch)
	if e != nil || len(rows) != full+1 {
		t.Fatalf("exact aggregate %d %v", len(rows), e)
	}
	charged := 0
	for _, r := range rows {
		charged += base + len(r.Remote.MessageID) + len(r.Remote.Locator)
	}
	if charged != IMAPInventoryMaxBytes {
		t.Fatalf("wrong fixture charge %d", charged)
	}
	rows = nil
	if _, e = a.store.db.ExecContext(ctx, "update messages set locator=locator||'x' where n=?", full+1); e != nil {
		t.Fatal(e)
	}
	out, e := a.ReadIMAPInventory(ctx, c.Ref, epoch)
	var limit *IMAPInventoryLimitError
	if !errors.As(e, &limit) || limit.Dimension != "aggregate_bytes" || out != nil {
		t.Fatal("cap+1 accepted", e)
	}
}
