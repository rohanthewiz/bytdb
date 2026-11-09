// System-level measurement: how startup replay, heap and compaction scale
// with row count. Uses SyncNever so the load itself is not fsync-bound.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

func heapMB() float64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / (1 << 20)
}

func main() {
	n, _ := strconv.Atoi(os.Args[1])
	dir, _ := os.MkdirTemp("", "sysm")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "m.db")

	e, err := bytdb.Open(path, bytdb.WithSyncNever())
	if err != nil {
		panic(err)
	}
	db := bsql.New(e)
	must := func(_ *bsql.Result, err error) {
		if err != nil {
			panic(err)
		}
	}
	must(db.Exec(`CREATE TABLE t (id INT PRIMARY KEY, name TEXT, amount FLOAT, ts TIMESTAMP)`))
	must(db.Exec(`CREATE INDEX t_name ON t (name)`))
	base := heapMB()
	t0 := time.Now()
	const batch = 1000
	for i := 0; i < n; i += batch {
		var sb strings.Builder
		sb.WriteString("INSERT INTO t VALUES ")
		for j := 0; j < batch; j++ {
			if j > 0 {
				sb.WriteByte(',')
			}
			id := i + j
			fmt.Fprintf(&sb, "(%d,'name-%08d',%d.5,'2026-01-01 00:00:00')", id, id*7919%n, id)
		}
		must(db.Exec(sb.String()))
	}
	load := time.Since(t0)
	fi, _ := os.Stat(path)
	fmt.Printf("rows=%d load=%v (%.1f µs/row) heap=%.0f MB (%.0f B/row) file=%.0f MB\n",
		n, load.Round(time.Millisecond), float64(load.Microseconds())/float64(n),
		heapMB()-base, (heapMB()-base)*(1<<20)/float64(n), float64(fi.Size())/(1<<20))

	// churn: update 20% of the rows so the log carries garbage
	t0 = time.Now()
	must(db.Exec(`UPDATE t SET amount = amount + 1 WHERE id < $1`, n/5))
	fmt.Printf("update 20%%: %v\n", time.Since(t0).Round(time.Millisecond))
	if err := e.Close(); err != nil {
		panic(err)
	}
	fi, _ = os.Stat(path)
	fmt.Printf("file before reopen=%.0f MB\n", float64(fi.Size())/(1<<20))

	runtime.GC()
	t0 = time.Now()
	e, err = bytdb.Open(path, bytdb.WithSyncNever())
	if err != nil {
		panic(err)
	}
	fmt.Printf("OPEN (replay): %v  heap=%.0f MB\n", time.Since(t0).Round(time.Millisecond), heapMB())

	t0 = time.Now()
	if err := e.Compact(); err != nil {
		panic(err)
	}
	fi, _ = os.Stat(path)
	fmt.Printf("COMPACT: %v file=%.0f MB\n", time.Since(t0).Round(time.Millisecond), float64(fi.Size())/(1<<20))

	db = bsql.New(e)
	t0 = time.Now()
	r, err := db.Exec(`SELECT count(*), sum(amount) FROM t`)
	if err != nil {
		panic(err)
	}
	fmt.Printf("count+sum: %v %v\n", time.Since(t0).Round(time.Millisecond), r.Rows)
	t0 = time.Now()
	_, err = db.Exec(`SELECT * FROM t ORDER BY amount DESC LIMIT 10`)
	fmt.Printf("order by unindexed limit 10: %v err=%v\n", time.Since(t0).Round(time.Millisecond), err)
	t0 = time.Now()
	_, err = db.Exec(`SELECT name, count(*) FROM t WHERE amount > 100 GROUP BY name LIMIT 5`)
	fmt.Printf("filter+group: %v err=%v\n", time.Since(t0).Round(time.Millisecond), err)
	e.Close()
}
