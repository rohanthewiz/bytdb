module engineaudit

go 1.26.1

require (
	github.com/rohanthewiz/btypedb v0.9.0
	github.com/rohanthewiz/bytdb v0.0.0
)

require (
	github.com/rohanthewiz/serr v1.4.0 // indirect
	github.com/tidwall/btype v0.3.0 // indirect
)

replace github.com/rohanthewiz/bytdb => ../../../..

replace github.com/rohanthewiz/btypedb => ../../../../../btypedb
