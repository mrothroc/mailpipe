package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mrothroc/mailpipe/pkg/mailbox"
	"golang.org/x/sys/unix"
)

const schemaVersion = 1
const providerSchema = `
create table if not exists store_binding(singleton integer primary key check(singleton=1),account_id text not null,provider_id text not null,generation integer not null check(generation>0),bound_at text not null);
create table if not exists collections(account_id text not null,provider_id text not null,collection_id text not null,name text not null,mailbox_id text not null default '',uidvalidity integer not null default 0,epoch_generation integer not null default 1,primary key(account_id,provider_id,collection_id));
create table if not exists memberships(account_id text not null,provider_id text not null,n integer not null,collection_id text not null,primary key(account_id,provider_id,n,collection_id),foreign key(n,account_id,provider_id) references messages(n,account_id,provider_id),foreign key(account_id,provider_id,collection_id) references collections(account_id,provider_id,collection_id));
create table if not exists imap_locators(n integer not null references messages(n),account_id text not null,provider_id text not null,collection_id text not null,mailbox_id text not null,uidvalidity integer not null check(uidvalidity between 1 and 4294967295),uid integer not null check(uid between 1 and 4294967295),epoch_generation integer not null,valid integer not null default 1,primary key(n,epoch_generation));
create unique index if not exists imap_active on imap_locators(account_id,provider_id,mailbox_id,uidvalidity,uid) where valid=1;
create table if not exists provider_sync_state(account_id text not null,provider_id text not null,collection_id text not null,checkpoint text not null default '',generation integer not null default 0,identity_epoch integer not null,primary key(account_id,provider_id,collection_id),foreign key(account_id,provider_id,collection_id) references collections(account_id,provider_id,collection_id));
create table if not exists sync_passes(id text primary key,owner text not null,account_id text not null,provider_id text not null,collection_id text not null,input text not null,generation integer not null,epoch integer not null,mode text not null default '',next text not null default '',complete integer not null default 0,checkpoint text not null default '');
create table if not exists sync_seen(pass_id text not null references sync_passes(id) on delete cascade,message_id text not null,primary key(pass_id,message_id));
create table if not exists sync_pages(pass_id text not null references sync_passes(id) on delete cascade,ordinal integer not null,page text not null,supplements text not null,primary key(pass_id,ordinal));
`

var addedColumns = map[string][]string{
	"plans":      {"account_id text not null default ''", "provider_id text not null default ''", "binding_generation integer not null default 0"},
	"plan_items": {"planned_folder text not null default ''", "no_block integer not null default 0", "account_id text not null default ''", "provider_id text not null default ''", "binding_generation integer not null default 0", "expected_state text not null default ''", "expected_epochs text not null default ''"},
	"actions":    {"account_id text not null default ''", "provider_id text not null default ''", "binding_generation integer not null default 0", "expected_state text not null default ''", "result_state text not null default ''"},
}

// sqlAccess is implemented by the real connection/transaction and test-only
// fault wrappers. Production callers always use the actual SQLite connection.
type sqlAccess interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func sqliteURI(path, mode string, offline bool) string {
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"mode": {mode}}
	if offline {
		q.Set("immutable", "1")
	} else {
		q["_pragma"] = []string{"busy_timeout(5000)", "foreign_keys(1)", "synchronous(FULL)"}
	}
	u.RawQuery = q.Encode()
	return u.String()
}
func sqliteOpen(ctx context.Context, path string) (*sql.DB, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlite", sqliteURI(path, "rw", false))
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
func tableColumns(ctx context.Context, q sqlAccess, table string) ([]string, error) {
	rows, e := q.QueryContext(ctx, "select name from pragma_table_info(?) order by cid", table)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if e = rows.Scan(&s); e != nil {
			return nil, e
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
func hasColumn(cols []string, c string) bool {
	for _, v := range cols {
		if v == c {
			return true
		}
	}
	return false
}
func schemaCurrent(ctx context.Context, q sqlAccess) (bool, error) {
	var version int
	if e := q.QueryRowContext(ctx, "pragma user_version").Scan(&version); e != nil {
		return false, e
	}
	if version > schemaVersion || version < 0 {
		return false, errors.New("store: unsupported schema version")
	}
	names, e := q.QueryContext(ctx, "select type,name from sqlite_schema where name not like 'sqlite_%' order by name")
	if e != nil {
		return false, e
	}
	known := map[string]bool{"messages": true, "sync_state": true, "verdicts": true, "plans": true, "plan_items": true, "actions": true, "store_binding": true, "collections": true, "memberships": true, "imap_locators": true, "provider_sync_state": true, "sync_passes": true, "sync_pages": true, "sync_seen": true}
	for names.Next() {
		var typ, name string
		if e = names.Scan(&typ, &name); e != nil {
			break
		}
		if (typ == "index" && !knownIndex(name)) || (typ != "index" && (typ != "table" || !known[name])) {
			e = errors.New("store: unknown schema objects; reconcile before migration")
			break
		}
	}
	names.Close()
	if e != nil {
		return false, e
	}
	if e = names.Err(); e != nil {
		return false, e
	}
	if e := validateIndexes(ctx, q, version); e != nil {
		return false, e
	}
	repair, e := validateShapes(ctx, q, version)
	if e != nil {
		return false, e
	}
	if repair {
		return false, nil
	}
	if version != schemaVersion {
		return false, nil
	}
	required := map[string][]string{"messages": {"n", "graph_id", "account_id", "provider_id", "provider_message_id", "locator", "revision", "identity_valid", "email_id", "content_digest", "metadata"}, "store_binding": {"singleton", "account_id", "provider_id", "generation", "bound_at"}, "collections": {"account_id", "provider_id", "collection_id", "epoch_generation"}, "memberships": {"n", "collection_id"}, "imap_locators": {"uidvalidity", "uid"}, "provider_sync_state": {"checkpoint", "generation"}, "sync_passes": {"owner", "epoch"}, "sync_pages": {"page"}}
	for t, cols := range required {
		actual, err := tableColumns(ctx, q, t)
		if err != nil {
			return false, err
		}
		for _, c := range cols {
			if !hasColumn(actual, c) {
				return false, errors.New("store: current schema is inconsistent")
			}
		}
	}
	for table, ddls := range addedColumns {
		cols, err := tableColumns(ctx, q, table)
		if err != nil {
			return false, err
		}
		for _, ddl := range ddls {
			if !hasColumn(cols, strings.Fields(ddl)[0]) {
				if table == "plan_items" {
					return false, nil
				}
				return false, errors.New("store: current schema is inconsistent")
			}
		}
	}
	var mode string
	if e = q.QueryRowContext(ctx, "pragma journal_mode").Scan(&mode); e != nil {
		return false, e
	}
	return strings.EqualFold(mode, "wal"), nil
}
func readBinding(ctx context.Context, q sqlAccess) (mailbox.Account, int64, bool, error) {
	var a mailbox.Account
	var generation int64
	e := q.QueryRowContext(ctx, "select account_id,provider_id,generation from store_binding where singleton=1").Scan(&a.ID, &a.ProviderID, &generation)
	if errors.Is(e, sql.ErrNoRows) {
		return a, 0, false, nil
	}
	return a, generation, e == nil, e
}
func matchBinding(ctx context.Context, q sqlAccess, want *mailbox.Account) (bool, error) {
	a, _, bound, e := readBinding(ctx, q)
	if e != nil {
		return false, e
	}
	if e = validateAttribution(ctx, q, a, bound); e != nil {
		return false, e
	}
	if want == nil {
		if bound {
			return false, errors.New("store: bound database requires account-aware opener")
		}
		return true, nil
	}
	if !bound {
		return false, nil
	}
	if a != *want {
		return false, &mailbox.AccountMismatchError{Operation: "open storage", Expected: want.ID, Actual: a.ID}
	}
	return true, nil
}
func openStore(ctx context.Context, path string, want *mailbox.Account) (*Store, error) {
	if want != nil {
		if e := want.Validate(); e != nil {
			return nil, e
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		waitCtx := ctx
		cancel := func() {}
		if want == nil {
			waitCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
		}
		life, e := acquireLifecycle(waitCtx, path, false, diskFiles{})
		cancel()
		if e != nil {
			return nil, e
		}
		path = life.entry.path
		_, exists, e := namespace(life.entry.fs, path)
		if e != nil {
			life.release()
			return nil, e
		}
		var db *sql.DB
		current := false
		if exists {
			var s fileStat
			s, e = life.entry.fs.stat(path, false)
			if e == nil && s.size > 0 {
				db, e = sqliteOpen(ctx, path)
				if e == nil {
					current, e = schemaCurrent(ctx, db)
				}
			}
		}
		if e == nil && current {
			var matched bool
			matched, e = matchBinding(ctx, db, want)
			if e == nil && matched {
				if e = life.validate(); e == nil {
					return &Store{db: db, life: life}, nil
				}
			}
			if e == nil {
				current = false
			}
		}
		if db != nil {
			if ce := db.Close(); ce != nil {
				return nil, life.uncertain(errors.Join(e, ce))
			}
		}
		releaseErr := life.release()
		if e != nil {
			return nil, e
		}
		if releaseErr != nil {
			return nil, releaseErr
		}
		life, e = acquireLifecycle(ctx, path, true, diskFiles{})
		if e != nil {
			return nil, e
		}
		e = maintain(ctx, life, want)
		if e != nil {
			life.release()
			return nil, e
		}
		if e = life.release(); e != nil {
			return nil, e
		}
	}
	return nil, errors.New("store: storage changed during opening; retry after maintenance")
}
func maintain(ctx context.Context, life *lifecycleLease, want *mailbox.Account) (result error) {
	if e := life.validate(); e != nil {
		return e
	}
	if e := life.createMain(); e != nil {
		return e
	}
	db, e := sqliteOpen(ctx, life.entry.path)
	if e != nil {
		return e
	}
	defer func() {
		if ce := db.Close(); ce != nil {
			result = life.uncertain(errors.Join(result, ce))
		}
	}()
	current, e := schemaCurrent(ctx, db)
	if e != nil {
		return e
	}
	if !current {
		var count int
		if e = db.QueryRowContext(ctx, "select count(*) from sqlite_schema where type='table' and name not like 'sqlite_%'").Scan(&count); e != nil {
			return e
		}
		conn, e := db.Conn(ctx)
		if e != nil {
			return e
		}
		e = func() (err error) {
			defer func() { err = errors.Join(err, conn.Close()) }()
			expected := ""
			if count > 0 {
				_, expected, err = backupDatabase(ctx, life, conn)
				if err != nil {
					return err
				}
			}
			if _, err = conn.ExecContext(ctx, "pragma journal_mode=WAL"); err != nil {
				return err
			}
			return migrateSchema(ctx, conn, expected)
		}()
		if e != nil {
			return e
		}
	}
	if want != nil {
		matched, e := matchBinding(ctx, db, want)
		if e != nil {
			return e
		}
		if !matched {
			var count int
			if e = db.QueryRowContext(ctx, `select (select count(*) from messages)+(select count(*) from verdicts)+(select count(*) from plans)+(select count(*) from actions)+(select count(*) from sync_state)`).Scan(&count); e != nil {
				return e
			}
			if count != 0 {
				return errors.New("store: historical database requires explicit verified legacy binding")
			}
			conn, e := db.Conn(ctx)
			if e != nil {
				return e
			}
			defer conn.Close()
			if _, _, e = backupDatabase(ctx, life, conn); e != nil {
				return e
			}
			if e = immediate(ctx, conn, func(q sqlAccess) error {
				_, e := q.ExecContext(ctx, "insert into store_binding values(1,?,?,1,?)", want.ID, want.ProviderID, time.Now().UTC().Format(time.RFC3339))
				return e
			}); e != nil {
				return e
			}
		}
	}
	return nil
}
func immediate(ctx context.Context, q sqlAccess, work func(sqlAccess) error) (err error) {
	if _, err = q.ExecContext(ctx, "begin immediate"); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = q.ExecContext(cleanup, "rollback")
	}()
	if err = work(q); err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, "commit")
	return err
}
func integrity(ctx context.Context, q sqlAccess) error {
	var result string
	if e := q.QueryRowContext(ctx, "pragma integrity_check").Scan(&result); e != nil {
		return e
	}
	if result != "ok" {
		return errors.New("store: database integrity check failed")
	}
	rows, e := q.QueryContext(ctx, "pragma foreign_key_check")
	if e != nil {
		return e
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("store: database references are inconsistent")
	}
	return rows.Err()
}
func migrateSchema(ctx context.Context, q sqlAccess, expected string) (err error) {
	cols, e := tableColumns(ctx, q, "messages")
	if e != nil {
		return e
	}
	tables, e := tableSpecs(ctx, q)
	if e != nil {
		return e
	}
	before, e := dataDigest(ctx, q, tables)
	if e != nil {
		return e
	}
	if _, err = q.ExecContext(ctx, "pragma foreign_keys=OFF"); err != nil {
		return err
	}
	var fk int
	if err = q.QueryRowContext(ctx, "pragma foreign_keys").Scan(&fk); err != nil || fk != 0 {
		return errors.New("store: cannot isolate migration references")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, e := q.ExecContext(cleanup, "pragma foreign_keys=ON")
		var enabled int
		if e == nil {
			e = q.QueryRowContext(cleanup, "pragma foreign_keys").Scan(&enabled)
			if e == nil && enabled != 1 {
				e = errors.New("store: reference enforcement unavailable")
			}
		}
		err = errors.Join(err, e)
	}()
	return immediate(ctx, q, func(q sqlAccess) error {
		if expected != "" {
			actual, e := logicalDigest(ctx, q)
			if e != nil {
				return e
			}
			if actual != expected {
				return stale("backup no longer matches source")
			}
		}
		if len(cols) > 0 && !hasColumn(cols, "account_id") {
			// Rebuild the parent without renaming the old parent and rewriting FKs.
			end := strings.Index(schema, "create unique index")
			create := strings.Replace(schema[:end], "create table if not exists messages", "create table messages_replacement", 1)
			if _, e = q.ExecContext(ctx, create); e != nil {
				return e
			}
			quoted := quoteColumns(cols)
			if _, e = q.ExecContext(ctx, "insert into messages_replacement("+quoted+") select "+quoted+" from messages"); e != nil {
				return e
			}
			if _, e = q.ExecContext(ctx, "drop table messages"); e != nil {
				return e
			}
			if _, e = q.ExecContext(ctx, "alter table messages_replacement rename to messages"); e != nil {
				return e
			}
		}
		if _, e = q.ExecContext(ctx, schema); e != nil {
			return e
		}
		for _, table := range []string{"plans", "plan_items", "actions"} {
			actual, e := tableColumns(ctx, q, table)
			if e != nil {
				return e
			}
			for _, ddl := range addedColumns[table] {
				if !hasColumn(actual, strings.Fields(ddl)[0]) {
					if _, e = q.ExecContext(ctx, "alter table "+table+" add column "+ddl); e != nil {
						return e
					}
				}
			}
		}
		if _, e = q.ExecContext(ctx, providerSchema); e != nil {
			return e
		}
		after, e := dataDigest(ctx, q, tables)
		if e != nil {
			return e
		}
		if before != after {
			return errors.New("store: migration did not preserve historical data")
		}
		if e = integrity(ctx, q); e != nil {
			return e
		}
		_, e = q.ExecContext(ctx, fmt.Sprintf("pragma user_version=%d", schemaVersion))
		return e
	})
}

type tableSpec struct {
	Name    string
	Columns []string
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func quoteColumns(cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = quote(c)
	}
	return strings.Join(out, ",")
}
func tableSpecs(ctx context.Context, q sqlAccess) ([]tableSpec, error) {
	rows, e := q.QueryContext(ctx, "select name from sqlite_schema where type='table' and name not like 'sqlite_%' order by name")
	if e != nil {
		return nil, e
	}
	var tables []tableSpec
	for rows.Next() {
		var name string
		if e = rows.Scan(&name); e != nil {
			rows.Close()
			return nil, e
		}
		tables = append(tables, tableSpec{Name: name})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	for i := range tables {
		tables[i].Columns, e = tableColumns(ctx, q, tables[i].Name)
		if e != nil {
			return nil, e
		}
	}
	return tables, nil
}
func hashPart(h hash.Hash, v any) { b, _ := json.Marshal(v); fmt.Fprintf(h, "%d:", len(b)); h.Write(b) }
func digestRows(ctx context.Context, q sqlAccess, h hash.Hash, tables []tableSpec) error {
	for _, t := range tables {
		hashPart(h, t)
		var last int64
		first := true
		for {
			if e := ctx.Err(); e != nil {
				return e
			}
			var row int64
			var e error
			if first {
				e = q.QueryRowContext(ctx, "select rowid from "+quote(t.Name)+" order by rowid limit 1").Scan(&row)
				first = false
			} else {
				e = q.QueryRowContext(ctx, "select rowid from "+quote(t.Name)+" where rowid>? order by rowid limit 1", last).Scan(&row)
			}
			if errors.Is(e, sql.ErrNoRows) {
				break
			}
			if e != nil {
				return e
			}
			last = row
			hashPart(h, "row")
			for _, col := range t.Columns {
				var typ string
				var length int64
				e = q.QueryRowContext(ctx, "select typeof("+quote(col)+"),coalesce(length(cast("+quote(col)+" as blob)),0) from "+quote(t.Name)+" where rowid=?", row).Scan(&typ, &length)
				if e != nil {
					return e
				}
				hashPart(h, typ)
				hashPart(h, length)
				for offset := int64(1); offset <= length; offset += 32768 {
					if e = ctx.Err(); e != nil {
						return e
					}
					var chunk []byte
					e = q.QueryRowContext(ctx, "select substr(cast("+quote(col)+" as blob),?,32768) from "+quote(t.Name)+" where rowid=?", offset, row).Scan(&chunk)
					if e != nil {
						return e
					}
					h.Write(chunk)
				}
			}
		}
		hashPart(h, "end")
	}
	return nil
}
func dataDigest(ctx context.Context, q sqlAccess, t []tableSpec) (string, error) {
	h := sha256.New()
	if e := digestRows(ctx, q, h, t); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func logicalDigest(ctx context.Context, q sqlAccess) (string, error) {
	h := sha256.New()
	var version int
	if e := q.QueryRowContext(ctx, "pragma user_version").Scan(&version); e != nil {
		return "", e
	}
	hashPart(h, version)
	rows, e := q.QueryContext(ctx, "select type,name,coalesce(sql,'') from sqlite_schema where name not like 'sqlite_%' order by type,name")
	if e != nil {
		return "", e
	}
	for rows.Next() {
		var a, b, c string
		if e = rows.Scan(&a, &b, &c); e != nil {
			rows.Close()
			return "", e
		}
		hashPart(h, []string{a, b, c})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return "", e
	}
	tables, e := tableSpecs(ctx, q)
	if e != nil {
		return "", e
	}
	if e = digestRows(ctx, q, h, tables); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func offlineSnapshot(ctx context.Context, path string, life *lifecycleLease) (digest string, err error) {
	canonical, e := canonicalDatabase(path)
	if e != nil {
		return "", e
	}
	if life == nil || !life.entry.exclusive || life.entry.path != canonical {
		return "", errors.New("store: offline verification requires exclusive snapshot ownership")
	}
	path = life.entry.path
	if e := life.validate(); e != nil {
		return "", e
	}
	_, exists, e := namespace(diskFiles{}, path)
	if e != nil || !exists {
		return "", errors.Join(e, unsafeNamespace())
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, e = os.Lstat(path + suffix); !errors.Is(e, os.ErrNotExist) {
			return "", unsafeNamespace()
		}
	}
	db, e := sql.Open("sqlite", sqliteURI(path, "ro", true))
	if e != nil {
		return "", e
	}
	defer func() {
		if ce := db.Close(); ce != nil {
			err = errors.Join(err, life.uncertain(ce))
		}
	}()
	if e = integrity(ctx, db); e != nil {
		return "", e
	}
	return logicalDigest(ctx, db)
}
func backupDatabase(ctx context.Context, life *lifecycleLease, q sqlAccess) (string, string, error) {
	if !life.entry.exclusive {
		return "", "", &LifecycleBusyError{"backup"}
	}
	if e := life.validate(); e != nil {
		return "", "", e
	}
	dir, e := os.MkdirTemp(filepath.Dir(life.entry.path), filepath.Base(life.entry.path)+".backup-")
	if e != nil {
		return "", "", storageErr("backup directory", e)
	}
	path := filepath.Join(dir, "snapshot.db")
	fs := life.entry.fs
	d, e := fs.openat(unix.AT_FDCWD, dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return "", "", storageErr("backup directory", e)
	}
	defer fs.close(d)
	target, e := acquireLifecycle(ctx, path, true, fs)
	if e != nil {
		return "", "", e
	}
	defer target.release()
	if e = target.createMain(); e != nil {
		return "", "", e
	}
	if _, e = q.ExecContext(ctx, "vacuum into ?", path); e != nil {
		return "", "", e
	}
	digest, e := offlineSnapshot(ctx, path, target)
	if e != nil {
		return "", "", e
	}
	fd, e := openQuiescentSnapshot(target)
	if e != nil {
		return "", "", e
	}
	e = fs.sync(fd)
	e = errors.Join(e, fs.close(fd), fs.sync(d))
	if e != nil {
		return "", "", storageErr("persist backup", e)
	}
	receipt, _ := json.Marshal(map[string]string{"sha256": digest, "snapshot": "snapshot.db"})
	receipt = append(receipt, '\n')
	fd, e = fs.openat(d, "receipt.json", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return "", "", storageErr("backup receipt", e)
	}
	for len(receipt) > 0 {
		if e = ctx.Err(); e != nil {
			break
		}
		var n int
		n, e = fs.write(fd, receipt)
		if e != nil {
			break
		}
		if n == 0 {
			e = io.ErrShortWrite
			break
		}
		receipt = receipt[n:]
	}
	if e == nil {
		e = fs.sync(fd)
	}
	e = errors.Join(e, fs.close(fd), fs.sync(d))
	if e != nil {
		return "", "", storageErr("persist backup receipt", e)
	}
	return path, digest, nil
}

// Keep table iteration deterministic when constructing snapshots of epochs.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func knownIndex(name string) bool {
	switch name {
	case "messages_content_digest", "messages_graph", "messages_provider", "messages_emailid", "messages_imid", "messages_sender", "messages_folder", "messages_live", "verdicts_stage", "verdicts_n", "actions_plan", "imap_active":
		return true
	}
	return false
}

// The reader has closed and EX excludes all other participating SQL users of
// this snapshot. Serialize uncertain raw admissions with the quarantine latch.
func openQuiescentSnapshot(target *lifecycleLease) (int, error) {
	lifecycleManager.Lock()
	defer lifecycleManager.Unlock()
	if lifecycleManager.poison != nil {
		return -1, lifecycleManager.poison
	}
	if !target.entry.exclusive || target.retained {
		return -1, unsafeNamespace()
	}
	if e := validateEntry(target.entry); e != nil {
		return -1, e
	}
	fs := target.entry.fs
	before, e := fs.stat(target.entry.path, false)
	if e != nil {
		return -1, storageErr("snapshot metadata", e)
	}
	fd, e := fs.openat(target.entry.dir, filepath.Base(target.entry.path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return -1, storageErr("backup sync", e)
	}
	after, e1 := fs.fstat(fd)
	again, e2 := fs.stat(target.entry.path, false)
	if e1 != nil || e2 != nil || !safeRegular(after) || after.id != before.id || after.id != again.id || after.id != target.entry.db {
		lifecycleManager.quarantine = fd
		lifecycleManager.poison = unsafeNamespace()
		return -1, lifecycleManager.poison
	}
	return fd, nil
}

func validateAttribution(ctx context.Context, q sqlAccess, a mailbox.Account, bound bool) error {
	for _, table := range []string{"messages", "plans", "plan_items", "actions", "collections", "memberships", "imap_locators", "provider_sync_state", "sync_passes"} {
		var bad int
		var e error
		if bound {
			e = q.QueryRowContext(ctx, "select exists(select 1 from "+table+" where account_id<>? or provider_id<>?)", a.ID, a.ProviderID).Scan(&bad)
		} else {
			switch table {
			case "messages", "plans", "plan_items", "actions":
				e = q.QueryRowContext(ctx, "select exists(select 1 from "+table+" where account_id<>'' or provider_id<>'')").Scan(&bad)
			default:
				e = q.QueryRowContext(ctx, "select exists(select 1 from "+table+")").Scan(&bad)
			}
		}
		if e != nil {
			return e
		}
		if bad != 0 {
			return errors.New("store: inconsistent account attribution; reconcile before opening")
		}
	}
	if !bound {
		var bad int
		if e := q.QueryRowContext(ctx, "select exists(select 1 from messages where provider_message_id is not null or locator is not null or metadata is not null or identity_valid<>0)").Scan(&bad); e != nil {
			return e
		}
		if bad != 0 {
			return errors.New("store: unbound provider identities require reconciliation")
		}
	}
	return nil
}
