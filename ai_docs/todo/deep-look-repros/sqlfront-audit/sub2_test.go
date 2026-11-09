package sqlfrontaudit

import (
	"testing"
	"time"
)

// Same shapes with no secondary index, plus an int control and bound params.
func TestSubqueryLiteralCoercionNoIndex(t *testing.T) {
	d := setup(t)
	want(t, d, "[[1]]", `select id from t where id in (select id from t where i = '10')`) // control: int adapts
	want(t, d, "[[1]]", `select id from t where id in (select id from t where d = '2024-02-29')`)
	want(t, d, "[[1]]", `with q as (select id from t where ts = '2024-02-29 12:00:00') select id from q`)
	want(t, d, "[[1]]", `select id from (select id from t where u = 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11') q`)
	tm := time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC)
	want(t, d, "[[1]]", `select id from t where id in (select id from t where ts = $1)`, tm)
	want(t, d, "[[1]]", `select id from t where id in (select id from t where d = $1)`, tm)
	want(t, d, "[[1]]", `select id from t where id in (select id from t where d = $1)`, "2024-02-29")
	want(t, d, "[[1]]", `select id from t where id in (select id from t where u = $1)`, "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11")
	st, _ := d.Prepare(`select id from t where id in (select id from t where d = $1 and u = $2)`)
	info, err := st.Describe()
	t.Logf("Describe params for a subquery predicate: %q err=%v", info.Params, err)
}
