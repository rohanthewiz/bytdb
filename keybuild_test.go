package bytdb

// Deep-look D-45: keys are built straight into one buffer. The bytes
// must be exactly what the previous construction produced.

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/rohanthewiz/bytdb/tuple"
)

func refIndexEntry(desc *TableDesc, idx *IndexDesc, row []any) (string, []byte, bool) {
	hasNull := false
	vals := make([]any, len(idx.Cols))
	for i, ord := range idx.Cols {
		if row[ord] == nil {
			hasNull = true
		}
		vals[i] = row[ord]
	}
	buf, err := appendDirected(indexPrefix(desc.ID, idx.ID), idx, vals)
	if err != nil {
		panic(err)
	}
	pk := pkValues(desc, row)
	if idx.Unique && !hasNull {
		pkVal, _ := tuple.Encode(pk...)
		return string(buf), pkVal, true
	}
	buf, _ = tuple.Append(buf, pk...)
	return string(buf), []byte{}, false
}

func TestKeyBuildingMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewPCG(45, 45))
	desc := &TableDesc{ID: 300, PKCols: []int{0, 2}, Columns: make([]Column, 5)}
	idxs := []IndexDesc{
		{ID: 2, Cols: []int{1}},
		{ID: 3, Cols: []int{3, 1}, Unique: true},
		{ID: 700, Cols: []int{4, 3}, Desc: []bool{true, false}},
		{ID: 9, Cols: []int{1, 4}, Unique: true, Desc: []bool{false, true}},
	}
	val := func() any {
		switch rng.IntN(5) {
		case 0:
			return nil
		case 1:
			return rng.Int64N(1000) - 500
		case 2:
			return fmt.Sprintf("s\x00%d", rng.IntN(9))
		case 3:
			return rng.Float64()
		}
		return []byte{0, byte(rng.IntN(256))}
	}
	for range 2000 {
		row := []any{rng.Int64(), val(), fmt.Sprint(rng.IntN(100)), val(), val()}
		wantKey, _ := tuple.Append(tablePrefix(desc.ID), pkValues(desc, row)...)
		if gotKey, err := rowKey(desc, pkValues(desc, row)); err != nil || gotKey != string(wantKey) {
			t.Fatalf("rowKey(%v) = %x, %v; want %x", row, gotKey, err, wantKey)
		}
		for i := range idxs {
			k, v, enf, err := indexEntry(desc, &idxs[i], row)
			wk, wv, wenf := refIndexEntry(desc, &idxs[i], row)
			if err != nil || k != wk || !bytes.Equal(v, wv) || enf != wenf {
				t.Fatalf("index %d row %v: got %x %x %v %v; want %x %x %v", idxs[i].ID, row, k, v, enf, err, wk, wv, wenf)
			}
		}
	}
}
