---
name: verify
description: Verify bytdb SQL/engine changes end to end by serving a scratch db over the Postgres wire protocol and driving it with a pgx client.
---

# Verifying bytdb changes at the wire surface

The user-facing surface is the Postgres wire protocol served by
`pgwire/cmd/bytdbd` (plus the embedded `sql.New(engine).Exec` API).
`psql` is NOT installed on this machine — drive the server with a
small Go client using `github.com/jackc/pgx/v5`, which is already in
the module cache (it's a test dependency of the `pgwire` module).

## Recipe

1. Build and run the server against a scratch db file (pick a free
   port; 5439 avoids the default 5433):

   ```bash
   cd pgwire && go build -o <scratch>/bytdbd ./cmd/bytdbd
   <scratch>/bytdbd -db <scratch>/test.bytdb -addr 127.0.0.1:5439   # background
   ```

2. Write a client in a scratch dir as its own module — pgx resolves
   offline from the cache if you copy `pgwire/go.sum` in first:

   ```bash
   cd <scratch>/client && GOWORK=off go mod init client
   cp <repo>/pgwire/go.sum .
   GOWORK=off GOFLAGS=-mod=mod GOPROXY=off go get github.com/jackc/pgx/v5@<version in pgwire/go.mod>
   GOWORK=off go run .
   ```

   Connect string: `postgres://any@127.0.0.1:5439/any?sslmode=disable`.

3. Drive both protocols: `conn.Exec`/`conn.Query` with inline SQL
   (simple), and queries with `$n` args (extended — exercises
   Describe's parameter-type inference; pgx encodes bindings by the
   *server-described* OIDs, so wrong inference shows up as a
   client-side encode error, e.g. "cannot find encode plan").

## Gotchas

- Untyped placeholders describe as text (OID 25); passing a Go int
  for one fails in pgx before reaching the server. That's the
  inference boundary in `sql/describe.go`, not a wire bug.
- Kill the server with `pkill -f <scratch>/bytdbd` (exit 144 is
  normal); delete the db file for a fresh catalog between runs.
