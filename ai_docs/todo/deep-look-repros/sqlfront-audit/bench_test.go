package sqlfrontaudit

import (
	"fmt"
	"strings"
	"testing"

	bsql "github.com/rohanthewiz/bytdb/sql"
)

func benchDB(b *testing.B, rows int) *bsql.DB {
	d := openDB(b)
	must(b, d, `create table users (id int primary key, name text, email text, age int, score float, city text, created timestamp)`)
	for i := 0; i < rows; i += 500 {
		var sb strings.Builder
		sb.WriteString(`insert into users values `)
		for j := i; j < i+500 && j < rows; j++ {
			if j > i {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "(%d, 'name%d', 'u%d@example.com', %d, %d.5, 'city%d', '2024-01-01 00:00:00')", j, j, j, j%90, j%100, j%50)
		}
		must(b, d, sb.String())
	}
	return d
}

const pointQ = `select id, name, email, age from users where id = $1`

// Parse alone, for the point query.
func BenchmarkParsePoint(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := bsql.Parse(pointQ); err != nil {
			b.Fatal(err)
		}
	}
}

// A moderately large ORM-shaped statement.
const ormQ = `select u.id, u.name, u.email, u.age, u.score, u.city, u.created from users u where u.age >= $1 and u.age < $2 and u.city in ($3, $4, $5) and (u.name like $6 or u.email like $7) order by u.created desc, u.id limit $8 offset $9`

func BenchmarkParseORM(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := bsql.Parse(ormQ); err != nil {
			b.Fatal(err)
		}
	}
}

// Text path: parse + bind + coerce + plan + execute each call.
func BenchmarkPointExecText(b *testing.B) {
	d := benchDB(b, 10000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Exec(pointQ, i%10000); err != nil {
			b.Fatal(err)
		}
	}
}

// Prepared path: bind + coerce + plan + execute each call.
func BenchmarkPointExecPrepared(b *testing.B) {
	d := benchDB(b, 10000)
	st, err := d.Prepare(pointQ)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.Exec(i % 10000); err != nil {
			b.Fatal(err)
		}
	}
}

// Session + prepared.
func BenchmarkPointSessionPrepared(b *testing.B) {
	d := benchDB(b, 10000)
	st, _ := d.Prepare(pointQ)
	s := d.NewSession()
	defer s.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.ExecStmt(st, i%10000); err != nil {
			b.Fatal(err)
		}
	}
}

// Engine-level point get, as the floor.
func BenchmarkPointEngineGet(b *testing.B) {
	d := benchDB(b, 10000)
	_ = d
	b.Skip("engine handle not reachable from sql.DB; see BenchmarkPointExecPrepared vs profile")
}

// Describe on each execution is what the wire Parse step pays.
func BenchmarkPrepareDescribePoint(b *testing.B) {
	d := benchDB(b, 100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := d.Prepare(pointQ)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := st.Describe(); err != nil {
			b.Fatal(err)
		}
	}
}

// Per-row expression evaluation: scan 100k rows.
func benchScan(b *testing.B, q string) {
	d := benchDB(b, 100000)
	st, err := d.Prepare(q)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.Exec(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScanCountOnly(b *testing.B)   { benchScan(b, `select count(*) from users`) }
func BenchmarkScanPredSimple(b *testing.B)  { benchScan(b, `select count(*) from users where age > 50`) }
func BenchmarkScanCondArith(b *testing.B)   { benchScan(b, `select count(*) from users where age + 1 > 51`) }
func BenchmarkScanCondArith3(b *testing.B)  { benchScan(b, `select count(*) from users where age + id * 2 - age > 51 and score * 2 > 10`) }
func BenchmarkScanCondIn(b *testing.B)      { benchScan(b, `select count(*) from users where age in (1, 5, 9, 13, 17, 21)`) }
func BenchmarkScanCondMinus(b *testing.B)   { benchScan(b, `select count(*) from users where id - age > 51`) }
func BenchmarkScanCondLike(b *testing.B)    { benchScan(b, `select count(*) from users where name like 'name1%'`) }
func BenchmarkScanSumExpr(b *testing.B)     { benchScan(b, `select sum(age * 2 + id) from users`) }
func BenchmarkScanProjExpr(b *testing.B)    { benchScan(b, `select id, age * 2 + id, name || '-' || city from users`) }
func BenchmarkScanProjPlain(b *testing.B)   { benchScan(b, `select id, age, name from users`) }
func BenchmarkScanCase(b *testing.B)        { benchScan(b, `select count(*) from users where case when age > 50 then score else age end > 20`) }
func BenchmarkScanCoalesce(b *testing.B)    { benchScan(b, `select count(*) from users where coalesce(age, 0) > 50`) }

// Catalog introspection with N user tables.
func benchCatalog(b *testing.B, tables int, q string) {
	d := openDB(b)
	for i := 0; i < tables; i++ {
		must(b, d, fmt.Sprintf(`create table tbl%d (id int primary key, a text, b int, c timestamp, d varchar(40), e bool, f float, g text, h int)`, i))
		must(b, d, fmt.Sprintf(`create index tbl%d_a on tbl%d (a)`, i, i))
	}
	st, err := d.Prepare(q)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.Exec(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCatalogHasTable200(b *testing.B) {
	benchCatalog(b, 200, `select count(*) from information_schema.tables where table_schema = 'public' and table_name = 'tbl7'`)
}
func BenchmarkCatalogColumns200(b *testing.B) {
	benchCatalog(b, 200, `select column_name, data_type from information_schema.columns where table_name = 'tbl7' order by ordinal_position`)
}
func BenchmarkCatalogColumns20(b *testing.B) {
	benchCatalog(b, 20, `select column_name, data_type from information_schema.columns where table_name = 'tbl7' order by ordinal_position`)
}
func BenchmarkCatalogAttrJoin200(b *testing.B) {
	benchCatalog(b, 200, `select a.attname, format_type(a.atttypid, a.atttypmod) from pg_attribute a join pg_class c on c.oid = a.attrelid where c.relname = 'tbl7' and a.attnum > 0 order by a.attnum`)
}
func BenchmarkCatalogSelect1(b *testing.B) { benchCatalog(b, 200, `select 1`) }
