# fycsql v1 System Invariants

Canonical, falsifiable invariants governing **fycsql v1**, derived from [GLOSSARY.md](../../GLOSSARY.md) and [ADR-0001](../adr/0001-mvcc-occ-snapshot-isolation.md)–[ADR-0010](../adr/0010-page1-header-versioning-stats-cli-and-package-layout.md). Implementation tickets and verification suites should cite invariant IDs directly as acceptance criteria.

---

## 1. Transactions, Concurrency & Lock Hierarchy

- **INV-TX-01 (Snapshot Immutability)**: Every **Transaction** binds a single **Commit Timestamp** (`snapTS`) immediately at `BEGIN` and observes only committed state with `CommitTS <= snapTS` overlaid with its own **Write Set** (RYOW). Prevents `G0`, `G1a/b/c`, `OTV`, `PMP`, `P4`, and `G-single` (write skew `G2-item`/`G2` is explicitly documented as the v1 Snapshot Isolation boundary).
- **INV-TX-02 (First-Committer-Wins)**. Validate row keys and old/new non-NULL unique prefixes against mutations after the writer’s Snapshot, including deletions and earlier successful batch members. A key collision returns retryable `ErrConflict`. Reclaim history through the oldest active writer’s Snapshot. Stale schema writers receive `ErrSchemaChanged`. A concurrent source-table change invalidates a snapshot index build with `ErrConflict`.
- **INV-TX-03 (Monotonic versions and allocation)**. Published Commit Timestamps increase in commit order. SchemaVersion advances on committed DDL. TableID allocation never reuses a committed identity. Per-table Internal RowID and AUTOINCREMENT increase in allocation order during an open lifetime. Restore the maximum committed high-water mark on reopen, including after deletion. Rollback leaves the visible schema unchanged. Gaps are valid.
- **INV-TX-04 (Statement Atomicity)**: Every DML statement stages mutations into an isolated **Statement Delta** and merges into the **Write Set** only on complete success; statement errors or `stmtCtx` cancellation leave zero partial mutations in the **Write Set** and prevent the Halloween Problem.
- **INV-TX-05 (Global Lock Order & Zero Deadlocks)**: All locks follow the strict partial order `CommitGate → WriteSerializeMu → TxRegistry → SnapshotRegistry → WAL/BufferPool` (with zero `PageLatch` on immutable published frames; `WriteSerializeMu` serializes physical WAL appends and Checkpoint coordination outside `SnapshotRegistry`). Because transactions hold no row or page write latches during execution, transaction-level deadlocks are structurally impossible.
- **INV-TX-06 (Bounded Write Set)**. Reserve space for the Write Set and Statement Delta together before retaining mutation data. Crossing MaxWriteSetSize (default 64 MB) returns `ErrTxTooLarge` and leaves the prior transaction view intact.

*Acceptance suites*: Hermitage SI matrix, Go 1.27 `testing/synctest`, Elle `list-append`, and `go test -race`.

---

## 2. Atomic Commit, Durability & Crash Recovery

- **INV-WAL-01 (Group Commit publication)**. Each successful batch member has immutable page images at its own TS, including Page 1. Discard failed members’ private pages and allocator changes before applying later members. Batch by encoded bytes with bounded WAL buffers. Under SyncFull publish only after the shared WAL sync succeeds.
- **INV-WAL-02 (Committed prefix)**. Scan salts, rolling CRCs and transaction boundaries, indexing frames only through a valid final marker. Keep the last complete offset and CRC. Ignore torn trailing bytes during Open. Return `ErrIO` on failed reads within the known file length. Repair and sync any invalid tail before the next writable append.
- **INV-WAL-03 (Four-step Checkpoint and generation lifetime)**. Sync WAL, copy exact page versions through the oldest active Snapshot (latest commit if none), then sync the database. Advance the backfill watermark only after DB sync. Under serialized reader admission recheck full backfill and drain old physical read references before rotating or truncating the WAL. Readers pin their source through cache fill. Include generation in WAL cache keys.
- **INV-WAL-04 (Idempotent recovery)**. Open performs a read-only WAL scan. After interruption during any Checkpoint step, even a torn main Page 1, reopening the file pair resolves the same committed prefix. A partial backfill leaves its WAL intact. Reopening read-only leaves file contents and lengths unchanged.
- **INV-WAL-05 (Fail-Stop and indeterminate commit)**. A failed durability sync or uncertain WAL append enters terminal Fail-Stop. Affected in-flight commit callers receive non-retryable `ErrCommitIndeterminate`. Preserve the WAL and reject subsequent mutations. Once a request crosses into append, cancellation waits for its outcome.
- **INV-WAL-06 (Locked open and bootstrap)**. Initialize only a newly created file under exclusive ownership. Validate an existing file after locking it. Only a file-not-found result means absent. Check magic, CRC, version and bounded page size before interpreting page offsets. A torn main header requires a verified committed Page 1 from the paired WAL (ADR-0010). Propagate unsupported versions and I/O errors distinctly.

*Acceptance suites*: Exhaustive 4-mode `CrashFS` cut-point enumeration + `ErrorFS` (`OnIndex(k)`).

---

## 3. Storage, Buffer Pool & B+Tree Structure

- **INV-STR-01 (Immutable Cached Frames & Zero Pin Leaks)**: Published **Buffer Pool** **Pages** and **Frames** are never mutated in place (the **Commit Gate** mutates private copy-on-modify staging buffers and writes through immutable frames). Every read/write path releases all pins (`PinCount == 0` on idle/close).
- **INV-STR-02 (Checksummed storage pages)**. Validate the Page 1 header CRC over bytes 0..123 and the common CRC of each live Page 2+ on loading it. Freelist trunks are checksummed. For unused leaf PageIDs listed by a trunk, verify ownership and count. Reuse writes a complete versioned page.
- **INV-STR-03 (Tree structure and capacity)**. Every tree retains its root PageID until DROP. Leaves have uniform depth and the RightSibling chain equals DFS order. Separators route ordered keys into disjoint child ranges. Four maximum 1000-byte leaf cells with slots fit a 4 KiB page. A non-root internal node has at least two children after deletion. Validate slot geometry, nonoverlap, fragment counts, overflow chunk lengths and chain termination before decoding.
- **INV-STR-04 (Whole-File Page Conservation)**: For any consistent **Snapshot**, page sets are pairwise disjoint and partition the entire database file:
  `{Page 1} ⊔ CatalogTree ⊔ UserClusteredTables ⊔ SecondaryIndexes ⊔ OverflowChains ⊔ Freelist == {1 .. TotalPageCount}`
  (zero leaked pages, zero overlaps, zero orphan **Overflow Pages**).
- **INV-STR-05 (Clustered Table ↔ Secondary Index Bijection)**: Every row in a **Clustered Table** maps 1-to-1 to its `encode(indexedCols..., RowKey)` entry in every **Secondary Index** of that table; `UNIQUE` indexes enforce prefix uniqueness whenever all indexed columns are non-`NULL` while treating `NULL` as distinct.

*Acceptance suites*: `DB.Verify()`, randomized B+Tree/Overflow/rebalance fuzzing, and `CrashFS` post-recovery `Verify()`.

---

## 4. Type System, Catalog & Schema Evolution

- **INV-CAT-01 (Strict Static Types & Finite Floats)**: Bound expressions and stored **Values** belong strictly to `{INTEGER, REAL, TEXT, BLOB, BOOLEAN, NULL}`; only `INTEGER → REAL` widens implicitly; `int64` overflow, division/modulo by zero, and `NaN`/`±Inf` error explicitly and never reach disk or index keys.
- **INV-CAT-02 (Order-preserving keys)**. Encoded key order under `bytes.Compare` matches the index component order, including normalized signed zero and binary text collation. Check the 900-byte limit after escaping and adding the Row Key. Range predicates and joins implement SQL three-valued logic. Sort elimination requires a matching order including NULL placement.
- **INV-CAT-03 (Permanent Monotonic Physical Slots)**: Every column in a table's **Catalog** entry receives a permanently monotonic, never-reused **Physical Slot** (≤ 1,024 per table lifetime). Decoding a historical row with `record.ColumnCount <= col.PhysicalSlot` synthesizes its immutable scalar `AddDefault` (or `NULL`); tombstoned `DROP COLUMN` slots are skipped via `VarintLenHeader` on read and written as 1-bit `NULL` (0 payload bytes) on subsequent writes.
- **INV-CAT-04 (Immutable Numeric Addressing)**: **Catalog** storage roots and **Secondary Indexes** bind to `(TableID, PhysicalSlot)` rather than string names, keeping `RENAME TABLE`/`RENAME COLUMN` $O(1)$ metadata-only. `DROP COLUMN` forbids removing PK columns, indexed columns, or the last visible column (`ErrConstraint`).

*Acceptance suites*: Property tests, `sqllogictest`, `modernc.org/sqlite` differential oracle (5-point normalization), and `Verify()`.

---

## 5. Query Execution & External Contracts

- **INV-QRY-01 (Bounded blocking memory)**. ORDER BY, GROUP BY, DISTINCT and joins obey per-operator WorkMem (default 16 MB), including equal-key group replay and merge buffers. Release temporary runs on completion, failure and cancellation. Read-only queries spill outside the database namespace.
- **INV-QRY-02 (Snapshot-local binding)**. Shared plans match the executing Snapshot’s committed SchemaVersion. Bind private DDL locally. A rollback cannot publish a private plan. A new plan for a newer schema leaves older readers’ bindings valid.
- **INV-API-01 (Honest Driver & Context Contract)**: `database/sql/driver` accepts only `LevelDefault`, `LevelSnapshot`, and `LevelRepeatableRead`, strictly enforces `opts.ReadOnly`, rejects `time.Time`, raw SQL `BEGIN`/`COMMIT`/`ROLLBACK` (supported only on native stateful `*fycsql.Conn`), multi-statement SQL strings, and unknown DSN parameters; `driver.Rows.Next` checks `ctx.Err()`.
- **INV-API-02 (Zero Third-Party Runtime Dependencies)**: All non-test packages (`.`, `./driver`, `./vfs`, `./cmd/...`, `./internal/...`) import strictly Go stdlib, enforced in CI via `go list -deps`.

---

## 6. High-Risk Cross-Layer Coupling Invariants

These four cross-layer invariants span multiple packages and must be explicitly tested during implementation:

1. **Multi-Version Page 1 Resolution (`INV-TX-01` × `INV-WAL-03` × `INV-CAT-04`)**:
   Resolve SchemaVersion, TotalPageCount and Freelist state from the complete Page 1 image visible at snapTS. Only committed schema metadata and stable object roots can be shared by SchemaVersion. Run prepared point lookups in both subtrees across a root split, collapse and subsequent page reuse while an old Snapshot remains open.
2. **Freelist Reuse Depends on Checkpoint Snapshot Watermark (`INV-WAL-03` × `INV-STR-04`)**:
   Popping a freed `PageID` from the **Freelist** (e.g. after `DROP TABLE A`) and overwriting it for `Table B` at `CommitTS = 12` is MVCC-safe for an active **Snapshot** at `CommitTS = 10` *only* because: (a) the overwrite is appended as a new **WAL** **Frame** (`CommitTS = 12`), and (b) **Checkpoint** strictly forbids copying `CommitTS > minActiveSnapshotTS` into `<db>`. Violating `INV-WAL-03` immediately causes silent cross-table corruption for older readers.
3. **Logical-Row Bijection Under Zero-Rewrite `ALTER TABLE` (`INV-STR-05` × `INV-CAT-03`)**:
   When `ALTER TABLE ADD COLUMN c DEFAULT 10` commits without rewriting existing rows and `CREATE INDEX idx_c ON t(c)` follows, `idx_c` contains `encode(10, RowKey)` while untouched on-disk rows still have `ColumnCount <= c.PhysicalSlot`. `Verify()` must check **Clustered Table** ↔ **Secondary Index** bijection over the **Catalog-decoded logical row** (synthesizing `AddDefault` and skipping tombstoned **Physical Slots**), not raw physical slot counts.
4. **Indeterminate batch outcome (`INV-WAL-01` × `INV-WAL-05`)**.
   Cut a two-member WAL write after A’s marker and inside B. Neither member is published in memory on error. Reopening may recover A. After a failed commit sync, either a process restart or power loss can retain a prefix of complete members. Every previously acknowledged SyncFull transaction survives. The driver must propagate the unknown outcome without replay.
