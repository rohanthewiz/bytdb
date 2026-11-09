module sqlfrontauditwire

go 1.26.1

require (
	github.com/jackc/pgx/v5 v5.10.0
	github.com/rohanthewiz/bytdb v0.22.0
	github.com/rohanthewiz/bytdb/pgwire v0.22.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/rohanthewiz/btypedb v0.9.0 // indirect
	github.com/rohanthewiz/serr v1.4.0 // indirect
	github.com/tidwall/btype v0.3.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)

replace github.com/rohanthewiz/bytdb => ../../../../..

replace github.com/rohanthewiz/bytdb/pgwire => ../../../../../pgwire

replace github.com/rohanthewiz/btypedb => ../../../../../../btypedb
