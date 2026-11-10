package sql

// errs.go: the SQL layer's classified errors (deep-look D-56). Each
// keeps Postgres's message, naming what is missing, and matches the
// bytdb sentinel under errors.Is, so embedders branch on
// errors.Is(err, bytdb.ErrNoSuchTable) rather than on message text,
// and the wire server maps the SQLSTATE the same way.

import "github.com/rohanthewiz/bytdb"

// noSuchTable is the undefined-table error (42P01).
func noSuchTable(name string) error {
	return bytdb.KindErr(1, `relation "`+name+`" does not exist`,
		[]error{bytdb.ErrNoSuchTable}, "table", name)
}

// noSuchColumn is the undefined-column error (42703); table may be ""
// when the reference had no qualifier to name.
func noSuchColumn(table, column string) error {
	fields := []string{"column", column}
	if table != "" {
		fields = []string{"table", table, "column", column}
	}
	return bytdb.KindErr(1, bytdb.ColumnMissingMsg(table, column),
		[]error{bytdb.ErrNoSuchColumn}, fields...)
}
