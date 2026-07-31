package main

import (
	"errors"
	"testing"

	badger "github.com/dgraph-io/badger/v4"
)

// benchBadger: Badger is the closest architectural cousin to bytdb's
// occ mode — SSI transactions validated at commit over an LSM. Ids come
// from badger.Sequence (leased, non-transactional — the same trade
// bytdb's occ identity makes). Writes land on fresh keys and the read
// set (seed keys 1..400) is never written, so ErrConflict can only be
// a fingerprint false positive; retry it as workload like a client
// would.
func benchBadger(b *testing.B, heavy bool) {
	opt := badger.DefaultOptions(b.TempDir()).WithSyncWrites(false).WithLogger(nil)
	db, err := badger.Open(opt)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	err = db.Update(func(txn *badger.Txn) error {
		for i := uint64(1); i <= seedRows; i++ {
			if err := txn.Set(k8(i), []byte("seed")); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	// The sequence lives under its own key, disjoint from row keys;
	// drawn values are offset past the seeds.
	seq, err := db.GetSequence([]byte("!seq"), 512)
	if err != nil {
		b.Fatal(err)
	}
	defer seq.Release()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			for {
				err := db.Update(func(txn *badger.Txn) error {
					if heavy {
						for i := uint64(1); i <= readsPer; i++ {
							item, err := txn.Get(k8(i))
							if err != nil {
								return err
							}
							if err := item.Value(func([]byte) error { return nil }); err != nil {
								return err
							}
						}
					}
					n, err := seq.Next()
					if err != nil {
						return err
					}
					return txn.Set(k8(seedRows+1+n), []byte("bench"))
				})
				if errors.Is(err, badger.ErrConflict) {
					continue
				}
				if err != nil {
					b.Error(err)
				}
				break
			}
		}
	})
}

func BenchmarkInsert_badger(b *testing.B) { benchBadger(b, false) }
func BenchmarkHeavy_badger(b *testing.B)  { benchBadger(b, true) }
