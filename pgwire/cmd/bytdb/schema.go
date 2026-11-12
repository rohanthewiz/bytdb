package main

// schema.go: reconstructing SQL from the catalog. bytdb stores table
// descriptors (columns, primary key, indexes, checks, foreign keys),
// view definitions and sequence state as structured records, not as
// the DDL text that created them, so .schema and .dump regenerate DDL
// from those records. The output targets the bytdb dialect — which is
// the Postgres dialect — so a dump also loads into Postgres for the
// common types.
//
// .dump statement order, chosen so a dump replays into an empty
// database with every constraint validated exactly once:
//
//	1. CREATE SEQUENCE + setval   column DEFAULTs may call nextval()
//	2. CREATE TABLE               with PK, NOT NULL, DEFAULT, CHECK
//	3. INSERT (in BEGIN/COMMIT)   rows satisfy the inline checks
//	   ALTER COLUMN ... RESTART   per identity column, after its rows
//	4. CREATE [UNIQUE] INDEX      built once over the loaded rows
//	5. ALTER TABLE ADD FOREIGN KEY  every parent row now exists
//	6. CREATE VIEW                views can reference views, so ordered
//	                              by dependency
//
// The RESTART line has to follow the rows: each restored explicit id
// bumps the counter past itself, so a RESTART placed before them would
// be overwritten whenever the counter was ahead of the largest stored
// id (a drawn value since deleted). Placed after, it sets the counter to
// exactly what the source held, and a replayed dump dumps back to the
// same text.
//
// Foreign keys are deferred to step 5 because table order cannot always
// satisfy them (a self-reference, or two tables referencing each other),
// and adding them last validates the whole dataset in one pass — the
// approach pg_dump takes.

import (
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/rohanthewiz/bytdb"
)

// columnDDL renders one column definition.
func columnDDL(c bytdb.Column) string {
	var b strings.Builder
	b.WriteString(quoteIdent(c.Name))
	b.WriteByte(' ')
	switch {
	case c.Identity:
		// serial is the bytdb spelling of an identity column (an int
		// with its own durable counter, implying NOT NULL).
		b.WriteString("serial")
	default:
		b.WriteString(columnTypeName(c)) // varchar(n), numeric(p,s), ...
	}
	if c.NotNull && !c.Identity {
		b.WriteString(" NOT NULL")
	}
	if c.Default != "" {
		// Default is stored as SQL text already: a quoted literal, or
		// an unquoted expression marker such as now().
		b.WriteString(" DEFAULT " + c.Default)
	}
	return b.String()
}

// colList renders column ordinals of desc as a parenthesized name list.
func colList(desc *bytdb.TableDesc, ords []int) string {
	names := make([]string, len(ords))
	for i, o := range ords {
		if o >= 0 && o < len(desc.Columns) {
			names[i] = quoteIdent(desc.Columns[o].Name)
		} else {
			names[i] = "?"
		}
	}
	return "(" + strings.Join(names, ", ") + ")"
}

// fkClause renders a foreign key's FOREIGN KEY ... REFERENCES body.
func fkClause(desc *bytdb.TableDesc, fk bytdb.FKDesc) string {
	refs := make([]string, len(fk.RefCols))
	for i, c := range fk.RefCols {
		refs[i] = quoteIdent(c)
	}
	s := "CONSTRAINT " + quoteIdent(fk.Name) + " FOREIGN KEY " + colList(desc, fk.Cols) +
		" REFERENCES " + quoteIdent(fk.RefTable) + " (" + strings.Join(refs, ", ") + ")"
	if fk.OnDelete == bytdb.FKCascade {
		s += " ON DELETE CASCADE"
	}
	return s
}

// createTableDDL renders a CREATE TABLE statement for desc. inlineFKs
// keeps foreign keys inside the statement (readable, for .schema);
// .dump passes false and adds them afterwards (see the file comment).
func createTableDDL(desc *bytdb.TableDesc, inlineFKs bool) string {
	var parts []string
	for _, c := range desc.Columns {
		parts = append(parts, columnDDL(c))
	}
	if len(desc.PKCols) > 0 {
		parts = append(parts, "PRIMARY KEY "+colList(desc, desc.PKCols))
	}
	for _, ck := range desc.Checks {
		parts = append(parts, "CONSTRAINT "+quoteIdent(ck.Name)+" CHECK ("+ck.Expr+")")
	}
	if inlineFKs {
		for _, fk := range desc.ForeignKeys {
			parts = append(parts, fkClause(desc, fk))
		}
	}
	return "CREATE TABLE " + quoteIdent(desc.Name) + " (\n    " +
		strings.Join(parts, ",\n    ") + "\n);"
}

// indexDDL renders a CREATE INDEX statement. A UNIQUE constraint is
// stored as a unique index, so it comes back as one: the same
// enforcement, spelled as the index it always was.
func indexDDL(desc *bytdb.TableDesc, ix *bytdb.IndexDesc) string {
	cols := make([]string, len(ix.Cols))
	for i, o := range ix.Cols {
		name := "?"
		if o >= 0 && o < len(desc.Columns) {
			name = quoteIdent(desc.Columns[o].Name)
		}
		if ix.DescAt(i) {
			name += " DESC"
		}
		cols[i] = name
	}
	kw := "CREATE INDEX "
	if ix.Unique {
		kw = "CREATE UNIQUE INDEX "
	}
	return kw + quoteIdent(ix.Name) + " ON " + quoteIdent(desc.Name) +
		" (" + strings.Join(cols, ", ") + ");"
}

// sequenceDDL renders CREATE SEQUENCE plus the setval that restores its
// position. setval's third argument carries "called": false means the
// next nextval returns Last itself, true means it returns the value
// after Last.
func sequenceDDL(sq *bytdb.SeqDesc) []string {
	var b strings.Builder
	b.WriteString("CREATE SEQUENCE " + quoteIdent(sq.Name))
	if sq.Type != "" && sq.Type != "bigint" {
		b.WriteString(" AS " + sq.Type)
	}
	fmt.Fprintf(&b, " INCREMENT BY %d MINVALUE %d MAXVALUE %d START WITH %d CACHE %d",
		sq.Increment, sq.Min, sq.Max, sq.Start, max(sq.Cache, 1))
	if sq.Cycle {
		b.WriteString(" CYCLE")
	}
	b.WriteByte(';')
	setval := fmt.Sprintf("SELECT setval(%s, %d, %t);", quoteString(sq.Name), sq.Last, sq.Called)
	return []string{b.String(), setval}
}

// orderViews sorts views so each comes after every view its query
// mentions. Views are stored as query text with no dependency list, so
// a mention is detected lexically: the other view's name as a whole
// word. A false positive (the name inside a string literal) only
// reorders, never breaks, so the heuristic is safe.
func orderViews(views []bytdb.ViewDesc) []bytdb.ViewDesc {
	byName := map[string]bytdb.ViewDesc{}
	for _, v := range views {
		byName[v.Name] = v
	}
	var out []bytdb.ViewDesc
	state := map[string]int{} // 0 unvisited, 1 visiting, 2 done
	var visit func(v bytdb.ViewDesc)
	visit = func(v bytdb.ViewDesc) {
		if state[v.Name] != 0 {
			return // done, or a cycle (impossible for valid views)
		}
		state[v.Name] = 1
		for _, other := range views {
			if other.Name != v.Name && mentions(v.Query, other.Name) {
				visit(byName[other.Name])
			}
		}
		state[v.Name] = 2
		out = append(out, v)
	}
	for _, v := range views {
		visit(v)
	}
	return out
}

// mentions reports whether name appears in query as a whole word,
// case-insensitively, bare or double-quoted.
func mentions(query, name string) bool {
	re, err := regexp.Compile(`(?i)(^|[^A-Za-z0-9_$"])"?` + regexp.QuoteMeta(name) + `"?($|[^A-Za-z0-9_$"])`)
	if err != nil {
		return false
	}
	return re.MatchString(query)
}

// likePattern compiles a SQL LIKE pattern (% and _ wildcards) into a
// case-insensitive matcher for .tables/.schema filters. An empty
// pattern matches everything.
func likePattern(p string) func(string) bool {
	if p == "" {
		return func(string) bool { return true }
	}
	var re strings.Builder
	re.WriteString("(?i)^")
	for _, r := range p {
		switch r {
		case '%':
			re.WriteString(".*")
		case '_':
			re.WriteByte('.')
		default:
			re.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	re.WriteByte('$')
	rx := regexp.MustCompile(re.String()) // QuoteMeta keeps it valid
	return rx.MatchString
}

// writeSchema writes the DDL of every table, index, sequence and view
// whose name matches pattern, foreign keys inline.
func writeSchema(w io.Writer, e *bytdb.Engine, pattern string) error {
	match := likePattern(pattern)
	var out []string
	for _, sq := range e.Sequences() {
		if match(sq.Name) {
			out = append(out, sequenceDDL(sq)[0])
		}
	}
	for _, name := range e.Tables() {
		desc := e.Table(name)
		if desc == nil || !match(name) {
			continue
		}
		out = append(out, createTableDDL(desc, true))
		for i := range desc.Indexes {
			out = append(out, indexDDL(desc, &desc.Indexes[i]))
		}
	}
	for _, v := range orderViews(e.Views()) {
		if match(v.Name) {
			out = append(out, "CREATE VIEW "+quoteIdent(v.Name)+" AS "+v.Query+";")
		}
	}
	if len(out) == 0 {
		return nil
	}
	_, err := io.WriteString(w, strings.Join(out, "\n")+"\n")
	return err
}

// dumpBatchRows is how many INSERTs a dump wraps in one transaction.
// Autocommitting each row would fsync once per row on replay; one
// transaction per table would trip the engine's per-transaction write
// cap (bytdb.DefaultMaxTxnWrites) on a large table. Ten thousand rows
// amortizes the fsync to nothing while staying far below the cap even
// with several indexes per row.
const dumpBatchRows = 10_000

// writeDump writes a replayable SQL script for the named tables (all
// tables, plus sequences and views, when names is empty). All rows come
// from one read transaction, so the dump is a consistent snapshot even
// while other writers run.
func writeDump(w io.Writer, e *bytdb.Engine, names []string) error {
	full := len(names) == 0
	if full {
		names = e.Tables()
	}
	pr := func(format string, args ...any) error {
		_, err := fmt.Fprintf(w, format, args...)
		return err
	}
	if err := pr("-- bytdb dump\n"); err != nil {
		return err
	}
	if full {
		for _, sq := range e.Sequences() {
			for _, s := range sequenceDDL(sq) {
				if err := pr("%s\n", s); err != nil {
					return err
				}
			}
		}
	}
	return e.ReadTxn(func(tx *bytdb.Txn) error {
		var descs []*bytdb.TableDesc
		for _, name := range names {
			desc := tx.Table(name)
			if desc == nil {
				return fmt.Errorf("no such table: %s", name)
			}
			descs = append(descs, desc)
		}
		for _, desc := range descs {
			if err := pr("%s\n", createTableDDL(desc, false)); err != nil {
				return err
			}
			if err := dumpRows(w, tx, desc); err != nil {
				return err
			}
			if err := dumpIdentityCounters(w, tx, desc); err != nil {
				return err
			}
		}
		for _, desc := range descs {
			for i := range desc.Indexes {
				if err := pr("%s\n", indexDDL(desc, &desc.Indexes[i])); err != nil {
					return err
				}
			}
		}
		for _, desc := range descs {
			for _, fk := range desc.ForeignKeys {
				if !full && !slices.Contains(names, fk.RefTable) {
					// The parent is not in this partial dump; the
					// constraint would fail on replay into a database
					// without it, so say so instead of emitting it.
					if err := pr("-- skipped %s on %s: references %s, not in this dump\n",
						fk.Name, desc.Name, fk.RefTable); err != nil {
						return err
					}
					continue
				}
				if err := pr("ALTER TABLE %s ADD %s;\n", quoteIdent(desc.Name), fkClause(desc, fk)); err != nil {
					return err
				}
			}
		}
		if full {
			for _, v := range orderViews(e.Views()) {
				if err := pr("CREATE VIEW %s AS %s;\n", quoteIdent(v.Name), v.Query); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// dumpRows streams one table's rows as INSERT statements in primary-key
// order, batched into transactions of dumpBatchRows. Streaming from the
// engine scan (rather than SELECT *) keeps memory flat for any table
// size and sidesteps the result-row cap.
func dumpRows(w io.Writer, tx *bytdb.Txn, desc *bytdb.TableDesc) error {
	cols := make([]string, len(desc.Columns))
	for i, c := range desc.Columns {
		cols[i] = quoteIdent(c.Name)
	}
	prefix := "INSERT INTO " + quoteIdent(desc.Name) + " (" + strings.Join(cols, ", ") + ") VALUES ("
	var b strings.Builder
	n := 0
	flush := func() error {
		if b.Len() == 0 {
			return nil
		}
		_, err := io.WriteString(w, b.String())
		b.Reset()
		return err
	}
	for row, err := range tx.Scan(desc.Name) {
		if err != nil {
			return err
		}
		if n%dumpBatchRows == 0 {
			if n > 0 {
				b.WriteString("COMMIT;\n")
			}
			b.WriteString("BEGIN;\n")
		}
		b.WriteString(prefix)
		for i := range desc.Columns {
			if i > 0 {
				b.WriteString(", ")
			}
			var v any
			if i < len(row.Vals) {
				v = row.Vals[i]
			}
			b.WriteString(sqlLiteral(v, desc.Columns[i].Type))
		}
		b.WriteString(");\n")
		n++
		// Flush periodically so a huge table never builds one huge
		// string; 64 KiB keeps writes few without holding much.
		if b.Len() > 64<<10 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if n > 0 {
		b.WriteString("COMMIT;\n")
	}
	return flush()
}

// dumpIdentityCounters writes one ALTER TABLE ... RESTART WITH line per
// identity column whose counter exists, so a restored serial column
// resumes where the source's would have (N-032). Without it, the
// restored counter would sit just past the largest restored id, and a
// value that was drawn and then deleted before the dump could be handed
// out a second time.
//
// A column with no counter yet (nothing drawn or inserted since the
// table was created or truncated with RESTART IDENTITY) gets no line:
// the restored table has none either, and both draw 1 first. An
// exhausted counter (past math.MaxInt64) cannot be expressed as a
// RESTART value, so it gets a comment instead; the restored column then
// resumes after its largest stored id.
func dumpIdentityCounters(w io.Writer, tx *bytdb.Txn, desc *bytdb.TableDesc) error {
	for _, c := range desc.Columns {
		if !c.Identity {
			continue
		}
		next, ok, err := tx.PeekIdentity(desc.Name, c.Name)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if next > math.MaxInt64 {
			_, err = fmt.Fprintf(w, "-- identity %s.%s is exhausted; its counter is not restored\n",
				quoteIdent(desc.Name), quoteIdent(c.Name))
		} else {
			_, err = fmt.Fprintf(w, "ALTER TABLE %s ALTER COLUMN %s RESTART WITH %d;\n",
				quoteIdent(desc.Name), quoteIdent(c.Name), next)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// describeTable builds DESCRIBE's result for a table, shaped like
// DuckDB's: column_name, column_type, null, key, default, extra.
// key is PRI for primary-key columns and UNI for a column that alone
// carries a unique index.
func describeTable(desc *bytdb.TableDesc) *resultSet {
	pk := map[int]bool{}
	for _, o := range desc.PKCols {
		pk[o] = true
	}
	uni := map[int]bool{}
	for _, ix := range desc.Indexes {
		if ix.Unique && len(ix.Cols) == 1 {
			uni[ix.Cols[0]] = true
		}
	}
	rs := &resultSet{
		cols:  []string{"column_name", "column_type", "null", "key", "default", "extra"},
		types: []bytdb.ColType{bytdb.TString, bytdb.TString, bytdb.TString, bytdb.TString, bytdb.TString, bytdb.TString},
	}
	for i, c := range desc.Columns {
		typ := columnTypeName(c)
		null := "YES"
		if c.NotNull || c.Identity || pk[i] {
			null = "NO"
		}
		var key, def, extra any
		switch {
		case pk[i]:
			key = "PRI"
		case uni[i]:
			key = "UNI"
		}
		if c.Default != "" {
			def = c.Default
		}
		if c.Identity {
			extra = "identity"
		}
		rs.rows = append(rs.rows, []any{c.Name, typ, null, key, def, extra})
	}
	return rs
}

// indexesResult lists secondary indexes (of one table, or all), plus
// each table's primary key, so "which lookups are cheap" has one
// answer.
func indexesResult(e *bytdb.Engine, table string) (*resultSet, error) {
	names := e.Tables()
	if table != "" {
		if e.Table(table) == nil {
			return nil, fmt.Errorf("no such table: %s", table)
		}
		names = []string{table}
	}
	rs := &resultSet{
		cols:  []string{"table_name", "index_name", "unique", "columns"},
		types: []bytdb.ColType{bytdb.TString, bytdb.TString, bytdb.TBool, bytdb.TString},
	}
	for _, name := range names {
		desc := e.Table(name)
		if desc == nil {
			continue
		}
		if len(desc.PKCols) > 0 {
			rs.rows = append(rs.rows, []any{name, bytdb.PKConstraintName(name) + " (primary key)", true,
				strings.Trim(colList(desc, desc.PKCols), "()")})
		}
		for i := range desc.Indexes {
			ix := &desc.Indexes[i]
			cols := make([]string, len(ix.Cols))
			for j, o := range ix.Cols {
				cols[j] = "?"
				if o >= 0 && o < len(desc.Columns) {
					cols[j] = quoteIdent(desc.Columns[o].Name)
				}
				if ix.DescAt(j) {
					cols[j] += " DESC"
				}
			}
			rs.rows = append(rs.rows, []any{name, ix.Name, ix.Unique, strings.Join(cols, ", ")})
		}
	}
	return rs, nil
}
