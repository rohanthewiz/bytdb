package citext

import "testing"

func TestCmp(t *testing.T) {
	for _, c := range []struct {
		a, b Text
		want int
	}{
		{"ABC", "abc", 0}, {"a", "B", -1}, {"B", "c", -1}, {"Straße", "STRASSE", 1},
		{"", "", 0}, {"Ä", "ä", 0}, {"_z", "a", -1},
	} {
		if got := c.a.Cmp(c.b); got != c.want {
			t.Errorf("Cmp(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if Text("MiXeD").String() != "MiXeD" || Text("MiXeD").Fold() != "mixed" {
		t.Error("String must keep the case, Fold must lower it")
	}
}
