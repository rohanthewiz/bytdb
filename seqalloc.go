package bytdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/serr"
)

// seqalloc.go: non-transactional sequence allocation for
// concurrent-writes (OCC) mode.
//
// In the default serialized mode a sequence draw is a transactional
// read-modify-write of its counter key — correct, gapless on rollback,
// and free, because only one write transaction exists at a time. Under
// OCC that same design is poison: every pair of concurrent inserts into
// a table with an identity column would collide on the counter key and
// one of them would abort, so the counter would serialize exactly the
// workload OCC exists to overlap.
//
// This file adopts the Postgres position instead: allocation is
// non-transactional and gap-tolerant. Each counter gets an in-memory
// allocator handing out values under its own mutex, backed by a durable
// high-watermark record at the counter's usual key:
//
//	    stored value (watermark)
//	           v
//	  [ handed out / burned )[ in-memory cache )[ future ]
//	  ^                      ^
//	  values <= these may    a.next .. a.watermark: hand out
//	  exist in rows          with no disk write
//
// The watermark is always written BEFORE any value below it is handed
// out, so crash recovery resumes past everything that may be in use —
// values in the unreturned tail are burned, exactly like a Postgres
// sequence with CACHE. Rolled-back transactions likewise burn their
// draws: allocation happens outside the transaction, so there is
// nothing to roll back.
//
// Watermark writes are short kv transactions of their own (seqPut), not
// writes inside the caller's transaction — which is what removes the
// counter keys from every user write set, and therefore from OCC
// conflict detection. Each such transaction re-reads the stored counter
// so a concurrent SetSeq/restore is respected, and refuses to resurrect
// a counter that a concurrent DROP/TRUNCATE deleted (errSeqReset), so a
// racing allocator degrades to a clean restart instead of undoing DDL.

const (
	// seqAllocBatch is the watermark extension quantum for plain uint64
	// counters (identity columns, named sequences): one durable write
	// buys this many allocations. Bigger batches amortize better but
	// burn more values on a crash; 32 keeps crash gaps small while
	// making the watermark write rare. Sequence objects use their
	// declared CACHE instead, defaulting to 1 as Postgres does.
	seqAllocBatch = 32

	// seqConflictRetries bounds re-running a watermark write that lost
	// an OCC conflict (e.g. against a concurrent TRUNCATE's range
	// claim). Each retry re-reads from a fresh snapshot, so the write
	// converges; the bound only prevents livelock.
	seqConflictRetries = 10

	// seqAllocRetries bounds re-fetching an allocator that was
	// invalidated between lookup and use. Invalidation is rare (DDL,
	// SetSeq), so more than a couple of laps means something is wedged.
	seqAllocRetries = 8
)

// errSeqReset reports that a counter's key vanished underneath its
// allocator — a DROP TABLE, DROP/DELETE of the sequence, or TRUNCATE
// ... RESTART IDENTITY committed concurrently. The allocator must be
// discarded and the counter re-anchored from stored state (typically:
// recreated at its start value).
var errSeqReset = errors.New("bytdb: sequence counter was reset concurrently")

// seqPut runs one short kv transaction for sequence state, retrying
// bounded times on OCC conflicts. fn must be a pure function of its
// snapshot (re-read everything it depends on), because a retry runs it
// again from scratch.
func (e *Engine) seqPut(fn func(tx *btypedb.Tx[string, []byte]) error) error {
	var err error
	for range seqConflictRetries {
		err = e.kv.Update(fn)
		if !errors.Is(err, btypedb.ErrTxConflict) {
			return err
		}
	}
	return err
}

// --- plain uint64 counters (identity columns, named sequences) ---

// counterAlloc is the in-memory allocator for one 8-byte counter key.
// All fields are guarded by mu. The per-allocator mutex (rather than
// one engine-wide lock) is the point: contention on one hot table's
// identity column never blocks draws on another.
type counterAlloc struct {
	mu sync.Mutex
	// dead marks an invalidated allocator: its cached range must not be
	// used because durable state moved underneath it (SetSeq, DROP,
	// TRUNCATE RESTART IDENTITY). Users re-fetch from the engine map,
	// which no longer holds it.
	dead   bool
	loaded bool
	// existed records whether the key was present when state was last
	// anchored. It is what lets a watermark write distinguish "lazily
	// creating this counter" (fine) from "resurrecting a counter a
	// concurrent DDL deleted" (refuse: errSeqReset).
	existed bool
	// next is the next value to hand out; watermark is the durable
	// bound: values in [next, watermark) can be handed out with no disk
	// write, because the stored record already covers them.
	next      uint64
	watermark uint64
}

// counterAllocFor returns the allocator for key, creating an empty
// (unloaded) one on first use.
func (e *Engine) counterAllocFor(key string) *counterAlloc {
	e.seqMu.Lock()
	defer e.seqMu.Unlock()
	a := e.seqAllocs[key]
	if a == nil {
		a = &counterAlloc{}
		e.seqAllocs[key] = a
	}
	return a
}

// invalidateCounter discards the allocator for key, if any: the next
// draw re-anchors from stored state. Always safe — at worst the old
// cache's unreturned tail is burned — which is why callers may
// invalidate optimistically. Call AFTER the durable change lands, so a
// re-anchor cannot read the old state and cache it right back.
func (e *Engine) invalidateCounter(key string) {
	e.seqMu.Lock()
	a := e.seqAllocs[key]
	delete(e.seqAllocs, key)
	e.seqMu.Unlock()
	if a != nil {
		a.mu.Lock()
		a.dead = true
		a.mu.Unlock()
	}
}

// invalidateCounterPrefix discards every allocator whose key starts
// with prefix — DROP TABLE and TRUNCATE ... RESTART IDENTITY cover all
// of a table's identity counters with one range.
func (e *Engine) invalidateCounterPrefix(prefix string) {
	e.seqMu.Lock()
	var dead []*counterAlloc
	for k, a := range e.seqAllocs {
		if strings.HasPrefix(k, prefix) {
			dead = append(dead, a)
			delete(e.seqAllocs, k)
		}
	}
	e.seqMu.Unlock()
	for _, a := range dead {
		a.mu.Lock()
		a.dead = true
		a.mu.Unlock()
	}
}

// dropCounterAlloc removes a specific allocator instance after it hit
// errSeqReset. The pointer comparison keeps it from clobbering a fresh
// replacement some other goroutine already installed.
func (e *Engine) dropCounterAlloc(key string, a *counterAlloc) {
	e.seqMu.Lock()
	if e.seqAllocs[key] == a {
		delete(e.seqAllocs, key)
	}
	e.seqMu.Unlock()
	a.mu.Lock()
	a.dead = true
	a.mu.Unlock()
}

// loadLocked anchors an unloaded allocator on the committed counter
// value (or start when absent). Caller holds a.mu.
func (a *counterAlloc) loadLocked(e *Engine, key string, start uint64, what string) error {
	if a.loaded {
		return nil
	}
	if raw, ok := e.kv.Get(key); ok {
		if len(raw) != 8 {
			return serr.New("sequence value is corrupt",
				"sequence", what, "len", fmt.Sprint(len(raw)))
		}
		a.next = binary.BigEndian.Uint64(raw)
		a.existed = true
	} else {
		a.next = start
	}
	// Nothing is reserved beyond the stored bound yet; the first draw
	// extends the watermark before handing anything out.
	a.watermark = a.next
	a.loaded = true
	return nil
}

// reserveLocked durably raises the watermark so that draws from floor
// onward are covered, then repositions the allocator at floor. The
// write re-anchors on the stored value inside its own transaction: a
// concurrent SetSeq/restore that moved the counter forward is jumped
// past, never clobbered, and a concurrently deleted counter is not
// resurrected (errSeqReset). Caller holds a.mu.
//
// extending distinguishes the two callers, because they mean different
// things by floor. A draw extension (takeLocked) passes the allocator's
// own position — pure memory of its last reservation, with no semantic
// weight — so a stored value BELOW the anchored watermark means an
// external SetSeq/restore moved the counter backward since we anchored,
// and maxing the stale position back in would durably erase that
// committed set. That case returns errSeqReset so the caller discards
// the allocator and re-anchors on the stored value (the same semantics
// a restart gives). A bump (bumpCounterAllocTo) passes an explicit
// value from user data as floor — a semantic requirement that must win
// over any stored value, backward-set or not — so it keeps the plain
// max.
func (a *counterAlloc) reserveLocked(e *Engine, key string, floor uint64, what string, extending bool) error {
	var base, wm uint64
	err := e.seqPut(func(tx *btypedb.Tx[string, []byte]) error {
		base = floor
		if raw, ok := tx.Get(key); ok {
			if len(raw) != 8 {
				return serr.New("sequence value is corrupt",
					"sequence", what, "len", fmt.Sprint(len(raw)))
			}
			s := binary.BigEndian.Uint64(raw)
			if extending && s < a.watermark {
				return errSeqReset
			}
			if s > base {
				base = s
			}
		} else if a.existed {
			return errSeqReset
		}
		if base == math.MaxUint64 {
			return serr.New("sequence exhausted", "sequence", what)
		}
		wm = base + seqAllocBatch
		if wm < base { // overflow near the top: reserve what remains
			wm = math.MaxUint64
		}
		return tx.Set(key, binary.BigEndian.AppendUint64(nil, wm))
	})
	if err != nil {
		return err
	}
	a.next, a.watermark, a.existed = base, wm, true
	return nil
}

// allocCounter draws the next value from the counter at key,
// non-transactionally: the draw is durable-before-visible via the
// watermark and is never rolled back. start seeds an absent counter.
func (e *Engine) allocCounter(key string, start uint64, what string) (uint64, error) {
	for range seqAllocRetries {
		a := e.counterAllocFor(key)
		a.mu.Lock()
		if a.dead {
			a.mu.Unlock()
			continue // invalidated between map lookup and lock; re-fetch
		}
		v, err := a.takeLocked(e, key, start, what)
		a.mu.Unlock()
		if errors.Is(err, errSeqReset) {
			// The counter vanished under this allocator (concurrent
			// DROP/TRUNCATE). Discard it and re-anchor: the fresh
			// allocator sees the post-DDL state — typically recreating
			// the counter at start, which is the documented restart
			// semantics.
			e.dropCounterAlloc(key, a)
			continue
		}
		return v, err
	}
	return 0, serr.New("sequence allocation kept losing to concurrent resets", "sequence", what)
}

// takeLocked hands out one value, extending the watermark first when
// the cache is exhausted. Caller holds a.mu.
func (a *counterAlloc) takeLocked(e *Engine, key string, start uint64, what string) (uint64, error) {
	if err := a.loadLocked(e, key, start, what); err != nil {
		return 0, err
	}
	if a.next >= a.watermark {
		if err := a.reserveLocked(e, key, a.next, what, true); err != nil {
			return 0, err
		}
	}
	v := a.next
	a.next++
	return v, nil
}

// bumpCounterAllocTo raises the counter so future draws return at
// least next — the OCC twin of bumpCounterTo, for explicit values
// inserted into identity columns. Like all allocation it is
// non-transactional: the bump survives the caller's rollback.
func (e *Engine) bumpCounterAllocTo(key string, next uint64, what string) error {
	for range seqAllocRetries {
		a := e.counterAllocFor(key)
		a.mu.Lock()
		if a.dead {
			a.mu.Unlock()
			continue
		}
		err := func() error {
			if err := a.loadLocked(e, key, 1, what); err != nil {
				return err
			}
			if next <= a.next {
				return nil // already past it
			}
			if next < a.watermark {
				// The stored record already covers next; skipping the
				// intervening values is just a gap.
				a.next = next
				return nil
			}
			return a.reserveLocked(e, key, next, what, false)
		}()
		a.mu.Unlock()
		if errors.Is(err, errSeqReset) {
			e.dropCounterAlloc(key, a)
			continue
		}
		return err
	}
	return serr.New("sequence bump kept losing to concurrent resets", "sequence", what)
}

// setCounterDirect is SetSeq's OCC path: write the exact stored value
// in a short transaction, then discard any allocator so the next draw
// re-anchors on it. Effective immediately — it does not roll back with
// any enclosing transaction (Postgres setval semantics).
func (e *Engine) setCounterDirect(key string, next uint64) error {
	err := e.seqPut(func(tx *btypedb.Tx[string, []byte]) error {
		return tx.Set(key, binary.BigEndian.AppendUint64(nil, next))
	})
	if err != nil {
		return err
	}
	e.invalidateCounter(key)
	return nil
}

// deleteCounterDirect is DeleteSeq's OCC path: delete the stored key
// in a short transaction, then discard the allocator. Effective
// immediately, like setCounterDirect.
func (e *Engine) deleteCounterDirect(key string) (existed bool, err error) {
	err = e.seqPut(func(tx *btypedb.Tx[string, []byte]) error {
		existed, err = tx.Delete(key)
		return err
	})
	if err != nil {
		return false, err
	}
	e.invalidateCounter(key)
	return existed, nil
}

// peekCounterOCC reports the live allocator's next value when one is
// loaded. handled is false when no allocator holds state for key — the
// caller should fall back to the stored record, which equals the next
// draw whenever no cache is outstanding.
func (e *Engine) peekCounterOCC(key string) (next uint64, ok, handled bool) {
	e.seqMu.Lock()
	a := e.seqAllocs[key]
	e.seqMu.Unlock()
	if a == nil {
		return 0, false, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.loaded || a.dead {
		return 0, false, false
	}
	return a.next, a.existed, true
}

// --- sequence objects (CREATE SEQUENCE) ---

// seqObjAlloc is the in-memory allocator for one sequence object. It
// caches up to CACHE draws between durable writes; the stored
// descriptor always sits at the position AFTER the reserved batch, so
// recovery resumes past anything that may have been handed out.
type seqObjAlloc struct {
	mu   sync.Mutex
	dead bool
	// desc is the hand-out position: Last/Called are the state of the
	// most recent draw. Options (Increment, bounds, Cycle, Cache) are a
	// copy taken at the last reservation, so an ALTER SEQUENCE applies
	// from the next reservation on — cached draws use the old options,
	// the same caveat Postgres documents for cached sequences. (ALTER
	// also invalidates the allocator, so in practice the stale window is
	// just draws already in flight.)
	desc SeqDesc
	// reserved counts draws still covered by the stored descriptor.
	reserved int64
}

func (e *Engine) seqObjAllocFor(key string) *seqObjAlloc {
	e.seqMu.Lock()
	defer e.seqMu.Unlock()
	a := e.seqObjAllocs[key]
	if a == nil {
		a = &seqObjAlloc{}
		e.seqObjAllocs[key] = a
	}
	return a
}

// invalidateSeqObj discards the allocator for a sequence object key —
// after ALTER/DROP SEQUENCE, setval, or restore. Same contract as
// invalidateCounter: call after the durable change lands.
func (e *Engine) invalidateSeqObj(key string) {
	e.seqMu.Lock()
	a := e.seqObjAllocs[key]
	delete(e.seqObjAllocs, key)
	e.seqMu.Unlock()
	if a != nil {
		a.mu.Lock()
		a.dead = true
		a.mu.Unlock()
	}
}

// reserveLocked anchors on the stored descriptor and durably advances
// it by up to CACHE draws, leaving the allocator positioned to hand
// those draws out from memory. Reading the descriptor inside the write
// transaction is what makes DROP and ALTER safe: a dropped sequence
// surfaces its "does not exist" error here, and an altered one is
// re-read with its new options. Caller holds a.mu.
func (a *seqObjAlloc) reserveLocked(e *Engine, name string) error {
	var pos SeqDesc
	var k int64
	err := e.seqPut(func(tx *btypedb.Tx[string, []byte]) error {
		stored, err := seqDescIn(tx, name)
		if err != nil {
			return err
		}
		pos = *stored
		batch := max(stored.Cache, 1)
		// Step a scratch copy forward to find where the batch ends. A
		// non-cycling sequence may hit its bound early: reserve the
		// draws that do exist, and only error when not even one does.
		work := *stored
		k = 0
		for k < batch {
			if _, err := stepSeq(&work, name); err != nil {
				if k == 0 {
					return err
				}
				break
			}
			k++
		}
		return writeSeqDescIn(tx, &work)
	})
	if err != nil {
		return err
	}
	a.desc = pos
	a.reserved = k
	return nil
}

// allocSeqObj draws the sequence object's next value,
// non-transactionally (nextval semantics: never rolled back).
func (e *Engine) allocSeqObj(name string) (int64, error) {
	key := sqlSeqKey(name)
	for range seqAllocRetries {
		a := e.seqObjAllocFor(key)
		a.mu.Lock()
		if a.dead {
			a.mu.Unlock()
			continue
		}
		defer a.mu.Unlock()
		if a.reserved == 0 {
			if err := a.reserveLocked(e, name); err != nil {
				return 0, err
			}
		}
		// Within a reservation the step cannot fail: the scratch walk
		// above proved these k draws exist, and stepping is
		// deterministic.
		v, err := stepSeq(&a.desc, name)
		if err != nil {
			return 0, err
		}
		a.reserved--
		return v, nil
	}
	return 0, serr.New("sequence allocation kept losing to concurrent resets", "sequence", name)
}

// setValDirect is setval's OCC path: reposition the stored descriptor
// in a short transaction, then discard the allocator's cache.
// Effective immediately; never rolled back (Postgres setval is
// explicitly non-transactional).
func (e *Engine) setValDirect(name string, v int64, called bool) error {
	err := e.seqPut(func(tx *btypedb.Tx[string, []byte]) error {
		desc, err := seqDescIn(tx, name)
		if err != nil {
			return err
		}
		if v < desc.Min || v > desc.Max {
			return serr.New(`setval: value ` + fmtInt(v) + ` is out of bounds for sequence "` +
				name + `" (` + fmtInt(desc.Min) + `..` + fmtInt(desc.Max) + `)`)
		}
		desc.Last, desc.Called = v, called
		return writeSeqDescIn(tx, desc)
	})
	if err != nil {
		return err
	}
	e.invalidateSeqObj(sqlSeqKey(name))
	return nil
}
