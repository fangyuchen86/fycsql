# fycsql v1 System Invariants

Canonical, falsifiable invariants governing **fycsql v1**, derived from [GLOSSARY.md](../../GLOSSARY.md) and [ADR-0001](../adr/0001-mvcc-occ-snapshot-isolation.md)–[ADR-0010](../adr/0010-page1-header-versioning-stats-cli-and-package-layout.md). Implementation tickets and verification suites should cite invariant IDs directly as acceptance criteria.

---

## 1. Transactions, Concurrency & Lock Hierarchy

- **INV-TX-01 (Snapshot Immutability)**: Every **Transaction** binds a single **Commit Timestamp** (`snapTS`) immediately at `BEGIN` and observes only committed state with `CommitTS <= snapTS` overlaid with its own **Write Set** (RYOW). Prevents `G0`, `G1a/b/c`, `OTV`, `PMP`, `P4`, and `G-single` (write skew `G2-item`/`G2` is explicitly documented as the v1 Snapshot Isolation boundary).
- **INV-TX-02 (First-Committer-Wins)**: At the **Commit Gate**, if two concurrent read-write **Transactions** mutate the same `(TableID, Row Key)`, the same non-`NULL` **Secondary Index** unique key, or collide across a **Schema Barrier**, at most one commits; the loser receives retryable `ErrConflict` or `ErrSchemaChanged`.
- **INV-TX-03 (Strict Monotonicity)**: `Commit Timestamp`, `SchemaVersion`, `TableID`, **Internal RowID**, and `AUTOINCREMENT` counters are strictly monotonically increasing and never roll back on statement errors or aborted **Transactions** (gaps are permitted).
- **INV-TX-04 (Statement Atomicity)**: Every DML statement stages mutations into an isolated **Statement Delta** and merges into the **Write Set** only on complete success; statement errors or `stmtCtx` cancellation leave zero partial mutations in the **Write Set** and prevent the Halloween Problem.
- **INV-TX-05 (Global Lock Order & Zero Deadlocks)**: All locks follow the strict partial order `CommitGate → SnapshotRegistry → WAL/BufferPool → PageLatch`. Because transactions hold no row or page write latches during execution, transaction-level deadlocks are structurally impossible.
- **INV-TX-06 (Bounded Write Set)**: Uncommitted mutations in a single **Transaction** never exceed `MaxWriteSetSize` (default `64MB`), failing cleanly with `ErrTxTooLarge`.

*Verified by*: Hermitage SI matrix, Go 1.27 `testing/synctest`, Elle `list-append`, and `go test -race`.

---

## 2. Atomic Commit, Durability & Crash Recovery

- **INV-WAL-01 (Single-WriteAt Group Commit & Post-Sync Publication)**: **Group Commit** encodes all frames of a validated batch into one contiguous `WriteAt` buffer. Under `SyncFull`, new **Commit Timestamps** are published to the **Frame Index** and **Buffer Pool** strictly after `walFile.Sync()` succeeds; a failed `WriteAt` leaves zero dirty or published frames.
- **INV-WAL-02 (Rolling Checksum Prefix Atomicity)**: Every **WAL** **Frame** carries `Salt1`/`Salt2`, rolling `PrevCRC`, and `FrameCRC`, with `DBPageCount > 0` marking only the final frame of a **Transaction**. Recovery halts and truncates at the first salt mismatch, CRC break, or trailing uncommitted sequence, guaranteeing atomic transaction prefixes across any torn write, truncation, or out-of-order disk flush.
- **INV-WAL-03 (Four-Step Checkpoint Barrier & Snapshot Watermark)**: **Checkpoint** executes strictly in order `WAL.Sync → DB.Write → DB.Sync → WAL.Reset/Truncate+Sync`, never copying any **Frame** with `CommitTS > minActiveSnapshotTS` into `<db>` and never resetting/overwriting **WAL** frames still required by any active **Snapshot**.
- **INV-WAL-04 (Idempotent Single-Pass Recovery)**: `Open()` executes a read-only single-pass **WAL** scan. Crashing at any point during `Open()` or during any step of **Checkpoint** converges to the exact same committed prefix on subsequent recovery.
- **INV-WAL-05 (Terminal Fail-Stop & Commit Gate `Sync()` Boundary)**: Any **VFS** `Sync()` error transitions the database permanently into **Fail-Stop**, rejecting all further mutations and preserving `<db>-wal` on `Close()`. Once WAL append/`Sync()` starts inside the **Commit Gate**, `ctx` cancellation does not abandon the in-flight `Sync()` (preventing phantom commits).
- **INV-WAL-06 (Exclusive Lock & Bootstrap-Safe Page 1 Validation)**: An open `<db>` holds a single-process OS lock (duplicate same-process `Open` returns `ErrDatabaseLocked`). `Open()` reads and validates Page 1's first 128 bytes (`Magic`, `HeaderCRC32C` → `ErrCorruption`; `MinReaderVersion > 1` / `FormatVersion == 0` / WAL `Version != 1` → `ErrUnsupportedVersion`) before trusting `PageSize`.

*Verified by*: Exhaustive 4-mode `CrashFS` cut-point enumeration + `ErrorFS` (`OnIndex(k)`).

---

## 3. Storage, Buffer Pool & B+Tree Structure

- **INV-STR-01 (Immutable Cached Frames & Zero Pin Leaks)**: Published **Buffer Pool** **Pages** and **Frames** are never mutated in place (the **Commit Gate** mutates private copy-on-modify staging buffers and writes through immutable frames). Every read/write path releases all pins (`PinCount == 0` on idle/close).
- **INV-STR-02 (Universal Page Checksums)**: Page 1 `[0..123]` is verified via `HeaderCRC32C`, and every Page 2+ (`Leaf`, `Internal`, `Overflow`, `Freelist`) is verified via its 16-byte common header `[0..3] PageCRC32C` on read into the **Buffer Pool** (`ErrCorruption` on mismatch).
- **INV-STR-03 (B+Tree Ordering, Depth, Fanout & Sibling Linkage)**: All leaves share uniform depth; keys/separators are strictly ordered by `bytes.Compare`; the singly-linked `RightSibling` chain matches left-to-right DFS leaf order exactly; `MaxKeySize = 900B` coupled with `MaxLeafCellBytes = 1000B` and byte-midpoint splitting on both Leaf and Internal nodes guarantees ≥ 4 cells per 4KB leaf and fanout ≥ 4 per internal node.
- **INV-STR-04 (Whole-File Page Conservation)**: For any consistent **Snapshot**, page sets are pairwise disjoint and partition the entire database file:
  `{Page 1} ⊔ ClusteredTables ⊔ SecondaryIndexes ⊔ OverflowChains ⊔ Freelist == {1 .. TotalPageCount}`
  (zero leaked pages, zero overlaps, zero orphan **Overflow Pages**).
- **INV-STR-05 (Clustered Table ↔ Secondary Index Bijection)**: Every row in a **Clustered Table** maps 1-to-1 to its `encode(indexedCols..., RowKey)` entry in every **Secondary Index** of that table; `UNIQUE` indexes enforce prefix uniqueness whenever all indexed columns are non-`NULL` while treating `NULL` as distinct.

*Verified by*: `DB.Verify()`, randomized B+Tree/Overflow/rebalance fuzzing, and `CrashFS` post-recovery `Verify()`.

---

## 4. Type System, Catalog & Schema Evolution

- **INV-CAT-01 (Strict Static Types & Finite Floats)**: Bound expressions and stored **Values** belong strictly to `{INTEGER, REAL, TEXT, BLOB, BOOLEAN, NULL}`; only `INTEGER → REAL` widens implicitly; `int64` overflow, division/modulo by zero, and `NaN`/`±Inf` error explicitly and never reach disk or index keys.
- **INV-CAT-02 (Order-Preserving Key Equivalence)**: Memcomparable key byte order under `bytes.Compare` matches SQL 3VL comparison semantics identically (`-0.0 == +0.0`, `BINARY` UTF-8 order, `NULLS FIRST` on `ASC`, bitwise inversion yielding `NULLS LAST` on `DESC`).
- **INV-CAT-03 (Permanent Monotonic Physical Slots)**: Every column in a table's **Catalog** entry receives a permanently monotonic, never-reused **Physical Slot** (≤ 1,024 per table lifetime). Decoding a historical row with `record.ColumnCount <= col.PhysicalSlot` synthesizes its immutable scalar `AddDefault` (or `NULL`); tombstoned `DROP COLUMN` slots are skipped via `VarintLenHeader` on read and written as 1-bit `NULL` (0 payload bytes) on subsequent writes.
- **INV-CAT-04 (Immutable Numeric Addressing)**: **Catalog** storage roots and **Secondary Indexes** bind to `(TableID, PhysicalSlot)` rather than string names, keeping `RENAME TABLE`/`RENAME COLUMN` $O(1)$ metadata-only. `DROP COLUMN` forbids removing PK columns, indexed columns, or the last visible column (`ErrConstraint`).

*Verified by*: Property tests, `sqllogictest`, `modernc.org/sqlite` differential oracle (5-point normalization), and `Verify()`.

---

## 5. Query Execution & External Contracts

- **INV-QRY-01 (Bounded Blocking Memory & Temp Cleanup)**: `ORDER BY`, `GROUP BY`, `DISTINCT`, and `Hash Join` obey `WorkMem` (default `16MB`), spilling via the unified **VFS** External Merge-Sort engine and deleting all temp files on **Operator** `Close()`.
- **INV-QRY-02 (SchemaVersion Plan Freshness)**: Prepared statements tag cached **Physical Plans** with `SchemaVersion` and transparently re-bind/re-plan whenever `SchemaVersion` advances.
- **INV-API-01 (Honest Driver & Context Contract)**: `database/sql/driver` accepts only `LevelDefault`, `LevelSnapshot`, and `LevelRepeatableRead`, strictly enforces `opts.ReadOnly`, rejects `time.Time`, raw SQL `BEGIN`/`COMMIT`/`ROLLBACK` (supported only on native stateful `*fycsql.Conn`), multi-statement SQL strings, and unknown DSN parameters; `driver.Rows.Next` checks `ctx.Err()`.
- **INV-API-02 (Zero Third-Party Runtime Dependencies)**: All non-test packages (`.`, `./driver`, `./vfs`, `./cmd/...`, `./internal/...`) import strictly Go stdlib, enforced in CI via `go list -deps`.

---

## 6. High-Risk Cross-Layer Coupling Invariants

These four cross-layer invariants span multiple packages and must be explicitly tested during implementation:

1. **Multi-Version Page 1 Resolution (`INV-TX-01` × `INV-WAL-03` × `INV-CAT-04`)**:
   Page 1 (`PageID = 1`) is versioned through **WAL** **Frames** like any other page. An active **Snapshot** pinned at `snapTS` must resolve its `CatalogRootPageID`, `SchemaVersion`, and `TotalPageCount` at `CommitTS <= snapTS` (or via an immutable `SchemaVersion`-keyed **Catalog** snapshot) — never from a single mutable engine-wide field overwritten by later DDL commits.
2. **Freelist Reuse Depends on Checkpoint Snapshot Watermark (`INV-WAL-03` × `INV-STR-04`)**:
   Popping a freed `PageID` from the **Freelist** (e.g. after `DROP TABLE A`) and overwriting it for `Table B` at `CommitTS = 12` is MVCC-safe for an active **Snapshot** at `CommitTS = 10` *only* because: (a) the overwrite is appended as a new **WAL** **Frame** (`CommitTS = 12`), and (b) **Checkpoint** strictly forbids copying `CommitTS > minActiveSnapshotTS` into `<db>`. Violating `INV-WAL-03` immediately causes silent cross-table corruption for older readers.
3. **Logical-Row Bijection Under Zero-Rewrite `ALTER TABLE` (`INV-STR-05` × `INV-CAT-03`)**:
   When `ALTER TABLE ADD COLUMN c DEFAULT 10` commits without rewriting existing rows and `CREATE INDEX idx_c ON t(c)` follows, `idx_c` contains `encode(10, RowKey)` while untouched on-disk rows still have `ColumnCount <= c.PhysicalSlot`. `Verify()` must check **Clustered Table** ↔ **Secondary Index** bijection over the **Catalog-decoded logical row** (synthesizing `AddDefault` and skipping tombstoned **Physical Slots**), not raw physical slot counts.
4. **Indeterminate Commit Outcome After `Sync()` Failure (`INV-WAL-01` × `INV-WAL-05`)**:
   If `walFile.WriteAt` succeeds and `walFile.Sync()` subsequently fails, the engine enters terminal **Fail-Stop** and preserves `<db>-wal` on `Close()`. Recovery tests (`ErrorFS`) and callers must never assert guaranteed rollback in this window: if the OS page cache survives process restart without power loss, the transaction may recover cleanly; if power is lost, it is discarded. The invariant is **atomic prefix preservation** (either completely committed or completely absent), never torn state.
