package bytdb

// embed_api.go: Go embedding conveniences (deep-look D-56, part 2) —
// argument normalization for named and wrapped Go types, Row getters,
// error-returning catalog lookups, and the one-shot operations that
// existed only on Txn (Truncate, NextVal, SetVal) plus key-only Count
// and Exists.

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// --- argument normalization ---

// ErrUnsupportedValue is NormalizeValue's refusal of a Go type it has
// no stored kind for (a struct, a map, a channel, ...).
var ErrUnsupportedValue = errors.New("unsupported value type")

// NormalizeValue reduces a Go value to the kinds the engine stores and
// compares: nil, int64, float64, string, bool, []byte, time.Time,
// []string (a text[] value), or Decimal (a numeric value). It accepts
// what callers naturally hold:
//
//	type UserID int64 / type Code string   named types, by reflect kind
//	*int64, *string, ...                   pointers (nil is SQL NULL)
//	driver.Valuer                          uuid.UUID, sql.NullString, a decimal library's type, ...
//	uint, uint64                           when they fit in int64
//	[16]byte and named 16-byte arrays      a uuid
//	[]string                               a text[] value
//
// Values already in a stored kind pass through unchanged. The engine
// calls it only when a value does not fit its column as is, so the
// common path pays nothing for the reflection.
func NormalizeValue(v any) (any, error) {
	switch x := v.(type) {
	case nil, int64, float64, string, bool, []byte, time.Time, []string, Decimal:
		return v, nil
	case driver.Valuer:
		dv, err := x.Value()
		if err != nil {
			return nil, serr.Wrap(err, "op", "driver.Valuer")
		}
		if _, again := dv.(driver.Valuer); again {
			return nil, serr.New("driver.Valuer returned another Valuer", "type", fmt.Sprintf("%T", v))
		}
		return NormalizeValue(dv)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return nil, nil
		}
		return NormalizeValue(rv.Elem().Interface())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		u := rv.Uint()
		if u > math.MaxInt64 {
			return nil, serr.New("unsigned value overflows int64", "value", fmt.Sprint(u))
		}
		return int64(u), nil
	case reflect.Float32, reflect.Float64:
		return rv.Float(), nil
	case reflect.String:
		return rv.String(), nil
	case reflect.Bool:
		return rv.Bool(), nil
	case reflect.Slice:
		switch rv.Type().Elem().Kind() {
		case reflect.Uint8:
			return rv.Bytes(), nil
		case reflect.String:
			out := make([]string, rv.Len())
			for i := range out {
				out[i] = rv.Index(i).String()
			}
			return out, nil
		}
	case reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 && rv.Len() == 16 {
			b := make([]byte, 16)
			reflect.Copy(reflect.ValueOf(b), rv)
			return b, nil
		}
	}
	return nil, serr.Wrap(ErrUnsupportedValue, "type", fmt.Sprintf("%T", v))
}

// --- Row access ---

// Map returns the row as column name -> value; nil for the zero Row a
// lookup miss returns.
func (r Row) Map() map[string]any {
	if r.Desc == nil {
		return nil
	}
	m := make(map[string]any, len(r.Vals))
	for i, c := range r.Desc.Columns {
		if i < len(r.Vals) {
			m[c.Name] = r.Vals[i]
		}
	}
	return m
}

// Int returns an int column's value; ok is false for NULL, a missing
// column, or another type. Timestamp and date columns are int64 too
// (micros and days): use Time for those.
func (r Row) Int(name string) (int64, bool) {
	v, ok := r.Col(name).(int64)
	return v, ok
}

// Float returns a float column's value; ok is false for NULL, a missing
// column, or another type.
func (r Row) Float(name string) (float64, bool) {
	v, ok := r.Col(name).(float64)
	return v, ok
}

// String returns a text (or text[] / jsonb, whose values are their
// canonical text) column's value; ok is false for NULL, a missing
// column, or another type.
func (r Row) String(name string) (string, bool) {
	v, ok := r.Col(name).(string)
	return v, ok
}

// Decimal returns a numeric column's value; ok is false for NULL, a
// missing column, or another type. Its String is the Postgres text
// form ("19.99"); Float64 converts when exactness is not needed.
func (r Row) Decimal(name string) (Decimal, bool) {
	v, ok := r.Col(name).(Decimal)
	return v, ok
}

// Bool returns a bool column's value; ok is false for NULL, a missing
// column, or another type.
func (r Row) Bool(name string) (bool, bool) {
	v, ok := r.Col(name).(bool)
	return v, ok
}

// Bytes returns a bytea (or uuid: 16 bytes) column's value; ok is false
// for NULL, a missing column, or another type.
func (r Row) Bytes(name string) ([]byte, bool) {
	v, ok := r.Col(name).([]byte)
	return v, ok
}

// Time returns a timestamp or date column's value as a UTC time.Time
// (a date at midnight); ok is false for NULL, a missing column, or a
// column of another type.
func (r Row) Time(name string) (time.Time, bool) {
	if r.Desc == nil {
		return time.Time{}, false
	}
	i := r.Desc.ColIndex(name)
	if i < 0 || i >= len(r.Vals) {
		return time.Time{}, false
	}
	n, ok := r.Vals[i].(int64)
	if !ok {
		return time.Time{}, false
	}
	switch r.Desc.Columns[i].Type {
	case TTimestamp:
		return time.UnixMicro(n).UTC(), true
	case TDate:
		return time.Unix(n*86400, 0).UTC(), true
	}
	return time.Time{}, false
}

// --- Update with Get/Delete's argument shape ---

// UpdateRow is Update with the primary key last and variadic, the shape
// Get and Delete take: e.UpdateRow("users", set, id) instead of
// e.Update("users", []any{id}, set).
func (e *Engine) UpdateRow(table string, set map[string]any, pkVals ...any) (bool, error) {
	return e.Update(table, pkVals, set)
}

// UpdateRow is Engine.UpdateRow in the transaction.
func (t *Txn) UpdateRow(table string, set map[string]any, pkVals ...any) (bool, error) {
	return t.Update(table, pkVals, set)
}

// --- catalog lookups that report errors ---
//
// Tables, Table, Views, View, Sequences and Sequence return "nothing"
// both for an absent object and for a failed read (a closed engine, a
// corrupt descriptor). These variants tell the two apart: absence is
// ErrNoSuchTable (errors.Is), anything else is the read's error.

// TableNames is Tables with the read's error.
func (e *Engine) TableNames() ([]string, error) {
	var names []string
	err := e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		prefix := descTablePrefix()
		end := string(tuple.PrefixEnd([]byte(prefix)))
		for k := range tx.Ascend(prefix) {
			if k >= end {
				break
			}
			names = append(names, descKeyName(k))
		}
		return nil
	})
	return names, err
}

// LookupTable is Table with errors: ErrNoSuchTable when absent.
func (e *Engine) LookupTable(name string) (*TableDesc, error) {
	var desc *TableDesc
	err := e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		var err error
		desc, err = e.tableFromView(tx, name)
		return err
	})
	if err != nil {
		return nil, err
	}
	if desc == nil {
		return nil, errNoSuchTable(name)
	}
	return desc.Clone(), nil
}

// ListViews is Views with the read's error (a view whose descriptor
// does not decode is an error here, where Views skips it).
func (e *Engine) ListViews() ([]ViewDesc, error) {
	var out []ViewDesc
	err := e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		prefix := mustEncode(int64(sysViewTableID), int64(primaryIndexID))
		end := string(tuple.PrefixEnd(prefix))
		for k, blob := range tx.Ascend(string(prefix)) {
			if k >= end {
				break
			}
			v := ViewDesc{}
			if err := json.Unmarshal(blob, &v); err != nil {
				return serr.Wrap(err, "op", "decode view descriptor")
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

// LookupView is View with errors: ErrNoSuchTable (views share the
// relation namespace) when absent.
func (e *Engine) LookupView(name string) (*ViewDesc, error) {
	var vd *ViewDesc
	err := e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		blob, ok := tx.Get(viewKey(name))
		if !ok {
			return nil
		}
		v := &ViewDesc{}
		if err := json.Unmarshal(blob, v); err != nil {
			return serr.Wrap(err, "op", "decode view descriptor", "view", name)
		}
		vd = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	if vd == nil {
		return nil, errNoSuchTable(name)
	}
	return vd, nil
}

// ListSequences is Sequences with the read's error.
func (e *Engine) ListSequences() ([]*SeqDesc, error) {
	var out []*SeqDesc
	err := e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		prefix := sqlSeqPrefix()
		end := string(tuple.PrefixEnd(prefix))
		for k, v := range tx.Ascend(string(prefix)) {
			if k >= end {
				break
			}
			d := &SeqDesc{}
			if err := json.Unmarshal(v, d); err != nil {
				return serr.Wrap(err, "op", "decode sequence descriptor")
			}
			out = append(out, d)
		}
		return nil
	})
	return out, err
}

// LookupSequence is Sequence with errors: ErrNoSuchTable (sequences
// share the relation namespace) when absent.
func (e *Engine) LookupSequence(name string) (*SeqDesc, error) {
	var desc *SeqDesc
	err := e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		var err error
		desc, err = seqDescIn(tx, name)
		return err
	})
	if err != nil {
		return nil, err
	}
	if desc == nil {
		return nil, errNoSuchTable(name)
	}
	return desc, nil
}

// --- one-shots ---

// Truncate removes every row of table in its own write transaction
// (Txn.Truncate's rules: a table referenced by a foreign key truncates
// only together with its referencing tables, which this one-table form
// cannot do). restartIdentity resets the table's identity counters.
func (e *Engine) Truncate(table string, restartIdentity bool) error {
	return e.WriteTxn(func(tx *Txn) error { return tx.Truncate(table, restartIdentity) })
}

// NextVal draws the next value of a sequence object in its own write
// transaction (Txn.NextVal).
func (e *Engine) NextVal(name string) (int64, error) {
	var v int64
	err := e.WriteTxn(func(tx *Txn) error {
		var err error
		v, err = tx.NextVal(name)
		return err
	})
	return v, err
}

// SetVal positions a sequence object in its own write transaction
// (Txn.SetVal): called true means v counts as already returned.
func (e *Engine) SetVal(name string, v int64, called bool) error {
	return e.WriteTxn(func(tx *Txn) error { return tx.SetVal(name, v, called) })
}

// Count returns the number of rows in table, from the committed state
// at the call. It walks the primary index's keys without decoding a
// row, so it costs a key iteration, not a scan.
func (e *Engine) Count(table string) (int64, error) {
	var n int64
	err := e.ReadTxn(func(tx *Txn) error {
		var err error
		n, err = tx.Count(table)
		return err
	})
	return n, err
}

// Count is Engine.Count in the transaction's view (its own writes
// included). Under serializable tracking the whole table range counts
// as read, as a scan's does.
func (t *Txn) Count(table string) (int64, error) {
	desc, err := t.desc(table)
	if err != nil {
		return 0, err
	}
	prefix := tablePrefix(desc.ID)
	start, end := string(prefix), string(tuple.PrefixEnd(prefix))
	markReadRange(t.tx, start, end)
	var n int64
	for k := range t.tx.Ascend(start) {
		if k >= end {
			break
		}
		n++
	}
	return n, nil
}

// Exists reports whether a row with the given primary key exists, from
// the committed state at the call, without decoding it.
func (e *Engine) Exists(table string, pkVals ...any) (bool, error) {
	var ok bool
	err := e.ReadTxn(func(tx *Txn) error {
		var err error
		ok, err = tx.Exists(table, pkVals...)
		return err
	})
	return ok, err
}

// Exists is Engine.Exists in the transaction's view.
func (t *Txn) Exists(table string, pkVals ...any) (bool, error) {
	desc, err := t.desc(table)
	if err != nil {
		return false, err
	}
	key, err := fullPKKey(desc, pkVals)
	if err != nil {
		return false, err
	}
	t.tx.MarkRead(key) // absence is information too (serializable mode)
	return t.tx.Contains(key), nil
}
