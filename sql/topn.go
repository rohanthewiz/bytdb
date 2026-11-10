package sql

// topn.go: bounded top-N collection for ORDER BY ... LIMIT without an
// index that serves the order (deep-look D-43).
//
// The executor used to collect every matching row, stable-sort them all
// and keep OFFSET+LIMIT: 60 ms for ORDER BY x LIMIT 10 over 100k rows
// against 27 ms for the scan alone, and every row counted against the
// result row cap, so a large table refused the query with 54000 though
// it returns ten rows. A max-heap of the best OFFSET+LIMIT rows seen so
// far holds the answer instead:
//
//	row arrives ─► heap not full? ── yes ─► push
//	                   │ no
//	                   ▼
//	         better than the heap's worst? ── yes ─► replace the worst, sift
//	                   │ no
//	                   ▼
//	                discard
//
// Ties must break exactly as the stable sort broke them — by arrival —
// so each entry carries its arrival number and the ordering compares it
// last. A newcomer that ties the worst on every key therefore loses, as
// it would have sorted after it.

import (
	"container/heap"
	"slices"
)

type topEntry struct {
	row []any
	seq int64 // arrival order: the stable sort's tie-break
}

// topN keeps the n best rows under keys.
type topN struct {
	keys []sortKey
	n    int64
	h    topHeap
	next int64
}

// topHeap is a max-heap: the root is the worst row kept.
type topHeap struct {
	keys    []sortKey
	entries []topEntry
}

func newTopN(keys []sortKey, n int64) *topN {
	return &topN{keys: keys, n: n, h: topHeap{keys: keys}}
}

// order compares two entries as the stable sort orders rows: by the sort
// keys in turn, then by arrival.
func (h *topHeap) order(a, b topEntry) int {
	for _, k := range h.keys {
		c := keyCmp(a.row[k.ord], b.row[k.ord], k.desc, k.nulls)
		if c != 0 {
			return c
		}
	}
	switch {
	case a.seq < b.seq:
		return -1
	case a.seq > b.seq:
		return 1
	}
	return 0
}

func (h *topHeap) Len() int           { return len(h.entries) }
func (h *topHeap) Less(i, j int) bool { return h.order(h.entries[i], h.entries[j]) > 0 }
func (h *topHeap) Swap(i, j int)      { h.entries[i], h.entries[j] = h.entries[j], h.entries[i] }
func (h *topHeap) Push(x any)         { h.entries = append(h.entries, x.(topEntry)) }
func (h *topHeap) Pop() any {
	last := h.entries[len(h.entries)-1]
	h.entries = h.entries[:len(h.entries)-1]
	return last
}

// add offers one row.
func (t *topN) add(row []any) {
	if t.n <= 0 {
		return
	}
	e := topEntry{row: row, seq: t.next}
	t.next++
	if int64(len(t.h.entries)) < t.n {
		heap.Push(&t.h, e)
		return
	}
	if t.h.order(e, t.h.entries[0]) < 0 {
		t.h.entries[0] = e
		heap.Fix(&t.h, 0)
	}
}

// held is how many rows the collector holds (what the row budget sees).
func (t *topN) held() int { return len(t.h.entries) }

// sorted returns the kept rows in final order.
func (t *topN) sorted() [][]any {
	slices.SortFunc(t.h.entries, t.h.order)
	rows := make([][]any, len(t.h.entries))
	for i, e := range t.h.entries {
		rows[i] = e.row
	}
	return rows
}
