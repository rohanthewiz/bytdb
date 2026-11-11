module github.com/rohanthewiz/bytdb/sql/difftest

go 1.26.1

require (
	github.com/jackc/pgx/v5 v5.10.0
	github.com/rohanthewiz/bytdb v0.23.0
	github.com/rohanthewiz/bytdb/pgwire v0.0.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/rohanthewiz/btypedb v0.10.0 // indirect
	github.com/rohanthewiz/serr v1.4.0 // indirect
	github.com/tidwall/btype v0.3.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)

// Never released or installed, so the replaces cost nothing and keep the
// harness on the working tree even with GOWORK=off (the bench/ pattern).
replace github.com/rohanthewiz/bytdb => ../..

replace github.com/rohanthewiz/bytdb/pgwire => ../../pgwire
