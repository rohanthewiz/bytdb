package sqlfrontaudit

import (
	"testing"

	bsql "github.com/rohanthewiz/bytdb/sql"
)

func TestErrorText(t *testing.T) {
	d := setup(t)
	for _, q := range []string{`select id frm t`, `select nope from t`, `select * from nope`, `insert into t (id) values ('x')`, `select nofunc(1)`, `select 1::nope`} {
		_, err := d.Exec(q)
		t.Logf("%-36s Error()=%q", q, err.Error())
	}
	_ = bsql.ServerVersion
}
