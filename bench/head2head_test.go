// Head-to-head concurrency benchmarks: the same two write-transaction
// shapes against embedded (and near-embedded) stores, so the numbers
// in the README's "Concurrent writes" section can be reproduced on any
// machine from this committed harness:
//
//   - Insert_*: one write transaction inserting a single row whose id
//     is drawn the way each engine natively draws ids (identity
//     column, rowid, NextSequence, Sequence, INCR).
//   - Heavy_*: one transaction doing 400 point reads of static seed
//     rows then a single insert — the shape of FK/unique-check-heavy
//     SQL writes, and the case bytdb's OCC mode exists for.
//
// Every engine runs with durability off (no fsync) so the comparison
// is transaction machinery, not disks. Reproduce with ./head2head.sh,
// or directly (Redis rows skip if no server on 127.0.0.1:63799):
//
//	cd bench && go test -run '^$' -bench 'Insert_|Heavy_' -cpu 8 -count 3
//
// This module's go.work resolves bytdb to the checkout you are sitting
// in, so you are benchmarking exactly the code you cloned.
package main

import "encoding/binary"

const (
	seedRows = 500 // rows present before the timer starts
	readsPer = 400 // point reads per heavy transaction
)

// k8 renders an id as a big-endian key so the KV stores get ordered,
// fixed-width keys — the same property bytdb's tuple encoding provides.
func k8(id uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], id)
	return b[:]
}
