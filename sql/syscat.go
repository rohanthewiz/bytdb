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
		// The statement's materialized CTEs, derived tables, and views
		// come first: a WITH name shadows a real table, as in Postgres.
		if v, ok := d.vtabs[name]; ok {
			return v.desc, v.rows
		}
		if !strings.Contains(name, ".") {
			if desc := base(name); desc != nil {
				return desc, nil
			}
			// A sequence reads as a one-row virtual table, the way
			// Postgres exposes sequence state relations — drivers and
			// psql's \d issue SELECT last_value FROM seq.
			if sd := d.e.Sequence(name); sd != nil {
				return sysDesc(name,
						sysCol("last_value", bytdb.TInt),
						sysCol("log_cnt", bytdb.TInt),
						sysCol("is_called", bytdb.TBool)),
					[][]any{{sd.Last, int64(0), sd.Called}}
			}
		}
		if st := sysLookup(name); st != nil {
			var rows [][]any
			if st.rows != nil {
				rows = st.rows(d)
			}
			if rows == nil {
				rows = [][]any{} // non-nil marks the table virtual
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
	case *AddConstraint:
		return s.Table
	case *AddFK:
		return s.Table
	case *RenameTable:
		return s.Table
	case *RenameColumn:
		return s.Table
	case *DropConstraint:
		return s.Table
	case *CreateIndex:
		return s.Table
	case *DropIndex:
		return s.Table
	case *CreateSequence:
		return s.Name
	case *DropSequence:
		return s.Name
	case *CreateView:
		return s.Name
	case *DropView:
		return s.Name
	case *AlterSequence:
		return s.Name
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
	rows func(d *DB) [][]any // nil: the table is always empty
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

// Access-method oids (Postgres's well-known values): every table
// reads as heap, every index as btree.
const (
	amHeap  = int64(2)
	amBtree = int64(403)
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
	case bytdb.TTimestamp:
		return 1184 // timestamptz: stored instants are UTC
	case bytdb.TDate:
		return 1082
	case bytdb.TUUID:
		return 2950
	case bytdb.TTextArray:
		return 1009 // _text
	case bytdb.TJSONB:
		return 3802
	}
	return 25 // text
}

// typeLen is pg_type.typlen / pg_attribute.attlen.
func typeLen(t bytdb.ColType) int64 {
	switch t {
	case bytdb.TBool:
		return 1
	case bytdb.TInt, bytdb.TFloat, bytdb.TTimestamp:
		return 8
	case bytdb.TDate:
		return 4
	case bytdb.TUUID:
		return 16
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
	case bytdb.TTimestamp:
		return "timestamp with time zone"
	case bytdb.TDate:
		return "date"
	case bytdb.TUUID:
		return "uuid"
	case bytdb.TTextArray:
		return "ARRAY" // information_schema spells every array type this way
	case bytdb.TJSONB:
		return "jsonb"
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
	case bytdb.TTimestamp:
		return "timestamptz"
	case bytdb.TDate:
		return "date"
	case bytdb.TUUID:
		return "uuid"
	case bytdb.TTextArray:
		return "_text" // pg_type's array-of-text name
	case bytdb.TJSONB:
		return "jsonb"
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

// checkOID is the oid of a table's i-th check constraint, placed high
// in the table's oid block, above any realistic index ID.
func checkOID(tableID uint64, i int) int64 { return int64(tableID*1000+900) + int64(i) }

// fkOID is the oid of a table's i-th foreign-key constraint — its own
// slice of the table's oid block, above the check-constraint slice.
func fkOID(tableID uint64, i int) int64 { return int64(tableID*1000+950) + int64(i) }

// viewOID is the i-th view's synthetic pg_class oid (views store no
// ID; Views() is name-sorted, so the numbering is stable between DDL).
func viewOID(i int) int64 { return int64(3_000_000_000) + int64(i) }

// attrdefOID is the oid of a column default's pg_attrdef row: its own
// slice of the table's oid block, below the check-constraint slice.
func attrdefOID(tableID uint64, attnum int) int64 { return int64(tableID*1000+800) + int64(attnum) }

// seqTypeOID maps a sequence's declared type to its pg_type oid.
func seqTypeOID(t string) int64 {
	switch t {
	case "smallint":
		return 21
	case "integer":
		return 23
	}
	return 20 // bigint
}

var sysTables = map[string]*sysTableDef{
	"pg_catalog.pg_namespace": {
		desc: sysDesc("pg_namespace", sysCol("oid", bytdb.TInt),
			sysCol("nspname", bytdb.TString), sysCol("nspowner", bytdb.TInt)),
		rows: func(*DB) [][]any {
			return [][]any{
				{oidPGCatalog, "pg_catalog", int64(10)},
				{oidPublic, "public", int64(10)},
				{oidInfoSchema, "information_schema", int64(10)},
			}
		},
	},
	"pg_catalog.pg_class": {
		desc: sysDesc("pg_class",
			sysCol("oid", bytdb.TInt), sysCol("relname", bytdb.TString),
			sysCol("relnamespace", bytdb.TInt), sysCol("relkind", bytdb.TString),
			sysCol("relowner", bytdb.TInt), sysCol("relpersistence", bytdb.TString),
			sysCol("relhasindex", bytdb.TBool), sysCol("reltuples", bytdb.TFloat),
			sysCol("relpages", bytdb.TInt),
			// The columns \d reads and bytdb has no story for; constants.
			sysCol("relam", bytdb.TInt), sysCol("relchecks", bytdb.TInt),
			sysCol("relhasrules", bytdb.TBool), sysCol("relhastriggers", bytdb.TBool),
			sysCol("relrowsecurity", bytdb.TBool), sysCol("relforcerowsecurity", bytdb.TBool),
			sysCol("relispartition", bytdb.TBool), sysCol("reloftype", bytdb.TInt),
			sysCol("reltablespace", bytdb.TInt), sysCol("relreplident", bytdb.TString),
			sysCol("reltoastrelid", bytdb.TInt), sysCol("reloptions", bytdb.TString)),
		rows: func(d *DB) [][]any {
			var rows [][]any
			rel := func(oid int64, name, kind string, hasIdx bool, nChecks int) {
				am := int64(0) // sequences have no access method
				switch kind {
				case "i":
					am = amBtree
				case "r":
					am = amHeap
				}
				rows = append(rows, []any{
					oid, name, oidPublic, kind, int64(10), "p", hasIdx, -1.0, int64(0),
					am, int64(nChecks), false, false, false, false, false, int64(0),
					int64(0), "d", int64(0), nil,
				})
			}
			for _, desc := range d.userDescs() {
				rel(int64(desc.ID), desc.Name, "r", len(desc.Indexes) > 0, len(desc.Checks))
				rel(indexOID(desc.ID, 0), desc.Name+"_pkey", "i", false, 0)
				for _, ix := range desc.Indexes {
					rel(indexOID(desc.ID, ix.ID), ix.Name, "i", false, 0)
				}
			}
			for _, sd := range d.e.Sequences() {
				rel(int64(sd.ID), sd.Name, "S", false, 0)
			}
			// Views get synthetic oids well above the table-ID range;
			// they have no stored ID of their own (see viewOID).
			for i, vd := range d.e.Views() {
				rel(viewOID(i), vd.Name, "v", false, 0)
			}
			return rows
		},
	},
	"pg_catalog.pg_attribute": {
		desc: sysDesc("pg_attribute",
			sysCol("attrelid", bytdb.TInt), sysCol("attname", bytdb.TString),
			sysCol("atttypid", bytdb.TInt), sysCol("attlen", bytdb.TInt),
			sysCol("attnum", bytdb.TInt), sysCol("attnotnull", bytdb.TBool),
			sysCol("atthasdef", bytdb.TBool), sysCol("attisdropped", bytdb.TBool),
			sysCol("atttypmod", bytdb.TInt), sysCol("attcollation", bytdb.TInt),
			sysCol("attidentity", bytdb.TString), sysCol("attgenerated", bytdb.TString),
			sysCol("attstorage", bytdb.TString), sysCol("attcompression", bytdb.TString),
			sysCol("attstattarget", bytdb.TInt)),
		rows: func(d *DB) [][]any {
			var rows [][]any
			attr := func(relid int64, num int, c bytdb.Column, notNull bool) {
				// atthasdef gates psql's pg_attrdef subquery; identity
				// reports through attidentity ('d': BY DEFAULT), as in
				// Postgres — not as a default.
				identity := ""
				if c.Identity {
					identity = "d"
				}
				rows = append(rows, []any{
					relid, c.Name, typeOID(c.Type), typeLen(c.Type),
					int64(num), notNull, c.Default != "", false,
					int64(-1), int64(0), identity, "",
					"p", "", int64(-1),
				})
			}
			for _, desc := range d.userDescs() {
				pk := map[int]bool{}
				for _, o := range desc.PKCols {
					pk[o] = true
				}
				for i, c := range desc.Columns {
					attr(int64(desc.ID), i+1, c, pk[i] || c.NotNull)
				}
				// Index relations list their key columns too (\d idx).
				for i, o := range desc.PKCols {
					attr(indexOID(desc.ID, 0), i+1, desc.Columns[o], false)
				}
				for _, ix := range desc.Indexes {
					for i, o := range ix.Cols {
						attr(indexOID(desc.ID, ix.ID), i+1, desc.Columns[o], false)
					}
				}
			}
			return rows
		},
	},
	"pg_catalog.pg_type": {
		desc: sysDesc("pg_type",
			sysCol("oid", bytdb.TInt), sysCol("typname", bytdb.TString),
			sysCol("typnamespace", bytdb.TInt), sysCol("typlen", bytdb.TInt),
			sysCol("typtype", bytdb.TString), sysCol("typcategory", bytdb.TString),
			sysCol("typcollation", bytdb.TInt)),
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
				{1009, "_text", -1, "A"},
				{1082, "date", 4, "D"}, {1114, "timestamp", 8, "D"},
				{1184, "timestamptz", 8, "D"}, {2950, "uuid", 16, "U"},
				{3802, "jsonb", -1, "U"},
			} {
				rows = append(rows, []any{t.oid, t.name, oidPGCatalog, t.len, "b", t.cat, int64(0)})
			}
			return rows
		},
	},
	"pg_catalog.pg_index": {
		desc: sysDesc("pg_index",
			sysCol("indexrelid", bytdb.TInt), sysCol("indrelid", bytdb.TInt),
			sysCol("indisunique", bytdb.TBool), sysCol("indisprimary", bytdb.TBool),
			sysCol("indnatts", bytdb.TInt), sysCol("indkey", bytdb.TString),
			sysCol("indisclustered", bytdb.TBool), sysCol("indisvalid", bytdb.TBool),
			sysCol("indisreplident", bytdb.TBool), sysCol("indnullsnotdistinct", bytdb.TBool),
			sysCol("indimmediate", bytdb.TBool), sysCol("indcheckxmin", bytdb.TBool),
			sysCol("indnkeyatts", bytdb.TInt), sysCol("indpred", bytdb.TString)),
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
					false, true, false, false, true, false,
					int64(len(desc.PKCols)), nil,
				})
				for _, ix := range desc.Indexes {
					rows = append(rows, []any{
						indexOID(desc.ID, ix.ID), int64(desc.ID), ix.Unique, false,
						int64(len(ix.Cols)), attnums(ix.Cols),
						false, true, false, false, true, false,
						int64(len(ix.Cols)), nil,
					})
				}
			}
			return rows
		},
	},
	// Most tables below exist so psql's probes parse, bind, and return
	// zero rows (pg_constraint lists CHECK constraints and pg_attrdef
	// column defaults): bytdb has no collations, inheritance, policies,
	// extended statistics, or publications.
	"pg_catalog.pg_am": {
		desc: sysDesc("pg_am",
			sysCol("oid", bytdb.TInt), sysCol("amname", bytdb.TString),
			sysCol("amtype", bytdb.TString)),
		rows: func(*DB) [][]any {
			return [][]any{{amHeap, "heap", "t"}, {amBtree, "btree", "i"}}
		},
	},
	"pg_catalog.pg_attrdef": {
		desc: sysDesc("pg_attrdef",
			sysCol("oid", bytdb.TInt), sysCol("adrelid", bytdb.TInt),
			sysCol("adnum", bytdb.TInt), sysCol("adbin", bytdb.TString)),
		rows: func(d *DB) [][]any {
			// One row per declared column default. adbin holds the
			// stored literal text rather than a Postgres expression
			// tree; pg_get_expr surfaces it as-is, which is exactly
			// what psql's \d Default column renders. Identity columns
			// are not defaults — they report via attidentity.
			var rows [][]any
			for _, desc := range d.userDescs() {
				for i, c := range desc.Columns {
					if c.Default != "" {
						rows = append(rows, []any{
							attrdefOID(desc.ID, i+1), int64(desc.ID),
							int64(i + 1), c.Default,
						})
					}
				}
			}
			return rows
		},
	},
	"pg_catalog.pg_sequence": {
		desc: sysDesc("pg_sequence",
			sysCol("seqrelid", bytdb.TInt), sysCol("seqtypid", bytdb.TInt),
			sysCol("seqstart", bytdb.TInt), sysCol("seqincrement", bytdb.TInt),
			sysCol("seqmax", bytdb.TInt), sysCol("seqmin", bytdb.TInt),
			sysCol("seqcache", bytdb.TInt), sysCol("seqcycle", bytdb.TBool)),
		rows: func(d *DB) [][]any {
			var rows [][]any
			for _, sd := range d.e.Sequences() {
				rows = append(rows, []any{
					int64(sd.ID), seqTypeOID(sd.Type), sd.Start, sd.Increment,
					sd.Max, sd.Min, sd.Cache, sd.Cycle,
				})
			}
			return rows
		},
	},
	"pg_catalog.pg_collation": {
		desc: sysDesc("pg_collation",
			sysCol("oid", bytdb.TInt), sysCol("collname", bytdb.TString),
			sysCol("collnamespace", bytdb.TInt)),
	},
	"pg_catalog.pg_constraint": {
		desc: sysDesc("pg_constraint",
			sysCol("oid", bytdb.TInt), sysCol("conname", bytdb.TString),
			sysCol("connamespace", bytdb.TInt), sysCol("contype", bytdb.TString),
			sysCol("condeferrable", bytdb.TBool), sysCol("condeferred", bytdb.TBool),
			sysCol("convalidated", bytdb.TBool), sysCol("conrelid", bytdb.TInt),
			sysCol("contypid", bytdb.TInt), sysCol("conindid", bytdb.TInt),
			sysCol("conparentid", bytdb.TInt), sysCol("confrelid", bytdb.TInt),
			sysCol("confupdtype", bytdb.TString), sysCol("confdeltype", bytdb.TString),
			sysCol("conkey", bytdb.TString), sysCol("confkey", bytdb.TString)),
		rows: func(d *DB) [][]any {
			// CHECK and FOREIGN KEY constraints; keys surface through
			// pg_index.
			var rows [][]any
			for _, desc := range d.userDescs() {
				for i, ck := range desc.Checks {
					rows = append(rows, []any{
						checkOID(desc.ID, i), ck.Name, oidPublic, "c",
						false, false, true, int64(desc.ID),
						int64(0), int64(0), int64(0), int64(0),
						" ", " ", // action chars are ' ' on non-FK rows, as in Postgres
						nil, nil,
					})
				}
				for i := range desc.ForeignKeys {
					fk := &desc.ForeignKeys[i]
					confrelid := int64(0)
					if p := d.e.Table(fk.RefTable); p != nil {
						confrelid = int64(p.ID)
					}
					deltype := "a" // NO ACTION
					if fk.OnDelete == bytdb.FKCascade {
						deltype = "c"
					}
					rows = append(rows, []any{
						fkOID(desc.ID, i), fk.Name, oidPublic, "f",
						false, false, true, int64(desc.ID),
						int64(0), int64(0), int64(0), confrelid,
						"a", deltype,
						nil, nil,
					})
				}
			}
			return rows
		},
	},
	"pg_catalog.pg_stat_activity": {
		desc: sysDesc("pg_stat_activity",
			sysCol("datname", bytdb.TString), sysCol("pid", bytdb.TInt),
			sysCol("usename", bytdb.TString), sysCol("application_name", bytdb.TString),
			sysCol("client_addr", bytdb.TString), sysCol("state", bytdb.TString),
			sysCol("query", bytdb.TString)),
		rows: func(d *DB) [][]any {
			// Fed by the serving layer's provider; an embedded DB with no
			// wire server has no backends to report.
			if d.activity == nil {
				return nil
			}
			var rows [][]any
			for _, a := range d.activity() {
				var addr any
				if a.ClientAddr != "" {
					addr = a.ClientAddr
				}
				rows = append(rows, []any{
					sysDatabase, int64(a.PID), a.User, a.AppName,
					addr, a.State, a.Query,
				})
			}
			return rows
		},
	},
	"pg_catalog.pg_inherits": {
		desc: sysDesc("pg_inherits",
			sysCol("inhrelid", bytdb.TInt), sysCol("inhparent", bytdb.TInt),
			sysCol("inhseqno", bytdb.TInt), sysCol("inhdetachpending", bytdb.TBool)),
	},
	"pg_catalog.pg_policy": {
		desc: sysDesc("pg_policy",
			sysCol("oid", bytdb.TInt), sysCol("polname", bytdb.TString),
			sysCol("polrelid", bytdb.TInt), sysCol("polcmd", bytdb.TString),
			sysCol("polpermissive", bytdb.TBool), sysCol("polroles", bytdb.TString),
			sysCol("polqual", bytdb.TString), sysCol("polwithcheck", bytdb.TString)),
	},
	"pg_catalog.pg_statistic_ext": {
		desc: sysDesc("pg_statistic_ext",
			sysCol("oid", bytdb.TInt), sysCol("stxrelid", bytdb.TInt),
			sysCol("stxname", bytdb.TString), sysCol("stxnamespace", bytdb.TInt),
			sysCol("stxstattarget", bytdb.TInt), sysCol("stxkind", bytdb.TString)),
	},
	"pg_catalog.pg_publication": {
		desc: sysDesc("pg_publication",
			sysCol("oid", bytdb.TInt), sysCol("pubname", bytdb.TString),
			sysCol("puballtables", bytdb.TBool), sysCol("pubinsert", bytdb.TBool),
			sysCol("pubupdate", bytdb.TBool), sysCol("pubdelete", bytdb.TBool),
			sysCol("pubtruncate", bytdb.TBool), sysCol("pubviaroot", bytdb.TBool)),
	},
	"pg_catalog.pg_publication_rel": {
		desc: sysDesc("pg_publication_rel",
			sysCol("oid", bytdb.TInt), sysCol("prpubid", bytdb.TInt),
			sysCol("prrelid", bytdb.TInt), sysCol("prqual", bytdb.TString),
			sysCol("prattrs", bytdb.TString)),
	},
	"pg_catalog.pg_database": {
		desc: sysDesc("pg_database",
			sysCol("oid", bytdb.TInt), sysCol("datname", bytdb.TString),
			sysCol("datdba", bytdb.TInt), sysCol("encoding", bytdb.TInt),
			sysCol("datlocprovider", bytdb.TString), sysCol("datcollate", bytdb.TString),
			sysCol("datctype", bytdb.TString), sysCol("daticulocale", bytdb.TString),
			sysCol("daticurules", bytdb.TString), sysCol("datacl", bytdb.TString),
			sysCol("datistemplate", bytdb.TBool), sysCol("datallowconn", bytdb.TBool)),
		rows: func(*DB) [][]any {
			return [][]any{{int64(1), sysDatabase, int64(10), int64(6), "c",
				"C", "C", nil, nil, nil, false, true}}
		},
	},
	"pg_catalog.pg_roles": {
		desc: sysDesc("pg_roles",
			sysCol("oid", bytdb.TInt), sysCol("rolname", bytdb.TString),
			sysCol("rolsuper", bytdb.TBool), sysCol("rolinherit", bytdb.TBool),
			sysCol("rolcreaterole", bytdb.TBool), sysCol("rolcreatedb", bytdb.TBool),
			sysCol("rolcanlogin", bytdb.TBool), sysCol("rolconnlimit", bytdb.TInt),
			sysCol("rolvaliduntil", bytdb.TString), sysCol("rolreplication", bytdb.TBool),
			sysCol("rolbypassrls", bytdb.TBool)),
		rows: func(*DB) [][]any {
			return [][]any{{int64(10), sysDatabase, true, true, true, true, true,
				int64(-1), nil, false, false}}
		},
	},
	"pg_catalog.pg_auth_members": {
		desc: sysDesc("pg_auth_members",
			sysCol("oid", bytdb.TInt), sysCol("roleid", bytdb.TInt),
			sysCol("member", bytdb.TInt), sysCol("grantor", bytdb.TInt),
			sysCol("admin_option", bytdb.TBool)),
	},
	"pg_catalog.pg_publication_namespace": {
		desc: sysDesc("pg_publication_namespace",
			sysCol("oid", bytdb.TInt), sysCol("pnpubid", bytdb.TInt),
			sysCol("pnnspid", bytdb.TInt)),
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
	"information_schema.sequences": {
		desc: sysDesc("sequences",
			sysCol("sequence_catalog", bytdb.TString), sysCol("sequence_schema", bytdb.TString),
			sysCol("sequence_name", bytdb.TString), sysCol("data_type", bytdb.TString),
			sysCol("start_value", bytdb.TString), sysCol("minimum_value", bytdb.TString),
			sysCol("maximum_value", bytdb.TString), sysCol("increment", bytdb.TString),
			sysCol("cycle_option", bytdb.TString)),
		rows: func(d *DB) [][]any {
			// The numeric fields are character_data in the standard (and
			// in Postgres), so they render as text.
			var rows [][]any
			for _, sd := range d.e.Sequences() {
				cycle := "NO"
				if sd.Cycle {
					cycle = "YES"
				}
				rows = append(rows, []any{
					sysDatabase, "public", sd.Name, seqTypeName(sd.Type),
					fmtI(sd.Start), fmtI(sd.Min), fmtI(sd.Max), fmtI(sd.Increment),
					cycle,
				})
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
			sysCol("udt_name", bytdb.TString),
			sysCol("character_maximum_length", bytdb.TInt)),
		rows: func(d *DB) [][]any {
			var rows [][]any
			for _, desc := range d.userDescs() {
				pk := map[int]bool{}
				for _, o := range desc.PKCols {
					pk[o] = true
				}
				for i, c := range desc.Columns {
					nullable := "YES"
					if pk[i] || c.NotNull || c.Identity {
						nullable = "NO"
					}
					// Identity columns report a serial-style default, which
					// is what introspecting clients key "omit on insert" off;
					// declared defaults report their stored literal text.
					var dflt any
					if c.Identity {
						dflt = fmt.Sprintf("nextval('%s_%s_seq'::regclass)", desc.Name, c.Name)
					} else if c.Default != "" {
						dflt = c.Default
					}
					// A declared VARCHAR(n) limit reports as
					// character_maximum_length; unbounded types report NULL.
					var maxLen any
					if c.MaxLen > 0 {
						maxLen = int64(c.MaxLen)
					}
					rows = append(rows, []any{
						sysDatabase, "public", desc.Name, c.Name,
						int64(i + 1), dflt, nullable, sqlTypeName(c.Type), udtName(c.Type),
						maxLen,
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
	"version":          "PostgreSQL " + ServerVersion,
	"current_database": sysDatabase,
	"current_schema":   "public",
	"current_user":     sysDatabase,
	"session_user":     sysDatabase,
}
