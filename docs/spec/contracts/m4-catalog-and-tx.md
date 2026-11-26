# M4 Contract — `internal/catalog` & `internal/tx`: Transactional Catalog, Sequences, MVCC/OCC, Commit Gate, and Whole-DB Verify

> **Parent Index**: [v1-module-contracts.md](../v1-module-contracts.md) · **Handoff**: [v1-architecture-handoff.md](../v1-architecture-handoff.md) · **Invariants**: [v1-invariants.md](../v1-invariants.md) · **Glossary**: [GLOSSARY.md](../../../GLOSSARY.md)
> **Related ADRs**: [ADR-0001](../../adr/0001-mvcc-occ-snapshot-isolation.md), [ADR-0005](../../adr/0005-clustered-btree-slotted-page-and-memcomparable-keys.md), [ADR-0006](../../adr/0006-query-pipeline-catalog-volcano-and-spill.md), [ADR-0009](../../adr/0009-metadata-only-alter-table-and-physical-slots.md)
> **Navigation**: ← [M3 — `internal/btree`](./m3-btree.md) · Next → [M5 — `internal/sql`](./m5-sql.md)

---

## Part A — `internal/catalog`

### 1. Depth & Responsibility

`internal/catalog` owns self-hosted metadata stored in fixed Page 2 (`_fycsql_catalog`), immutable `SchemaVersion`-keyed `*Schema` caching, `(TableID, PhysicalSlot)` numeric addressing, $O(1)$ metadata-only `ALTER TABLE`, the **single canonical logical-row encoder/decoder** shared across reads/projections/index-builds/`Verify()`, and per-table atomic `Internal RowID`/`AUTOINCREMENT` sequence allocators with persisted committed high-water marks.

- **Package Dependencies**: `internal/btree`, `internal/errs`, `internal/val`, Go stdlib only (**zero knowledge of `internal/tx` or `internal/pager`**).

### 2. Interface Surface

> Signatures below define the behavioral contract, not production code. All exported types are concrete structs.

```text
const ProvisionalIDMask uint64 = 1 << 63 // Distinguishes uncommitted private CREATE IDs from committed TableID/IndexID

type Manager struct { ... } // Owns SchemaVersion cache & per-table committed atomic sequence counters

func Open(r btree.PageReader, schemaVersion, nextTableID uint64) (*Manager, error)
func (m *Manager) SchemaAt(r btree.PageReader, schemaVersion uint64) (*Schema, error)
func (m *Manager) PublishSchema(schemaVersion uint64, s *Schema, newSeqs []SeqHighWater) // Initializes & raises live counters
func (m *Manager) NextRowID(tableID uint64) (int64, error)   // Committed tables only
func (m *Manager) NextAutoInc(tableID uint64) (int64, error) // Committed tables only
func (m *Manager) ObserveCommittedID(tableID uint64, id int64) // Atomic max before publication

// Immutable Schema snapshots (safe for lock-free concurrent reads across readers & plan cache)
type Schema struct { Version uint64; NextTableID uint64; ... }
func (s *Schema) TableByName(name string) (*TableDef, bool)
func (s *Schema) TableByID(id uint64) (*TableDef, bool)
func (s *Schema) Tables() []*TableDef

// Pure DDL overlay transforms (return new private *Schema + CatalogOp; never mutate receiver)
// Private CREATE uses tx-local provisional IDs and RootPageID = 0; CommitGate assigns real TableID & RootPageID on commit.
func (s *Schema) WithCreateTable(spec CreateTableSpec, provisionalTableID uint64) (*Schema, CatalogOp, error)
func (s *Schema) WithDropTable(name string) (*Schema, CatalogOp, []uint32 /*committedFreedRoots*/, error)
func (s *Schema) WithCreateIndex(spec CreateIndexSpec, provisionalIndexID uint64) (*Schema, CatalogOp, error)
func (s *Schema) WithDropIndex(name string) (*Schema, CatalogOp, uint32 /*committedFreedRoot*/, error)
func (s *Schema) WithAlterTable(spec AlterTableSpec) (*Schema, CatalogOp, error)

// Single canonical logical-row mapping (shared by reads, projections, index builds, and Verify)
type TableDef struct {
    ID                uint64 // Provisional (ProvisionalIDMask) until committed
    Name              string
    RootPageID        uint32 // 0 while privately created & uncommitted; stable PageID once committed
    PhysicalSlotCount int    // Count of allocated slots (0 <= col.PhysicalSlot < PhysicalSlotCount <= 1024)
    ...
}
func (t *TableDef) DecodeLogicalRow(raw []byte, dst []val.Value) error
func (t *TableDef) ProjectLogicalRow(raw []byte, visibleCols []int, dst []val.Value) error
func (t *TableDef) EncodeLogicalRow(dst []byte, visibleVals []val.Value) ([]byte, error)
func (t *TableDef) EncodeRowKey(dst []byte, visibleVals []val.Value) ([]byte, error)
func (t *TableDef) EncodeIndexKey(dst []byte, idx *IndexDef, visibleVals []val.Value, rowKey []byte) ([]byte, error)
func (t *TableDef) VerifyRowRecord(raw []byte, visibleDst []val.Value) error // Checks ColumnCount <= PhysicalSlotCount, visible NOT NULL & types

// Commit-time materialization inside Commit Gate (resolves provisional IDs -> real TableIDs & allocated RootPageIDs)
func ApplyCatalogOps(w btree.PageWriter, baseSchema *Schema, ops []CatalogOp, seqMarks []SeqHighWater, nextTableID *uint64) (*Schema, []RootBinding, error)
```

### 3. Behavioral Contracts & Error Modes

1. **Permanent Monotonic Physical Slots & Metadata-Only `ALTER TABLE` (`INV-CAT-03`, `INV-CAT-04`, `ADR-0009`)**:
   - Every column receives a permanently monotonic, never-reused `PhysicalSlot` with invariant `0 <= col.PhysicalSlot < tbl.PhysicalSlotCount <= 1024`.
   - `ALTER TABLE ADD COLUMN`:
     - Allocates `PhysicalSlot = tbl.PhysicalSlotCount` and increments `PhysicalSlotCount` (failing with `errs.ErrConstraint` on the 1,025th slot even if prior columns were dropped).
     - `DEFAULT` is restricted to constant scalar literals (applying ADR-0002 `INTEGER → REAL` widening where declared `REAL`); `ADD COLUMN ... NOT NULL` MUST supply a non-`NULL` `DEFAULT` (`errs.ErrConstraint` otherwise).
     - Rejects `ADD COLUMN ... PRIMARY KEY` or `UNIQUE` (`errs.ErrConstraint`).
   - `ALTER TABLE DROP COLUMN`: tombstones the column's `PhysicalSlot` without touching table/index pages; forbids dropping PK columns, indexed columns, or the last visible column (`errs.ErrConstraint`).
   - `ALTER TABLE RENAME TABLE` / `RENAME COLUMN`: metadata-only ($O(1)$) because Secondary Indexes bind to `(TableID, []PhysicalSlot)`.
   - **Logical Row Encoding & Decoding**:
     - New and rewritten rows encode `record.ColumnCount == tbl.PhysicalSlotCount` (including trailing explicit `NULL`s and 1-bit `NULL` / 0 payload bytes for dropped slots).
     - On decode, a present `NULL` bitmap bit (`col.PhysicalSlot < record.ColumnCount`) yields `NULL` even when `AddDefault` exists; `AddDefault` is synthesized **only** when `record.ColumnCount <= col.PhysicalSlot`.
     - `VerifyRowRecord` rejects `record.ColumnCount > tbl.PhysicalSlotCount` (`errs.ErrCorruption`), skips tombstoned slots, checks declared types, and rejects `NULL` in any visible `NOT NULL` column (`errs.ErrCorruption`).
2. **Provisional Private DDL Identities (`INV-TX-03`)**:
   - Privately created tables and indexes receive tx-local provisional IDs (`ProvisionalIDMask | localSeq`) and `RootPageID = 0`, never touching `Page1Header.NextTableID`, physical page allocation, or `catalog.Manager`'s shared sequence map during execution.
   - Only inside the serialized `CommitGate` does `ApplyCatalogOps` assign real monotonic `TableID`s from `NextTableID` and allocate stable `RootPageID`s via `btree.InitRoot(stagePageWriter)`.
3. **Versioned Schema Isolation & Persisted Sequence High-Water Marks (`INV-TX-03`, `ADR-0006`)**:
   - `SchemaAt` never returns a newer schema for an older `Snapshot`; uncached historical versions load directly from that Snapshot's Page 2.
   - `NextRowID`/`NextAutoInc` increase monotonically in issue order (`errs.ErrOverflow` on `math.MaxInt64` exhaustion). At commit, `CommitGate` computes `max(prevHighWater, committedIDs...)` including explicit `INTEGER AUTOINCREMENT` keys (even if the largest row is later deleted), raises live counters via `PublishSchema`/`ObserveCommittedID` before visibility publication, and persists the mark into `_fycsql_catalog` within the same WAL transaction outside logical conflict tracking. Reopen always resumes strictly above the recovered committed mark.

---

## Part B — `internal/tx`

### 1. Depth & Responsibility

`internal/tx` concentrates all concurrency control, transaction isolation, RYOW merging, 64MB memory admission, statement atomicity, OCC conflict tracking, single-cutover FIFO Group Commit batching, bidirectional Schema Barrier, zero-alloc `pager → btree` adapters, and whole-database `Verify()` in one deep module.

- **Package Dependencies**: `internal/pager`, `internal/btree`, `internal/catalog`, `internal/errs`, `internal/val`, Go stdlib only (**never imports `internal/sql`**).
- **Compile-Time Adapter Assertions**: `internal/tx` defines thin zero-alloc `snapshotPageReader` (wrapping `*pager.Snapshot`) and `stagePageWriter` (wrapping `*pager.MemberStage`) with mandatory compile-time assertions:
  `var _ btree.PageReader = (*snapshotPageReader)(nil)` and `var _ btree.PageWriter = (*stagePageWriter)(nil)`.

### 2. Interface Surface

> Signatures below define the behavioral contract, not production code. All exported types are concrete structs.

```text
type Manager struct { ... } // Owns TxRegistry, FIFO CommitGate & bounded OCC ConflictHistory

func NewManager(p *pager.Pager, cat *catalog.Manager, opts Options) *Manager
func (m *Manager) Begin(ctx context.Context, readOnly bool) (*Tx, error)
func (m *Manager) Verify(ctx context.Context) error
func (m *Manager) Stats() TxStats
func (m *Manager) StopAdmission()                     // Non-blocking cutoff; rejects new Begin/Commit with ErrDatabaseClosed
func (m *Manager) Drain(ctx context.Context) error    // Waits for active txs/queued commits; cancels unsealed queue on ctx expiry

type Tx struct { ... }
func (t *Tx) ReadOnly() bool
func (t *Tx) SnapshotTS() pager.CommitTS
func (t *Tx) Schema() *catalog.Schema       // Committed schema or tx-private DDL overlay
func (t *Tx) HasPrivateDDL() bool           // Signals internal/sql to bypass shared plan cache
func (t *Tx) NextRowID(tableID uint64) (int64, error)   // Uses tx-private counter if ProvisionalIDMask, else catalog.Manager
func (t *Tx) NextAutoInc(tableID uint64) (int64, error) // Uses tx-private counter if ProvisionalIDMask, else catalog.Manager

// RYOW Reads (merging Snapshot btree.Cursor with WriteSet sorted overlay; RootPageID==0 reads WriteSet only)
func (t *Tx) GetRow(tableID uint64, rowKey []byte, dst []byte) (rawRecord []byte, found bool, err error)
func (t *Tx) ScanTable(tableID uint64, bounds KeyBounds, reverse bool) (*RowIter, error)
func (t *Tx) ScanIndex(indexID uint64, bounds KeyBounds, reverse bool) (*RowIter, error)

type RowIter struct { ... }
func (it *RowIter) Next() bool
func (it *RowIter) Key() []byte
func (it *RowIter) ValueLen() int
func (it *RowIter) Value(dst []byte) ([]byte, error)
func (it *RowIter) ProjectCurrent(tbl *catalog.TableDef, visibleCols []int, dst []val.Value) error
func (it *RowIter) ProjectCurrentBounded(tbl *catalog.TableDef, visibleCols []int, maxInlineBytes int, dst []val.Value) (oversizedCols []int, err error)
func (it *RowIter) StreamCurrentValue(w io.Writer) error                         // Bounded streaming for full oversized row record
func (it *RowIter) StreamColumnValue(tbl *catalog.TableDef, visibleCol int, w io.Writer) error // Bounded streaming of one oversized TEXT/BLOB column
func (it *RowIter) Err() error
func (it *RowIter) Close()

// Statement Atomicity & Table/Index-Aware Logical Mutation Staging
func (t *Tx) BeginStatement() (*StatementDelta, error)
func (t *Tx) CommitStatement(d *StatementDelta) error // Transfers ownership & budget delta atomically into WriteSet
func (t *Tx) DiscardStatement(d *StatementDelta)      // Releases 100% of delta budget; WriteSet & private schema untouched

func (d *StatementDelta) InsertRow(tbl *catalog.TableDef, rowKey []byte, visibleVals []val.Value) error
func (d *StatementDelta) UpdateRow(tbl *catalog.TableDef, oldKey, newKey []byte, oldVals, newVals []val.Value) error
func (d *StatementDelta) DeleteRow(tbl *catalog.TableDef, rowKey []byte, oldVals []val.Value) error
func (d *StatementDelta) StageCreateTable(spec catalog.CreateTableSpec) (*catalog.TableDef, error)
func (d *StatementDelta) StageDropTable(name string) error
func (d *StatementDelta) StageAlterTable(spec catalog.AlterTableSpec) error
func (d *StatementDelta) StageCreateIndex(ctx context.Context, spec catalog.CreateIndexSpec) error // Backfills from RYOW rows, checks ctx.Err() every ~128 rows, checks uniqueness & budget
func (d *StatementDelta) StageDropIndex(name string) error

// Termination
func (t *Tx) Commit(ctx context.Context) error
func (t *Tx) Rollback() error
```

### 3. Behavioral Contracts & Error Modes

1. **Atomic `Begin` & OCC History Protection (`INV-TX-01`, `INV-TX-02`)**:
   - For read-only transactions, `Begin` calls `pager.AcquireSnapshot()` directly (`SnapshotRegistry`) and loads `catalog.SchemaAt(snap, snap.Header().SchemaVersion)`.
   - For read-write transactions, `Begin` acquires `TxRegistry` **before** calling `pager.AcquireSnapshot()` and registers `snap.CommitTS()` in the active RW writer set before releasing `TxRegistry` (matching lock order `CommitGate → WriteSerializeMu → TxRegistry → SnapshotRegistry`). Because CommitGate updates and prunes `ConflictHistory` under `TxRegistry`, a newly begun writer can **never** have concurrent commit history pruned before its watermark is registered.
   - If `catalog.SchemaAt` fails after snapshot acquisition, `Begin` immediately unwinds **both** the `TxRegistry` watermark and `snap.Release()`.
2. **Statement Delta Admission, Ownership & Intra-Statement Visibility (`INV-TX-04`, `INV-TX-06`, `ADR-0001`)**:
   - **Input ownership**: `StatementDelta` pre-reserves bytes from the transaction's single shared 64MB `MaxWriteSetSize` counter *before* copying caller-provided key/value slices into its own buffers, so Volcano operators may reuse row buffers immediately.
   - **Reservation transfer on merge**: `CommitStatement(d)` transfers `d`'s retained entries directly into `WriteSet` (zero extra copy) and credits back the bytes of any superseded `WriteSet` entries so capacity is never double-charged. `DiscardStatement(d)` releases 100% of `d`'s reserved bytes and leaves prior rows, secondary indexes, and private `*catalog.Schema` completely intact.
   - **Halloween-proof source scans vs intra-statement uniqueness**: Source scans (`GetRow`/`ScanTable`/`ScanIndex`) observe strictly `Snapshot + committed WriteSet` (excluding the active `StatementDelta`), while `InsertRow`/`UpdateRow` uniqueness checks inspect **both** `Snapshot + WriteSet` **and** earlier rows already staged in the **same `StatementDelta`**, catching multi-row intra-statement duplicates immediately with `ErrPrimaryKeyViolation`/`ErrUniqueViolation`.
3. **OCC Conflict History & Bidirectional Schema Barrier (`INV-TX-02`, `ADR-0001`)**:
   - Tracks last mutation `CommitTS` for `(TableID, RowKey)` and `(IndexID, non-NULL unique prefix)` (including deletions and both old/new sides of key changes) plus per-table last data-change `CommitTS`.
   - Retains all entries newer than the oldest active RW writer's `snapTS`. An intervening write restoring the original value still returns retryable `errs.ErrConflict`.
   - **Bidirectional Schema Barrier**: Writers bound to an obsolete `SchemaVersion` fail with retryable `errs.ErrSchemaChanged`; DDL transactions scanning a source table (including `CREATE UNIQUE INDEX`) validate the table's last data-change `CommitTS` against `tx.SnapshotTS()` (including earlier batch members) and return `errs.ErrConflict` on concurrent source writes.
4. **Single-Linearization FIFO Group Commit Protocol (`INV-WAL-01`, `INV-WAL-05`, `ADR-0001`, `ADR-0007`)**:
   - Empty write transactions return without acquiring `pager.WriteSession` or allocating a `CommitTS`.
   - **Single Gate-Owned Request State Machine (`gateMu`)**: Each queued commit request holds a state in `{queued, staged, withdrawn, sealed}` transitioned **exclusively** under `gateMu`.
     - While waiting, a caller goroutine selects on `req.done` and `ctx.Done()`. Upon `ctx.Done()`, the caller invokes `req.tryWithdraw()` under `gateMu`: if state is `queued` or `staged`, `tryWithdraw()` flips it to `withdrawn` and returns immediately with `ctx.Err()`; if the Gate has already flipped state to `sealed`, `tryWithdraw()` returns `false` and the caller MUST wait on `req.done` for the real commit outcome.
     - Because both caller withdrawal and Gate batch sealing serialize through `gateMu`, there is **one atomic linearization point** with zero race window between cancellation checking and sealing.
   - **Phase 1 — FIFO Provisional Staging**: Under `CommitGate` + `WriteSerializeMu` (`pager.BeginWrite`), queued transactions are validated and staged in FIFO order onto cumulative `pager.MemberStage` state, recording each FIFO entry's distinct physical `physIdx` (`>= 0` for physically staged members; `-1` for provisional rejections):
     - Conflicts against already-committed history are final, **but conflicts/schema barriers caused by an earlier staged member `A` in the current batch are held as provisional (`physIdx = -1`, contributing zero physical frames)** so `B` is NOT completed early while `A` can still withdraw.
     - If `MemberStage` returns `errs.ErrBatchFull` while physical `memberIdx > 0`, `ws.DiscardMember()` is called, this member stays at the head of the queue for the next batch, and the current batch proceeds to Phase 2. If `memberIdx == 0` returns `errs.ErrTxTooLarge`, discard and fail that member immediately.
     - Any preparation error on member `k` discards only member `k` (`ws.DiscardMember()`) and its provisional logical deltas without harming earlier or later members.
   - **Phase 2 — Single Atomic Withdrawal-vs-Seal Cutover (`gateMu`)**: Under `gateMu`, inspect every FIFO entry in the batch:
     - If any accepted entry at FIFO index `i` (with physical index `physIdx >= 0`) has already transitioned to `withdrawn`: release `gateMu`, call `ws.RollbackTo(physIdx)` (targeting the exact physical member index, never conflating FIFO index with physical index when provisional rejections precede it), discard all provisional logical deltas (conflict history, schema/DDL bindings, sequence high-water deltas, and provisional rejection decisions for all FIFO entries `> i`), and re-run Phase 1 from FIFO index `i + 1` onward!
     - If zero accepted entries are `withdrawn`, **under that exact same `gateMu` critical section**, atomically transition every accepted physically-staged entry (`physIdx >= 0`) to `sealed` (provisional rejections remain held pending Phase 3/4 outcome) and release `gateMu`.
   - **Phase 3 — Physical Durable Append (`ws.AppendAndSync()`)**:
     - Executes the one-time pre-frame new-WAL header write + `Sync()` + `SyncDir` (if WAL is new/empty, in **all** sync modes), then writes batch frames and performs mode-dependent sync (`SyncFull`: foreground `walFile.Sync()`; `SyncNormal`: append + async coalesced sync; `SyncOff`: append only — see [M2 §3.3–3.4](./m2-pager.md#33-pre-allocation-physical-batch-admission-stage-lifetime--two-phase-flush-inv-wal-01-adr-0004)).
     - On pre-frame failure (`ErrIO`, before any member frame is written): discard all provisional logical deltas and wake every batch member (sealed & provisional-conflict) and unsealed queued writers with known failure `ErrIO` / `ErrFailStop`.
     - On frame-append or foreground-sync failure (`errs.ErrCommitIndeterminate`): discard all provisional logical deltas (zero schema/sequence/conflict-history updates), wake every **sealed physically-appended member** (`physIdx >= 0`) with non-retryable `ErrCommitIndeterminate`, and wake every **provisional-conflict member** (`physIdx == -1`, which contributed zero physical frames) and **still-queued unsealed writer** with **known failure `ErrFailStop`**!
   - **Phase 4 — Atomic Logical + Physical Publication**:
     - Under `TxRegistry`:
       1. Publish new committed `*catalog.Schema` and raise live sequence allocators (`catalog.PublishSchema` / `ObserveCommittedID`).
       2. Commit provisional `ConflictHistory` entries and prune entries `<= minActiveRWWriterSnapTS`.
       3. Call `ws.Publish(durableBatch)` (acquiring `SnapshotRegistry` → `WAL/BufferPool`) to install frames and advance `latestCommitTS` atomically so any new `Snapshot` observes consistent Page 1, `SchemaVersion`, sequence counters, and committed pages.
     - Release locks and **immediately wake every caller of this batch** (both succeeded sealed members and provisional-conflict members whose conflict against sealed member `A` is now final) even if subsequent requests are queued.
5. **Whole-Database `Verify(ctx)` (`INV-STR-04`, `INV-STR-05`, `INV-CAT-03`)**:
   - Acquires one `*pager.Snapshot`.
   - Audits `snap.VerifyFreelist()`, runs `btree.VerifyTree` on Page 2 (`_fycsql_catalog`) and every committed table and index, decodes and validates every table row via `TableDef.VerifyRowRecord` (`ColumnCount <= PhysicalSlotCount`, visible `NOT NULL`, declared types, synthesized `AddDefault`, tombstones skipped), verifies 1-to-1 Clustered Table ↔ Secondary Index bijection (exact secondary entry, empty cell value, count equality, unique non-NULL prefix uniqueness), and verifies exact whole-file page conservation:
     `{Page 1} ⊔ CatalogTree ⊔ UserTables ⊔ SecondaryIndexes ⊔ OverflowChains ⊔ Freelist == {1 .. TotalPageCount}`.

---

## 4. Resource Ownership & Lifecycle

- `*catalog.Schema` and its `TableDef`/`IndexDef`/`ColumnDef` structs are **strictly immutable**; GC-managed once older readers/plans release them.
- `*catalog.Manager` owns the `SchemaVersion → *Schema` cache and committed per-table atomic sequence counters for the database's open lifetime.
- `*tx.Tx` owns one `*pager.Snapshot`, its 64MB `WriteSet` budget, any tx-private sequence counters (for provisional tables), and its `TxRegistry` RW writer registration. `Commit()` and `Rollback()` release all owned resources idempotently (closing any dangling `*RowIter`s before releasing `Snapshot`); subsequent calls return `errs.ErrTxDone`.
- `*tx.RowIter` owns a `btree.Cursor` (≤ 1 pinned leaf page) and borrows the tx's snapshot/WriteSet; `Close()` unpins immediately.
- `Manager.StopAdmission()` is non-blocking; `Manager.Drain(ctx)` waits for active transactions/batches within `ctx`, then aborts unsealed queued commits without touching an in-flight `AppendAndSync`.

---

## 5. Test Surface & Verification Contract

- Tested through `tx.Manager` (`Begin`, `StatementDelta`, `Commit`, `Rollback`, `Verify`, `StopAdmission`, `Drain`) and `catalog.Manager` over `pager.Pager` + `MemFS`/`CrashFS`:
  - **OCC & `Begin` race**: Interleave `Begin(rw)` with concurrent commits & conflict-history pruning to verify zero missed conflicts; test unwind when `SchemaAt` fails; test same-leaf disjoint updates, unique-prefix collisions, delete/reinsert, multi-row intra-statement duplicate, and half-way `stmtCtx` cancellation inside `StageCreateIndex` leaving prior rows/indexes/schema intact.
  - **Group Commit cutover & rollback**:
    - Exercise caller cancellation racing against the `gateMu` seal point (verifying strict linearizability: either withdrawn before append or sealed and receiving actual commit outcome).
    - Withdraw member `A` before append when dependent `B` provisionally conflicted with `A` (assert `B` revalidates and succeeds); also test a provisional rejection preceding a withdrawn staged member to verify `RollbackTo(physIdx)` targets the exact physical suffix.
    - Fail `A`'s append while `B` is held as a provisional conflict (assert `A` gets `ErrCommitIndeterminate` and `B` wakes promptly with known `ErrFailStop`, zero hang).
    - Fail a middle batch member after allocator mutations and verify its successors succeed with clean Freelist/Page 1; repeat M3 allocation-failure rollback against real `MemberStage`.
  - **Private `CREATE` & sequences**: Concurrent `CREATE TABLE` transactions, insert into privately created table, rollback (zero shared sequence pollution), failed `CREATE` member followed by successful `CREATE`; delete max `RowID`/`AUTOINCREMENT`, reopen, and verify persisted high-water mark.
  - **Schema evolution & cross-layer invariants**: 1,024th (allowed) and 1,025th (`ErrConstraint`) `PhysicalSlotCount` allocations across `DROP`/re-add; trailing explicit `NULL` vs absent slot (`AddDefault`); assert `ALTER TABLE` modifies zero table/index pages; run concurrent DML vs `CREATE UNIQUE INDEX`, reopen, and run `Verify()` to prove recovered table/index bijection; run old `Snapshot` across `DROP TABLE` + `PageID` reuse + partial Checkpoint backfill.
  - **Isolation matrix**: Hermitage SI (`G0`, `G1a/b/c`, `OTV`, `PMP`, `P4`, `G-single` absent; `G2-item`/`G2` documented) + `testing/synctest`.
- **Invariants Enforced**: `INV-TX-01`–`06`, `INV-STR-04`–`05`, `INV-CAT-03`–`04`.
