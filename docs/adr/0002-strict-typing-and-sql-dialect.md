---
status: accepted
---

# Strict Static Column Typing and SQL Dialect Semantics

fycsql v1 adopts strict static column typing across five canonical types (`INTEGER`, `REAL`, `TEXT`, `BLOB`, `BOOLEAN`) plus `NULL`, PostgreSQL-style arithmetic error semantics (`int64` overflow, division/modulo by zero, and `NaN`/`±Inf` all return explicit errors), a single `BINARY` (UTF-8 byte-order) collation, and case-sensitive `LIKE`, rather than SQLite's dynamic type affinity and silent coercions. Every table has a unique Row Key — either a single/composite explicit `PRIMARY KEY` (with `AUTOINCREMENT` on `INTEGER` PKs) or a hidden monotonic `int64` Internal RowID when `PRIMARY KEY` is omitted.

## Considered Options

- **SQLite Dynamic Type Affinity**: Enables near-verbatim execution of legacy SQLite `sqllogictest` files, but requires per-value type tags in every B+Tree key/cell, cross-type comparison rules, and surprising implicit string-to-number coercions in Go `database/sql` callers.
- **Mandatory Explicit Primary Keys**: Simplifies table metadata by removing hidden RowIDs, but rejects standard `CREATE TABLE t(a INT, b TEXT)` schemas common in SQL workloads and `sqllogictest`.
- **SQLite ASCII Case-Insensitive `LIKE`**: Matches SQLite's default `LIKE` behavior, but prevents prefix predicates (`WHERE name LIKE 'abc%'`) from using a standard `BINARY` B+Tree index range scan without a separate `NOCASE` index.

## Consequences

- **Compact, deterministic B+Tree keys & Go scans**: Column types are fixed at bind time, allowing tag-free fixed-width/memcomparable index encoding, finite IEEE-754 total ordering for `REAL`, and direct Go type mapping (`int64`, `float64`, `string`, `[]byte`, `bool`).
- **Coercion & identifier rules**: Only widening `INTEGER → REAL` is implicit (all other conversions require `CAST`); unquoted identifiers fold to lowercase, `"..."` quotes identifiers, `'...'` is strictly a string literal; `?` and `$N` parameter placeholders are both supported (no mixing in one statement).
- **Standard 3VL & SQLite-aligned `NULL` ordering**: `NULL` is distinct in `UNIQUE` indexes, ignored by non-`COUNT(*)` aggregates, and defaults to `NULLS FIRST` on `ASC` / `NULLS LAST` on `DESC` (with optional explicit `NULLS FIRST`/`LAST`).
- **Explicit 5-point SQLite differential oracle normalization**: Differential tests against `modernc.org/sqlite` use `STRICT` tables, set `PRAGMA case_sensitive_like = ON`, map `BOOLEAN ↔ 0/1`, assert explicit errors for overflow/div-by-zero/type mismatch, and filter out unsupported SQL (`sqllogictest` allowlist / `skipif`).
- **Scoped v1 exclusions**: Subqueries, set operations (`UNION`/`INTERSECT`/`EXCEPT`), `CHECK`/`FOREIGN KEY` enforcement, `DECIMAL`/`NUMERIC`, native `DATE`/`TIME`/`TIMESTAMP`, and non-`BINARY` collations are explicitly out of scope for v1.
