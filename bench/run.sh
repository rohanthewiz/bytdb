#!/usr/bin/env bash
# Reproduce the numbers in docs/benchmarks.md.
# Requires: Go, Docker (Postgres), redis-server on PATH.
set -euo pipefail
cd "$(dirname "$0")"

docker rm -f bytdb-bench-pg 2>/dev/null || true
docker run --rm -d --name bytdb-bench-pg \
  -e POSTGRES_PASSWORD=bench -p 54329:5432 \
  --tmpfs /var/lib/postgresql/data:rw,size=1g \
  postgres:16-alpine
redis-server --port 63799 --save '' --appendonly no --daemonize yes
sleep 3
docker exec bytdb-bench-pg pg_isready -U postgres

GOWORK=off go run . -targets bytdb-always,bytdb-lazy,bytdb-pgwire,postgres,duckdb,redis -out results.json

docker rm -f bytdb-bench-pg
redis-cli -p 63799 shutdown nosave || true
