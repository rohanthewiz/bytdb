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
	"server_version":              ServerVersion,
	"server_encoding":             "UTF8",
	"client_encoding":             "UTF8",
	"datestyle":                   "ISO, MDY",
	"integer_datetimes":           "on",
	"standard_conforming_strings": "on",
	"timezone":                    "UTC",
	"search_path":                 `"$user", public`,
	// The two isolation parameters are placeholders: every caller
	// overlays the live values via execShow's iso argument, computed
	// from the engine's write mode and the session's state. They stay
	// in this map so SHOW ALL enumerates them.
	"transaction_isolation":         "serializable",
	"default_transaction_isolation": "serializable",
	"statement_timeout":             "0",
	"max_identifier_length":         "63",

	// What tools probe before ever setting (deep-look D-51), with the
	// values that describe bytdb truthfully: byte-order collation ("C"),
	// no roles (every session acts as a superuser), no lock or idle
	// timeouts unless the server sets them. application_name is filled
	// from the connection's startup parameters.
	"server_version_num":                  "160000",
	"application_name":                    "",
	"lock_timeout":                        "0",
	"idle_in_transaction_session_timeout": "0",
	"max_connections":                     "100",
	"is_superuser":                        "on",
	"session_authorization":               sysDatabase,
	"role":                                "none",
	"transaction_read_only":               "off",
	"transaction_deferrable":              "off",
	"default_transaction_deferrable":      "off",
	"default_transaction_read_only":       "off",
	"bytea_output":                        "hex",
	"extra_float_digits":                  "1",
	"lc_collate":                          "C",
	"lc_ctype":                            "C",
	"lc_messages":                         "C",
	"lc_monetary":                         "C",
	"lc_numeric":                          "C",
	"lc_time":                             "C",
	"intervalstyle":                       "postgres",
	"in_hot_standby":                      "off",
}

// isoDefault is the isolation level the engine actually provides when
// nobody asked for one: the single-writer default serializes every
// transaction, and concurrent-writes mode runs snapshot isolation,
// which Postgres calls repeatable read.
func isoDefault(occ bool) string {
	if occ {
		return "repeatable read"
	}
	return "serializable"
}

// showValue is one parameter's current value: the parsed
// statement_timeout, then live (the parameters a Session gives real
// semantics and therefore never stores as text — isolation, read-only
// state), then the session's SET text, then the defaults. ok is false
// for a parameter nothing defines.
func showValue(name string, vars map[string]string, timeout time.Duration,
	live map[string]string) (string, bool) {
	if name == "statement_timeout" && timeout > 0 {
		return strconv.FormatInt(timeout.Milliseconds(), 10) + "ms", true
	}
	if v, ok := live[name]; ok {
		return v, true
	}
	if v, ok := vars[name]; ok {
		return v, true
	}
	v, ok := showDefaults[name]
	return v, ok
}

// execShow answers SHOW name / SHOW ALL from the session's SET state
// (vars, and the parsed statement_timeout) over the defaults. live
// overlays the parameters with live values (see showValue) — SET never
// lands them in vars, so they must be supplied. An unknown, never-SET
// parameter is the Postgres error, wording and all.
func execShow(sv *ShowVar, vars map[string]string, timeout time.Duration,
	live map[string]string) (*Result, error) {
	get := func(name string) (string, bool) { return showValue(name, vars, timeout, live) }
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
