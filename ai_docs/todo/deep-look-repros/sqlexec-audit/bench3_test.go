package sqlexecaudit

import "testing"

func BenchmarkInSubquery500(b *testing.B) {
	run(b, `SELECT count(*) FROM orders WHERE cust IN (SELECT id FROM cust WHERE region = 3)`)
}
func BenchmarkSameAsJoin(b *testing.B) {
	run(b, `SELECT count(*) FROM orders o JOIN cust c ON c.id = o.cust WHERE c.region = 3`)
}
func BenchmarkLikePrefixPK(b *testing.B) {
	run(b, `SELECT count(*) FROM orders WHERE status LIKE 'st1%'`)
}
func BenchmarkCorrelatedExists(b *testing.B) {
	run(b, `SELECT count(*) FROM cust c WHERE EXISTS (SELECT 1 FROM orders o WHERE o.id = c.id)`)
}
func BenchmarkOrderByPKLimit(b *testing.B) {
	run(b, `SELECT id, amount FROM orders ORDER BY id DESC LIMIT 10`)
}
func BenchmarkSortAll(b *testing.B) {
	run(b, `SELECT id FROM orders ORDER BY amount`)
}
func BenchmarkDistinct(b *testing.B) {
	run(b, `SELECT DISTINCT status FROM orders`)
}
func BenchmarkUpdatePoint(b *testing.B) {
	run(b, `SELECT count(*) FROM orders WHERE id = 5`)
}
