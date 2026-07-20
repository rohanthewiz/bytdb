package sql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// TestLimitOffsetParams covers $n placeholders as LIMIT/OFFSET counts:
// binding, re-execution with different values, NULL's no-op reading,
// and the rejections a literal count would also draw.
func TestLimitOffsetParams(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key)`)
	exec(t, d, `insert into t values (1), (2), (3), (4), (5)`)

	if got := ids(t, d, `select id from t order by id limit $1`, 2); !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Fatalf("LIMIT $1: %v", got)
	}
	if got := ids(t, d, `select id from t order by id limit $1 offset $2`, 2, 2); !reflect.DeepEqual(got, []int64{3, 4}) {
		t.Fatalf("LIMIT $1 OFFSET $2: %v", got)
	}
	// The same $n may serve both clauses.
	if got := ids(t, d, `select id from t order by id limit $1 offset $1`, 2); !reflect.DeepEqual(got, []int64{3, 4}) {
		t.Fatalf("shared $1: %v", got)
	}
	if got := ids(t, d, `select id from t order by id limit $1`, 0); len(got) != 0 {
		t.Fatalf("LIMIT $1=0: %v", got)
	}
	// NULL is Postgres's no-op: LIMIT NULL means no limit, OFFSET NULL
	// skips nothing.
	if got := ids(t, d, `select id from t order by id limit $1 offset $2`, nil, nil); !reflect.DeepEqual(got, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("NULL limit/offset: %v", got)
	}

	// A prepared statement re-binds per execution — the parsed form
	// must not retain an earlier binding's count.
	st, err := d.Prepare(`select id from t order by id limit $1`)
	if err != nil {
		t.Fatal(err)
	}
	for want := 1; want <= 3; want++ {
		res, err := st.Exec(int64(want))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) != want {
			t.Fatalf("prepared LIMIT %d returned %d rows", want, len(res.Rows))
		}
	}
	// Describe types the count as an integer so wire drivers encode it
	// as int8, matching Postgres's Describe for the same query.
	info, err := st.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Params) != 1 || info.Params[0] != bytdb.TInt {
		t.Fatalf("described params: %v; want [int]", info.Params)
	}

	// The bound value faces the same rule as a literal count.
	if _, err := d.Exec(`select id from t limit $1`, -1); err == nil ||
		!strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("negative LIMIT param: %v", err)
	}
	if _, err := d.Exec(`select id from t offset $1`, "abc"); err == nil ||
		!strings.Contains(err.Error(), "non-negative integer") {
		t.Fatalf("text OFFSET param: %v", err)
	}

	// Placeholders resolve in nested SELECTs too — any select that
	// binds through the same walk. The scalar-subquery form is the
	// probe because it is a path that honors a subquery's LIMIT (the
	// IN-subquery evaluation ignores LIMIT even as a literal — a
	// separate, pre-existing gap).
	if got := ids(t, d, `select (select id from t order by id limit 1 offset $1)`, 1); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("scalar subquery OFFSET $1: %v", got)
	}
	if got := ids(t, d, `select id from t union all select id from t order by id limit $1`, 2); !reflect.DeepEqual(got, []int64{1, 1}) {
		t.Fatalf("UNION LIMIT $1: %v", got)
	}
}
