package pgwire_test

// Concurrent DDL + DML through the whole stack — pgx over TCP, the
// wire protocol, sessions, planner, engine — run under -race. The
// engine level already tests insert-vs-CreateIndex and reader-vs-DDL
// snapshots; this exercises the same races end to end, where each
// connection is its own session and the planner picks access paths
// while the schema churns underneath it.

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestConcurrentDDLandDML(t *testing.T) {
	ctx := context.Background()
	connString := startServer(t)

	setup := connect(t, connString)
	mustExec(t, setup, `create table ev (id int primary key, grp int, val int)`)

	const (
		writers      = 3
		perWriter    = 40
		readers      = 2
		ddlChurns    = 25
		readerRounds = 60
	)

	var wg sync.WaitGroup
	errs := make(chan error, writers+readers+1)

	// Writers: disjoint id ranges, so every insert must succeed no
	// matter how it interleaves with index creation (backfill and
	// concurrent inserts must agree on the final index contents).
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := connect(t, connString)
			for i := range perWriter {
				id := w*perWriter + i
				if _, err := c.Exec(ctx, `insert into ev values ($1, $2, $3)`,
					int64(id), int64(id%7), int64(id*3)); err != nil {
					errs <- fmt.Errorf("writer %d insert %d: %w", w, id, err)
					return
				}
				if i%10 == 9 { // sprinkle updates over already-inserted rows
					// (SET takes literals only in this dialect — no
					// column expressions.)
					if _, err := c.Exec(ctx, `update ev set val = $2 where id = $1`,
						int64(id-5), int64(id)); err != nil {
						errs <- fmt.Errorf("writer %d update: %w", w, err)
						return
					}
				}
			}
		}()
	}

	// DDL churn: create and drop a secondary index over and over while
	// writers and readers run.
	wg.Add(1)
	go func() {
		defer wg.Done()
		c := connect(t, connString)
		for i := range ddlChurns {
			if _, err := c.Exec(ctx, `create index by_grp on ev (grp, val desc)`); err != nil {
				errs <- fmt.Errorf("churn %d create: %w", i, err)
				return
			}
			if _, err := c.Exec(ctx, `drop index by_grp`); err != nil {
				errs <- fmt.Errorf("churn %d drop: %w", i, err)
				return
			}
		}
	}()

	// Readers: queries the planner may serve from the churning index.
	// Whatever snapshot a query gets — index present or absent — the
	// row set must be right; "wrong count" here is the reader-sees-
	// half-built-index bug surfacing at the SQL layer.
	for r := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := connect(t, connString)
			for i := range readerRounds {
				var withGrp, total int64
				if err := c.QueryRow(ctx, `select count(*) from ev where grp = $1`,
					int64(i%7)).Scan(&withGrp); err != nil {
					errs <- fmt.Errorf("reader %d grp count: %w", r, err)
					return
				}
				if err := c.QueryRow(ctx, `select count(*) from ev`).Scan(&total); err != nil {
					errs <- fmt.Errorf("reader %d total: %w", r, err)
					return
				}
				if withGrp > total {
					errs <- fmt.Errorf("reader %d: grp count %d exceeds total %d", r, withGrp, total)
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		// A dropped index racing a reader's plan is required to be
		// invisible (atomic snapshots); anything surfacing is a bug.
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	// Settle: full row count, then index vs seq scan agreement on the
	// final state.
	c := connect(t, connString)
	var total int64
	if err := c.QueryRow(ctx, `select count(*) from ev`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if want := int64(writers * perWriter); total != want {
		t.Fatalf("final count %d; want %d", total, want)
	}
	mustExec(t, c, `create index by_grp on ev (grp, val desc)`)
	rows, err := c.Query(ctx, `select id from ev where grp = 3 order by val desc`)
	if err != nil {
		t.Fatal(err)
	}
	var viaIndex int
	for rows.Next() {
		viaIndex++
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	var viaSeq int64
	if err := c.QueryRow(ctx, `select count(*) from ev where grp = 3`).Scan(&viaSeq); err != nil {
		t.Fatal(err)
	}
	if int64(viaIndex) != viaSeq {
		t.Fatalf("index scan found %d rows, seq count %d", viaIndex, viaSeq)
	}
}
