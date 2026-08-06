# Type-codec fuzz runs: text[] whitespace canonicalization corruption found and fixed

Session: 75c2ba9c-6d1f-4b64-8d11-209efc6befee
Date: 2026-08-05

## What happened

Ran the four type-codec fuzz targets (types_fuzz_test.go) for 10
minutes each. FuzzTextArrayCanon failed at 48s with a real
data-corruption bug; the rest were clean.

| Target                 | Execs | Result |
|------------------------|-------|--------|
| FuzzTextArrayCanon     | —     | idempotence failure at 48s |
| FuzzJSONBCanon         | 21.9M | clean |
| FuzzTimestampRoundTrip | 32.3M | clean |
| FuzzUUIDRoundTrip      | 25M   | clean |
| FuzzTextArrayCanon re-run post-fix (8m) | 52.2M | clean |

## The bug: parser/formatter whitespace disagreement in text[]

Failing input: `{"\f0"}` → canon `{\f0}` → re-canon `{0}`. The form
feed silently vanished. Canonical text IS the stored form and text[]
equality is string equality, so this is silent data corruption.

Two mismatched definitions of whitespace in types.go:

- `textArrayNeedsQuotes` quoted elements containing space/\t/\n/\r
  but NOT \v or \f — so a \f element formatted bare;
- unquoted-element parsing trimmed with strings.TrimSpace, which
  strips \v and \f AND all Unicode whitespace (U+00A0, U+2000…) —
  a second latent corruption class the fuzzer hadn't reached yet
  (a bare element with a non-breaking space would lose it on
  re-parse).

## Fix (types.go)

One shared whitespace alphabet, exactly Postgres's array_isspace:
`const arraySpace = " \t\n\r\v\f"` plus `isArraySpace(byte)`. Used by
every trim/skip in ParseTextArray (outer literal trim, empty-body
check, element skip loops, unquoted-token trim) AND by
textArrayNeedsQuotes' quoting decision. Consequences:

- \v/\f-bearing elements are now quoted by array_out rules and
  survive round trips;
- Unicode spaces are element content (matching Postgres byte
  semantics), never trimmed;
- `{"a"\f, b}`-style whitespace between structural tokens is now
  accepted, as in Postgres (previously only space/tab were skipped).

## Regression guards

- Minimized fuzzer input kept as corpus:
  testdata/fuzz/FuzzTextArrayCanon/a539fd920997b53c (replayed by
  plain `go test`).
- New seeds in FuzzTextArrayCanon: `{"\f0"}`, `{"\v0"}`, `{\f}`,
  `{\va,b\f}`, and three seeds containing literal U+00A0 bytes
  (`{ }`, `{a b}`, `{" "}` with nbsp) covering the Unicode-space
  content class.

## Verification

- Crasher replay passes; `go test ./...` green in root and pgwire
  modules.
- Post-fix 8-minute FuzzTextArrayCanon run: 52.2M execs, clean.

## Campaign context

Across the whole day's fuzzing (seven targets, 10m each): two real
bugs found and fixed — the exponential exprType recursion (previous
session doc) and this text[] corruption. Both reachable from
user-supplied input over pgwire.

## Files touched

- types.go — arraySpace/isArraySpace, ParseTextArray trims/skips,
  textArrayNeedsQuotes
- types_fuzz_test.go — whitespace-edge seeds
- testdata/fuzz/FuzzTextArrayCanon/a539fd920997b53c — new corpus entry
