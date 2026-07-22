// Package replicate provides litestream-style asynchronous replication
// of a bytdb (or bare btypedb) database to an object store, plus
// point-in-time restore from that store.
//
// The storage log is append-only between compactions, so replication is
// byte-range shipping: a Replicator polls the engine's log state and
// uploads whatever appended since its watermark as immutable chunk
// objects. A compaction (or process restart) starts a fresh
// "generation" shipped from offset zero; older generations are pruned
// once enough newer ones exist. The first time a generation is shipped
// in full it gets a manifest object certifying completeness, and Restore
// concatenates the newest such generation's chunks back into a database
// file.
//
// Bucket layout (all under an optional caller prefix):
//
//	gen/<generation-id>/<start>-<end>.wlog   one shipped byte range
//	gen/<generation-id>/manifest.json        completeness marker (once caught up)
//
// where start/end are 16-hex-digit byte offsets. Generation IDs embed a
// UTC timestamp so plain lexicographic ordering is chronological, and
// chunk names embed both ends of their range so a restore can verify
// contiguity from the listing alone, before downloading anything. The
// manifest records the generation's certified-complete size, so restore
// can tell a valid torn tail from a generation that has lost chunks.
package replicate

import "context"

// Storage is the object-store surface the replicator and restore need.
// The built-in S3 implementation lives in the s3 subpackage; the bar
// for other implementations is deliberately low — anything with atomic PUT
// and lexicographically ordered listing (every S3 compatible, GCS,
// Azure via a thin adapter, or an in-memory fake for tests) fits.
type Storage interface {
	// Put stores data under key, atomically: a reader must never
	// observe a partially written object. S3 PUT gives this natively.
	Put(ctx context.Context, key string, data []byte) error

	// Get returns the full object stored under key.
	Get(ctx context.Context, key string) ([]byte, error)

	// List returns every key beginning with prefix, in ascending
	// lexicographic (byte) order — the order S3 ListObjectsV2
	// guarantees. Implementations must page internally and return the
	// complete set.
	List(ctx context.Context, prefix string) ([]string, error)

	// Delete removes key. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
}
