package main

// importcsv.go: .import FILE TABLE — loading delimited text into a
// table. bytdb has no COPY, so the shell parses the file and runs a
// prepared INSERT per row.
//
// Semantics, chosen to match what DuckDB's read_csv and Postgres's COPY
// make people expect:
//
//   - All or nothing. Every row goes in one transaction; any bad row
//     rolls the whole import back, and a table the import created is
//     dropped again. (Inside the user's own BEGIN block the import joins
//     that block instead, and the block decides.)
//   - Header row: always present when the table does not exist yet (its
//     names become the column names). For an existing table the first
//     row is a header exactly when every field in it names a column of
//     the table — a data row spelled like the column names is not a
//     plausible accident, and the rule makes re-importing the same file
//     just work. --header / --no-header override. With a header, fields
//     map to columns by name, so column order in the file does not
//     matter.
//   - New tables fold header names to lower case, the way Postgres
//     folds unquoted identifiers, so a "Name" header becomes a name
//     column that SQL can mention without double quotes.
//   - New tables get inferred types from a sample of the first rows:
//     bigint, double precision, boolean, date, timestamp, else text.
//   - Every bytdb table needs a primary key, and a CSV declares none. A
//     new table takes --pk COL[,COL...] when given; otherwise a column
//     named id whose sampled values are present and distinct; otherwise
//     the import prepends a "rowid serial" column, which numbers the
//     rows in file order.
//   - An empty field is NULL, for every type (as in DuckDB).
//
// Values travel as strings and the engine coerces them to each column's
// type — the same path pgwire's text-format parameters take — so every
// type bytdb accepts as a quoted literal imports, including uuid,
// bytea (\x...), text[] ({a,b}) and jsonb.

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

// importSampleRows is how many data rows type inference looks at. The
// sample is held in memory before the table exists, so it is bounded;
// a later row that contradicts the inferred type fails the import (and
// rolls it back) with its line number.
const importSampleRows = 10_000

// importOpts are the parsed .import arguments.
type importOpts struct {
	file, table string
	sep         rune
	header      int // 0 auto, 1 forced on, -1 forced off
	skip        int
	pk          []string // --pk: primary-key columns for a new table
}

func parseImportArgs(args []string) (importOpts, error) {
	o := importOpts{}
	var pos []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--csv":
			o.sep = ','
		case "--tsv", "--tabs":
			o.sep = '\t'
		case "--header":
			o.header = 1
		case "--no-header", "--noheader":
			o.header = -1
		case "--pk":
			if i+1 >= len(args) {
				return o, errUsage
			}
			for _, c := range strings.Split(args[i+1], ",") {
				if c = strings.TrimSpace(c); c != "" {
					o.pk = append(o.pk, c)
				}
			}
			i++
		case "--skip":
			if i+1 >= len(args) {
				return o, errUsage
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 0 {
				return o, fmt.Errorf("--skip needs a non-negative number, got %q", args[i+1])
			}
			o.skip = n
			i++
		default:
			if strings.HasPrefix(a, "--") {
				return o, fmt.Errorf("unknown .import option %s", a)
			}
			pos = append(pos, a)
		}
	}
	if len(pos) != 2 {
		return o, errUsage
	}
	o.file, o.table = expandHome(pos[0]), pos[1]
	if o.sep == 0 {
		// Infer the delimiter from the extension; CSV otherwise.
		lower := strings.ToLower(o.file)
		if strings.HasSuffix(lower, ".tsv") || strings.HasSuffix(lower, ".tab") {
			o.sep = '\t'
		} else {
			o.sep = ','
		}
	}
	return o, nil
}

// importFile implements .import.
func (s *Shell) importFile(args []string) error {
	o, err := parseImportArgs(args)
	if err != nil {
		return err
	}
	f, err := os.Open(o.file)
	if err != nil {
		return err
	}
	defer f.Close()
	r := csv.NewReader(bufio.NewReaderSize(f, 1<<16))
	r.Comma = o.sep
	r.FieldsPerRecord = -1 // ragged rows get a line-numbered error below, not csv's
	if o.sep == '\t' {
		r.LazyQuotes = true // TSV rarely quotes; a stray " must not fail the file
	}
	for i := 0; i < o.skip; i++ {
		if _, err := r.Read(); err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("%s has fewer than %d lines to skip", o.file, o.skip)
			}
			return err
		}
	}

	table := unquoteIdent(o.table)
	desc := s.eng.Table(table)

	// Read the inference sample, header candidate included. The sample
	// doubles as the first batch of rows to insert.
	var sample [][]string
	var sampleLines []int
	for len(sample) < importSampleRows+1 {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		line, _ := r.FieldPos(0)
		sample = append(sample, rec)
		sampleLines = append(sampleLines, line)
	}
	if len(sample) > 0 && len(sample[0]) > 0 {
		sample[0][0] = strings.TrimPrefix(sample[0][0], "\ufeff") // Excel's UTF-8 BOM
	}

	// Decide whether the first row is a header (see the file comment).
	var header []string
	switch {
	case o.header == 1, o.header == 0 && desc == nil:
		if len(sample) == 0 {
			return fmt.Errorf("%s is empty", o.file)
		}
		header = sample[0]
	case o.header == 0 && len(sample) > 0 && namesColumns(desc, sample[0]):
		header = sample[0]
	}
	if header != nil {
		sample, sampleLines = sample[1:], sampleLines[1:]
	}

	created := false
	createdNote := ""   // reported only once the import has committed
	var target []string // the columns each record's fields fill, in order
	if desc == nil {
		if s.sess.Status() != bsql.TxIdle {
			return fmt.Errorf("table %s does not exist, and it cannot be created inside a transaction block; create it first or COMMIT", table)
		}
		var cols []string
		for _, h := range header {
			cols = append(cols, strings.ToLower(h))
		}
		if cols == nil {
			if len(sample) == 0 {
				return fmt.Errorf("%s has no rows to infer columns from", o.file)
			}
			cols = make([]string, len(sample[0]))
			for i := range cols {
				cols[i] = "column" + strconv.Itoa(i)
			}
		}
		cols = uniqueNames(cols)
		types := inferTypes(len(cols), sample)
		pk, rowid, err := choosePK(o.pk, cols, types, sample)
		if err != nil {
			return err
		}
		if _, err := s.sess.Exec(importCreateDDL(table, cols, types, pk, rowid)); err != nil {
			return fmt.Errorf("creating %s: %s", table, bytdb.ErrText(err))
		}
		created = true
		if desc = s.eng.Table(table); desc == nil {
			return fmt.Errorf("table %s vanished after CREATE", table)
		}
		target = cols
		createdNote = " (created; primary key " + strings.Join(pk, ", ") + ")"
		if rowid != "" {
			createdNote = " (created; primary key " + rowid + ", added)"
		}
	} else {
		if len(o.pk) > 0 {
			return fmt.Errorf("--pk applies only when the import creates the table; %s exists", table)
		}
		if target, err = importTarget(desc, header, sample); err != nil {
			return err
		}
	}

	quoted := make([]string, len(target))
	params := make([]string, len(target))
	for i, c := range target {
		quoted[i] = quoteIdent(c)
		params[i] = "$" + strconv.Itoa(i+1)
	}
	st, err := s.db.Prepare("INSERT INTO " + quoteIdent(table) + " (" + strings.Join(quoted, ", ") +
		") VALUES (" + strings.Join(params, ", ") + ")")
	if err != nil {
		return s.importAbort(created, table, err)
	}

	// One transaction for the whole file, unless the user's own block is
	// already open (then the rows join it, and their COMMIT decides).
	ownTx := s.sess.Status() == bsql.TxIdle
	if ownTx {
		if _, err := s.sess.Exec("BEGIN"); err != nil {
			return s.importAbort(created, table, err)
		}
	}
	ctx, done := s.statementContext() // Ctrl-C cancels the import
	defer done()
	vals := make([]any, len(target))
	n := 0
	insert := func(rec []string, line int) error {
		if len(rec) != len(target) {
			return fmt.Errorf("line %d: %d fields, want %d", line, len(rec), len(target))
		}
		for i, v := range rec {
			if v == "" {
				vals[i] = nil
			} else {
				vals[i] = v
			}
		}
		if _, err := s.sess.ExecStmtCtx(ctx, st, vals...); err != nil {
			if errors.Is(err, context.Canceled) {
				s.aborted = true
				return errors.New("import canceled by user request")
			}
			return fmt.Errorf("line %d: %s", line, bytdb.ErrText(err))
		}
		n++
		return nil
	}
	fail := func(err error) error {
		if ownTx {
			s.sess.Exec("ROLLBACK")
		}
		return s.importAbort(created, table, err)
	}
	for i, rec := range sample {
		if err := insert(rec, sampleLines[i]); err != nil {
			return fail(err)
		}
	}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(err)
		}
		line, _ := r.FieldPos(0)
		if err := insert(rec, line); err != nil {
			return fail(err)
		}
	}
	if ownTx {
		if _, err := s.sess.Exec("COMMIT"); err != nil {
			return s.importAbort(created, table, err)
		}
	}
	fmt.Fprintf(s.stdout, "imported %d rows into %s%s\n", n, table, createdNote)
	return nil
}

// namesColumns reports whether every field of rec names a column of
// desc (case-insensitively) — the test for "this row is a header".
func namesColumns(desc *bytdb.TableDesc, rec []string) bool {
	if len(rec) == 0 {
		return false
	}
	for _, f := range rec {
		if colOrdinal(desc, strings.TrimSpace(f)) < 0 {
			return false
		}
	}
	return true
}

// colOrdinal finds a column by exact name, then case-insensitively (a
// "Name" header for a name column).
func colOrdinal(desc *bytdb.TableDesc, name string) int {
	if i := desc.ColIndex(name); i >= 0 {
		return i
	}
	for i, c := range desc.Columns {
		if strings.EqualFold(c.Name, name) {
			return i
		}
	}
	return -1
}

// importTarget picks the columns an existing table's records fill: by
// name with a header; without one, every column in table order — or,
// when the records are exactly one field per non-identity column, those
// columns, letting identity columns (a rowid an earlier import added)
// number the new rows themselves.
func importTarget(desc *bytdb.TableDesc, header []string, sample [][]string) ([]string, error) {
	var target []string
	if header != nil {
		for _, h := range header {
			ord := colOrdinal(desc, strings.TrimSpace(h))
			if ord < 0 {
				return nil, fmt.Errorf("column %q in the header is not in table %s", h, desc.Name)
			}
			target = append(target, desc.Columns[ord].Name)
		}
		return target, nil
	}
	var all, plain []string
	for _, c := range desc.Columns {
		all = append(all, c.Name)
		if !c.Identity {
			plain = append(plain, c.Name)
		}
	}
	if len(sample) > 0 && len(sample[0]) == len(plain) && len(plain) < len(all) {
		return plain, nil
	}
	return all, nil
}

// choosePK picks a new table's primary key (see the file comment). It
// returns the key columns and, when it had to add one, the name of the
// added identity column ("" otherwise).
func choosePK(requested, cols []string, types []bytdb.ColType, sample [][]string) ([]string, string, error) {
	find := func(name string) int {
		for i, c := range cols {
			if strings.EqualFold(c, name) {
				return i
			}
		}
		return -1
	}
	if len(requested) > 0 {
		var pk []string
		for _, r := range requested {
			i := find(r)
			if i < 0 {
				return nil, "", fmt.Errorf("--pk column %q is not in the file's header", r)
			}
			pk = append(pk, cols[i])
		}
		return pk, "", nil
	}
	if i := find("id"); i >= 0 && distinctPresent(sample, i) {
		return []string{cols[i]}, "", nil
	}
	rowid := "rowid"
	for k := 2; find(rowid) >= 0; k++ {
		rowid = "rowid_" + strconv.Itoa(k)
	}
	return []string{rowid}, rowid, nil
}

// distinctPresent reports whether column i is non-empty and unique
// across the sample — the evidence that it can serve as a key.
func distinctPresent(sample [][]string, i int) bool {
	if len(sample) == 0 {
		return false
	}
	seen := make(map[string]bool, len(sample))
	for _, rec := range sample {
		if i >= len(rec) || rec[i] == "" || seen[rec[i]] {
			return false
		}
		seen[rec[i]] = true
	}
	return true
}

// importAbort undoes a table the import created, then returns err.
func (s *Shell) importAbort(created bool, table string, err error) error {
	if created {
		if _, derr := s.sess.Exec("DROP TABLE " + quoteIdent(table)); derr != nil {
			return fmt.Errorf("%w (and dropping the half-created table failed: %s)", err, bytdb.ErrText(derr))
		}
	}
	return err
}

// uniqueNames fixes up header names for use as columns: blanks become
// columnN, and repeats get _2, _3 suffixes.
func uniqueNames(names []string) []string {
	out := make([]string, len(names))
	seen := map[string]bool{} // lower-cased: "ID" and "id" collide once folded
	for i, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			n = "column" + strconv.Itoa(i)
		}
		cand := n
		for k := 2; seen[strings.ToLower(cand)]; k++ {
			cand = n + "_" + strconv.Itoa(k)
		}
		seen[strings.ToLower(cand)] = true
		out[i] = cand
	}
	return out
}

// importCreateDDL renders the CREATE TABLE for an import. rowid, when
// set, is an identity column prepended to hold the primary key.
func importCreateDDL(table string, cols []string, types []bytdb.ColType, pk []string, rowid string) string {
	var defs []string
	if rowid != "" {
		defs = append(defs, quoteIdent(rowid)+" serial")
	}
	for i, c := range cols {
		defs = append(defs, quoteIdent(c)+" "+typeName(types[i]))
	}
	keys := make([]string, len(pk))
	for i, k := range pk {
		keys[i] = quoteIdent(k)
	}
	defs = append(defs, "PRIMARY KEY ("+strings.Join(keys, ", ")+")")
	return "CREATE TABLE " + quoteIdent(table) + " (" + strings.Join(defs, ", ") + ")"
}

var (
	reDate      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	reTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}`)
)

// inferTypes picks each column's type from the sample: the narrowest
// type every non-empty value parses as, trying bigint, double precision,
// boolean, date, timestamp, then falling back to text. A column with no
// values at all is text.
//
// Integers with a leading zero ("02134") are text, not numbers: they are
// almost always codes (ZIPs, phone numbers), and a number would silently
// drop the zero.
func inferTypes(ncols int, sample [][]string) []bytdb.ColType {
	type cand struct{ isInt, isFloat, isBool, isDate, isTS, any bool }
	cs := make([]cand, ncols)
	for i := range cs {
		cs[i] = cand{true, true, true, true, true, false}
	}
	for _, rec := range sample {
		for i := 0; i < ncols && i < len(rec); i++ {
			v := rec[i]
			if v == "" {
				continue
			}
			c := &cs[i]
			c.any = true
			if c.isInt {
				_, err := strconv.ParseInt(v, 10, 64)
				digits := strings.TrimLeft(v, "+-")
				c.isInt = err == nil && !(len(digits) > 1 && digits[0] == '0')
			}
			if c.isFloat {
				_, err := strconv.ParseFloat(v, 64)
				digits := strings.TrimLeft(v, "+-")
				leadingZero := len(digits) > 1 && digits[0] == '0' && digits[1] != '.'
				c.isFloat = err == nil && !leadingZero && !strings.ContainsAny(v, "xX_") &&
					!strings.EqualFold(digits, "inf") && !strings.EqualFold(digits, "infinity") &&
					!strings.EqualFold(digits, "nan")
			}
			if c.isBool {
				l := strings.ToLower(v)
				c.isBool = l == "true" || l == "false"
			}
			if c.isDate {
				_, err := bytdb.ParseDate(v)
				c.isDate = err == nil && reDate.MatchString(v)
			}
			if c.isTS {
				_, err := bytdb.ParseTimestamp(v)
				c.isTS = err == nil && reTimestamp.MatchString(v)
			}
		}
	}
	out := make([]bytdb.ColType, ncols)
	for i, c := range cs {
		switch {
		case !c.any:
			out[i] = bytdb.TString
		case c.isInt:
			out[i] = bytdb.TInt
		case c.isFloat:
			out[i] = bytdb.TFloat
		case c.isBool:
			out[i] = bytdb.TBool
		case c.isDate:
			out[i] = bytdb.TDate
		case c.isTS:
			out[i] = bytdb.TTimestamp
		default:
			out[i] = bytdb.TString
		}
	}
	return out
}
