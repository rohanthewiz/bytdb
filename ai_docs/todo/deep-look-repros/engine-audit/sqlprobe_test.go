package engineaudit

import (
	"testing"
	"time"

	bsql "github.com/rohanthewiz/bytdb/sql"
)

// Pointer for the sql/ reviewer: nextval inside a DDL row callback.
func TestSQLUsingNextval(t *testing.T) {
	e := open(t, false)
	d := bsql.New(e)
	for _, q := range []string{
		"CREATE TABLE t (id INT PRIMARY KEY, v TEXT)",
		"INSERT INTO t VALUES (1,'a'),(2,'b')",
		"CREATE SEQUENCE s",
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	for _, q := range []string{
		"ALTER TABLE t ALTER COLUMN v TYPE BIGINT USING nextval('s')",
		"ALTER TABLE t ADD CONSTRAINT c CHECK (nextval('s') > 0)",
	} {
		var err error
		var pan any
		ok := runWithTimeout(3*time.Second, func() {
			defer func() { pan = recover() }()
			_, err = d.Exec(q)
		})
		t.Logf("%s\n    finished=%v err=%v panic=%v", q, ok, err, pan)
	}
	// Is the engine still writable afterwards?
	ok := runWithTimeout(3*time.Second, func() { _, _ = d.Exec("INSERT INTO t VALUES (9, NULL)") })
	t.Logf("engine writable afterwards: %v", ok)
}
