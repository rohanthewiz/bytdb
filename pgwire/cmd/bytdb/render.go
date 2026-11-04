package main

// render.go: the output modes. Every mode renders the same thing — a
// result set of named, typed columns — so each is a function of
// (resultSet, renderOpts) writing to an io.Writer. Modes that a script
// consumes (csv, json, list, insert, ...) never truncate or elide; only
// duckbox, the interactive default, fits itself to the terminal.
//
// duckbox layout, for a result too tall and too wide for the screen:
//
//	┌───────┬─────────────────┬───┬────────┐
//	│  id   │      name       │ … │  city  │   names, centered
//	│bigint │      text       │   │  text  │   declared types
//	├───────┼─────────────────┼───┼────────┤
//	│     1 │ ada             │ … │ london │   numbers right-aligned
//	│     · │ ·               │ · │ ·      │   elided middle rows
//	│  1000 │ a very long st… │ … │ nyc    │   cells truncated with …
//	├───────┴─────────────────┴───┴────────┤
//	│ 1000 rows (40 shown)   9 columns (3 shown) │
//	└──────────────────────────────────────┘
//
// Rows are elided from the middle (first and last halves stay visible,
// because the ends of an ordered result are what people check), and
// columns likewise: the leftmost and rightmost survive, the middle
// collapses into a single "…" column.

import (
	"encoding/csv"
	"fmt"
	"html"
	"io"
	"strings"

	"github.com/mattn/go-runewidth"
	"github.com/rohanthewiz/bytdb"
)

// resultSet is one statement's tabular output.
type resultSet struct {
	cols  []string
	types []bytdb.ColType
	rows  [][]any
}

// colType returns the declared type of column i ("" when the result
// carries fewer types than columns, which only hand-built sets do).
func (r *resultSet) colType(i int) bytdb.ColType {
	if i < len(r.types) {
		return r.types[i]
	}
	return ""
}

// renderOpts are the settings that shape output; the shell builds one
// from its current state for every result.
type renderOpts struct {
	mode        string
	headers     bool
	null        string // NULL text for human-facing modes
	csvNull     string // NULL text for csv/tabs/list, "" unless set explicitly
	colSep      string // list-mode column separator
	rowSep      string // list-mode row terminator
	insertTable string // target table name for insert mode
	// maxRows and width bound duckbox only; 0 means unbounded. The
	// shell sets them when stdout is a terminal, so piped output is
	// never silently cut.
	maxRows int
	width   int
}

// modes lists every output mode with a one-line description, for .mode
// validation and help. Order is the order .help prints.
var modes = []struct{ name, desc string }{
	{"duckbox", "boxed table with column types, fitted to the terminal (default)"},
	{"box", "boxed table drawn with Unicode lines"},
	{"table", "boxed table drawn with ASCII characters"},
	{"markdown", "Markdown table"},
	{"csv", "comma-separated values"},
	{"tabs", "tab-separated values"},
	{"list", "values delimited by .separator (default |)"},
	{"line", "one value per line, column = value"},
	{"json", "JSON array of objects"},
	{"jsonlines", "one JSON object per line (NDJSON)"},
	{"html", "HTML <table>"},
	{"insert", "SQL INSERT statements for TABLE (.mode insert TABLE)"},
	{"trash", "discard output (still runs the query; pairs with .timer)"},
}

// modeAliases maps alternative spellings to a canonical mode.
var modeAliases = map[string]string{
	"ascii": "table", "md": "markdown", "tsv": "tabs", "ndjson": "jsonlines",
	"lines": "line", "sql": "insert",
}

// canonicalMode resolves a user-supplied mode name; ok is false when
// the name is unknown.
func canonicalMode(name string) (string, bool) {
	name = strings.ToLower(name)
	if a, ok := modeAliases[name]; ok {
		name = a
	}
	for _, m := range modes {
		if m.name == name {
			return name, true
		}
	}
	return "", false
}

// render writes rs to w in the requested mode.
func render(w io.Writer, rs *resultSet, o renderOpts) error {
	switch o.mode {
	case "duckbox":
		return renderBox(w, rs, o, unicodeBox, true)
	case "box":
		return renderBox(w, rs, o, unicodeBox, false)
	case "table":
		return renderBox(w, rs, o, asciiBox, false)
	case "markdown":
		return renderMarkdown(w, rs, o)
	case "csv":
		return renderCSV(w, rs, o, ',')
	case "tabs":
		return renderCSV(w, rs, o, '\t')
	case "list":
		return renderList(w, rs, o)
	case "line":
		return renderLine(w, rs, o)
	case "json":
		return renderJSON(w, rs, false)
	case "jsonlines":
		return renderJSON(w, rs, true)
	case "html":
		return renderHTML(w, rs, o)
	case "insert":
		return renderInsert(w, rs, o)
	case "trash":
		return nil
	}
	return fmt.Errorf("unknown output mode %q", o.mode)
}

// cellText renders one value for a boxed mode: NULL as o.null, and
// control characters escaped so a value can never break the grid.
func cellText(v any, t bytdb.ColType, o renderOpts) string {
	s, ok := displayValue(v, t)
	if !ok {
		return o.null
	}
	if strings.ContainsAny(s, "\n\r\t") {
		s = strings.NewReplacer("\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s)
	}
	return s
}

// boxStyle is the glyph set for a boxed table. Corner/junction names
// read as row (top, middle, bottom) then column (left, mid, right).
type boxStyle struct {
	h, v       string
	tl, tm, tr string
	ml, mm, mr string
	bl, bm, br string
}

var (
	unicodeBox = boxStyle{"─", "│", "┌", "┬", "┐", "├", "┼", "┤", "└", "┴", "┘"}
	asciiBox   = boxStyle{"-", "|", "+", "+", "+", "+", "+", "+", "+", "+", "+"}
)

// ellipsisCol marks the position of elided middle columns in the
// visible-column list.
const ellipsisCol = -1

// boxMinWidth is the narrowest a column shrinks to before the layout
// starts dropping whole columns instead. Below roughly this width a
// truncated value stops being recognizable.
const boxMinWidth = 8

// renderBox draws the boxed modes. duck selects the duckbox extras:
// the types row, row/column elision, width fitting and the footer.
func renderBox(w io.Writer, rs *resultSet, o renderOpts, st boxStyle, duck bool) error {
	n := len(rs.cols)
	if n == 0 {
		return nil
	}

	// Choose the rows to show. gapAt is the index in shown at which the
	// elided-rows marker goes (-1: nothing elided).
	shown := rs.rows
	gapAt := -1
	if duck && o.maxRows > 0 && len(rs.rows) > o.maxRows {
		top := (o.maxRows + 1) / 2
		bottom := o.maxRows - top
		shown = make([][]any, 0, o.maxRows)
		shown = append(shown, rs.rows[:top]...)
		shown = append(shown, rs.rows[len(rs.rows)-bottom:]...)
		gapAt = top
	}

	// Render every visible cell once; widths and drawing both use it.
	cells := make([][]string, len(shown))
	for r, row := range shown {
		cells[r] = make([]string, n)
		for c := 0; c < n && c < len(row); c++ {
			cells[r][c] = cellText(row[c], rs.colType(c), o)
		}
	}
	types := make([]string, n)
	natural := make([]int, n)
	for c := 0; c < n; c++ {
		if duck {
			types[c] = shortTypeName(rs.colType(c))
		}
		natural[c] = max(runewidth.StringWidth(rs.cols[c]), runewidth.StringWidth(types[c]), 1)
		for r := range cells {
			natural[c] = max(natural[c], runewidth.StringWidth(cells[r][c]))
		}
	}

	// Fit to the width budget: shrink, then drop middle columns.
	visible := make([]int, n)
	for c := range visible {
		visible[c] = c
	}
	widths := append([]int(nil), natural...)
	if duck && o.width > 0 {
		visible, widths = fitColumns(natural, o.width)
	}
	colsElided := containsInt(visible, ellipsisCol)

	// The footer, when there is one, must fit inside the table; widen
	// the last visible column if the table is narrower than its text.
	var footL, footR string
	if duck {
		switch {
		case len(rs.rows) == 0:
			footL = "0 rows"
		case gapAt >= 0:
			footL = fmt.Sprintf("%d rows (%d shown)", len(rs.rows), len(shown))
		}
		if colsElided {
			if footL == "" {
				footL = fmt.Sprintf("%d rows", len(rs.rows))
				if len(rs.rows) == 1 {
					footL = "1 row"
				}
			}
			kept := 0
			for _, c := range visible {
				if c != ellipsisCol {
					kept++
				}
			}
			footR = fmt.Sprintf("%d columns (%d shown)", n, kept)
		}
	}
	inner := func() int { // width between the outer borders
		t := 0
		for i := range visible {
			t += widths[i] + 3
		}
		return t - 1
	}
	if footL != "" {
		need := runewidth.StringWidth(footL) + 2
		if footR != "" {
			need += runewidth.StringWidth(footR) + 3
		}
		if extra := need - inner(); extra > 0 {
			// Widen the last real column; widening the "…" marker
			// column would look like a rendering glitch.
			last := len(visible) - 1
			for last > 0 && visible[last] == ellipsisCol {
				last--
			}
			widths[last] += extra
		}
	}

	var b strings.Builder
	line := func(l, m, r string) {
		b.WriteString(l)
		for i := range visible {
			if i > 0 {
				b.WriteString(m)
			}
			b.WriteString(strings.Repeat(st.h, widths[i]+2))
		}
		b.WriteString(r)
		b.WriteByte('\n')
	}
	// row draws one grid row; text(i) gives the visible column's text
	// and align(i) its alignment ('l', 'r' or 'c').
	row := func(text func(c int) string, align func(c int) byte) {
		b.WriteString(st.v)
		for i, c := range visible {
			s := ""
			a := byte('l')
			if c == ellipsisCol {
				s, a = "…", 'c'
			} else {
				s, a = text(c), align(c)
			}
			b.WriteByte(' ')
			b.WriteString(pad(s, widths[i], a))
			b.WriteByte(' ')
			b.WriteString(st.v)
		}
		b.WriteByte('\n')
	}
	center := func(int) byte { return 'c' }
	byType := func(c int) byte {
		if isNumeric(rs.colType(c)) {
			return 'r'
		}
		return 'l'
	}

	line(st.tl, st.tm, st.tr)
	if o.headers {
		row(func(c int) string { return rs.cols[c] }, center)
		if duck {
			row(func(c int) string { return types[c] }, center)
		}
		if len(shown) > 0 || footL == "" {
			line(st.ml, st.mm, st.mr)
		}
	}
	for r := range cells {
		if r == gapAt {
			// Three dot rows mark the elided middle, as DuckDB does: one
			// row of dots reads as a value, three read as "and so on".
			for k := 0; k < 3; k++ {
				row(func(int) string { return "·" }, byType)
			}
		}
		row(func(c int) string { return cells[r][c] }, byType)
	}
	if footL != "" {
		line(st.ml, st.bm, st.mr)
		width := inner()
		text := footL
		if footR != "" {
			gap := width - 2 - runewidth.StringWidth(footL) - runewidth.StringWidth(footR)
			text = footL + strings.Repeat(" ", max(gap, 3)) + footR
		}
		b.WriteString(st.v + " " + pad(text, width-2, 'l') + " " + st.v + "\n")
		b.WriteString(st.bl + strings.Repeat(st.h, width) + st.br + "\n")
	} else {
		line(st.bl, st.bm, st.br)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// fitColumns chooses which columns to show and how wide, so the boxed
// table is at most budget cells wide. It returns the visible column
// ordinals (with ellipsisCol where middle columns were dropped) and
// each visible column's width.
//
// Two stages. First shrink: find the largest cap C such that capping
// every column at max(C, its floor) fits — a water-filling search, so
// the widest columns give up width first and narrow ones keep theirs.
// The floor is boxMinWidth (or the natural width, when smaller). If
// even every column at its floor is too wide, drop columns: keep
// alternately from the left and right ends while they fit beside a
// one-character "…" column.
func fitColumns(natural []int, budget int) ([]int, []int) {
	n := len(natural)
	floor := make([]int, n)
	for i, w := range natural {
		floor[i] = min(w, boxMinWidth)
	}
	total := func(ws []int) int { // full table width for widths ws
		t := 1
		for _, w := range ws {
			t += w + 3
		}
		return t
	}
	capped := func(c int) []int {
		ws := make([]int, n)
		for i, w := range natural {
			ws[i] = max(floor[i], min(w, c))
		}
		return ws
	}
	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	if total(natural) <= budget {
		return all, append([]int(nil), natural...)
	}
	if ws := capped(0); total(ws) <= budget {
		// Binary search the largest cap that still fits.
		lo, hi := 0, 0
		for _, w := range natural {
			hi = max(hi, w)
		}
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if total(capped(mid)) <= budget {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		return all, capped(lo)
	}

	// Drop middle columns. The "…" column costs 4 cells (" … " plus
	// its border); keep at least one real column even if it must be
	// truncated below its floor.
	used := 1 + 4
	var left, right []int
	l, r := 0, n-1
	for l <= r {
		took := false
		if w := floor[l] + 3; used+w <= budget {
			left = append(left, l)
			used += w
			l++
			took = true
		}
		if l <= r {
			if w := floor[r] + 3; used+w <= budget {
				right = append([]int{r}, right...)
				used += w
				r--
				took = true
			}
		}
		if !took {
			break
		}
	}
	if len(left) == 0 && len(right) == 0 {
		// Not even one column fits at its floor: show the first one,
		// squeezed to whatever space remains.
		return []int{0, ellipsisCol}, []int{max(budget-1-4-3, 1), 1}
	}
	vis := append(append(append([]int(nil), left...), ellipsisCol), right...)
	ws := make([]int, len(vis))
	for i, c := range vis {
		if c == ellipsisCol {
			ws[i] = 1
		} else {
			ws[i] = floor[c]
		}
	}
	// Give any leftover budget back, widest-natural-first, so the kept
	// columns are not needlessly truncated.
	spare := budget - total(ws)
	for spare > 0 {
		best := -1
		for i, c := range vis {
			if c != ellipsisCol && ws[i] < natural[c] && (best < 0 || natural[c]-ws[i] > natural[vis[best]]-ws[best]) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		ws[best]++
		spare--
	}
	return vis, ws
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// pad fits s to exactly width display cells: truncated with "…" when
// too wide, padded per align ('l', 'r', 'c') when too narrow. Widths are
// display cells (runewidth), not bytes or runes, so CJK text and emoji
// line up.
func pad(s string, width int, align byte) string {
	sw := runewidth.StringWidth(s)
	if sw > width {
		s = runewidth.Truncate(s, width, "…")
		sw = runewidth.StringWidth(s)
	}
	gap := width - sw
	switch align {
	case 'r':
		return strings.Repeat(" ", gap) + s
	case 'c':
		left := gap / 2
		return strings.Repeat(" ", left) + s + strings.Repeat(" ", gap-left)
	}
	return s + strings.Repeat(" ", gap)
}

// renderMarkdown writes a GitHub-flavored Markdown table. Numeric
// columns get a right-aligned separator (---:), and '|' in values is
// escaped so it cannot split a cell.
func renderMarkdown(w io.Writer, rs *resultSet, o renderOpts) error {
	n := len(rs.cols)
	esc := func(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
	cells := make([][]string, len(rs.rows))
	widths := make([]int, n)
	for c := 0; c < n; c++ {
		widths[c] = max(runewidth.StringWidth(esc(rs.cols[c])), 3)
	}
	for r, row := range rs.rows {
		cells[r] = make([]string, n)
		for c := 0; c < n && c < len(row); c++ {
			cells[r][c] = esc(cellText(row[c], rs.colType(c), o))
			widths[c] = max(widths[c], runewidth.StringWidth(cells[r][c]))
		}
	}
	var b strings.Builder
	writeRow := func(text func(c int) string, align func(c int) byte) {
		b.WriteString("|")
		for c := 0; c < n; c++ {
			b.WriteString(" " + pad(text(c), widths[c], align(c)) + " |")
		}
		b.WriteByte('\n')
	}
	byType := func(c int) byte {
		if isNumeric(rs.colType(c)) {
			return 'r'
		}
		return 'l'
	}
	// Markdown requires a header row; with headers off the column names
	// still have to go somewhere, so headers off is ignored here.
	writeRow(func(c int) string { return esc(rs.cols[c]) }, func(int) byte { return 'l' })
	b.WriteString("|")
	for c := 0; c < n; c++ {
		if isNumeric(rs.colType(c)) {
			b.WriteString(strings.Repeat("-", widths[c]+1) + ":|")
		} else {
			b.WriteString(strings.Repeat("-", widths[c]+2) + "|")
		}
	}
	b.WriteByte('\n')
	for r := range cells {
		writeRow(func(c int) string { return cells[r][c] }, byType)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// renderCSV writes RFC 4180 CSV (or TSV with sep '\t'); quoting is
// encoding/csv's: only fields that need it. A multi-byte or multi-
// character .separator falls back to the mode's default, since CSV
// delimiters are single characters.
func renderCSV(w io.Writer, rs *resultSet, o renderOpts, sep rune) error {
	cw := csv.NewWriter(w)
	cw.Comma = sep
	// The shell sets colSep to the mode's own delimiter on every mode
	// switch, so a single-byte colSep here is either that default or an
	// explicit .separator. Delimiters CSV cannot express (quote, CR,
	// LF) keep the mode's default instead of failing every write.
	if len(o.colSep) == 1 && !strings.ContainsAny(o.colSep, "\"\r\n") {
		cw.Comma = rune(o.colSep[0])
	}
	if o.headers {
		if err := cw.Write(rs.cols); err != nil {
			return err
		}
	}
	rec := make([]string, len(rs.cols))
	for _, row := range rs.rows {
		for c := range rec {
			rec[c] = o.csvNull
			if c < len(row) {
				if s, ok := displayValue(row[c], rs.colType(c)); ok {
					rec[c] = s
				}
			}
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// renderList writes values joined by the column separator, one row per
// row separator, with no quoting at all — the sqlite list mode, handy
// for shell pipelines (cut -d'|').
func renderList(w io.Writer, rs *resultSet, o renderOpts) error {
	var b strings.Builder
	if o.headers {
		b.WriteString(strings.Join(rs.cols, o.colSep))
		b.WriteString(o.rowSep)
	}
	for _, row := range rs.rows {
		for c := range rs.cols {
			if c > 0 {
				b.WriteString(o.colSep)
			}
			s := o.csvNull
			if c < len(row) {
				if v, ok := displayValue(row[c], rs.colType(c)); ok {
					s = v
				}
			}
			b.WriteString(s)
		}
		b.WriteString(o.rowSep)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// renderLine writes each row as "column = value" lines, names right
// aligned, with a blank line between rows — the mode for wide rows that
// do not fit a grid (psql's \x).
func renderLine(w io.Writer, rs *resultSet, o renderOpts) error {
	nameW := 0
	for _, c := range rs.cols {
		nameW = max(nameW, runewidth.StringWidth(c))
	}
	var b strings.Builder
	for r, row := range rs.rows {
		if r > 0 {
			b.WriteByte('\n')
		}
		for c, name := range rs.cols {
			s := o.null
			if c < len(row) {
				if v, ok := displayValue(row[c], rs.colType(c)); ok {
					s = v
				}
			}
			b.WriteString(pad(name, nameW, 'r'))
			b.WriteString(" = ")
			b.WriteString(s)
			b.WriteByte('\n')
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// renderJSON writes the rows as JSON objects keyed by column name, in
// column order (a map would reorder keys). lines selects NDJSON;
// otherwise the objects form one array, one object per line, which
// stays both valid JSON and greppable.
func renderJSON(w io.Writer, rs *resultSet, lines bool) error {
	var b strings.Builder
	keys := make([]string, len(rs.cols))
	for c, name := range rs.cols {
		keys[c] = jsonValue(name, bytdb.TString)
	}
	if !lines {
		b.WriteByte('[')
	}
	for r, row := range rs.rows {
		if r > 0 {
			if lines {
				b.WriteByte('\n')
			} else {
				b.WriteString(",\n")
			}
		}
		b.WriteByte('{')
		for c := range rs.cols {
			if c > 0 {
				b.WriteByte(',')
			}
			b.WriteString(keys[c])
			b.WriteByte(':')
			var v any
			if c < len(row) {
				v = row[c]
			}
			b.WriteString(jsonValue(v, rs.colType(c)))
		}
		b.WriteByte('}')
	}
	if !lines {
		b.WriteByte(']')
	}
	if !lines || len(rs.rows) > 0 {
		b.WriteByte('\n')
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// renderHTML writes an HTML table with every value escaped.
func renderHTML(w io.Writer, rs *resultSet, o renderOpts) error {
	var b strings.Builder
	b.WriteString("<table>\n")
	if o.headers {
		b.WriteString("<tr>")
		for _, c := range rs.cols {
			b.WriteString("<th>" + html.EscapeString(c) + "</th>")
		}
		b.WriteString("</tr>\n")
	}
	for _, row := range rs.rows {
		b.WriteString("<tr>")
		for c := range rs.cols {
			s := o.null
			if c < len(row) {
				if v, ok := displayValue(row[c], rs.colType(c)); ok {
					s = v
				}
			}
			b.WriteString("<td>" + html.EscapeString(s) + "</td>")
		}
		b.WriteString("</tr>\n")
	}
	b.WriteString("</table>\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// renderInsert writes one INSERT statement per row, with an explicit
// column list so the output stays valid against a table whose column
// order differs.
func renderInsert(w io.Writer, rs *resultSet, o renderOpts) error {
	table := o.insertTable
	if table == "" {
		table = "tbl"
	}
	cols := make([]string, len(rs.cols))
	for c, name := range rs.cols {
		cols[c] = quoteIdent(name)
	}
	prefix := "INSERT INTO " + quoteIdent(table) + " (" + strings.Join(cols, ", ") + ") VALUES ("
	var b strings.Builder
	for _, row := range rs.rows {
		b.WriteString(prefix)
		for c := range rs.cols {
			if c > 0 {
				b.WriteString(", ")
			}
			var v any
			if c < len(row) {
				v = row[c]
			}
			b.WriteString(sqlLiteral(v, rs.colType(c)))
		}
		b.WriteString(");\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
