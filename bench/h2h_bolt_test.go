package main

import (
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// benchBolt: one bucket, big-endian uint64 keys, ids from the bucket's
// NextSequence (Bolt's native identity). NoSync is the SyncNever
// equivalent. Bolt serializes db.Update — this is the pure
// single-writer B+tree baseline.
func benchBolt(b *testing.B, heavy bool) {
	db, err := bolt.Open(filepath.Join(b.TempDir(), "bench.db"), 0o600, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	db.NoSync = true

	bucket := []byte("events")
	err = db.Update(func(tx *bolt.Tx) error {
		bkt, err := tx.CreateBucket(bucket)
		if err != nil {
			return err
		}
		for i := uint64(1); i <= seedRows; i++ {
			if err := bkt.Put(k8(i), []byte("seed")); err != nil {
				return err
			}
		}
		return bkt.SetSequence(seedRows) // NextSequence continues at 501
	})
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			err := db.Update(func(tx *bolt.Tx) error {
				bkt := tx.Bucket(bucket)
				if heavy {
					for i := uint64(1); i <= readsPer; i++ {
						if v := bkt.Get(k8(i)); v == nil {
							b.Error("missing seed row")
						}
					}
				}
				id, err := bkt.NextSequence()
				if err != nil {
					return err
				}
				return bkt.Put(k8(id), []byte("bench"))
			})
			if err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkInsert_bolt(b *testing.B) { benchBolt(b, false) }
func BenchmarkHeavy_bolt(b *testing.B)  { benchBolt(b, true) }
