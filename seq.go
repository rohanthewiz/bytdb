package bytdb

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/serr"
)

// seq.go: named sequences — durable, monotonic uint64 counters in the
// system keyspace. Each sequence is one kv pair holding the next value
// to hand out as a big-endian uint64, created lazily on first use.
// Sequence names are their own namespace, separate from table names.
//
// Allocation is transactional: a sequence bumped inside a transaction
// that rolls back is not bumped at all, so today's values have no gaps.
// Callers should still not depend on gaplessness — SetSeq can skip
// values, and a future engine may cache allocations for concurrency —
// only on distinctness and monotonicity per name.

// nextFromCounter allocates the next value from the 8-byte big-endian
// counter at key, starting at start when the key is absent. A malformed
// stored value is corruption, and restarting the counter at its default
// would be worse than failing: the next value handed out would collide
// with one already in use. what names the counter in that error.
func nextFromCounter(tx *btypedb.Tx[string, []byte], key string, start uint64, what string) (uint64, error) {
	next := start
	if raw, ok := tx.Get(key); ok {
		if len(raw) != 8 {
			return 0, serr.New("sequence value is corrupt",
				"sequence", what, "len", fmt.Sprint(len(raw)))
		}
		next = binary.BigEndian.Uint64(raw)
	}
	if next == math.MaxUint64 {
		return 0, serr.New("sequence exhausted", "sequence", what)
	}
	if err := tx.Set(key, binary.BigEndian.AppendUint64(nil, next+1)); err != nil {
		return 0, err
	}
	return next, nil
}

// bumpCounterTo raises the counter at key to at least next, creating
// it if absent. Counters never move backward through this path. An
// absent counter starts at 1, so a bump to 1 or below is a no-op.
func bumpCounterTo(tx *btypedb.Tx[string, []byte], key string, next uint64, what string) error {
	if raw, ok := tx.Get(key); ok {
		if len(raw) != 8 {
			return serr.New("sequence value is corrupt",
				"sequence", what, "len", fmt.Sprint(len(raw)))
		}
		if binary.BigEndian.Uint64(raw) >= next {
			return nil
		}
	} else if next <= 1 {
		return nil
	}
	return tx.Set(key, binary.BigEndian.AppendUint64(nil, next))
}

// checkSeqName rejects the empty name; anything else is a valid
// sequence name.
func checkSeqName(name string) error {
	if name == "" {
		return serr.New("sequence name is required")
	}
	return nil
}

// NextSeq allocates the next value from the named sequence, creating
// it at 1 on first use. Values are distinct and increasing per name
// for the life of the database (but see SetSeq).
//
// An error does not guarantee the value was discarded — see Insert on
// why a failed commit reads as "durability unknown."
func (e *Engine) NextSeq(name string) (uint64, error) {
	if err := e.checkReentrantWrite("next sequence value"); err != nil {
		return 0, err
	}
	var v uint64
	err := e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
		var err error
		v, err = nextSeqIn(tx, name)
		return err
	})
	if err != nil {
		return 0, serr.Wrap(err, "op", "next sequence value", "sequence", name)
	}
	return v, nil
}

// NextSeq allocates the next value from the named sequence within the
// transaction (see Engine.NextSeq). The bump commits or rolls back
// with the transaction.
func (t *Txn) NextSeq(name string) (uint64, error) {
	v, err := nextSeqIn(t.tx, name)
	if err != nil {
		return 0, serr.Wrap(err, "op", "next sequence value", "sequence", name)
	}
	return v, nil
}

func nextSeqIn(tx *btypedb.Tx[string, []byte], name string) (uint64, error) {
	if err := checkSeqName(name); err != nil {
		return 0, err
	}
	return nextFromCounter(tx, seqNameKey(name), 1, name)
}

// SetSeq sets the next value the named sequence will return, creating
// the sequence if absent. Setting it below values already handed out
// makes NextSeq repeat them — that is the caller's contract to keep
// (it exists for restore and setval-style adjustment).
func (e *Engine) SetSeq(name string, next uint64) error {
	if err := e.checkReentrantWrite("set sequence"); err != nil {
		return err
	}
	err := e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
		return setSeqIn(tx, name, next)
	})
	if err != nil {
		return serr.Wrap(err, "op", "set sequence", "sequence", name)
	}
	return nil
}

// SetSeq sets the named sequence's next value within the transaction
// (see Engine.SetSeq).
func (t *Txn) SetSeq(name string, next uint64) error {
	if err := setSeqIn(t.tx, name, next); err != nil {
		return serr.Wrap(err, "op", "set sequence", "sequence", name)
	}
	return nil
}

func setSeqIn(tx *btypedb.Tx[string, []byte], name string, next uint64) error {
	if err := checkSeqName(name); err != nil {
		return err
	}
	return tx.Set(seqNameKey(name), binary.BigEndian.AppendUint64(nil, next))
}

// PeekSeq reports the value the named sequence would return next,
// without allocating it. ok is false when the sequence does not exist
// (never used, or deleted).
func (e *Engine) PeekSeq(name string) (next uint64, ok bool, err error) {
	err = e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		next, ok, err = peekSeqIn(tx, name)
		return err
	})
	if err != nil {
		return 0, false, serr.Wrap(err, "op", "peek sequence", "sequence", name)
	}
	return next, ok, nil
}

// PeekSeq reports the named sequence's next value in the transaction's
// view (see Engine.PeekSeq).
func (t *Txn) PeekSeq(name string) (next uint64, ok bool, err error) {
	next, ok, err = peekSeqIn(t.tx, name)
	if err != nil {
		return 0, false, serr.Wrap(err, "op", "peek sequence", "sequence", name)
	}
	return next, ok, nil
}

func peekSeqIn(v kvView, name string) (uint64, bool, error) {
	if err := checkSeqName(name); err != nil {
		return 0, false, err
	}
	raw, ok := v.Get(seqNameKey(name))
	if !ok {
		return 0, false, nil
	}
	if len(raw) != 8 {
		return 0, false, serr.New("sequence value is corrupt",
			"sequence", name, "len", fmt.Sprint(len(raw)))
	}
	return binary.BigEndian.Uint64(raw), true, nil
}

// DeleteSeq removes the named sequence; a later NextSeq recreates it
// at 1. Deleting an absent sequence is a no-op reported by existed.
func (e *Engine) DeleteSeq(name string) (existed bool, err error) {
	if err := e.checkReentrantWrite("delete sequence"); err != nil {
		return false, err
	}
	err = e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
		existed, err = deleteSeqIn(tx, name)
		return err
	})
	if err != nil {
		return false, serr.Wrap(err, "op", "delete sequence", "sequence", name)
	}
	return existed, nil
}

// DeleteSeq removes the named sequence within the transaction (see
// Engine.DeleteSeq).
func (t *Txn) DeleteSeq(name string) (existed bool, err error) {
	existed, err = deleteSeqIn(t.tx, name)
	if err != nil {
		return false, serr.Wrap(err, "op", "delete sequence", "sequence", name)
	}
	return existed, nil
}

func deleteSeqIn(tx *btypedb.Tx[string, []byte], name string) (bool, error) {
	if err := checkSeqName(name); err != nil {
		return false, err
	}
	return tx.Delete(seqNameKey(name))
}
