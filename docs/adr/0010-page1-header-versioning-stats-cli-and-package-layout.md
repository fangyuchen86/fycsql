---
status: accepted
---

# Self-Verifying 128-Byte Page 1 Header, Runtime Stats, Pure-Stdlib CLI, and Single-Module Package Layout

fycsql v1 locks Page 1's first 128 bytes as a little-endian, self-verifying database header (`Magic = "FYCSQL\x00\x00"`, `FormatVersion = 1`, `MinReaderVersion = 1`, `PageSize`, `TotalPageCount`, `CatalogRootPageID`, `FreelistHeadPageID`, `FreelistPageCount`, `Flags`, `SchemaVersion`, `LastCommitTS`, `NextTableID`, 60 reserved zero bytes, and `HeaderCRC32C` over bytes `[0..123]`) so `Open()` validates header integrity and version compatibility (`ErrUnsupportedVersion` when `MinReaderVersion > 1`, `FormatVersion == 0`, or WAL `Version != 1`) before trusting `PageSize`. Runtime observability is exposed via a zero-allocation `DB.Stats() Stats` snapshot plus an optional `WithWarningHook(func(Warning))` callback; interactive access is provided by a minimal pure-stdlib `cmd/fycsql` REPL over `db.Conn(ctx)`; and the repository uses a single root `go.mod` (`go 1.27`) with cohesive deep packages (`fycsql`, `driver`, `vfs`, `cmd/fycsql`, `internal/{pager,btree,catalog,tx,sql,sqllogictest}`, and `integration/*_test.go`, amending ADR-0008).

## Considered Options

- **CRC32C Covering All of Page 1 (`0..PageSize-1`)**: Covers trailing padding bytes, but creates a bootstrap circularity where `Open()` must trust an unverified `PageSize` field in order to know how many bytes to checksum.
- **SQL `PRAGMA` / Virtual System Tables for Runtime Stats**: Matches SQLite conventions, but adds parser, binder, and virtual-table machinery unnecessary when Go callers have direct typed `DB.Stats()` and `Connector.DB().Stats()`.
- **Third-Party Readline / TUI Library in `cmd/fycsql`**: Adds tab completion and history polish, but violates the zero-third-party-runtime-dependency boundary for core binaries.

## Consequences

- **Bootstrap-safe header verification & forward-compatible version gating**: `Open()` reads 128 bytes first, verifies `Magic` and `HeaderCRC32C` (`ErrCorruption` on mismatch), checks `MinReaderVersion` / `FormatVersion` (`ErrUnsupportedVersion` on incompatible format), then validates `PageSize` (power of 2 in `[4096, 65536]`) and zero-filled padding `[128..PageSize-1]`. No auto-migration machinery ships in v1.
- **Zero-dependency observability & CLI**: `DB.Stats()` reports Buffer Pool hits/misses/pins/evictions, WAL frames/bytes/watermark status, Checkpoint counts/durations, active Snapshots/oldest age, commit/conflict counts, `SchemaVersion`, page/freelist counts, and `FailStop`; `WithWarningHook` surfaces ADR-0004 watermark alerts without binding a logging library. `cmd/fycsql` provides `-readonly`, `-sync`, `-format table|csv|json`, `-c`, multi-line SQL + `BEGIN`/`COMMIT`/`ROLLBACK`, and 8 dot-commands (`.help`, `.tables`, `.schema`, `.checkpoint`, `.verify`, `.stats`, `.mode`, `.quit`).
- **Single-`go.mod` ergonomics with strict runtime boundary**: Developers run `go test -race ./...` from the repo root with zero multi-module friction; CI enforces via `go list -deps` that all non-test packages depend strictly on Go stdlib while `modernc.org/sqlite` and `go.etcd.io/bbolt` stay confined to `integration/*_test.go`.
