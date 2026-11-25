package interval

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// FuzzParse: interval input never panics, and whatever parses prints as
// text that parses back to the same fields (dumps rely on it). The one
// exception is Postgres's own: a clock of exactly -2562047788:00:54.775808
// (MinInt64 microseconds) prints but does not read back, because the
// reader parses the clock's magnitude before applying its sign.
func FuzzParse(f *testing.F) {
	// Seed with every golden input, and the tricky shapes.
	if fh, err := os.Open("testdata/golden.tsv"); err == nil {
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			if parts := strings.Split(sc.Text(), "\t"); len(parts) == 4 && parts[0] == "in" {
				f.Add(parts[1])
			}
		}
		fh.Close()
	}
	for _, s := range []string{"", "@", "-", "+1:", "P", "PT", "P1Y-", "1-", "1 day ago ago", "\x00", "9999999999999999999 days"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		iv, err := Parse(s)
		if err != nil {
			return
		}
		if iv.Micros == -9223372036854775808 {
			return
		}
		out := iv.String()
		back, err := Parse(out)
		if err != nil {
			t.Fatalf("Parse(%q) = %v prints %q, which does not parse: %v", s, iv, out, err)
		}
		if back != iv {
			t.Fatalf("Parse(%q) = %#v prints %q, which parses to %#v", s, iv, out, back)
		}
	})
}
