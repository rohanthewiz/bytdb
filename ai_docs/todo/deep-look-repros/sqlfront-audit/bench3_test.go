package sqlfrontaudit

import (
	"fmt"
	"strings"
	"testing"
)

// Wide table: does per-row name resolution cost scale with column position?
func benchWide(b *testing.B, q string) {
	d := openDB(b)
	var cols []string
	for i := 1; i <= 40; i++ {
		cols = append(cols, fmt.Sprintf("column_number_%02d int", i))
	}
	must(b, d, `create table wide (id int primary key, `+strings.Join(cols, ", ")+`)`)
	for i := 0; i < 20000; i += 500 {
		var sb strings.Builder
		sb.WriteString(`insert into wide values `)
		for j := i; j < i+500; j++ {
			if j > i {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "(%d", j)
			for k := 0; k < 40; k++ {
				sb.WriteString(",1")
			}
			sb.WriteByte(')')
		}
		must(b, d, sb.String())
	}
	st, err := d.Prepare(q)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.Exec(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWideNoExpr(b *testing.B) { benchWide(b, `select count(*) from wide`) }
func BenchmarkWideFirstCol(b *testing.B) {
	benchWide(b, `select count(*) from wide where id + id + id + id + id + id + id + id > 0`)
}
func BenchmarkWideLastCol(b *testing.B) {
	benchWide(b, `select count(*) from wide where column_number_40 + column_number_40 + column_number_40 + column_number_40 + column_number_40 + column_number_40 + column_number_40 + column_number_40 > 0`)
}
func BenchmarkWideLastColMinus(b *testing.B) {
	benchWide(b, `select count(*) from wide where column_number_40 - column_number_40 - column_number_40 - column_number_40 - column_number_40 - column_number_40 - column_number_40 - column_number_40 > -100`)
}
