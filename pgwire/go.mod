module github.com/rohanthewiz/bytdb/pgwire

go 1.26.1

require (
	github.com/jackc/pgx/v5 v5.10.0
	github.com/rohanthewiz/btypedb v0.5.0
	github.com/rohanthewiz/bytdb v0.0.0
	github.com/rohanthewiz/serr v1.4.0
	golang.org/x/text v0.29.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/tidwall/btype v0.3.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
)

replace github.com/rohanthewiz/bytdb => ../
