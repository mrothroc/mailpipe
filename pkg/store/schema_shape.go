package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
)

type columnShape struct {
	Name, Type string
	NotNull    int
	Default    sql.NullString
	PK         int
}
type tableShape struct {
	Columns    []columnShape
	References []string
	Unique     []string
}

var schemaModel struct {
	sync.Once
	tables  map[string]tableShape
	indexes map[string]string
	err     error
}

func readTableShape(ctx context.Context, q sqlAccess, name string) (out tableShape, err error) {
	rows, e := q.QueryContext(ctx, "pragma table_info("+quote(name)+")")
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var cid int
		var c columnShape
		if e = rows.Scan(&cid, &c.Name, &c.Type, &c.NotNull, &c.Default, &c.PK); e != nil {
			break
		}
		c.Type = strings.ToLower(c.Type)
		out.Columns = append(out.Columns, c)
	}
	err = errors.Join(e, rows.Err())
	rows.Close()
	if err != nil {
		return
	}
	rows, e = q.QueryContext(ctx, "select \"table\",\"from\",\"to\",on_update,on_delete,match from pragma_foreign_key_list(?) order by id,seq", name)
	if e != nil {
		err = e
		return
	}
	for rows.Next() {
		var fields [6]string
		if e = rows.Scan(&fields[0], &fields[1], &fields[2], &fields[3], &fields[4], &fields[5]); e != nil {
			break
		}
		out.References = append(out.References, strings.Join(fields[:], "|"))
	}
	err = errors.Join(e, rows.Err())
	rows.Close()
	if err != nil {
		return
	}
	// Compare automatic uniqueness too: primary keys/foreign keys must not be
	// silently weakened by a table reconstructed with the same column names.
	rows, e = q.QueryContext(ctx, "select name from pragma_index_list(?) where \"unique\"=1 order by name", name)
	if e != nil {
		err = e
		return
	}
	var indexes []string
	for rows.Next() {
		var idx string
		if e = rows.Scan(&idx); e != nil {
			break
		}
		indexes = append(indexes, idx)
	}
	err = errors.Join(e, rows.Err())
	rows.Close()
	if err != nil {
		return
	}
	for _, idx := range indexes {
		rs, e := q.QueryContext(ctx, "select coalesce(name,'') from pragma_index_info(?) order by seqno", idx)
		if e != nil {
			err = e
			return
		}
		var names []string
		for rs.Next() {
			var n string
			if e = rs.Scan(&n); e != nil {
				break
			}
			names = append(names, n)
		}
		err = errors.Join(e, rs.Err())
		rs.Close()
		if err != nil {
			return
		}
		out.Unique = append(out.Unique, strings.Join(names, ","))
	}
	sort.Strings(out.References)
	sort.Strings(out.Unique)
	return
}
func expectedTables() (map[string]tableShape, error) {
	schemaModel.Do(func() {
		// An in-memory schema built solely from constants models the pinned SQLite
		// engine's metadata. It never opens a filesystem database or user resource.
		db, e := sql.Open("sqlite", ":memory:")
		if e != nil {
			schemaModel.err = e
			return
		}
		db.SetMaxOpenConns(1)
		defer func() { schemaModel.err = errors.Join(schemaModel.err, db.Close()) }()
		ctx := context.Background()
		if _, e = db.ExecContext(ctx, schema); e != nil {
			schemaModel.err = e
			return
		}
		for _, table := range []string{"plans", "plan_items", "actions"} {
			cols, e := tableColumns(ctx, db, table)
			if e != nil {
				schemaModel.err = e
				return
			}
			for _, ddl := range addedColumns[table] {
				if !hasColumn(cols, strings.Fields(ddl)[0]) {
					if _, e = db.ExecContext(ctx, "alter table "+table+" add column "+ddl); e != nil {
						schemaModel.err = e
						return
					}
				}
			}
		}
		if _, e = db.ExecContext(ctx, providerSchema); e != nil {
			schemaModel.err = e
			return
		}
		specs, e := tableSpecs(ctx, db)
		if e != nil {
			schemaModel.err = e
			return
		}
		schemaModel.indexes = map[string]string{}
		rows, err := db.QueryContext(ctx, "select name,sql from sqlite_schema where type='index' and name not like 'sqlite_%'")
		if err != nil {
			schemaModel.err = err
			return
		}
		for rows.Next() {
			var name, ddl string
			if err = rows.Scan(&name, &ddl); err != nil {
				break
			}
			schemaModel.indexes[name] = normalizeIndex(ddl)
		}
		schemaModel.err = errors.Join(err, rows.Err())
		rows.Close()
		if schemaModel.err != nil {
			return
		}
		schemaModel.tables = map[string]tableShape{}
		for _, spec := range specs {
			shape, e := readTableShape(ctx, db, spec.Name)
			if e != nil {
				schemaModel.err = e
				return
			}
			schemaModel.tables[spec.Name] = shape
		}
	})
	return schemaModel.tables, schemaModel.err
}
func validateShapes(ctx context.Context, q sqlAccess, version int) (repair bool, err error) {
	expected, e := expectedTables()
	if e != nil {
		return false, e
	}
	actual, e := tableSpecs(ctx, q)
	if e != nil {
		return false, e
	}
	if len(actual) == 0 {
		if version != 0 {
			return false, errors.New("store: current schema is missing")
		}
		return true, nil
	}
	legacyCols := map[string]bool{"account_id": true, "provider_id": true, "provider_message_id": true, "locator": true, "revision": true, "identity_valid": true, "email_id": true, "content_digest": true, "metadata": true}
	for _, spec := range actual {
		if version == 0 {
			switch spec.Name {
			case "messages", "verdicts", "plans", "plan_items", "actions", "sync_state":
			default:
				return false, errors.New("store: unversioned provider schema is not a legacy database")
			}
		}
		want, ok := expected[spec.Name]
		if !ok {
			return false, errors.New("store: unknown table")
		}
		shape, e := readTableShape(ctx, q, spec.Name)
		if e != nil {
			return false, e
		}
		oldMessages := spec.Name == "messages" && !hasColumn(spec.Columns, "account_id")
		if version == 0 && hasColumn(spec.Columns, "account_id") {
			return false, errors.New("store: unversioned account identity columns")
		}
		if oldMessages {
			if version != 0 {
				return false, errors.New("store: current message identity schema missing")
			}
			var cols []columnShape
			for _, c := range want.Columns {
				if !legacyCols[c.Name] {
					if c.Name == "graph_id" {
						c.NotNull = 1
					}
					cols = append(cols, c)
				}
			}
			want.Columns = cols
			want.Unique = []string{"graph_id"}
			repair = true
		}
		if version == 0 && (spec.Name == "plans" || spec.Name == "plan_items" || spec.Name == "actions") || spec.Name == "plan_items" {
			// Exactly known old plan-item layouts can be repaired even after the
			// original regression fixture deliberately reconstructs the old table.
			var cols []columnShape
			for _, c := range want.Columns {
				if hasColumn(spec.Columns, c.Name) {
					cols = append(cols, c)
				} else {
					permitted := false
					for _, ddl := range addedColumns[spec.Name] {
						if strings.Fields(ddl)[0] == c.Name {
							permitted = true
							break
						}
					}
					if !permitted {
						return false, errors.New("store: required historical column missing")
					}
					repair = true
				}
			}
			want.Columns = cols
		}
		if !reflect.DeepEqual(shape.Columns, want.Columns) || !reflect.DeepEqual(shape.References, want.References) || !reflect.DeepEqual(shape.Unique, want.Unique) {
			return false, errors.New("store: unrecognized table shape; reconcile before migration")
		}
	}
	for name := range expected {
		present := false
		for _, a := range actual {
			if a.Name == name {
				present = true
				break
			}
		}
		if !present {
			if version == 0 {
				if name == "messages" || name == "plans" || name == "plan_items" || name == "actions" || name == "verdicts" || name == "sync_state" {
					return false, errors.New("store: incomplete legacy schema")
				}
				repair = true
			} else {
				return false, errors.New("store: required provider table missing")
			}
		}
	}
	return repair, nil
}

func normalizeIndex(ddl string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(ddl, `"`, "")), " "))
}
func validateIndexes(ctx context.Context, q sqlAccess, version int) error {
	if _, e := expectedTables(); e != nil {
		return e
	}
	rows, e := q.QueryContext(ctx, "select name,sql from sqlite_schema where type='index' and name not like 'sqlite_%'")
	if e != nil {
		return e
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var name, ddl string
		if e = rows.Scan(&name, &ddl); e != nil {
			return e
		}
		want, ok := schemaModel.indexes[name]
		if !ok || want != normalizeIndex(ddl) {
			return errors.New("store: unrecognized index definition")
		}
		seen[name] = true
	}
	if e = rows.Err(); e != nil {
		return e
	}
	if version == schemaVersion {
		for name := range schemaModel.indexes {
			if !seen[name] {
				return errors.New("store: required index missing")
			}
		}
	}
	return nil
}
