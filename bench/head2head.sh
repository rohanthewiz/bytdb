#!/usr/bin/env bash
# Reproduce the head-to-head concurrency table in the README's
# "Concurrent writes" section (see head2head_test.go for methodology).
# Requires: Go, a C toolchain (mattn SQLite + DuckDB are cgo), and
# optionally redis-server on PATH (Redis rows skip cleanly without it).
set -euo pipefail
cd "$(dirname "$0")"

redis-server --port 63799 --save '' --appendonly no --daemonize yes 2>/dev/null || true

go test -run '^$' -bench 'Insert_|Heavy_' -cpu 8 -count 3 -benchtime 1s -timeout 30m .

redis-cli -p 63799 shutdown nosave 2>/dev/null || true
