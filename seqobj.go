package bytdb

import (
	"encoding/json"
	"strconv"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// seqobj.go: SQL sequence objects — the durable form behind CREATE
// SEQUENCE. Unlike the bare named counters in seq.go (8-byte "next
// value" cells with no options), a sequence object carries Postgres'
// full option set (start, increment, min/max, cycle, cache) plus its
// allocation state, persisted together as one JSON record:
//
//	tuple(sysSeqTableID, sqlSeqIndexID, name) -> JSON SeqDesc
//
// One record keeps every NextVal a single read-modify-write inside
// whatever transaction it runs in, and the options can never tear
// from the state. Object IDs come from the same counter as table IDs,
// so a sequence's ID doubles as a unique pg_class oid.
//
// Allocation is transactional, like identity columns: a NextVal in a
// rolled-back transaction is not consumed (Postgres burns the value
// instead — callers wanting gap behavior for compatibility should not
// rely on either). Cache is stored and reported but allocation is
// effectively CACHE 1: this engine has a single writer, so batching
// values would only manufacture gaps.

// fmtInt renders an int64 for error messages.
func fmtInt(v int64) string { return strconv.FormatInt(v, 10) }

// sqlSeqIndexID is the "index" within the system sequences table
// holding sequence objects, keyed by name — separate from named
// counters (1) and identity counters (2).
const sqlSeqIndexID = 3

// SeqDesc describes one sequence object: its options and its current
// state. Last/Called mirror Postgres' last_value/is_called: Called is
// false until the first NextVal, whose result is Start itself.
type SeqDesc struct {
	FormatVersion uint32 `json:"format_version,omitempty"`
	ID            uint64 `json:"id"`
	Name          string `json:"name"`
	// Type is the declared AS type name — "smallint", "integer", or
	// "bigint" ("" reads as bigint, the default). It only bounds the
	// declarable MIN/MAXVALUE and drives catalog reporting; values are
	// int64 throughout.
	Type      string `json:"type,omitempty"`
	Start     int64  `json:"start"`
	Increment int64  `json:"increment"`
	Min       int64  `json:"min"`
	Max       int64  `json:"max"`
	Cycle     bool   `json:"cycle,omitempty"`
	Cache     int64  `json:"cache"`
	Last      int64  `json:"last"`
	Called    bool   `json:"called"`
}

// sqlSeqKey is a sequence object's key in the system sequences table.
func sqlSeqKey(name string) string {
	return string(mustEncode(int64(sysSeqTableID), int64(sqlSeqIndexID), name))
}

// sqlSeqPrefix covers every sequence object, for listing.
func sqlSeqPrefix() []byte {
	return mustEncode(int64(sysSeqTableID), int64(sqlSeqIndexID))
}

// checkSeqDesc validates a descriptor's option invariants — the ones
// that must hold however the descriptor was built (CREATE or ALTER).
// The messages carry Postgres' wording.
func checkSeqDesc(d *SeqDesc) error {
	if d.Increment == 0 {
		return serr.New("INCREMENT must not be zero")
	}
	if d.Min > d.Max {
		return serr.New("MINVALUE (" + fmtInt(d.Min) + ") must be less than MAXVALUE (" + fmtInt(d.Max) + ")")
	}
	if d.Start < d.Min {
		return serr.New("START value (" + fmtInt(d.Start) + ") cannot be less than MINVALUE (" + fmtInt(d.Min) + ")")
	}
	if d.Start > d.Max {
		return serr.New("START value (" + fmtInt(d.Start) + ") cannot be greater than MAXVALUE (" + fmtInt(d.Max) + ")")
	}
	if d.Cache < 1 {
		return serr.New("CACHE (" + fmtInt(d.Cache) + ") must be greater than zero")
	}
	return nil
}

// CreateSequence registers a sequence object. The descriptor's option
// fields must be fully resolved (defaults applied by the caller); ID,
// Last, and Called are set here. Sequences share the relation
// namespace with tables, as in Postgres, so the name must collide
// with neither.
func (e *Engine) CreateSequence(desc *SeqDesc) error {
	if err := checkSeqName(desc.Name); err != nil {
		return err
	}
	if err := checkSeqDesc(desc); err != nil {
		return err
	}
	err := e.updateDDL(func(tx *btypedb.Tx[string, []byte]) error {
		if tx.Contains(sqlSeqKey(desc.Name)) || tx.Contains(descKey(desc.Name)) || tx.Contains(viewKey(desc.Name)) {
			return serr.New(`relation "` + desc.Name + `" already exists`)
		}
		id, err := nextFromCounter(tx, seqKey(), firstUserTableID, "table-id")
		if err != nil {
			return err
		}
		desc.ID = id
		desc.Last, desc.Called = desc.Start, false
		return writeSeqDescIn(tx, desc)
	})
	if err != nil {
		return serr.Wrap(err, "op", "create sequence", "sequence", desc.Name)
	}
	return nil
}

// DropSequence removes a sequence object, reporting whether it
// existed.
func (e *Engine) DropSequence(name string) (existed bool, err error) {
	if err := checkSeqName(name); err != nil {
		return false, err
	}
	err = e.updateDDL(func(tx *btypedb.Tx[string, []byte]) error {
		existed, err = tx.Delete(sqlSeqKey(name))
		return err
	})
	if err != nil {
		return false, serr.Wrap(err, "op", "drop sequence", "sequence", name)
	}
	return existed, nil
}

// AlterSequence rewrites a sequence object's descriptor: mutate edits
// it in place (options and, for RESTART/setval-like changes, state),
// and the result is re-validated and persisted atomically. A missing
// sequence is the Postgres "does not exist" error.
func (e *Engine) AlterSequence(name string, mutate func(*SeqDesc) error) error {
	err := e.updateDDL(func(tx *btypedb.Tx[string, []byte]) error {
		desc, err := seqDescIn(tx, name)
		if err != nil {
			return err
		}
		if err := mutate(desc); err != nil {
			return err
		}
		if err := checkSeqDesc(desc); err != nil {
			return err
		}
		return writeSeqDescIn(tx, desc)
	})
	if err != nil {
		return serr.Wrap(err, "op", "alter sequence", "sequence", name)
	}
	return nil
}

// Sequence resolves a sequence object from the committed state, nil
// when absent. Transactions should use Txn.Sequence.
func (e *Engine) Sequence(name string) *SeqDesc {
	var desc *SeqDesc
	e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		desc, _ = seqDescIn(tx, name)
		return nil
	})
	return desc
}

// Sequence resolves a sequence object from the transaction's
// snapshot, nil when absent.
func (t *Txn) Sequence(name string) *SeqDesc {
	desc, _ := seqDescIn(t.tx, name)
	return desc
}

// Sequences lists every sequence object, in name order (the key
// encoding's order), as of the committed state at the call.
func (e *Engine) Sequences() []*SeqDesc {
	var out []*SeqDesc
	e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
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
	return out
}

// NextVal allocates the sequence's next value within the transaction:
// Start on the first call, then steps of Increment, cycling to the
// opposite bound when CYCLE was declared and erroring past a bound
// otherwise, with Postgres' wording.
func (t *Txn) NextVal(name string) (int64, error) {
	desc, err := seqDescIn(t.tx, name)
	if err != nil {
		return 0, err
	}
	if !desc.Called {
		desc.Called = true
	} else {
		next := desc.Last + desc.Increment
		// The step can leave [Min, Max] arithmetically or by int64
		// wraparound; wraparound shows up as the sum moving against the
		// increment's direction.
		over := desc.Increment > 0 && (next > desc.Max || next < desc.Last)
		under := desc.Increment < 0 && (next < desc.Min || next > desc.Last)
		if over || under {
			if !desc.Cycle {
				bound, word := desc.Max, "maximum"
				if under {
					bound, word = desc.Min, "minimum"
				}
				return 0, serr.New(`nextval: reached ` + word + ` value of sequence "` +
					name + `" (` + fmtInt(bound) + `)`)
			}
			if over {
				next = desc.Min
			} else {
				next = desc.Max
			}
		}
		desc.Last = next
	}
	if err := writeSeqDescIn(t.tx, desc); err != nil {
		return 0, err
	}
	return desc.Last, nil
}

// SetVal is setval: it repositions the sequence at v within the
// transaction. called=true means v was "already returned" (the next
// NextVal steps past it); false means the next NextVal returns v
// itself.
func (t *Txn) SetVal(name string, v int64, called bool) error {
	desc, err := seqDescIn(t.tx, name)
	if err != nil {
		return err
	}
	if v < desc.Min || v > desc.Max {
		return serr.New(`setval: value ` + fmtInt(v) + ` is out of bounds for sequence "` +
			name + `" (` + fmtInt(desc.Min) + `..` + fmtInt(desc.Max) + `)`)
	}
	desc.Last, desc.Called = v, called
	return writeSeqDescIn(t.tx, desc)
}

// seqDescIn resolves a sequence object inside a transaction, with the
// Postgres error shape for absence.
func seqDescIn(tx *btypedb.Tx[string, []byte], name string) (*SeqDesc, error) {
	if err := checkSeqName(name); err != nil {
		return nil, err
	}
	raw, ok := tx.Get(sqlSeqKey(name))
	if !ok {
		return nil, serr.New(`relation "` + name + `" does not exist`)
	}
	desc := &SeqDesc{}
	if err := json.Unmarshal(raw, desc); err != nil {
		return nil, serr.Wrap(err, "op", "decode sequence descriptor", "sequence", name)
	}
	return desc, nil
}

// writeSeqDescIn stages a sequence descriptor write, stamping the
// current format version (shared with table descriptors: both are
// JSON records whose layout evolves together).
func writeSeqDescIn(tx *btypedb.Tx[string, []byte], desc *SeqDesc) error {
	desc.FormatVersion = descFormatVersion
	blob, err := json.Marshal(desc)
	if err != nil {
		return err
	}
	return tx.Set(sqlSeqKey(desc.Name), blob)
}
