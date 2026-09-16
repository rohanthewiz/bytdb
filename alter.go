package bytdb

import (
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// alter.go: the schema changes that must rewrite a table's physical
// layout rather than append to it — changing a column's type and
// changing the primary key. Both share rebuildTable.
//
// Why a full rebuild and not an in-place edit. The key space is
//
//	tuple(tableID, 1, pk...)                        -> (colID, value) pairs
//	tuple(tableID, indexID, indexed..., [pk...])    -> () or tuple(pk...)
//
// so a primary-key change moves every row key AND every index entry
// (each carries the PK, as a key suffix or as its value), and a type
// change alters the encoded bytes of a value wherever it appears — in
// the row value, in the row key when the column is a key column, and in
// every index that orders by it, where the new type's ordering also
// decides the entry's position. Patching those in place would need a
// per-case map of which structures a column touches; re-deriving the
// whole table from its decoded rows under the new descriptor is one
// code path that cannot miss one, and it is the same code the write
// path already trusts (coerceRow, rowKey, encodeRowValue, indexEntry).
//
// The cost is the backfill's cost: every row is materialized and held
// until commit, so both operations respect Engine.SetBackfillLimit.

// ColumnTypeChange describes an ALTER COLUMN TYPE.
type ColumnTypeChange struct {
	// Type and MaxLen are the column's new type and VARCHAR(n) limit
	// (0 = unbounded; only valid on string columns).
	Type   ColType
	MaxLen int

	// Using, when non-nil, computes each row's new value from the whole
	// old row — the engine side of SQL's USING expression, which may
	// reference any column and may turn a NULL into a value. The result
	// is coerced to the new type like an inserted value. When nil, the
	// old value is converted by the automatic (assignment) casts
	// Postgres allows without USING; any other type pair is refused.
	Using func(old Row) (any, error)

	// Validate, when non-nil, is called with every rewritten row under
	// the new descriptor, inside the transaction; its error aborts the
	// change. The SQL layer re-checks CHECK constraints here, since the
	// engine treats their expressions as opaque.
	Validate func(Row) error
}

// AlterColumnType changes a column's type, rewriting every row (and
// every index entry that orders by the column, and the row keys when
// it is a key column) in the transaction that publishes the new
// descriptor. The change is atomic: a value that fails to convert, a
// conversion that makes two keys collide, or a Validate error leaves
// the table untouched.
//
// Refused outright:
//   - a column on either side of a foreign key — the parent and child
//     types must stay comparable, and changing both sides in one
//     statement is not expressible; drop the constraint first;
//   - an identity column to anything but int;
//   - a type pair with no automatic cast when Using is nil (Postgres's
//     "cannot be cast automatically", naming USING as the way out).
//
// A column DEFAULT is carried across by casting its value the same
// automatic way — Postgres does not apply USING to defaults either — and
// the statement fails if it cannot be, rather than leaving a default
// that no longer fits its column. Drop the default first in that case.
func (e *Engine) AlterColumnType(table, column string, ch ColumnTypeChange) error {
	if !validTypes[ch.Type] {
		return serr.New("unknown column type", "table", table, "column", column, "type", string(ch.Type))
	}
	err := e.alterDesc(table, func(tx *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error) {
		ord := old.ColIndex(column)
		if ord < 0 {
			return nil, serr.New("no such column", "table", table, "column", column)
		}
		oldCol := old.Columns[ord]
		newCol := oldCol
		newCol.Type, newCol.MaxLen = ch.Type, ch.MaxLen
		if err := validMaxLen(newCol); err != nil {
			return nil, serr.Wrap(err, "table", table)
		}
		if oldCol.Identity && ch.Type != TInt {
			return nil, serr.New("identity column must be an int column",
				"table", table, "column", column, "type", string(ch.Type))
		}
		if ch.Using == nil && oldCol.Type == ch.Type && oldCol.MaxLen == ch.MaxLen {
			return nil, nil // nothing changes; Postgres also skips the rewrite
		}
		if err := refuseFKColumn(e, tx, old, ord, "change the type of"); err != nil {
			return nil, err
		}
		cast := assignmentCast(oldCol.Type, ch.Type)
		if ch.Using == nil && cast == nil {
			return nil, cannotCastErr(column, ch.Type)
		}
		if oldCol.Default != "" {
			text, err := castDefault(&oldCol, &newCol)
			if err != nil {
				return nil, err
			}
			newCol.Default = text
		}

		next := old.clone()
		next.Columns[ord] = newCol
		transform := func(row Row) ([]any, error) {
			vals := slices.Clone(row.Vals)
			var err error
			if ch.Using != nil {
				vals[ord], err = ch.Using(row)
			} else if vals[ord] != nil {
				vals[ord], err = cast(vals[ord])
			}
			if err != nil {
				return nil, serr.Wrap(err, "table", table, "column", column)
			}
			return vals, nil
		}
		if err := e.checkRewriteLimit(tx, old, "alter column type"); err != nil {
			return nil, err
		}
		if err := rebuildTable(tx, old, next, transform, ch.Validate); err != nil {
			return nil, err
		}
		return next, nil
	})
	if err != nil {
		return serr.Wrap(err, "op", "alter column type", "table", table, "column", column)
	}
	return nil
}

// SetPrimaryKey replaces a table's primary key with the named columns,
// in key order — the engine side of Postgres's
//
//	ALTER TABLE t DROP CONSTRAINT t_pkey, ADD PRIMARY KEY (a, b)
//
// (a bytdb table cannot exist without a primary key, so "add" is only
// ever "replace"). Every row is re-keyed and every index rebuilt, in the
// transaction that publishes the descriptor.
//
// As ADD PRIMARY KEY does in Postgres, the new key's columns become NOT
// NULL; the old key's columns keep a NOT NULL flag too (Postgres leaves
// it behind when the constraint is dropped), so no row can later hold a
// NULL that the old key's guarantee had ruled out. The change fails,
// leaving the table untouched, when a new key column holds a NULL, when
// two rows share a new key, or when a foreign key from another table
// depends on the old key's uniqueness and no unique index still covers
// its columns.
func (e *Engine) SetPrimaryKey(table string, cols ...string) error {
	if len(cols) == 0 {
		return serr.New("a primary key is required", "table", table)
	}
	err := e.alterDesc(table, func(tx *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error) {
		ords := make([]int, 0, len(cols))
		for _, c := range cols {
			ord := old.ColIndex(c)
			if ord < 0 {
				return nil, serr.New(`column "`+c+`" of relation "`+table+`" does not exist`,
					"table", table, "column", c)
			}
			if slices.Contains(ords, ord) {
				return nil, serr.New(`column "`+c+`" appears twice in primary key constraint`,
					"table", table)
			}
			ords = append(ords, ord)
		}
		if slices.Equal(ords, old.PKCols) {
			return nil, nil // same key, same order: nothing to rebuild
		}
		next := old.clone()
		next.PKCols = ords
		for _, ord := range old.PKCols {
			next.Columns[ord].NotNull = true
		}
		for _, ord := range ords {
			next.Columns[ord].NotNull = true
		}

		// An inbound foreign key needs its referenced columns to stay a
		// unique key. Self-references are included: the table's own FK
		// is judged against the table's own next descriptor.
		refs, err := e.referencingFKs(tx, table, false)
		if err != nil {
			return nil, err
		}
		for _, r := range refs {
			if uniqueKeyCovers(old, r.FK.RefCols) && !uniqueKeyCovers(next, r.FK.RefCols) {
				return nil, serr.New("cannot drop the primary key a foreign key depends on",
					"table", table, "constraint", r.FK.Name, "referencing_table", r.Child.Name,
					"hint", "create a unique index on the referenced columns first")
			}
		}

		// Rows are unchanged; only their keys move. The NULL probe runs
		// here, before coerceRow, so the error is Postgres's ALTER
		// wording (23502) rather than the insert path's.
		transform := func(row Row) ([]any, error) {
			for _, ord := range ords {
				if row.Vals[ord] == nil {
					return nil, notNullValueErr(table, old.Columns[ord].Name)
				}
			}
			return row.Vals, nil
		}
		if err := e.checkRewriteLimit(tx, old, "set primary key"); err != nil {
			return nil, err
		}
		if err := rebuildTable(tx, old, next, transform, nil); err != nil {
			return nil, err
		}
		return next, nil
	})
	if err != nil {
		return serr.Wrap(err, "op", "set primary key", "table", table)
	}
	return nil
}

// PKConstraintName is the name bytdb reports for a table's primary-key
// constraint, following Postgres's default: <table>_pkey. There is no
// stored name — the key is structural — so a rename carries it along.
func PKConstraintName(table string) string { return table + "_pkey" }

// rebuildTable re-derives a table's entire key space — rows and every
// secondary index — from its current rows under a new descriptor.
//
//	phase 1  read    decode each row under old, transform, coerce under
//	                 next, validate. Nothing is written yet, so a bad
//	                 value fails before any work is done, and the scan
//	                 never walks a tree it is mutating.
//	phase 2  clear   one range delete of the table's space (primary and
//	                 all indexes share the tableID prefix).
//	phase 3  write   each row's key, value and index entries under next.
//	                 The space is empty, so any occupied key found now
//	                 was written earlier in this phase: a collision.
//
// transform receives the decoded old row and returns its values in the
// same column order (neither caller moves columns); it must not mutate
// row.Vals unless it returns them unchanged. validate, when non-nil,
// sees each coerced row under next.
func rebuildTable(tx *btypedb.Tx[string, []byte], old, next *TableDesc,
	transform func(Row) ([]any, error), validate func(Row) error) error {
	prefix := tablePrefix(old.ID)
	end := string(tuple.PrefixEnd(prefix))
	var rows [][]any
	for k, v := range tx.Ascend(string(prefix)) {
		if k >= end {
			break
		}
		row, err := decodeRow(old, k, v)
		if err != nil {
			return err
		}
		vals, err := transform(row)
		if err != nil {
			return err
		}
		cv, err := coerceRow(next, vals)
		if err != nil {
			return err
		}
		if validate != nil {
			if err := validate(Row{Desc: next, Vals: cv}); err != nil {
				return err
			}
		}
		rows = append(rows, cv)
	}

	space := tableSpace(old.ID)
	if _, err := tx.DeleteRange(string(space), string(tuple.PrefixEnd(space))); err != nil {
		return err
	}

	for _, r := range rows {
		pk := pkValues(next, r)
		key, err := rowKey(next, pk)
		if err != nil {
			return err
		}
		if tx.Contains(key) {
			return duplicateKeyErr(next, PKConstraintName(next.Name), next.PKCols, r)
		}
		val, err := encodeRowValue(next, r)
		if err != nil {
			return err
		}
		if err := tx.Set(key, val); err != nil {
			return err
		}
		for i := range next.Indexes {
			idx := &next.Indexes[i]
			ek, ev, enforced, err := indexEntry(next, idx, r)
			if err != nil {
				return err
			}
			if enforced && tx.Contains(ek) {
				return duplicateKeyErr(next, idx.Name, idx.Cols, r)
			}
			if err := tx.Set(ek, ev); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkRewriteLimit refuses a full-table rewrite over the engine's
// backfill cap, for the same reason AddColumn's DEFAULT backfill does:
// the rewrite is held live until commit, so an oversized table fails by
// OOM kill part-way rather than by error. Counting stops at limit+1.
func (e *Engine) checkRewriteLimit(tx *btypedb.Tx[string, []byte], desc *TableDesc, op string) error {
	limit := e.backfillLimit.Load()
	if limit <= 0 {
		return nil
	}
	if n := countRowsUpTo(tx, desc.ID, limit+1); n > limit {
		return serr.New(op+" would rewrite more rows than the backfill limit allows in one "+
			"transaction; raise Engine.SetBackfillLimit, or copy the rows into a new table in batches",
			"table", desc.Name, "backfillLimit", strconv.FormatInt(limit, 10))
	}
	return nil
}

// refuseFKColumn errors when the column at ord takes part in a foreign
// key on either side: as a child column of one of the table's own
// constraints, or as a referenced column of any table's (including a
// self-reference).
func refuseFKColumn(e *Engine, tx *btypedb.Tx[string, []byte], desc *TableDesc, ord int, verb string) error {
	name := desc.Columns[ord].Name
	for i := range desc.ForeignKeys {
		if slices.Contains(desc.ForeignKeys[i].Cols, ord) {
			return serr.New("cannot "+verb+" a foreign key column; drop the constraint first",
				"table", desc.Name, "column", name, "constraint", desc.ForeignKeys[i].Name)
		}
	}
	refs, err := e.referencingFKs(tx, desc.Name, false)
	if err != nil {
		return err
	}
	for _, r := range refs {
		if slices.Contains(r.FK.RefCols, name) {
			return serr.New("cannot "+verb+" a column referenced by a foreign key; drop the constraint first",
				"table", desc.Name, "column", name,
				"constraint", r.FK.Name, "referencing_table", r.Child.Name)
		}
	}
	return nil
}

// duplicateKeyErr is Postgres's rebuild-time uniqueness failure,
// "could not create unique index", with the colliding key in the
// detail the way Postgres renders it: Key (a, b)=(1, x) is duplicated.
func duplicateKeyErr(desc *TableDesc, index string, ords []int, row []any) error {
	names := make([]string, len(ords))
	vals := make([]string, len(ords))
	for i, ord := range ords {
		names[i] = desc.Columns[ord].Name
		if s, err := castToText(row[ord], desc.Columns[ord].Type); err == nil {
			vals[i] = s
		} else {
			vals[i] = fmt.Sprint(row[ord])
		}
	}
	return serr.New(`could not create unique index "`+index+`"`,
		"detail", "Key ("+strings.Join(names, ", ")+")=("+strings.Join(vals, ", ")+") is duplicated.")
}

// cannotCastErr is Postgres's wording (SQLSTATE 42804) for a type
// change with no automatic cast, including its USING hint.
func cannotCastErr(column string, to ColType) error {
	return serr.New(`column "`+column+`" cannot be cast automatically to type `+sqlTypeName(to),
		"hint", `You might need to specify "USING `+column+`::`+sqlTypeName(to)+`".`)
}

// sqlTypeName is the Postgres spelling of a column type, for messages.
func sqlTypeName(t ColType) string {
	switch t {
	case TBool:
		return "boolean"
	case TInt:
		return "bigint"
	case TFloat:
		return "double precision"
	case TString:
		return "text"
	case TBytes:
		return "bytea"
	case TTimestamp:
		return "timestamp"
	case TDate:
		return "date"
	case TUUID:
		return "uuid"
	case TTextArray:
		return "text[]"
	case TJSONB:
		return "jsonb"
	}
	return string(t)
}

// assignmentCast returns the conversion ALTER COLUMN TYPE applies
// without a USING clause, or nil when Postgres would demand one. The
// set mirrors Postgres's implicit and assignment casts over the types
// bytdb has:
//
//	same type          identity (a VARCHAR(n) change is enforced by coerceRow)
//	int   -> float     exact for |n| <= 2^53, nearest otherwise
//	float -> int       round half to even (Postgres's rint), range-checked
//	date  -> timestamp midnight UTC
//	timestamp -> date  the UTC day, floored for pre-1970 instants
//	any   -> text      the value's text output form
//
// Everything else (text -> int, text -> jsonb, int -> bool, ...) is an
// explicit cast in Postgres and needs USING.
//
// Values are the engine's runtime representations (see ColType): the
// input is never nil (NULL stays NULL without a cast).
func assignmentCast(from, to ColType) func(any) (any, error) {
	switch {
	case from == to:
		return func(v any) (any, error) { return v, nil }
	case to == TString:
		return func(v any) (any, error) { return castToText(v, from) }
	case from == TInt && to == TFloat:
		return func(v any) (any, error) { return float64(v.(int64)), nil }
	case from == TFloat && to == TInt:
		return func(v any) (any, error) {
			r := math.RoundToEven(v.(float64))
			// 2^63 is exactly representable; it and anything above (or
			// below -2^63, itself valid) is out of range, as is NaN.
			if math.IsNaN(r) || r < -9223372036854775808.0 || r >= 9223372036854775808.0 {
				return nil, serr.New("bigint out of range")
			}
			return int64(r), nil
		}
	case from == TDate && to == TTimestamp:
		return func(v any) (any, error) {
			days := v.(int64)
			const microsPerDay = 86400 * 1_000_000
			if days > math.MaxInt64/microsPerDay || days < math.MinInt64/microsPerDay {
				return nil, serr.New("date out of range for timestamp")
			}
			return days * microsPerDay, nil
		}
	case from == TTimestamp && to == TDate:
		return func(v any) (any, error) {
			const microsPerDay = 86400 * 1_000_000
			m := v.(int64)
			d := m / microsPerDay
			if m%microsPerDay < 0 { // floor, not truncation toward the epoch
				d--
			}
			return d, nil
		}
	}
	return nil
}

// castToText renders a runtime value of type t in its text output form
// — what Postgres's ::text would produce for the same value.
func castToText(v any, t ColType) (string, error) {
	switch t {
	case TBool:
		return strconv.FormatBool(v.(bool)), nil
	case TInt:
		return strconv.FormatInt(v.(int64), 10), nil
	case TFloat:
		f := v.(float64)
		switch {
		case math.IsNaN(f):
			return "NaN", nil
		case math.IsInf(f, 1):
			return "Infinity", nil
		case math.IsInf(f, -1):
			return "-Infinity", nil
		}
		return strconv.FormatFloat(f, 'g', -1, 64), nil
	case TString, TTextArray, TJSONB:
		// Arrays and jsonb already hold their canonical text.
		return v.(string), nil
	case TBytes:
		return `\x` + hex.EncodeToString(v.([]byte)), nil
	case TTimestamp:
		return FormatTimestamp(v.(int64)), nil
	case TDate:
		return FormatDate(v.(int64)), nil
	case TUUID:
		return FormatUUID(v.([]byte)), nil
	}
	return "", serr.New("no text form for column type", "type", string(t))
}

// castDefault carries a column's DEFAULT across a type change,
// returning the stored text for the new column.
//
// The clock markers are expressions, not values: they stay as they are
// when the new type is one they evaluate into (timestamp, date), and
// otherwise refuse. A constant is evaluated under the old column, cast
// with the automatic cast, and re-rendered in the literal forms
// default.go reads back — then evaluated under the new column as a
// final check, so a default the engine could not later apply never
// gets stored.
func castDefault(oldCol, newCol *Column) (string, error) {
	fail := func(cause error) error {
		err := serr.New(`default for column "`+oldCol.Name+`" cannot be cast automatically to type `+
			sqlTypeName(newCol.Type), "hint", "drop the default first, then set it again after the change")
		if cause != nil {
			err = serr.Wrap(err, "cause", cause.Error())
		}
		return err
	}
	text := strings.TrimSpace(oldCol.Default)
	if text == defaultNowText || text == defaultCurrentDateText {
		if newCol.Type != TTimestamp && newCol.Type != TDate {
			return "", fail(nil)
		}
		return oldCol.Default, nil
	}
	v, err := columnDefaultValue(oldCol, time.Now().UTC())
	if err != nil {
		return "", fail(err)
	}
	if v == nil {
		return "", nil // a NULL default is no default
	}
	cast := assignmentCast(oldCol.Type, newCol.Type)
	if cast == nil {
		return "", fail(nil)
	}
	if v, err = cast(v); err != nil {
		return "", fail(err)
	}
	var out string
	switch x := v.(type) {
	case int64:
		out = strconv.FormatInt(x, 10)
	case float64:
		out = strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		out = strconv.FormatBool(x)
	case string:
		out = "'" + strings.ReplaceAll(x, "'", "''") + "'"
	default:
		// []byte (bytea, uuid) has no literal form default.go reads.
		return "", fail(nil)
	}
	probe := *newCol
	probe.Default = out
	if _, err := columnDefaultValue(&probe, time.Now().UTC()); err != nil {
		return "", fail(err)
	}
	return out, nil
}
