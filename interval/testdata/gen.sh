#!/bin/sh
# gen.sh: regenerate golden.tsv from a Postgres 16 server.
#
#   PGURL=postgres://postgres:pg@localhost:55432/postgres ./gen.sh
#
# Needs psql. The goldens were captured from postgres:16.9 (docker).
set -e
cd "$(dirname "$0")"
psql "${PGURL:?set PGURL}" -X -q -A -t -F '	' -v ON_ERROR_STOP=1 -f gen.sql > golden.tsv
wc -l golden.tsv
