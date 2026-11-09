package sqlfrontaudit

import "testing"

func TestEStringUnicode(t *testing.T) {
	d := setup(t)
	q := "select E'" + "\\" + "u00e9', length(E'" + "\\" + "u00e9')"
	t.Logf("%s => %s", q, rows(t, d, q))
}
