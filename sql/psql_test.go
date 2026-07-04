package sql

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The queries psql 17 actually sends for \dt, \d, and \d <name>
// against a v16 server, verbatim. They exercise the whole expression
// language at once: CASE, IN, regex operators, OPERATOR()/COLLATE,
// functions with arguments, casts, correlated scalar subqueries,
// EXISTS, ARRAY(SELECT ...), and UNION.

func seedCatalog(t *testing.T, d *DB) {
	t.Helper()
	exec(t, d, `create table users (id int primary key, name text, age int)`)
	exec(t, d, `create index users_age on users (age)`)
}

func usersOID(t *testing.T, d *DB) int64 {
	t.Helper()
	res := exec(t, d, `select oid from pg_class where relname = 'users'`)
	return res.Rows[0][0].(int64)
}

func TestPsqlListTables(t *testing.T) {
	d := openDB(t)
	seedCatalog(t, d)

	res := exec(t, d, `SELECT n.nspname as "Schema",
  c.relname as "Name",
  CASE c.relkind WHEN 'r' THEN 'table' WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view' WHEN 'i' THEN 'index' WHEN 'S' THEN 'sequence' WHEN 't' THEN 'TOAST table' WHEN 'f' THEN 'foreign table' WHEN 'p' THEN 'partitioned table' WHEN 'I' THEN 'partitioned index' END as "Type",
  pg_catalog.pg_get_userbyid(c.relowner) as "Owner"
FROM pg_catalog.pg_class c
     LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     LEFT JOIN pg_catalog.pg_am am ON am.oid = c.relam
WHERE c.relkind IN ('r','p','')
      AND n.nspname <> 'pg_catalog'
      AND n.nspname !~ '^pg_toast'
      AND n.nspname <> 'information_schema'
  AND pg_catalog.pg_table_is_visible(c.oid)
ORDER BY 1,2`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"public", "users", "table", "bytdb"}}) {
		t.Fatalf("got %v", res.Rows)
	}
	if !reflect.DeepEqual(res.Cols, []string{"Schema", "Name", "Type", "Owner"}) {
		t.Fatalf("cols %v", res.Cols)
	}
}

func TestPsqlDescribeTable(t *testing.T) {
	d := openDB(t)
	seedCatalog(t, d)
	oid := usersOID(t, d)

	// Name resolution.
	res := exec(t, d, `SELECT c.oid, n.nspname, c.relname
FROM pg_catalog.pg_class c
     LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname OPERATOR(pg_catalog.~) '^(users)$' COLLATE pg_catalog.default
  AND pg_catalog.pg_table_is_visible(c.oid)
ORDER BY 2, 3`)
	if !reflect.DeepEqual(res.Rows, [][]any{{oid, "public", "users"}}) {
		t.Fatalf("got %v", res.Rows)
	}

	// Relation details: CASE, casts, a literal-only item, am join.
	res = exec(t, d, fmt.Sprintf(`SELECT c.relchecks, c.relkind, c.relhasindex, c.relhasrules, c.relhastriggers, c.relrowsecurity, c.relforcerowsecurity, false AS relhasoids, c.relispartition, '', c.reltablespace, CASE WHEN c.reloftype = 0 THEN '' ELSE c.reloftype::pg_catalog.regtype::pg_catalog.text END, c.relpersistence, c.relreplident, am.amname
FROM pg_catalog.pg_class c
 LEFT JOIN pg_catalog.pg_class tc ON (c.reltoastrelid = tc.oid)
LEFT JOIN pg_catalog.pg_am am ON (c.relam = am.oid)
WHERE c.oid = '%d'`, oid))
	want := [][]any{{int64(0), "r", true, false, false, false, false, false, false,
		"", int64(0), "", "p", "d", "heap"}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}

	// Columns: format_type plus two correlated scalar subqueries.
	res = exec(t, d, fmt.Sprintf(`SELECT a.attname,
  pg_catalog.format_type(a.atttypid, a.atttypmod),
  (SELECT pg_catalog.pg_get_expr(d.adbin, d.adrelid, true)
   FROM pg_catalog.pg_attrdef d
   WHERE d.adrelid = a.attrelid AND d.adnum = a.attnum AND a.atthasdef),
  a.attnotnull,
  (SELECT c.collname FROM pg_catalog.pg_collation c, pg_catalog.pg_type t
   WHERE c.oid = a.attcollation AND t.oid = a.atttypid AND a.attcollation <> t.typcollation) AS attcollation,
  a.attidentity,
  a.attgenerated
FROM pg_catalog.pg_attribute a
WHERE a.attrelid = '%d' AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`, oid))
	wantCols := [][]any{
		{"id", "bigint", nil, true, nil, "", ""},
		{"name", "text", nil, false, nil, "", ""},
		{"age", "bigint", nil, false, nil, "", ""},
	}
	if !reflect.DeepEqual(res.Rows, wantCols) {
		t.Fatalf("got %v", res.Rows)
	}

	// Index listing: IN inside a LEFT JOIN's ON, pg_get_indexdef.
	res = exec(t, d, fmt.Sprintf(`SELECT c2.relname, i.indisprimary, i.indisunique, i.indisclustered, i.indisvalid, pg_catalog.pg_get_indexdef(i.indexrelid, 0, true),
  pg_catalog.pg_get_constraintdef(con.oid, true), contype, condeferrable, condeferred, i.indisreplident, c2.reltablespace
FROM pg_catalog.pg_class c, pg_catalog.pg_class c2, pg_catalog.pg_index i
  LEFT JOIN pg_catalog.pg_constraint con ON (conrelid = i.indrelid AND conindid = i.indexrelid AND contype IN ('p','u','x'))
WHERE c.oid = '%d' AND c.oid = i.indrelid AND i.indexrelid = c2.oid
ORDER BY i.indisprimary DESC, c2.relname`, oid))
	if len(res.Rows) != 2 {
		t.Fatalf("got %v", res.Rows)
	}
	if res.Rows[0][0] != "users_pkey" || res.Rows[0][1] != true ||
		!strings.Contains(res.Rows[0][5].(string), "UNIQUE INDEX users_pkey ON public.users USING btree (id)") {
		t.Fatalf("pkey row %v", res.Rows[0])
	}
	if res.Rows[1][0] != "users_age" || res.Rows[1][1] != false ||
		!strings.Contains(res.Rows[1][5].(string), "INDEX users_age ON public.users USING btree (age)") {
		t.Fatalf("index row %v", res.Rows[1])
	}

	// Publications: the three-arm UNION with never-evaluated exotica
	// (generate_series in FROM, array subscripts, string_agg).
	res = exec(t, d, fmt.Sprintf(`SELECT pubname
     , NULL
     , NULL
FROM pg_catalog.pg_publication p
     JOIN pg_catalog.pg_publication_namespace pn ON p.oid = pn.pnpubid
     JOIN pg_catalog.pg_class pc ON pc.relnamespace = pn.pnnspid
WHERE pc.oid ='%d' and pg_catalog.pg_relation_is_publishable('%d')
UNION
SELECT pubname
     , pg_get_expr(pr.prqual, c.oid)
     , (CASE WHEN pr.prattrs IS NOT NULL THEN
         (SELECT string_agg(attname, ', ')
           FROM pg_catalog.generate_series(0, pg_catalog.array_upper(pr.prattrs::pg_catalog.int2[], 1)) s,
                pg_catalog.pg_attribute
          WHERE attrelid = pr.prrelid AND attnum = prattrs[s+1])
        ELSE NULL END) FROM pg_catalog.pg_publication p
     JOIN pg_catalog.pg_publication_rel pr ON p.oid = pr.prpubid
     JOIN pg_catalog.pg_class c ON c.oid = pr.prrelid
WHERE pr.prrelid = '%d'
UNION
SELECT pubname
     , NULL
     , NULL
FROM pg_catalog.pg_publication p
WHERE p.puballtables AND pg_catalog.pg_relation_is_publishable('%d')
ORDER BY 1`, oid, oid, oid, oid))
	if len(res.Rows) != 0 {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestPsqlDescribeIndex(t *testing.T) {
	d := openDB(t)
	seedCatalog(t, d)
	res := exec(t, d, `select oid from pg_class where relname = 'users_pkey'`)
	oid := res.Rows[0][0].(int64)

	// The deferrable checks are EXISTS over correlated subqueries.
	res = exec(t, d, fmt.Sprintf(`SELECT i.indisunique, i.indisprimary, i.indisclustered, i.indisvalid,
  (NOT i.indimmediate) AND EXISTS (SELECT 1 FROM pg_catalog.pg_constraint WHERE conrelid = i.indrelid AND conindid = i.indexrelid AND contype IN ('p','u','x') AND condeferrable) AS condeferrable,
  (NOT i.indimmediate) AND EXISTS (SELECT 1 FROM pg_catalog.pg_constraint WHERE conrelid = i.indrelid AND conindid = i.indexrelid AND contype IN ('p','u','x') AND condeferred) AS condeferred,
i.indisreplident,
i.indnullsnotdistinct,
  a.amname, c2.relname, pg_catalog.pg_get_expr(i.indpred, i.indrelid, true)
FROM pg_catalog.pg_index i, pg_catalog.pg_class c, pg_catalog.pg_class c2, pg_catalog.pg_am a
WHERE i.indexrelid = c.oid AND c.oid = '%d' AND c.relam = a.oid
AND i.indrelid = c2.oid`, oid))
	want := [][]any{{true, true, false, true, false, false, false, false,
		"btree", "users", nil}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}

	// The is_key column reads indnkeyatts through a scalar subquery.
	res = exec(t, d, fmt.Sprintf(`SELECT a.attname,
  pg_catalog.format_type(a.atttypid, a.atttypmod),
  CASE WHEN a.attnum <= (SELECT i.indnkeyatts FROM pg_catalog.pg_index i WHERE i.indexrelid = '%d') THEN 'yes' ELSE 'no' END AS is_key,
  pg_catalog.pg_get_indexdef(a.attrelid, a.attnum, TRUE) AS indexdef
FROM pg_catalog.pg_attribute a
WHERE a.attrelid = '%d' AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`, oid, oid))
	if !reflect.DeepEqual(res.Rows, [][]any{{"id", "bigint", "yes", "id"}}) {
		t.Fatalf("got %v", res.Rows)
	}
}

func TestPsqlRolesAndDatabases(t *testing.T) {
	d := openDB(t)

	// \du: ARRAY(SELECT ...) over the (empty) membership table.
	res := exec(t, d, `SELECT r.rolname, r.rolsuper, r.rolinherit,
  r.rolcreaterole, r.rolcreatedb, r.rolcanlogin,
  r.rolconnlimit, r.rolvaliduntil,
  ARRAY(SELECT b.rolname
        FROM pg_catalog.pg_auth_members m
        JOIN pg_catalog.pg_roles b ON (m.roleid = b.oid)
        WHERE m.member = r.oid) as memberof
, r.rolreplication
, r.rolbypassrls
FROM pg_catalog.pg_roles r
WHERE r.rolname !~ '^pg_'
ORDER BY 1`)
	if len(res.Rows) != 1 || res.Rows[0][0] != "bytdb" || res.Rows[0][8] != "{}" {
		t.Fatalf("got %v", res.Rows)
	}

	// \l: E'\n' strings and array functions over NULL.
	res = exec(t, d, `SELECT
  d.datname as "Name",
  pg_catalog.pg_get_userbyid(d.datdba) as "Owner",
  pg_catalog.pg_encoding_to_char(d.encoding) as "Encoding",
  CASE d.datlocprovider WHEN 'b' THEN 'builtin' WHEN 'c' THEN 'libc' WHEN 'i' THEN 'icu' END AS "Locale Provider",
  d.datcollate as "Collate",
  d.datctype as "Ctype",
  d.daticulocale as "Locale",
  d.daticurules as "ICU Rules",
  CASE WHEN pg_catalog.array_length(d.datacl, 1) = 0 THEN '(none)' ELSE pg_catalog.array_to_string(d.datacl, E'\n') END AS "Access privileges"
FROM pg_catalog.pg_database d
ORDER BY 1`)
	if len(res.Rows) != 1 || res.Rows[0][0] != "bytdb" || res.Rows[0][3] != "libc" {
		t.Fatalf("got %v", res.Rows)
	}
}
