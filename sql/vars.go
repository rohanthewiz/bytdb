package sql

// vars.go: SHOW — reporting configuration parameters. SET already
// works (a Session remembers parameters; statement_timeout has real
// semantics); SHOW is its read side, which ORMs and JDBC probe on
// connect (SHOW TRANSACTION ISOLATION LEVEL, SHOW server_version).

import (
	"sort"
	"strconv"
	"time"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// ServerVersion is the value reported as server_version — over the
// wire at startup, by SHOW server_version, and (with a "PostgreSQL "
// prefix) by version(). The numeric prefix keeps drivers' version
// parsing working; the suffix keeps it honest about what is serving.
const ServerVersion = "16.0 (bytdb)"

// showDefaults are the parameters SHOW can always answer, with the
// values the wire server also reports at startup. A Session's SET
// values overlay these.
var showDefaults = map[string]string{
	"server_version":                ServerVersion,
	"server_encoding":               "UTF8",
	"client_encoding":               "UTF8",
	"datestyle":                     "ISO, MDY",
	"integer_datetimes":             "on",
	"standard_conforming_strings":   "on",
	"timezone":                      "UTC",
	"search_path":                   `"$user", public`,
	"transaction_isolation":         "serializable", // every engine txn is
	"default_transaction_isolation": "serializable",
	"statement_timeout":             "0",
	"max_identifier_length":         "63",
}

// execShow answers SHOW name / SHOW ALL from the session's SET state
// (vars, and the parsed statement_timeout) over the defaults. An
// unknown, never-SET parameter is the Postgres error, wording and all.
func execShow(sv *ShowVar, vars map[string]string, timeout time.Duration) (*Result, error) {
	get := func(name string) (string, bool) {
		if name == "statement_timeout" && timeout > 0 {
			return strconv.FormatInt(timeout.Milliseconds(), 10) + "ms", true
		}
		if v, ok := vars[name]; ok {
			return v, true
		}
		v, ok := showDefaults[name]
		return v, ok
	}
	if sv.Name == "all" {
		names := make(map[string]bool, len(showDefaults)+len(vars))
		for n := range showDefaults {
			names[n] = true
		}
		for n := range vars {
			names[n] = true
		}
		sorted := make([]string, 0, len(names))
		for n := range names {
			sorted = append(sorted, n)
		}
		sort.Strings(sorted)
		res := &Result{
			Cols:  []string{"name", "setting", "description"},
			Types: []bytdb.ColType{bytdb.TString, bytdb.TString, bytdb.TString},
			Tag:   "SHOW",
		}
		for _, n := range sorted {
			v, _ := get(n)
			res.Rows = append(res.Rows, []any{n, v, ""})
		}
		return res, nil
	}
	v, ok := get(sv.Name)
	if !ok {
		return nil, serr.New(`unrecognized configuration parameter "` + sv.Name + `"`)
	}
	return &Result{
		Cols:  []string{sv.Name},
		Types: []bytdb.ColType{bytdb.TString},
		Rows:  [][]any{{v}},
		Tag:   "SHOW",
	}, nil
}
