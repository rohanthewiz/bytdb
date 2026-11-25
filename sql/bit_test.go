package sql

// N-053: bit strings and the bitwise operators through the SQL layer,
// for what corpus/84_bit.sql (checked against Postgres 16) does not
// reach: shift counts past the width, bit_length/octet_length staying
// byte counts over text while counting bits over bit strings, and the
// bit operators' typed kind surviving a windowed select list.

import "testing"

func TestBitwiseSQL(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table b (id int primary key, f bit(4), s text)`)
	exec(t, d, `insert into b values (1, B'1100', 'héllo'), (2, X'3', 'ab')`)
	for q, want := range map[string]string{
		// Shift counts wrap at 64, as the int8 shift does on the hardware.
		`select 1 << 64, 1 << 65, -1 >> 70`: "1 2 -1",
		// Over text these stay byte counts (é is two bytes)...
		`select bit_length(s), octet_length(s) from b where id = 1`: "48 6",
		// ...over a bit string they count bits and whole bytes of bits.
		`select bit_length(f), octet_length(f), length(f) from b where id = 1`: "4 1 4",
		// A windowed select list keeps the bit operators' kind.
		`select f & B'1010', ~f, count(*) over () from b order by id`: "1000 0011 2 | 0010 1100 2",
	} {
		if got := rowsText(exec(t, d, q)); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
}
