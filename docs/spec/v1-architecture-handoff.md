# fycsql v1 Architecture & `/to-tickets` Handoff Baseline

This document is the consolidated, decision-complete architectural handoff baseline for **fycsql v1** — a pure-Go embedded relational database (zero third-party runtime dependencies, Go 1.27 stdlib core, custom on-disk format) — produced by [fycsql v1 Architecture & Technical Decision Map](https://github.com/fangyuchen86/fycsql/issues/1).

Use this document together with `GLOSSARY.md` and `docs/adr/0001`–`0010` as the single source of truth when decomposing v1 into implementation tickets via `/to-tickets`.

---

## 1. Canonical Architectural Sources

- **Domain Vocabulary**: [GLOSSARY.md](../../GLOSSARY.md)
- **System Invariants (Acceptance Criteria Reference)**: [v1-invariants.md](./v1-invariants.md)
- **Module Seams & M1–M6 Interface Contracts**: [v1-module-contracts.md](./v1-module-contracts.md)
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
  - `prototype/vfs-pager-wal-recovery` — VFS seam, immutable Buffer Pool, WAL/Group Commit, 4-step Checkpoint, 76 crash images from the prototype workload (additional acceptance cases below), WSL2 `fsync` profile
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
│   ├── errs/                # Zero-dependency canonical sentinel errors & structured Error payload (re-exported by root `fycsql`)
│   ├── val/                 # Strict 5-type scalar Value/Type representation & finite-float/3VL primitives (depends only on `internal/errs`)
│   ├── pager/               # SnapshotRegistry, 128B Page 1 header, Page 2+ CRC/header, Freelist, noscan slab Buffer Pool + sharded CLOCK, WAL/FrameIndex, Group Commit, Checkpoint, Recovery
│   ├── btree/               # Clustered Table & Secondary Index B+Tree, Slotted Page ops, memcomparable key codec, row record codec, Overflow chains, Verify()
│   ├── catalog/             # Self-hosted `_fycsql_catalog`, SchemaVersion cache, TableID/PhysicalSlot & atomic RowID/AUTOINCREMENT, metadata-only ALTER TABLE
│   ├── tx/                  # Transaction snapshots backed by the pager registry, WriteSet, StatementDelta, OCC conflict validator, FIFO Commit Gate, Schema Barrier
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
| `24..27` | 4B | `CatalogRootPageID` | Fixed PageID 2 for the lifetime of the database |
| `28..31` | 4B | `FreelistHeadPageID` | Head trunk PageID (`0` if empty) |
| `32..35` | 4B | `FreelistPageCount` | Cached free page count |
| `36..39` | 4B | `Flags` | `0` in v1 |
| `40..47` | 8B | `SchemaVersion` | Incremented on every DDL commit |
| `48..55` | 8B | `LastCommitTS` | TS of this Page 1 image (the backfilled version in a fully synced main file) |
| `56..63` | 8B | `NextTableID` | Next available TableID in this version |
| `64..123` | 60B | `Reserved` | Zero-filled in v1 |
| `124..127` | 4B | `HeaderCRC32C` | CRC32C Castagnoli over `[0..123]` |

### Page 2+ Common Header (16 bytes at offset `0..15`, little-endian)

- `[0..3]` `PageCRC32C` (CRC32C Castagnoli over `[4..PageSize-1]`)
- `[4]` `PageType` (`1=Leaf`, `2=Internal`, `3=Overflow`, `4=Freelist`)
- `[5]` `Flags`
- `[6..7]` `CellCount` (payload byte count on Overflow; `FreeStart = 16 + 2*CellCount` implicit)
- `[8..9]` `FreeEnd` (initially PageSize, with stored value 0 decoding to 65536 on 64 KiB slotted pages)
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
| `MaxWriteSetSize` | `64 MB` | Combined Write Set and Statement Delta cap (`ErrTxTooLarge`) |
| `SyncMode` | `SyncFull` | `SyncFull` / `SyncNormal` / `SyncOff` |
| `CheckpointThreshold` | `1000` frames | Async `CheckpointPassive` trigger |
| `WALWarnWatermark` | `10000` frames | Triggers `WarningHook` & `Stats.WALWarnWatermarkExceeded` |
| `MaxSnapshotAge` | `0` (disabled) | Optional reader lifetime bound |
| `AutocommitMaxRetries` / `MaxUpdateRetries` | `5` | Jittered exponential backoff within `ctx` |
| `CloseTimeout` | `5s` | Used by `Close()` delegating to `CloseContext` |

### Canonical Sentinel Errors (`*fycsql.Error` with `Unwrap() []error`)

- **Concurrency / Transaction**: `ErrConflict`, `ErrSchemaChanged`, `ErrTxTooLarge`, `ErrTxDone`, `ErrReadOnly` (`IsRetryable` true for `ErrConflict` and `ErrSchemaChanged`)
- **SQL / Data**: `ErrSyntax`, `ErrConstraint` (sub-sentinels: `ErrUniqueViolation`, `ErrNotNullViolation`, `ErrPrimaryKeyViolation`), `ErrTypeMismatch`, `ErrOverflow`, `ErrDivisionByZero`, `ErrInvalidFloat`, `ErrNotFound`, `ErrNoRows`, `ErrNoLastInsertId`
- **Storage / Lifecycle**: `ErrDatabaseLocked`, `ErrDatabaseClosed`, `ErrCloseTimeout`, `ErrUnsupportedVersion`, `ErrCorruption`, `ErrIO`, `ErrFailStop`, `ErrCommitIndeterminate` (affected commit, never replay)

---

## 5. Recommended Vertical Implementation Milestones (for `/to-tickets`)

1. **M1 — VFS Seam & Fault-Injection Primitives (`vfs`)** ([contract](./contracts/m1-vfs.md)): `FS`/`File` interfaces, `OSFS` (Unix `flock` + parent-dir `Sync()`, Windows exclusive `CreateFile`), `MemFS`, `CrashFS` (4-mode exhaustive cut-point simulator), `ErrorFS` (`OnIndex(k)`).
2. **M2 — Pager, Buffer Pool, WAL, Checkpointing, & Recovery (`internal/pager`)** ([contract](./contracts/m2-pager.md)): 128B Page 1 header & versioning, `noscan` slab Buffer Pool + sharded CLOCK/`HintSeqScan`, Freelist trunk/leaf chain, WAL rolling CRC32C + Frame Index, byte-bounded Group Commit, 4-step Checkpointing, single-pass recovery, Fail-Stop + exhaustive `CrashFS`/`ErrorFS` tests.
3. **M3 — Slotted-Page B+Tree, Codecs, Overflow, & `Verify()` (`internal/btree`)** ([contract](./contracts/m3-btree.md)): 16B Slotted Page, zero-alloc memcomparable key codec (`ASC`/`DESC` + `NULLS FIRST`/`LAST`), compact row record + single-column projection, Clustered Table + Secondary Index, `MaxKeySize=900B` / `MaxLeafCellBytes=1000B` + Overflow chains, 90/10 & byte-midpoint splits, 25% merge/borrow, depth-safe forward/reverse cursors, single-tree structural verification.
4. **M4 — MVCC/OCC Transaction Manager & Self-Hosted Catalog (`internal/tx`, `internal/catalog`)** ([contract](./contracts/m4-catalog-and-tx.md)): transaction snapshot binding, RYOW `WriteSet` (64MB cap), `StatementDelta`, row/key OCC conflict validation, FIFO `CommitGate`, `SchemaBarrier`, `_fycsql_catalog` with `SchemaVersion` cache, atomic `Internal RowID`/`AUTOINCREMENT`, $O(1)$ metadata-only `ALTER TABLE` (`PhysicalSlot` tombstones + `AddDefault`), Catalog-aware whole-database verification, Hermitage + `testing/synctest` tests.
5. **M5 — SQL Frontend, Binder, Planner, & Volcano Executor (`internal/sql`)** ([contract](./contracts/m5-sql.md)): Hand-written Lexer + RD/Pratt Parser, strict Binder, Rule-Based Logical→Physical Planner (pushdown, sargable/prefix-`LIKE`, covering scan, sort elimination, Top-N, Index-NLJ/Hash/NLJ), Volcano Operators with periodic `ctx` checks, `WorkMem` External Merge-Sort spill, `EXPLAIN`, Prepared Statement cache.
6. **M6 — Public Go API, `database/sql/driver`, Observability, & CLI (`fycsql`, `driver`, `cmd/fycsql`)** ([contract](./contracts/m6-api-driver-cli.md)): `Open`/`DB`/`Conn`/`Tx`/`Stmt`/`Rows`/`Result`/`View`/`Update`/`NewConnector`/`Connector.DB()`, `fycsql/driver` (`io.Closer`, `SessionResetter`, `Validator`, `Pinger`, `NamedValueChecker`, `RowsColumnType*`), `*fycsql.Error`, `DB.Stats()`, `CloseContext`, `WithReadOnly`, pure-stdlib `cmd/fycsql` REPL.
7. **M7 — End-to-End Verification & Differential Oracle (`internal/sqllogictest`, `integration/`)**: ~300-LOC `sqllogictest` runner + curated corpus + fycsql strict-typing suites, `modernc.org/sqlite` differential fuzzing (5-point normalization), Elle `list-append` stress, end-to-end SQL + `CrashFS` recovery tests.
8. **M8 — Benchmarks, GC Profiling, & CI Gates (`integration/`, `.github/workflows/`)**: 5-category `benchstat` suite vs `modernc.org/sqlite` & `bbolt`, GC/alloc regression tracking, Fast-PR (<2m `CGO_ENABLED=0` + `go list -deps` stdlib gate + `-race`) & Extended-Nightly CI pipelines.

### Required milestone acceptance cases

| Stage | Scenario and observable result |
| --- | --- |
| M1–M2 | Pause the first opener before file creation. Let a second create, commit and close. The resumed opener reads the committed data. A WAL stat/read EIO fails Open. Read-only open on a read-only volume leaves the pair byte-identical. |
| M2 | Short-write across a batch marker, tear the main Page 1 during backfill, and crash again during recovery. Reopen twice and compare every page with a legal committed prefix. Preserve every acknowledged SyncFull commit. |
| M2 | Keep an old Snapshot across a new commit and Close. Partial Checkpoint retains the newer WAL data. Block an in-flight sync past CloseTimeout and verify a second Open remains locked until it exits. |
| M2 | Pause a reader after resolving a WAL offset and again before cache insertion. Rotate the generation and reuse the frame number when safe. Both old and new Snapshots read their own pages. Run Passive repeatedly with and without a pinned reader and measure watermark progress and WAL reuse. |
| M3 | For each supported page size, round-trip an empty page, first insert, split and compaction. Delete into an underfull internal node with a long parent separator. Fail allocation of an overflow page, sibling and root child in turn. Check old values, reachability and Freelist ownership. Feed CRC-valid malformed slots to the verifier and require ErrCorruption. |
| M4 | Interleave same-leaf disjoint updates, unique-prefix collisions, delete/reinsert and index build against concurrent DML. Check transaction outcomes and the recovered table/index bijection. Run an old Snapshot across DROP, PageID reuse and partial backfill. Check sequence high-water marks after deleting the largest ID and reopening. |
| M4–M5 | Run point lookups through root splits and collapse. Compare full scans and index plans across private ALTER/rollback, absent slots, explicit NULL and dropped/re-added names. Cover escaped keys near 900 bytes, wide composite directions, explicit NULL ordering and mixed numeric bounds. |
| M5–M6 | Compare a spilling join whose duplicate group exceeds WorkMem to a reference join, then cancel the same plan and check cleanup. Cancel one queued commit, then cancel another after append starts. Native API, driver and CLI report the correct outcome without replay. Exit the CLI with an open transaction. |

The pager owns snapshot registration and physical read lifetimes from M2. M4 builds transaction semantics on that interface. M3 exposes structural traversal to M4, where Catalog and tree data can be verified at the same Snapshot without a dependency from btree to catalog.

---

## 6. Explicit v1 Out-of-Scope Boundaries

- SQLite on-disk format compatibility; multi-process concurrent DB access; CGo or third-party runtime dependencies in core packages.
- Nested transactions / `SAVEPOINT`; disk-spilling huge uncommitted Write Sets (>64MB).
- Subqueries, `UNION`/`INTERSECT`/`EXCEPT`, CTEs (`WITH`), window functions, views, triggers, stored procedures, `CHECK`/`FOREIGN KEY`, full-text search.
- `DECIMAL`/`NUMERIC`, native `DATE`/`TIME`/`TIMESTAMP`, non-`BINARY` collations.
- `ALTER COLUMN TYPE`, `ADD/DROP PRIMARY KEY`, `VACUUM` physical file shrinking, `ANALYZE`/CBO statistics, `EXPLAIN ANALYZE`.
- Distributed replication, sharding, network wire protocol, client-server mode.
