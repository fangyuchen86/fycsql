# fycsql v1 Architecture & `/to-tickets` Handoff Baseline

This document is the consolidated, decision-complete architectural handoff baseline for **fycsql v1** — a pure-Go embedded relational database (zero third-party runtime dependencies, Go 1.27 stdlib core, custom on-disk format) — produced by [fycsql v1 Architecture & Technical Decision Map](https://github.com/fangyuchen86/fycsql/issues/1).

Use this document together with `GLOSSARY.md` and `docs/adr/0001`–`0010` as the single source of truth when decomposing v1 into implementation tickets via `/to-tickets`.

---

## 1. Canonical Architectural Sources

- **Domain Vocabulary**: [GLOSSARY.md](../../GLOSSARY.md)
- **System Invariants (Acceptance Criteria Reference)**: [v1-invariants.md](./v1-invariants.md)
- **Architecture Decision Records**:
  - [ADR-0001](../adr/0001-mvcc-occ-snapshot-isolation.md) — Multi-Writer MVCC + OCC, Snapshot Isolation, FIFO Commit Gate, Schema Barrier
  - [ADR-0002](../adr/0002-strict-typing-and-sql-dialect.md) — Strict Static Typing, SQL Dialect, 3VL, Binary Collation, 5-Point SQLite Oracle Normalization
  - [ADR-0003](../adr/0003-page-level-wal-and-immutable-buffer-pool.md) — Two-File Page-Level WAL, Dedicated Page 1, Freelist, `vfs.FS`, Immutable `noscan` Buffer Pool
  - [ADR-0004](../adr/0004-wal-format-checkpointing-and-recovery.md) — WAL Binary Format, Rolling CRC32C, Zero-Delay Group Commit, 4-Step Checkpointing, Single-Pass Recovery
  - [ADR-0005](../adr/0005-clustered-btree-slotted-page-and-memcomparable-keys.md) — Clustered Row-Key B+Tree, 16B Common Page Header, Slotted Pages, Memcomparable Keys, Overflow, `Verify()`
  - [ADR-0006](../adr/0006-query-pipeline-catalog-volcano-and-spill.md) — Hand-Written Lexer/Parser, Binder, Self-Hosted `_fycsql_catalog`, Rule-Based Planner, Volcano Executor, Statement Delta, External Merge-Sort Spill
  - [ADR-0007](../adr/0007-go-public-api-sql-driver-and-error-semantics.md) — Native Go API, Opt-In `fycsql/driver`, 3-Layer `context.Context` Boundaries, `*fycsql.Error`, `CloseContext`
  - [ADR-0008](../adr/0008-verification-fault-injection-and-benchmark-architecture.md) — Verification Harnesses (`sqllogictest`, Differential Oracle, `CrashFS`/`ErrorFS`, Hermitage/`synctest`/Elle, `benchstat`)
  - [ADR-0009](../adr/0009-metadata-only-alter-table-and-physical-slots.md) — $O(1)$ Metadata-Only `ALTER TABLE` via Monotonic Physical Slots and Catalog Tombstones
  - [ADR-0010](../adr/0010-page1-header-versioning-stats-cli-and-package-layout.md) — 128B Self-Verifying Page 1 Header, Format Versioning, `DB.Stats()`, Pure-Stdlib CLI, Single-`go.mod` Package Layout
- **Validated Throwaway Prototypes (reference branches, never merged to `main`)**:
  - `prototype/vfs-pager-wal-recovery` — VFS seam, immutable Buffer Pool, WAL/Group Commit, 4-step Checkpoint, 76/76 exhaustive `CrashFS` cut-points, WSL2 `fsync` profile
  - `prototype/btree-codec-gc-profile` — 16B Slotted Page, memcomparable key/record codec, `0 B/op` hot paths, 64MB `noscan` slab GC profile, byte-midpoint splits, `Verify()`
- **Primary-Source Research Reports (reference branches)**:
  - `research/go-file-io-fsync-mmap`
  - `research/storage-wal-cow-aries`
  - `research/go-database-sql-driver`
  - `research/verification-and-crash-harnesses`

---

## 2. Package Layout & Module Boundary

Single root `go.mod` (`module github.com/fangyuchen86/fycsql`, `go 1.27`). Third-party test/benchmark dependencies (`modernc.org/sqlite`, `go.etcd.io/bbolt`, optional `-tags cgo_bench` `mattn/go-sqlite3`) are strictly confined to `integration/*_test.go` and verified in CI via `go list -deps` across all non-test packages.

```text
/
├── go.mod                   # github.com/fangyuchen86/fycsql (go 1.27; test-only deps in integration/*_test.go)
├── *.go                     # Root package `fycsql`: Open, DB, Conn, Tx, Stmt, Rows, Result, Value, Option, Stats, Error/Err*, NewConnector
├── driver/                  # Opt-in `fycsql/driver`: init() sql.Register("fycsql", ...)
├── vfs/                     # Public narrow VFS seam: FS, File, OSFS (unix/windows build tags), MemFS, CrashFS, ErrorFS
├── cmd/fycsql/              # Minimal pure-stdlib interactive CLI shell
├── internal/
│   ├── pager/               # 128B Page 1 header, Page 2+ CRC/header, Freelist, noscan slab Buffer Pool + sharded CLOCK, WAL/FrameIndex, Group Commit, Checkpoint, Recovery
│   ├── btree/               # Clustered Table & Secondary Index B+Tree, Slotted Page ops, memcomparable key codec, row record codec, Overflow chains, Verify()
│   ├── catalog/             # Self-hosted `_fycsql_catalog`, SchemaVersion cache, TableID/PhysicalSlot & atomic RowID/AUTOINCREMENT, metadata-only ALTER TABLE
│   ├── tx/                  # SnapshotRegistry, WriteSet, StatementDelta, OCC conflict validator, FIFO Commit Gate, Schema Barrier
│   ├── sql/                 # Lexer, RD/Pratt Parser, AST, Binder, Rule-Based Logical→Physical Planner, Volcano Executor, External Merge-Sort Spill, EXPLAIN
│   └── sqllogictest/        # ~300-LOC pure-Go sqllogictest runner (stdlib only)
└── integration/
    ├── diff_test.go         # modernc.org/sqlite differential fuzzing + curated sqllogictest corpus
    ├── isolation_test.go    # Hermitage SI matrix + testing/synctest + Elle list-append
    └── bench_test.go        # 5-category benchstat suite vs modernc.org/sqlite & bbolt
```

---

## 3. On-Disk Binary Header Summary

### Page 1 Database Header (128 bytes at offset `0..127`, little-endian; bytes `128..PageSize-1` zeroed in v1)

| Offset | Size | Field | Description |
| --- | ---: | --- | --- |
| `0..7` | 8B | `Magic` | `"FYCSQL\x00\x00"` |
| `8..11` | 4B | `FormatVersion` | `1` |
| `12..15` | 4B | `MinReaderVersion` | `1` (`>1` → `ErrUnsupportedVersion`) |
| `16..19` | 4B | `PageSize` | Power of 2 in `[4096, 65536]`, default `4096` |
| `20..23` | 4B | `TotalPageCount` | `>= 2` once initialized |
| `24..27` | 4B | `CatalogRootPageID` | Root PageID of `_fycsql_catalog` B+Tree |
| `28..31` | 4B | `FreelistHeadPageID` | Head trunk PageID (`0` if empty) |
| `32..35` | 4B | `FreelistPageCount` | Cached free page count |
| `36..39` | 4B | `Flags` | `0` in v1 |
| `40..47` | 8B | `SchemaVersion` | Incremented on every DDL commit |
| `48..55` | 8B | `LastCommitTS` | Highest `CommitTS` checkpointed to `<db>` |
| `56..63` | 8B | `NextTableID` | Monotonic `TableID` counter |
| `64..123` | 60B | `Reserved` | Zero-filled in v1 |
| `124..127` | 4B | `HeaderCRC32C` | CRC32C Castagnoli over `[0..123]` |

### Page 2+ Common Header (16 bytes at offset `0..15`, little-endian)

- `[0..3]` `PageCRC32C` (CRC32C Castagnoli over `[4..PageSize-1]`)
- `[4]` `PageType` (`1=Leaf`, `2=Internal`, `3=Overflow`, `4=Freelist`)
- `[5]` `Flags`
- `[6..7]` `CellCount` (payload byte count on Overflow; `FreeStart = 16 + 2*CellCount` implicit)
- `[8..9]` `FreeEnd` (starts at `PageSize`)
- `[10..11]` `FragBytes` (dead bytes awaiting lazy compaction)
- `[12..15]` `LinkPage` (`RightSibling` on Leaf, `RightmostChild` on Internal, `NextPage` on Overflow/Freelist)

### WAL Header (32B) & Frame Header (32B)

- **WAL Header**: `FYCWAL\x00\x00`, `Version = 1`, `PageSize`, `CheckpointGen`, `Salt1`, `Salt2`, `HeaderCRC32C`.
- **WAL Frame Header**: `PageID`, `DBPageCount` (`>0` commit marker on final frame of tx), `CommitTS`, `Salt1`, `Salt2`, rolling `PrevCRC`, `FrameCRC`, followed by `PageSize` bytes of page payload.

---

## 4. Default Configuration & Sentinel Error Catalog

### Key Engine Defaults

| Option | Default | Notes |
| --- | --- | --- |
| `PageSize` | `4096` | Power of 2 in `[4096, 65536]` |
| `BufferPoolSize` | `64 MB` | Contiguous `noscan` `[]byte` slab + sharded atomic-CLOCK |
| `WorkMem` | `16 MB` | Per-operator External Merge-Sort spill budget |
| `MaxWriteSetSize` | `64 MB` | Per-transaction RYOW Write Set cap (`ErrTxTooLarge`) |
| `SyncMode` | `SyncFull` | `SyncFull` / `SyncNormal` / `SyncOff` |
| `CheckpointThreshold` | `1000` frames | Async `CheckpointPassive` trigger |
| `WALWarnWatermark` | `10000` frames | Triggers `WarningHook` & `Stats.WALWarnWatermarkExceeded` |
| `MaxSnapshotAge` | `0` (disabled) | Optional reader lifetime bound |
| `AutocommitMaxRetries` / `MaxUpdateRetries` | `5` | Jittered exponential backoff within `ctx` |
| `CloseTimeout` | `5s` | Used by `Close()` delegating to `CloseContext` |

### Canonical Sentinel Errors (`*fycsql.Error` with `Unwrap() []error`)

- **Concurrency / Transaction**: `ErrConflict`, `ErrSchemaChanged`, `ErrTxTooLarge`, `ErrTxDone`, `ErrReadOnly` (`IsRetryable` true for `ErrConflict` and `ErrSchemaChanged`)
- **SQL / Data**: `ErrSyntax`, `ErrConstraint` (sub-sentinels: `ErrUniqueViolation`, `ErrNotNullViolation`, `ErrPrimaryKeyViolation`), `ErrTypeMismatch`, `ErrOverflow`, `ErrDivisionByZero`, `ErrInvalidFloat`, `ErrNotFound`, `ErrNoRows`, `ErrNoLastInsertId`
- **Storage / Lifecycle**: `ErrDatabaseLocked`, `ErrDatabaseClosed`, `ErrCloseTimeout`, `ErrUnsupportedVersion`, `ErrCorruption`, `ErrIO`, `ErrFailStop`

---

## 5. Recommended Vertical Implementation Milestones (for `/to-tickets`)

1. **M1 — VFS Seam & Fault-Injection Primitives (`vfs`)**: `FS`/`File` interfaces, `OSFS` (Unix `flock` + parent-dir `Sync()`, Windows exclusive `CreateFile`), `MemFS`, `CrashFS` (4-mode exhaustive cut-point simulator), `ErrorFS` (`OnIndex(k)`).
2. **M2 — Pager, Buffer Pool, WAL, Checkpointing, & Recovery (`internal/pager`)**: 128B Page 1 header & versioning, `noscan` slab Buffer Pool + sharded CLOCK/`HintSeqScan`, Freelist trunk/leaf chain, WAL rolling CRC32C + Frame Index, single-`WriteAt` Group Commit, 4-step Checkpointing, single-pass recovery, Fail-Stop + exhaustive `CrashFS`/`ErrorFS` tests.
3. **M3 — Slotted-Page B+Tree, Codecs, Overflow, & `Verify()` (`internal/btree`)**: 16B Slotted Page, zero-alloc memcomparable key codec (`ASC`/`DESC` + `NULLS FIRST`/`LAST`), compact row record + single-column projection, Clustered Table + Secondary Index, `MaxKeySize=900B` / `MaxLeafCellBytes=1000B` + Overflow chains, 90/10 & byte-midpoint splits, 25% merge/borrow, forward/reverse cursors, full `Verify()`.
4. **M4 — MVCC/OCC Transaction Manager & Self-Hosted Catalog (`internal/tx`, `internal/catalog`)**: `SnapshotRegistry`, RYOW `WriteSet` (64MB cap), `StatementDelta`, row/key OCC conflict validation, FIFO `CommitGate`, `SchemaBarrier`, `_fycsql_catalog` with `SchemaVersion` cache, atomic `Internal RowID`/`AUTOINCREMENT`, $O(1)$ metadata-only `ALTER TABLE` (`PhysicalSlot` tombstones + `AddDefault`), Hermitage + `testing/synctest` tests.
5. **M5 — SQL Frontend, Binder, Planner, & Volcano Executor (`internal/sql`)**: Hand-written Lexer + RD/Pratt Parser, strict Binder, Rule-Based Logical→Physical Planner (pushdown, sargable/prefix-`LIKE`, covering scan, sort elimination, Top-N, Index-NLJ/Hash/NLJ), Volcano Operators with periodic `ctx` checks, `WorkMem` External Merge-Sort spill, `EXPLAIN`, Prepared Statement cache.
6. **M6 — Public Go API, `database/sql/driver`, Observability, & CLI (`fycsql`, `driver`, `cmd/fycsql`)**: `Open`/`DB`/`Conn`/`Tx`/`Stmt`/`Rows`/`Result`/`View`/`Update`/`NewConnector`/`Connector.DB()`, `fycsql/driver` (`io.Closer`, `SessionResetter`, `Validator`, `Pinger`, `NamedValueChecker`, `RowsColumnType*`), `*fycsql.Error`, `DB.Stats()`, `CloseContext`, `WithReadOnly`, pure-stdlib `cmd/fycsql` REPL.
7. **M7 — End-to-End Verification & Differential Oracle (`internal/sqllogictest`, `integration/`)**: ~300-LOC `sqllogictest` runner + curated corpus + fycsql strict-typing suites, `modernc.org/sqlite` differential fuzzing (5-point normalization), Elle `list-append` stress, end-to-end SQL + `CrashFS` recovery tests.
8. **M8 — Benchmarks, GC Profiling, & CI Gates (`integration/`, `.github/workflows/`)**: 5-category `benchstat` suite vs `modernc.org/sqlite` & `bbolt`, GC/alloc regression tracking, Fast-PR (<2m `CGO_ENABLED=0` + `go list -deps` stdlib gate + `-race`) & Extended-Nightly CI pipelines.

---

## 6. Explicit v1 Out-of-Scope Boundaries

- SQLite on-disk format compatibility; multi-process concurrent DB access; CGo or third-party runtime dependencies in core packages.
- Nested transactions / `SAVEPOINT`; disk-spilling huge uncommitted Write Sets (>64MB).
- Subqueries, `UNION`/`INTERSECT`/`EXCEPT`, CTEs (`WITH`), window functions, views, triggers, stored procedures, `CHECK`/`FOREIGN KEY`, full-text search.
- `DECIMAL`/`NUMERIC`, native `DATE`/`TIME`/`TIMESTAMP`, non-`BINARY` collations.
- `ALTER COLUMN TYPE`, `ADD/DROP PRIMARY KEY`, `VACUUM` physical file shrinking, `ANALYZE`/CBO statistics, `EXPLAIN ANALYZE`.
- Distributed replication, sharding, network wire protocol, client-server mode.
