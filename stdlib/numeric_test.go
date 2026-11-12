package stdlib

// N-048 through database/sql: a numeric scans as its exact text, and
// into float64 and int64 destinations through database/sql's own
// conversions; parameters of the usual Go shapes (and bytdb.Decimal)
// bind.

import (
	"database/sql"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func TestNumericThroughDatabaseSQL(t *testing.T) {
	db := open(t, tempDSN(t, ""))
	if _, err := db.Exec(`create table p (id int primary key, amt numeric(8,2))`); err != nil {
		t.Fatal(err)
	}
	d, _ := bytdb.ParseDecimal("3.333")
	for i, v := range []any{19.99, "0.005", int64(7), d} {
		if _, err := db.Exec(`insert into p values ($1, $2)`, i+1, v); err != nil {
			t.Fatalf("insert %#v: %v", v, err)
		}
	}
	rows, err := db.Query(`select amt from p order by id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"19.99", "0.01", "7.00", "3.33"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("amounts = %v, want %v", got, want)
		}
	}
	var f float64
	var n int64
	var ns sql.NullString
	if err := db.QueryRow(`select amt, amt::int, null::numeric from p where id = 1`).Scan(&f, &n, &ns); err != nil {
		t.Fatal(err)
	}
	if f != 19.99 || n != 20 || ns.Valid {
		t.Errorf("scan into float/int/null = %v %v %v", f, n, ns)
	}
	ct, err := db.Query(`select amt from p limit 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer ct.Close()
	cts, _ := ct.ColumnTypes()
	if cts[0].DatabaseTypeName() != "NUMERIC" || cts[0].ScanType().Kind().String() != "string" {
		t.Errorf("column type = %s / %v", cts[0].DatabaseTypeName(), cts[0].ScanType())
	}
}
