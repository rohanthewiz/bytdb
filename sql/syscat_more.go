package sql

// syscat_more.go: the catalog tables and relation-name plumbing tools
// read around migrations and introspection (deep-look D-51, part 2):
// pg_tables, pg_indexes, pg_views, pg_settings, pg_description,
// pg_sequences, and the information_schema views schemata, views,
// referential_constraints and constraint_column_usage; plus regclass
// resolution in both directions (name -> oid for 'x'::regclass, oid ->
// name for oid::regclass output and ::regclass::text).
//
// The tables register from init into sysTables rather than sitting in
// its literal: their builders call helpers that reach sysLookup, and a
// literal that references itself is an initialization cycle (see
// viewColumnsFn in syscat.go).

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/rohanthewiz/bytdb"
)

func init() {
	for name, def := range moreSysTables() {
		sysTables[name] = def
	}
}

// pgVarchar is pg_type's oid for character varying, which a column
// declared VARCHAR(n) reports instead of text.
const pgVarchar = int64(1043)

// colTypeOID is the pg_type oid a column reports: its type's, except a
// length-limited text column, which is varchar.
func colTypeOID(c bytdb.Column) int64 {
	if c.Type == bytdb.TString && c.MaxLen > 0 {
		return pgVarchar
	}
	return typeOID(c.Type)
}

// colTypmod is pg_attribute.atttypmod: VARCHAR(n) carries n plus the
// four-byte header Postgres counts, everything else -1. The common -1
// is boxed once: a runtime int64 outside 0..255 allocates when it
// becomes an interface, once per catalog row.
func colTypmod(c bytdb.Column) any {
	if c.Type == bytdb.TString && c.MaxLen > 0 {
		return int64(c.MaxLen) + 4
	}
	return typmodNone
}

var typmodNone any = int64(-1)

// colDataType / colUdtName are information_schema.columns' data_type
// and udt_name, varchar-aware.
func colDataType(c bytdb.Column) string {
	if c.Type == bytdb.TString && c.MaxLen > 0 {
		return "character varying"
	}
	return sqlTypeName(c.Type)
}

func colUdtName(c bytdb.Column) string {
	if c.Type == bytdb.TString && c.MaxLen > 0 {
		return "varchar"
	}
	return udtName(c.Type)
}

// numericPrecision is information_schema.columns' numeric_precision,
// numeric_precision_radix and numeric_scale: bits for the binary types,
// as Postgres reports int8 (64, 2, 0) and float8 (53, 2, NULL).
func numericPrecision(t bytdb.ColType) (prec, radix, scale any) {
	switch t {
	case bytdb.TInt:
		return int64(64), int64(2), int64(0)
	case bytdb.TFloat:
		return int64(53), int64(2), nil
	}
	return nil, nil, nil
}

// datetimePrecision is information_schema.columns.datetime_precision:
// microseconds for timestamps, 0 for dates.
func datetimePrecision(t bytdb.ColType) any {
	switch t {
	case bytdb.TTimestamp:
		return int64(6)
	case bytdb.TDate:
		return int64(0)
	}
	return nil
}

// serialDefault is the nextval default an identity column reports, its
// implied sequence name quoted the way Postgres's regclass output
// quotes it (nextval('"MixedT_id_seq"'::regclass)).
func serialDefault(table, column string) string {
	return "nextval('" + strings.ReplaceAll(quoteIdent(identitySeqName(table, column)), "'", "''") + "'::regclass)"
}

// --- regclass ---

// sysRelOIDs are the oids Postgres gives its core catalogs; clients
// hardcode some of them (classoid = 1259 in pg_description lookups).
// Other system tables get synthetic oids from sysRelOID.
var sysRelOIDs = map[string]int64{
	"pg_catalog.pg_type": 1247, "pg_catalog.pg_attribute": 1249,
	"pg_catalog.pg_class": 1259, "pg_catalog.pg_database": 1262,
	"pg_catalog.pg_am": 2601, "pg_catalog.pg_attrdef": 2604,
	"pg_catalog.pg_constraint": 2606, "pg_catalog.pg_description": 2609,
	"pg_catalog.pg_index": 2610, "pg_catalog.pg_inherits": 2611,
	"pg_catalog.pg_namespace": 2615, "pg_catalog.pg_sequence": 2224,
	"pg_catalog.pg_collation": 3456,
}

// sysRelNames is sysTables' names in sorted order, the basis of the
// synthetic oids; built on first use, after every init has registered
// its tables.
var sysRelNames = sync.OnceValue(func() []string {
	names := make([]string, 0, len(sysTables))
	for n := range sysTables {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
})

// sysRelOID is a system table's oid: Postgres's own for the core
// catalogs, else 4_000_000_000 + its position among the sorted names
// (stable between releases that add no tables; nothing persists it).
func sysRelOID(name string) int64 {
	if oid, ok := sysRelOIDs[name]; ok {
		return oid
	}
	i, _ := slices.BinarySearch(sysRelNames(), name)
	return 4_000_000_000 + int64(i)
}

// parseRelName reads a regclass input string the way Postgres does: an
// optional schema qualifier, then an identifier that folds to lowercase
// unless double-quoted ("MixedT" keeps its case).
func parseRelName(s string) (schema, name string) {
	s = strings.TrimSpace(s)
	parts := splitQualified(s)
	norm := func(p string) string {
		if len(p) > 1 && p[0] == '"' && p[len(p)-1] == '"' {
			return strings.ReplaceAll(p[1:len(p)-1], `""`, `"`)
		}
		return strings.ToLower(p)
	}
	if len(parts) == 2 {
		return norm(parts[0]), norm(parts[1])
	}
	return "", norm(s)
}

// splitQualified splits schema.name at the first dot outside quotes.
func splitQualified(s string) []string {
	inQ := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQ = !inQ
		case '.':
			if !inQ {
				return []string{s[:i], s[i+1:]}
			}
		}
	}
	return []string{s}
}

// relOID resolves a relation name to its pg_class oid: a table, an
// index (its own or a primary key's), a sequence, a view, or a system
// table ('pg_class'::regclass). ok is false for no such relation.
func (d *DB) relOID(text string) (int64, bool) {
	schema, name := parseRelName(text)
	if schema == "" || schema == "public" {
		if desc := d.e.TableShared(name); desc != nil {
			return int64(desc.ID), true
		}
		if sd := d.e.Sequence(name); sd != nil {
			return int64(sd.ID), true
		}
		for i, vd := range d.e.Views() {
			if vd.Name == name {
				return viewOID(i), true
			}
		}
		for _, desc := range d.userDescs() {
			if name == bytdb.PKConstraintName(desc.Name) {
				return indexOID(desc.ID, 0), true
			}
			for _, ix := range desc.Indexes {
				if ix.Name == name {
					return indexOID(desc.ID, ix.ID), true
				}
			}
		}
	}
	if schema == "" || schema == "pg_catalog" {
		if _, ok := sysTables["pg_catalog."+name]; ok {
			return sysRelOID("pg_catalog." + name), true
		}
	}
	if schema == "information_schema" {
		if _, ok := sysTables["information_schema."+name]; ok {
			return sysRelOID("information_schema." + name), true
		}
	}
	return 0, false
}

// relName renders an oid as regclass output does: the relation's name,
// quoted where an identifier needs it, schema-qualified for system
// tables (which are not on bytdb's search path as user names are), and
// the bare number for an oid that names nothing — Postgres's rule.
func (d *DB) relName(oid int64) string {
	for _, desc := range d.userDescs() {
		if oid == int64(desc.ID) {
			return quoteIdent(desc.Name)
		}
		if oid == indexOID(desc.ID, 0) {
			return quoteIdent(bytdb.PKConstraintName(desc.Name))
		}
		for _, ix := range desc.Indexes {
			if oid == indexOID(desc.ID, ix.ID) {
				return quoteIdent(ix.Name)
			}
		}
	}
	for _, sd := range d.e.Sequences() {
		if oid == int64(sd.ID) {
			return quoteIdent(sd.Name)
		}
	}
	for i, vd := range d.e.Views() {
		if oid == viewOID(i) {
			return quoteIdent(vd.Name)
		}
	}
	for _, n := range sysRelNames() {
		if sysRelOID(n) == oid {
			schema, bare, _ := strings.Cut(n, ".")
			if schema == "pg_catalog" {
				return bare // pg_catalog is on the search path
			}
			return n
		}
	}
	return strconv.FormatInt(oid, 10)
}

// isRegclassCast reports whether e is x::regclass.
func isRegclassCast(e Expr) bool {
	c, ok := e.(*ExCast)
	return ok && c.Type == "regclass"
}

// isTextType reports whether a cast target is one of the text types.
func isTextType(typ string) bool {
	switch typ {
	case "text", "varchar", "character varying", "name", "char", "character", "bpchar":
		return true
	}
	return false
}

// regclassOutputs makes a top-level select item x::regclass render as
// the relation's name, as Postgres's regclass output does: the item
// becomes (x::regclass)::text under its original column name. Only the
// statement's own select list is rewritten — inside a subquery, a view
// or a WHERE clause a regclass stays an oid, so comparisons with oid
// columns keep working (bytdb has no regclass value type to carry both).
// The parsed tree is shared through the parse cache, so the rewrite
// copies.
func regclassOutputs(s *Select) *Select {
	if len(s.Union) > 0 {
		return s // arms must agree on types; leave set operations alone
	}
	var items []SelectItem
	for i, it := range s.Items {
		if !isRegclassCast(it.Ex) {
			continue
		}
		if items == nil {
			items = slices.Clone(s.Items)
		}
		if items[i].As == "" {
			items[i].As = exprName(it.Ex)
		}
		items[i].Ex = &ExCast{E: it.Ex, Type: "text", From: bytdb.TInt}
	}
	if items == nil {
		return s
	}
	cp := *s
	cp.Items = items
	return &cp
}

// --- the tables ---

func moreSysTables() map[string]*sysTableDef {
	str, i64, boolean := bytdb.TString, bytdb.TInt, bytdb.TBool
	return map[string]*sysTableDef{
		"pg_catalog.pg_tables": {
			desc: sysDesc("pg_tables",
				sysCol("schemaname", str), sysCol("tablename", str),
				sysCol("tableowner", str), sysCol("tablespace", str),
				sysCol("hasindexes", boolean), sysCol("hasrules", boolean),
				sysCol("hastriggers", boolean), sysCol("rowsecurity", boolean)),
			rows: func(d *DB) [][]any {
				// User tables only; system tables are virtual and not what
				// a tool listing schema tables is after.
				var rows [][]any
				for _, desc := range d.userDescs() {
					rows = append(rows, []any{"public", desc.Name, sysDatabase, nil,
						true, false, false, false})
				}
				return rows
			},
		},
		"pg_catalog.pg_indexes": {
			desc: sysDesc("pg_indexes",
				sysCol("schemaname", str), sysCol("tablename", str),
				sysCol("indexname", str), sysCol("tablespace", str),
				sysCol("indexdef", str)),
			rows: func(d *DB) [][]any {
				var rows [][]any
				add := func(desc *bytdb.TableDesc, name string, oid int64) {
					def, _ := indexdef(d, oid, 0).(string)
					rows = append(rows, []any{"public", desc.Name, name, nil, def})
				}
				for _, desc := range d.userDescs() {
					add(desc, bytdb.PKConstraintName(desc.Name), indexOID(desc.ID, 0))
					for _, ix := range desc.Indexes {
						add(desc, ix.Name, indexOID(desc.ID, ix.ID))
					}
				}
				return rows
			},
		},
		"pg_catalog.pg_views": {
			desc: sysDesc("pg_views",
				sysCol("schemaname", str), sysCol("viewname", str),
				sysCol("viewowner", str), sysCol("definition", str)),
			rows: func(d *DB) [][]any {
				var rows [][]any
				for _, vd := range d.e.Views() {
					rows = append(rows, []any{"public", vd.Name, sysDatabase, vd.Query})
				}
				return rows
			},
		},
		// pg_settings lists what SHOW ALL lists, from the reading
		// session's state: SET values show with source 'session'.
		"pg_catalog.pg_settings": {
			desc: sysDesc("pg_settings",
				sysCol("name", str), sysCol("setting", str), sysCol("unit", str),
				sysCol("category", str), sysCol("short_desc", str),
				sysCol("extra_desc", str), sysCol("context", str),
				sysCol("vartype", str), sysCol("source", str),
				sysCol("min_val", str), sysCol("max_val", str),
				sysCol("enumvals", str), sysCol("boot_val", str),
				sysCol("reset_val", str), sysCol("sourcefile", str),
				sysCol("sourceline", i64), sysCol("pending_restart", boolean)),
			rows: func(d *DB) [][]any {
				var vars map[string]string
				if d.ss != nil && d.ss.ses != nil {
					vars = d.ss.ses.vars
				}
				names := make([]string, 0, len(showDefaults)+len(vars))
				for n := range showDefaults {
					names = append(names, n)
				}
				for n := range vars {
					if _, dup := showDefaults[n]; !dup {
						names = append(names, n)
					}
				}
				sort.Strings(names)
				var rows [][]any
				for _, n := range names {
					v, _ := d.settingValue(n)
					source := "default"
					if _, set := vars[n]; set {
						source = "session"
					}
					vartype := "string"
					switch {
					case v == "on" || v == "off":
						vartype = "bool"
					case v != "" && strings.Trim(v, "0123456789") == "":
						vartype = "integer"
					}
					rows = append(rows, []any{n, v, nil, "Ungrouped", "", nil, "user",
						vartype, source, nil, nil, nil, showDefaults[n], showDefaults[n],
						nil, nil, false})
				}
				return rows
			},
		},
		// COMMENT ON is accepted and ignored, so there are never rows.
		"pg_catalog.pg_description": {
			desc: sysDesc("pg_description",
				sysCol("objoid", i64), sysCol("classoid", i64),
				sysCol("objsubid", i64), sysCol("description", str)),
		},
		"pg_catalog.pg_sequences": {
			desc: sysDesc("pg_sequences",
				sysCol("schemaname", str), sysCol("sequencename", str),
				sysCol("sequenceowner", str), sysCol("data_type", str),
				sysCol("start_value", i64), sysCol("min_value", i64),
				sysCol("max_value", i64), sysCol("increment_by", i64),
				sysCol("cycle", boolean), sysCol("cache_size", i64),
				sysCol("last_value", i64)),
			rows: func(d *DB) [][]any {
				// last_value is NULL until the first nextval, as in
				// Postgres. Identity columns' implied sequences are not
				// listed: they have no sequence object of their own.
				var rows [][]any
				for _, sd := range d.e.Sequences() {
					var last any
					if sd.Called {
						last = sd.Last
					}
					rows = append(rows, []any{"public", sd.Name, sysDatabase,
						seqTypeName(sd.Type), sd.Start, sd.Min, sd.Max, sd.Increment,
						sd.Cycle, sd.Cache, last})
				}
				return rows
			},
		},
		"information_schema.schemata": {
			desc: sysDesc("schemata",
				sysCol("catalog_name", str), sysCol("schema_name", str),
				sysCol("schema_owner", str), sysCol("default_character_set_catalog", str),
				sysCol("default_character_set_schema", str),
				sysCol("default_character_set_name", str), sysCol("sql_path", str)),
			rows: func(*DB) [][]any {
				var rows [][]any
				for _, n := range []string{"information_schema", "pg_catalog", "public"} {
					rows = append(rows, []any{sysDatabase, n, sysDatabase, nil, nil, nil, nil})
				}
				return rows
			},
		},
		"information_schema.views": {
			desc: sysDesc("views",
				sysCol("table_catalog", str), sysCol("table_schema", str),
				sysCol("table_name", str), sysCol("view_definition", str),
				sysCol("check_option", str), sysCol("is_updatable", str),
				sysCol("is_insertable_into", str), sysCol("is_trigger_updatable", str),
				sysCol("is_trigger_deletable", str), sysCol("is_trigger_insertable_into", str)),
			rows: func(d *DB) [][]any {
				// bytdb views are read-only: every is_* is NO.
				var rows [][]any
				for _, vd := range d.e.Views() {
					rows = append(rows, []any{sysDatabase, "public", vd.Name, vd.Query,
						"NONE", "NO", "NO", "NO", "NO", "NO"})
				}
				return rows
			},
		},
		// referential_constraints names each FK's referenced key
		// (unique_constraint_name): the parent's primary key, or the
		// unique index over exactly the referenced columns.
		"information_schema.referential_constraints": {
			desc: sysDesc("referential_constraints",
				sysCol("constraint_catalog", str), sysCol("constraint_schema", str),
				sysCol("constraint_name", str), sysCol("unique_constraint_catalog", str),
				sysCol("unique_constraint_schema", str), sysCol("unique_constraint_name", str),
				sysCol("match_option", str), sysCol("update_rule", str),
				sysCol("delete_rule", str)),
			rows: func(d *DB) [][]any {
				var rows [][]any
				for _, desc := range d.userDescs() {
					for _, fk := range desc.ForeignKeys {
						var uname any
						if p := d.e.TableShared(fk.RefTable); p != nil {
							if n := referencedKeyName(p, fk.RefCols); n != "" {
								uname = n
							}
						}
						del := "NO ACTION"
						if fk.OnDelete == bytdb.FKCascade {
							del = "CASCADE"
						}
						rows = append(rows, []any{sysDatabase, "public", fk.Name,
							sysDatabase, "public", uname, "NONE", "NO ACTION", del})
					}
				}
				return rows
			},
		},
		// constraint_column_usage lists the columns a constraint uses —
		// for a foreign key, the referenced (parent) columns, which is
		// what ORMs join it for. CHECK constraints are left out: listing
		// their columns would mean parsing the stored expression.
		"information_schema.constraint_column_usage": {
			desc: sysDesc("constraint_column_usage",
				sysCol("table_catalog", str), sysCol("table_schema", str),
				sysCol("table_name", str), sysCol("column_name", str),
				sysCol("constraint_catalog", str), sysCol("constraint_schema", str),
				sysCol("constraint_name", str)),
			rows: func(d *DB) [][]any {
				var rows [][]any
				add := func(table, col, con string) {
					rows = append(rows, []any{sysDatabase, "public", table, col,
						sysDatabase, "public", con})
				}
				for _, desc := range d.userDescs() {
					for _, o := range desc.PKCols {
						add(desc.Name, desc.Columns[o].Name, bytdb.PKConstraintName(desc.Name))
					}
					for _, ix := range desc.Indexes {
						if ix.Unique {
							for _, o := range ix.Cols {
								add(desc.Name, desc.Columns[o].Name, ix.Name)
							}
						}
					}
					for _, fk := range desc.ForeignKeys {
						for _, c := range fk.RefCols {
							add(fk.RefTable, c, fk.Name)
						}
					}
				}
				return rows
			},
		},
	}
}

// referencedKeyName names the key of parent over exactly cols: its
// primary key, or a unique index; "" when neither matches.
func referencedKeyName(parent *bytdb.TableDesc, cols []string) string {
	names := func(ords []int) []string {
		out := make([]string, len(ords))
		for i, o := range ords {
			out[i] = parent.Columns[o].Name
		}
		return out
	}
	if slices.Equal(names(parent.PKCols), cols) {
		return bytdb.PKConstraintName(parent.Name)
	}
	for _, ix := range parent.Indexes {
		if ix.Unique && slices.Equal(names(ix.Cols), cols) {
			return ix.Name
		}
	}
	return ""
}
