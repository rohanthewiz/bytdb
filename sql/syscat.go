package sql

// syscat.go: the virtual system catalog — pg_catalog and
// information_schema tables synthesized from the engine catalog, so
// ORMs and tools can introspect over the wire with the queries they
// already know. Virtual tables enter a query as materialized rows in
// the FROM scope and flow through the ordinary join, filter, and
// aggregate machinery; they are read-only and their names are
// reserved.
//
// OIDs: a table's oid is its engine table ID; an index's oid is
// tableID*1000 + its index ID, with the primary key's synthetic
// "<table>_pkey" index at tableID*1000. Namespace oids are Postgres's
// well-known 11 (pg_catalog) and 2200 (public). The catalog lists
// user tables only — the system tables do not describe themselves.

import (
	"fmt"
	"strings"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// tableLookup resolves a FROM name: a real table's descriptor, or a
// virtual table's descriptor plus its materialized rows (rows is
// non-nil exactly when the table is virtual).
type tableLookup func(name string) (desc *bytdb.TableDesc, rows [][]any)

// lookup layers the virtual system catalog under base (an Engine or
// Txn catalog): user tables win bare names; pg_catalog-qualified and
// information_schema-qualified names, and bare pg_catalog member
// names, resolve to synthesized tables.
func (d *DB) lookup(base func(string) *bytdb.TableDesc) tableLookup {
	return func(name string) (*bytdb.TableDesc, [][]any) {
		if !strings.Contains(name, ".") {
			if desc := base(name); desc != nil {
				return desc, nil
			}
		}
		if st := sysLookup(name); st != nil {
			rows := st.rows(d)
			if rows == nil {
				rows = [][]any{}
			}
			return st.desc, rows
		}
		return nil, nil
	}
}

// sysLookup finds a system table by canonical name
// ("pg_catalog.pg_class", "information_schema.tables") or by bare
// name for pg_catalog members — pg_catalog is on the search path, as
// in Postgres; information_schema is not.
func sysLookup(name string) *sysTableDef {
	if st, ok := sysTables[name]; ok {
		return st
	}
	if !strings.Contains(name, ".") {
		if st, ok := sysTables["pg_catalog."+name]; ok {
			return st
		}
	}
	return nil
}

// sysWriteGuard rejects DML and DDL aimed at (or named like) a
// system table — including creating anything in a system schema (a
// dotted name can only be one; the parser folds public. away).
func sysWriteGuard(table string) error {
	if table == "" {
		return nil
	}
	if sysLookup(table) != nil || strings.Contains(table, ".") {
		return serr.New("system catalog is read-only", "table", table)
	}
	return nil
}

// writeTarget is the table a statement writes to or defines; "" for
// SELECT (and DROP INDEX without an ON clause).
func writeTarget(st Statement) string {
	switch s := st.(type) {
	case *CreateTable:
		return s.Table
	case *DropTable:
		return s.Table
	case *AddColumn:
		return s.Table
	case *DropColumn:
		return s.Table
	case *CreateIndex:
		return s.Table
	case *DropIndex:
		return s.Table
	case *Insert:
		return s.Table
	case *Update:
		return s.Table
	case *Delete:
		return s.Table
	}
	return ""
}

type sysTableDef struct {
	desc *bytdb.TableDesc
	rows func(d *DB) [][]any
}

func sysDesc(name string, cols ...bytdb.Column) *bytdb.TableDesc {
	return &bytdb.TableDesc{Name: name, Columns: cols}
}

func sysCol(name string, t bytdb.ColType) bytdb.Column { return bytdb.Column{Name: name, Type: t} }

// Namespace oids (Postgres's well-known values).
const (
	oidPGCatalog  = int64(11)
	oidPublic     = int64(2200)
	oidInfoSchema = int64(13000)
)

// typeOID is the pg_type oid a column type presents as.
func typeOID(t bytdb.ColType) int64 {
	switch t {
	case bytdb.TBool:
		return 16
	case bytdb.TBytes:
		return 17
	case bytdb.TInt:
		return 20
	case bytdb.TFloat:
		return 701
	}
	return 25 // text
}

// typeLen is pg_type.typlen / pg_attribute.attlen.
func typeLen(t bytdb.ColType) int64 {
	switch t {
	case bytdb.TBool:
		return 1
	case bytdb.TInt, bytdb.TFloat:
		return 8
	}
	return -1
}

// sqlTypeName is information_schema.columns.data_type.
func sqlTypeName(t bytdb.ColType) string {
	switch t {
	case bytdb.TBool:
		return "boolean"
	case bytdb.TBytes:
		return "bytea"
	case bytdb.TInt:
		return "bigint"
	case bytdb.TFloat:
		return "double precision"
	}
	return "text"
}

// udtName is the pg_type name behind a column type
// (information_schema.columns.udt_name).
func udtName(t bytdb.ColType) string {
	switch t {
	case bytdb.TBool:
		return "bool"
	case bytdb.TBytes:
		return "bytea"
	case bytdb.TInt:
		return "int8"
	case bytdb.TFloat:
		return "float8"
	}
	return "text"
}

// userDescs is the catalog snapshot the virtual tables render, in
// stable (sorted-name) order.
func (d *DB) userDescs() []*bytdb.TableDesc {
	var descs []*bytdb.TableDesc
	for _, name := range d.e.Tables() {
		if desc := d.e.Table(name); desc != nil {
			descs = append(descs, desc)
		}
	}
	return descs
}

// indexOID is a secondary index's oid; the primary key's synthetic
// index is id 0.
func indexOID(tableID uint64, indexID uint64) int64 { return int64(tableID*1000 + indexID) }

var sysTables = map[string]*sysTableDef{
	"pg_catalog.pg_namespace": {
		desc: sysDesc("pg_namespace", sysCol("oid", bytdb.TInt), sysCol("nspname", bytdb.TString)),
		rows: func(*DB) [][]any {
			return [][]any{
				{oidPGCatalog, "pg_catalog"},
				{oidPublic, "public"},
				{oidInfoSchema, "information_schema"},
			}
		},
	},
	"pg_catalog.pg_class": {
		desc: sysDesc("pg_class",
			sysCol("oid", bytdb.TInt), sysCol("relname", bytdb.TString),
			sysCol("relnamespace", bytdb.TInt), sysCol("relkind", bytdb.TString),
			sysCol("relowner", bytdb.TInt), sysCol("relpersistence", bytdb.TString),
			sysCol("relhasindex", bytdb.TBool), sysCol("reltuples", bytdb.TFloat),
			sysCol("relpages", bytdb.TInt)),
		rows: func(d *DB) [][]any {
			var rows [][]any
			rel := func(oid int64, name, kind string, hasIdx bool) {
				rows = append(rows, []any{oid, name, oidPublic, kind, int64(10), "p", hasIdx, -1.0, int64(0)})
			}
			for _, desc := range d.userDescs() {
				rel(int64(desc.ID), desc.Name, "r", len(desc.Indexes) > 0)
				rel(indexOID(desc.ID, 0), desc.Name+"_pkey", "i", false)
				for _, ix := range desc.Indexes {
					rel(indexOID(desc.ID, ix.ID), ix.Name, "i", false)
				}
			}
			return rows
		},
	},
	"pg_catalog.pg_attribute": {
		desc: sysDesc("pg_attribute",
			sysCol("attrelid", bytdb.TInt), sysCol("attname", bytdb.TString),
			sysCol("atttypid", bytdb.TInt), sysCol("attlen", bytdb.TInt),
			sysCol("attnum", bytdb.TInt), sysCol("attnotnull", bytdb.TBool),
			sysCol("atthasdef", bytdb.TBool), sysCol("attisdropped", bytdb.TBool)),
		rows: func(d *DB) [][]any {
			var rows [][]any
			for _, desc := range d.userDescs() {
				pk := map[int]bool{}
				for _, o := range desc.PKCols {
					pk[o] = true
				}
				for i, c := range desc.Columns {
					rows = append(rows, []any{
						int64(desc.ID), c.Name, typeOID(c.Type), typeLen(c.Type),
						int64(i + 1), pk[i], false, false,
					})
				}
			}
			return rows
		},
	},
	"pg_catalog.pg_type": {
		desc: sysDesc("pg_type",
			sysCol("oid", bytdb.TInt), sysCol("typname", bytdb.TString),
			sysCol("typnamespace", bytdb.TInt), sysCol("typlen", bytdb.TInt),
			sysCol("typtype", bytdb.TString), sysCol("typcategory", bytdb.TString)),
		rows: func(*DB) [][]any {
			var rows [][]any
			for _, t := range []struct {
				oid  int64
				name string
				len  int64
				cat  string
			}{
				{16, "bool", 1, "B"}, {17, "bytea", -1, "U"}, {19, "name", 64, "S"},
				{20, "int8", 8, "N"}, {21, "int2", 2, "N"}, {23, "int4", 4, "N"},
				{25, "text", -1, "S"}, {26, "oid", 4, "N"}, {700, "float4", 4, "N"},
				{701, "float8", 8, "N"}, {1043, "varchar", -1, "S"},
			} {
				rows = append(rows, []any{t.oid, t.name, oidPGCatalog, t.len, "b", t.cat})
			}
			return rows
		},
	},
	"pg_catalog.pg_index": {
		desc: sysDesc("pg_index",
			sysCol("indexrelid", bytdb.TInt), sysCol("indrelid", bytdb.TInt),
			sysCol("indisunique", bytdb.TBool), sysCol("indisprimary", bytdb.TBool),
			sysCol("indnatts", bytdb.TInt), sysCol("indkey", bytdb.TString)),
		rows: func(d *DB) [][]any {
			var rows [][]any
			attnums := func(ords []int) string {
				parts := make([]string, len(ords))
				for i, o := range ords {
					parts[i] = fmt.Sprint(o + 1)
				}
				return strings.Join(parts, " ")
			}
			for _, desc := range d.userDescs() {
				rows = append(rows, []any{
					indexOID(desc.ID, 0), int64(desc.ID), true, true,
					int64(len(desc.PKCols)), attnums(desc.PKCols),
				})
				for _, ix := range desc.Indexes {
					rows = append(rows, []any{
						indexOID(desc.ID, ix.ID), int64(desc.ID), ix.Unique, false,
						int64(len(ix.Cols)), attnums(ix.Cols),
					})
				}
			}
			return rows
		},
	},
	"information_schema.tables": {
		desc: sysDesc("tables",
			sysCol("table_catalog", bytdb.TString), sysCol("table_schema", bytdb.TString),
			sysCol("table_name", bytdb.TString), sysCol("table_type", bytdb.TString)),
		rows: func(d *DB) [][]any {
			var rows [][]any
			for _, desc := range d.userDescs() {
				rows = append(rows, []any{sysDatabase, "public", desc.Name, "BASE TABLE"})
			}
			return rows
		},
	},
	"information_schema.columns": {
		desc: sysDesc("columns",
			sysCol("table_catalog", bytdb.TString), sysCol("table_schema", bytdb.TString),
			sysCol("table_name", bytdb.TString), sysCol("column_name", bytdb.TString),
			sysCol("ordinal_position", bytdb.TInt), sysCol("column_default", bytdb.TString),
			sysCol("is_nullable", bytdb.TString), sysCol("data_type", bytdb.TString),
			sysCol("udt_name", bytdb.TString)),
		rows: func(d *DB) [][]any {
			var rows [][]any
			for _, desc := range d.userDescs() {
				pk := map[int]bool{}
				for _, o := range desc.PKCols {
					pk[o] = true
				}
				for i, c := range desc.Columns {
					nullable := "YES"
					if pk[i] {
						nullable = "NO"
					}
					rows = append(rows, []any{
						sysDatabase, "public", desc.Name, c.Name,
						int64(i + 1), nil, nullable, sqlTypeName(c.Type), udtName(c.Type),
					})
				}
			}
			return rows
		},
	},
}

// sysDatabase is the database name the catalog reports; the engine
// has no database concept, so it is a constant.
const sysDatabase = "bytdb"

// sysFuncs are the zero-argument functions clients probe; each folds
// to its constant at parse time (an optional pg_catalog. qualifier is
// accepted).
var sysFuncs = map[string]string{
	"version":          "PostgreSQL 16.0 (bytdb)",
	"current_database": sysDatabase,
	"current_schema":   "public",
	"current_user":     sysDatabase,
	"session_user":     sysDatabase,
}
