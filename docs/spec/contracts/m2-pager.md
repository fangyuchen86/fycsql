# M2 Contract — `internal/pager`: MVCC Page Store, Buffer Pool, WAL, Checkpoint, and Recovery

> **Parent Index**: [v1-module-contracts.md](../v1-module-contracts.md) · **Handoff**: [v1-architecture-handoff.md](../v1-architecture-handoff.md) · **Invariants**: [v1-invariants.md](../v1-invariants.md) · **Glossary**: [GLOSSARY.md](../../../GLOSSARY.md)
> **Related ADRs**: [ADR-0001](../../adr/0001-mvcc-occ-snapshot-isolation.md), [ADR-0003](../../adr/0003-page-level-wal-and-immutable-buffer-pool.md), [ADR-0004](../../adr/0004-wal-format-checkpointing-and-recovery.md), [ADR-0007](../../adr/0007-go-public-api-sql-driver-and-error-semantics.md), [ADR-0010](../../adr/0010-page1-header-versioning-stats-cli-and-package-layout.md)
> **Navigation**: ← [M1 — `vfs`](./m1-vfs.md) · Next → [M3 — `internal/btree`](./m3-btree.md)

---

## 1. Depth & Responsibility

`internal/pager` is the deepest storage module in fycsql. It encapsulates:

- Canonical `<db>` + `<db>-wal` locked file-pair bootstrap (using `vfs.FS.PublishExclusive`) and torn Page 1 recovery.
- 128-byte self-verifying Page 1 header, 16-byte Page 2+ common header constants & CRC32C sealing/verification, and empty Leaf Page 2 initialization on brand-new database creation (zero `pager → btree` import).
- Bounded `noscan` slab Buffer Pool (64MB default) with sharded atomic-CLOCK eviction and `HintSeqScan`.
- `SnapshotRegistry` and physical read-source generation lifetimes.
- WAL wire format (32B header + 32B frame headers, generation salts, rolling CRC32C, `FrameIndex`, retained last valid offset & rolling CRC).
- Transactional Freelist trunk/leaf allocation and structural audit.
- Four-step barriered Checkpointing (`Passive`, `Restart`, `Truncate`).
- Pre-allocation byte-bounded Group Commit physical staging, oversized single-transaction chunked streaming, two-phase durable append vs visibility publication, and terminal **Fail-Stop** state across every durability barrier.

All internal components (`BufferPool`, `WAL`, `FrameIndex`, `SnapshotRegistry`, `Freelist`, `Checkpointer`, `Recovery`) are **concrete unexported structs** inside `internal/pager` — never separate packages or single-adapter Go interfaces.

- **Package Dependencies**: `vfs`, `internal/errs`, Go stdlib only (zero dependency on `internal/val`, `internal/btree`, or `internal/catalog`).

---

## 2. Interface Surface

> Signatures below define the behavioral contract, not production code. All exported types are concrete structs.

```text
type PageID = uint32       // 1-based; Page 1 = DB header, Page 2 = Catalog root
type CommitTS uint64       // Monotonic; 0 = initial empty database
type ReadHint = uint8      // HintRandom (0) | HintSeqScan (1)
type SyncMode uint8        // SyncFull | SyncNormal | SyncOff
type CheckpointMode uint8  // CheckpointPassive | CheckpointRestart | CheckpointTruncate

const (
    PageTypeLeaf     uint8 = 1
    PageTypeInternal uint8 = 2
    PageTypeOverflow uint8 = 3
    PageTypeFreelist uint8 = 4
)

type Page1Header struct {
    FormatVersion      uint32
    MinReaderVersion   uint32
    PageSize           uint32
    TotalPageCount     uint32
    CatalogRootPageID  PageID // Fixed PageID 2
    FreelistHeadPageID PageID
    FreelistPageCount  uint32
    Flags              uint32
    SchemaVersion      uint64
    LastCommitTS       CommitTS
    NextTableID        uint64
}

type Pager struct { ... }

func Open(fs vfs.FS, path string, opts Options) (*Pager, error)
func (p *Pager) Close(ctx context.Context) error
func (p *Pager) AcquireSnapshot() (*Snapshot, error)
func (p *Pager) BeginWrite(ctx context.Context) (*WriteSession, error)
func (p *Pager) Checkpoint(ctx context.Context, mode CheckpointMode) (CheckpointStats, error)
func (p *Pager) Stats() Stats
func (p *Pager) PageSize() int

type Snapshot struct { ... }
func (s *Snapshot) PageSize() int
func (s *Snapshot) CommitTS() CommitTS
func (s *Snapshot) Header() Page1Header
func (s *Snapshot) PinPage(id PageID, hint ReadHint) (PagePin, error)
func (s *Snapshot) PinPageToken(id PageID, hint ReadHint) (data []byte, token uint64, err error) // 0-alloc seam helper
func (s *Snapshot) UnpinToken(token uint64)                                                       // Idempotent single-release
func (s *Snapshot) VerifyFreelist() (FreelistAudit, error)
func (s *Snapshot) Release()

type PagePin struct { ... } // Single-release handle; Unpin() is idempotent (double-Unpin is a safe no-op)
func (pin PagePin) Bytes() []byte // Immutable slice valid until Unpin()
func (pin *PagePin) Unpin()

type WriteSession struct { ... } // Serialized physical WAL writer (holds WriteSerializeMu)
func (ws *WriteSession) BaseHeader() Page1Header
func (ws *WriteSession) BeginMember(commitTS CommitTS) *MemberStage
func (ws *WriteSession) CommitMember() (memberIdx int, err error) // Seals Page 2+ CRCs, freezes member + Page 1 frame
func (ws *WriteSession) DiscardMember()                           // Reverts uncommitted member pages/allocator/Page 1
func (ws *WriteSession) RollbackTo(memberIdx int)                 // Reverts suffix [memberIdx..] before append cutover
func (ws *WriteSession) AppendAndSync() (*DurableBatch, error)    // No cancelling ctx: called ONLY after Gate seals batch
func (ws *WriteSession) Publish(batch *DurableBatch)              // Atomically publishes frames & advances latestCommitTS
func (ws *WriteSession) Close()                                   // Releases WriteSerializeMu & un-published staging buffers

type MemberStage struct { ... }
func (ms *MemberStage) PageSize() int
func (ms *MemberStage) PinPageToken(id PageID, hint ReadHint) (data []byte, token uint64, err error)
func (ms *MemberStage) UnpinToken(token uint64)
func (ms *MemberStage) MutatePage(id PageID) ([]byte, error)             // Pre-reserves staging bytes; ErrBatchFull or ErrTxTooLarge
func (ms *MemberStage) AllocPage(pageType uint8) (PageID, []byte, error) // Pre-reserves staging bytes; Freelist pop or extend
func (ms *MemberStage) FreePage(id PageID) error                         // Pre-reserves trunk page if needed; transactional Freelist push
func (ms *MemberStage) SetHeaderMeta(schemaVersion, nextTableID uint64)
func (ms *MemberStage) StagedBytes() int64
```

---

## 3. Behavioral Contracts & Error Modes

### 3.1 Locked Open, Atomic Bootstrap & Torn-Header Recovery (`INV-WAL-02`, `INV-WAL-04`, `INV-WAL-06`, `ADR-0003`, `ADR-0010`)
1. **Canonical Resolution & Lock-First**: Resolves `<db>` and `<db>-wal` under one canonical path. For existing files, acquires `OpenLockExclusive` on `<db>` **before** interpreting any bytes.
2. **Atomic Non-Clobbering First Creation & Orphan-WAL Race Protection**:
   - When `<db>` is initially absent (`os.ErrNotExist`), checks `<db>-wal`:
     - Any non-`os.ErrNotExist` error from `Stat`/`Open` fails with `errs.ErrIO`.
     - If `<db>-wal` appears present, **retry opening `<db>` with `OpenReadWrite | OpenLockExclusive`** before declaring an orphan WAL (another opener may have just published `<db>` and `<db>-wal` between the two checks!). If `<db>` now exists, proceed with normal locked validation/recovery; if contention yields `vfs.ErrLocked`, return `errs.ErrDatabaseLocked`; only if `<db>` is **still** `os.ErrNotExist` while `<db>-wal` exists, fail with `errs.ErrCorruption`.
   - Creates a unique temporary file in the same directory (`OpenCreateExcl`), writes and seals Page 1 (`TotalPageCount = 2`, `CatalogRootPageID = 2`, `LastCommitTS = 0`, `SchemaVersion = 0`, `NextTableID = 1`) and a valid empty Leaf Page 2 (`PageType = 1`, `CellCount = 0`, `FreeEnd = PageSize` [stored as `0` on 64 KiB], `FragBytes = 0`, `LinkPage = 0`, valid `PageCRC32C`), calls `tmp.Sync()`, closes `tmp`, and calls `fs.PublishExclusive(tmpName, dbPath)`.
   - **Winner path**: calls `fs.SyncDir(dir)`, then opens `dbPath` with `OpenReadWrite | OpenLockExclusive` and runs full header validation.
   - **Loser path** (`os.ErrExist` from `PublishExclusive`): removes `tmpName` (best-effort), then opens the winning `dbPath` under `OpenLockExclusive` and runs standard existing-database validation and recovery.
   - **Partial-Open Unwind**: Any error during `Open` releases all already-acquired FDs, locks, and slabs in reverse order before returning.
3. **Read-Only Open (`WithReadOnly(true)`)**:
   - Opens `<db>` with `OpenReadOnly | OpenLockExclusive` (never creates `<db>`). If `<db>-wal` exists, opens it with `OpenReadOnly`; if `<db>-wal` is absent (`os.ErrNotExist`, e.g., cleanly closed DB), succeeds with no WAL overlay.
   - Leaves file contents and lengths completely untouched on open and close; rejects `BeginWrite` and `Checkpoint` with `errs.ErrReadOnly`.
4. **Header Validation & Torn Main Page 1 Bootstrap (`ADR-0010`)**:
   - Reads the first 128 bytes of `<db>` and checks: `Magic == "FYCSQL\x00\x00"`, `HeaderCRC32C` over `[0..123]`, `FormatVersion == 1 && MinReaderVersion <= 1` (`errs.ErrUnsupportedVersion` on intact unsupported version), power-of-2 `PageSize ∈ [4096, 65536]`, `TotalPageCount >= 2`, `CatalogRootPageID == 2`, and zero padding `[128..PageSize-1]`.
   - If the main Page 1 header is torn/corrupt, validates the paired WAL header independently (`Magic == "FYCWAL\x00\x00"`, `HeaderCRC32C`, `Version == 1` [`errs.ErrUnsupportedVersion` if intact unsupported version], power-of-2 `PageSize ∈ [4096, 65536]`), scans the committed WAL prefix, and requires a complete Page 1 image from the last valid committed transaction.
   - The candidate WAL Page 1 image MUST pass the **exact same full validation** as an intact main Page 1 (`Magic`, `HeaderCRC32C`, supported versions, zero padding `[128..PageSize-1]`, `PageSize` matching WAL header, and `TotalPageCount`/`LastCommitTS` matching the final valid commit marker):
     - If `Magic` and `HeaderCRC32C` are valid but `FormatVersion != 1 || MinReaderVersion > 1`, propagate `errs.ErrUnsupportedVersion` explicitly (NEVER downgrade to `ErrCorruption`).
     - If all checks pass, use this image as the logical Page 1 header with valid WAL pages as the overlay (including on read-only opens); otherwise return `errs.ErrCorruption`.
5. **WAL Scan & Tail Repair (`INV-WAL-02`)**:
   - Empty WAL (`size == 0`) or truncated/invalid initial header (`size <= 32` with invalid magic/CRC and zero frame bytes) is ignored when `<db>` is valid.
   - An **intact 32-byte WAL header with an unsupported `Version != 1`** (even with zero frames!) MUST return `errs.ErrUnsupportedVersion`.
   - An invalid WAL header followed by frame data (`size > 32`) returns `errs.ErrCorruption`.
   - Single-pass read-only scan validates generation salts, rolling CRC chain, monotonic `CommitTS`, and commit marker (`DBPageCount > 0`), indexing a transaction's frames only upon reaching its valid commit marker. Retains **both** `lastValidOffset` **and** `lastValidRollingCRC` of the last complete committed transaction.
   - Any read error within the measured WAL file length aborts `Open` with `errs.ErrIO`.
   - A read-write opener repairs any invalid trailing bytes (`Truncate(lastValidOffset)` + `Sync()`) before its first frame append, chaining subsequent frames from `lastValidRollingCRC`.

### 3.2 Immutable Frames, Pin Safety & Read-Source Lifetime (`INV-STR-01`, `INV-STR-02`, `INV-WAL-03`)
- Published Buffer Pool frames are **never mutated in place**. WAL frames are keyed by `(WALGen, FrameNo)`; main-file pages by `(MainFile, PageID)`.
- `PagePin` / `pinToken` enforces single release (repeated `Unpin()`/`UnpinToken()` is an idempotent no-op and can never underflow refcounts).
- `PinPage` / `PinPageToken` acquires `SnapshotRegistry` briefly to resolve the physical source and either pins the cached frame or increments the WAL generation's atomic `genRefCount` (or main-file read guard), then **releases `SnapshotRegistry` before `ReadAt` and before `BufferPool` shard insertion**.
- Main-file overwrites during Checkpoint exclude stale in-flight cache fills (via generation/epoch tag) and invalidate old main-file lookups while preserving already pinned bytes. Cache warming is optional when all slots are pinned.
- Every Page 2+ loaded from `<db>` verifies `[0..3] PageCRC32C` (`errs.ErrCorruption` on mismatch). `VerifyFreelist` checks trunk CRCs, excludes both reserved pages (`Page 1` header and permanent `Page 2` Catalog root) by enforcing `3 <= id <= TotalPageCount` on all trunk and free-leaf IDs, and verifies uniqueness, disjointness, and total count including trunks.

### 3.3 Pre-Allocation Physical Batch Admission, Stage Lifetime & Two-Phase Flush (`INV-WAL-01`, `ADR-0004`)
- **Pre-allocation byte budgeting**: `MemberStage` reserves physical staging bytes **before** copying or allocating any private page (`MutatePage`, `AllocPage`, Freelist trunk mutation, and the member's Page 1 frame).
  - If adding a page would exceed the physical batch staging budget **while earlier members (`memberIdx > 0`) are already staged**, returns `errs.ErrBatchFull` (signaling `tx.CommitGate` to `DiscardMember()`, flush earlier members first, and retry this member alone).
  - If a member is **alone** (`memberIdx == 0`) and its own private page images exceed the physical staging budget, returns `errs.ErrTxTooLarge` before any append occurs.
- **Stage slice expiration**: Any `[]byte` returned by `MemberStage.MutatePage` or `AllocPage` is valid **only** until `CommitMember()`, `DiscardMember()`, `RollbackTo()`, or `WriteSession.Close()`; callers MUST NOT retain or mutate slices past those calls.
- **Sealing & Encoding**: `CommitMember()` computes `[0..3] PageCRC32C` on every modified Page 2+ of the member and freezes its full Page 1 frame (`LastCommitTS`, roots, allocator state, `TotalPageCount`, `SchemaVersion`, `NextTableID`) with the commit marker (`DBPageCount > 0`) on its final frame. WAL encoding uses a bounded encoding/streaming buffer (separate from logical `WriteSet`): ordinary batches use one contiguous `WriteAt`; an oversized lone transaction streams in bounded chunks with the commit marker frame written last.
- **Pre-Frame WAL Header Barrier & Mode-Dependent Two-Phase Flush**:
  - Whenever `<db>-wal` is newly created (or empty) before the first frame write in **any** `SyncMode` (`SyncFull`, `SyncNormal`, or `SyncOff`), `AppendAndSync()` MUST first create `<db>-wal`, write the 32-byte salted WAL header, call `walFile.Sync()`, and call `fs.SyncDir(dir)` **before writing any member frame** (`ADR-0004`: WAL header sync precedes all frame writes so a crash during frame append can never leave frame bytes after a torn initial header).
  - `AppendAndSync()` takes **no cancelling `context.Context`** (the Gate seals cancellation before invoking it) and then writes batch frames and applies mode-dependent durability: `SyncFull` runs foreground `walFile.Sync()`, `SyncNormal` schedules coalesced background `walFile.Sync()`, and `SyncOff` omits commit/background syncs (while still enforcing the one-time pre-frame WAL header sync + `SyncDir` and Checkpoint barriers).
  - `AppendAndSync()` returns `*DurableBatch` without exposing frames to readers; `Publish(batch)` is then invoked by `tx.CommitGate` after installing schema/allocator/conflict-history updates so readers observe an atomic transition.

### 3.4 Complete Durability Barrier Failure Table (`INV-WAL-05`, `ADR-0003`, `ADR-0004`)

| Durability Barrier / Failure Point | Engine State | WAL Disposition | Watermark / Memory Visibility | Caller Outcome |
| :--- | :--- | :--- | :--- | :--- |
| **1. Pre-append invalid WAL tail repair (`Truncate`/`Sync`) fails** | Terminal **Fail-Stop** | Preserve WAL | No new frames written; nothing published | Known failure `ErrIO` (NOT `ErrCommitIndeterminate` because zero batch frames were appended) |
| **2. Pre-frame new-WAL creation, 32B header write, header `Sync()`, or initial `SyncDir` fails (before any member frame is written, ALL sync modes)** | Terminal **Fail-Stop** | Preserve WAL safely (no frames appended) | Zero member frames written; nothing published | Known failure `ErrIO` (NOT `ErrCommitIndeterminate` because zero member frames were written) |
| **3. Foreground WAL frame `WriteAt` error or short write (`0 < n < len`)** | Terminal **Fail-Stop** | Preserve WAL | Publish **none** of the batch | Sealed physically-appended batch members: non-retryable `ErrCommitIndeterminate` (wrapping cause); provisional-conflict & unsealed queued writers: known `ErrFailStop` |
| **4. Foreground `SyncFull` `walFile.Sync()` failure (after frame `WriteAt`)** | Terminal **Fail-Stop** | Preserve WAL | Publish **none** of the batch | Sealed physically-appended batch members: `ErrCommitIndeterminate`; provisional-conflict & unsealed queued writers: known `ErrFailStop` |
| **5. Background `SyncNormal` `walFile.Sync()` failure** | Terminal **Fail-Stop** | Preserve WAL | Reject all subsequent mutations (`ErrFailStop`) | Previously acknowledged Normal-mode callers retain Normal-mode durability semantics (unsynced suffix may be lost on power loss) |
| **6. Checkpoint Step 1 (`WAL.Sync`) failure** | Terminal **Fail-Stop** | Preserve WAL | Do NOT write `<db>` or advance backfill watermark | Checkpoint caller receives `ErrIO`/`ErrFailStop` |
| **7. Checkpoint Step 2 (`DB.WriteAt`) or Step 3 (`DB.Sync`) failure** | Terminal **Fail-Stop** | Preserve WAL | Do **NOT** advance backfill watermark or retire `FrameIndex` entries (WAL remains authoritative) | Checkpoint caller receives `ErrIO`/`ErrFailStop` |
| **8. Checkpoint Step 4 (durable empty WAL or new salted header `WriteAt`/`Sync`) failure** | Terminal **Fail-Stop** | Preserve WAL as-is (forbid further appends) | `<db>` holds synced full backfill | Checkpoint caller receives `ErrIO`/`ErrFailStop` |
| **9. Clean `Close` WAL `Remove` or `SyncDir` failure** | Closed | Error reported | `<db>` already holds synced full backfill | `Close` propagates the removal / `SyncDir` error |

### 3.5 Four-Step Checkpointing & Deadlock-Free Step 4 Wait (`INV-WAL-03`, `INV-TX-05`, `ADR-0004`)
1. Acquire `WriteSerializeMu`; read `safeMaxTS = min(latestCommitTS, minActiveSnapshotTS)` under `SnapshotRegistry` (then release `SnapshotRegistry`); call `walFile.Sync()`.
2. Copy the latest changed image of each page since the last successful backfill through `safeMaxTS` (including `safeMaxTS`'s exact Page 1 image and historical `TotalPageCount`). First writable Checkpoint after restart recopies latest changed pages from that generation before retirement.
3. Call `dbFile.Sync()`; only then publish backfill watermark, exclude stale main-file cache fills, redirect reads to `<db>`, and retire covered `FrameIndex` entries. `CheckpointPassive` skips repeat work when watermark is unchanged.
4. **Deadlock-Free Generation Retirement**:
   - Holding `WriteSerializeMu`, acquire `SnapshotRegistry`: recheck `backfilledTS == latestCommitTS` and `minActiveSnapshotTS >= backfilledTS`. If not, release `SnapshotRegistry` and `WriteSerializeMu` (retain WAL).
   - Inspect the retiring WAL generation's atomic `genRefCount`:
     - **`CheckpointPassive`**: If `genRefCount > 0`, **immediately retain the WAL, release `SnapshotRegistry` and `WriteSerializeMu`, and return** so background passive checkpoints never stall concurrent writers behind a paused reader (`ADR-0004`: passive rotates only when readers have already drained).
     - **`CheckpointRestart` / `CheckpointTruncate`**: Switch future reader admission away from the retiring WAL generation (all data is already synced in `<db>`) and **release `SnapshotRegistry`** before waiting for `genRefCount == 0` (respecting `ctx.Done()`).
   - Completing readers only touch `BufferPool` shard locks (never `WriteSerializeMu` or `SnapshotRegistry`) and atomically decrement `genRefCount`, signaling a wait channel on `1 → 0` with zero lock contention.
   - If `ctx` cancels while waiting, abort rotation without touching the WAL and return `ctx.Err()`.
   - Once `genRefCount == 0`, re-verify under `SnapshotRegistry` that zero active snapshots reference the old generation, then establish a durable empty WAL (`Truncate(0)` + `Sync()`), write a new salted 32B WAL header, and `Sync()` that header before releasing `WriteSerializeMu`.

### 3.6 Canonical Lock Order (`INV-TX-05`)
`WriteSerializeMu` → `SnapshotRegistry` → `WAL/BufferPool (FrameIndex, BufferPool shards)`. Readers never acquire `WriteSerializeMu`.

---

## 4. Resource Ownership & Lifecycle

- `*Pager` owns `<db>` and `<db>-wal` `vfs.File` handles, process lock, Buffer Pool `noscan` slabs, WAL generation counters, and the background async checkpointer goroutine.
- **Single Finalizer Close (`ADR-0007`)**: `Close(ctx)` stops new `AcquireSnapshot`/`BeginWrite`/`Checkpoint` admissions and drains active physical operations:
  - Read-only open: skips Checkpoint and WAL removal; waits for active reads/snapshots, then releases FDs and lock.
  - Clean read-write drain (not Fail-Stop, `ctx` not expired): runs `CheckpointTruncate` (full backfill + generation retirement), removes `<db>-wal` + `SyncDir` (propagating errors), then releases FDs and lock.
  - Fail-Stop, partial backfill, or `ctx` timeout: preserves `<db>-wal` and returns `ErrFailStop` or `ErrCloseTimeout` while **retaining FDs and the OS process lock until the last in-flight physical read, commit, or checkpoint exits**, at which point the finalizer closes FDs and releases the lock.
  - Repeated `Close` calls are idempotent and never double-close FDs.
- Callers own every `*Snapshot` (`Release()`) and every `PagePin` / `pinToken` (`Unpin()` / `UnpinToken()` before `Snapshot.Release()`). On idle/close, `Stats().PinnedPages == 0`.

---

## 5. Test Surface & Verification Contract

- Exercised through `Open`, `Close`, `AcquireSnapshot`, `BeginWrite`, `Checkpoint`, and `Stats` with injected `CrashFS` and `ErrorFS`:
  - **M1–M2 bootstrap & orphan-WAL races**:
    - Pause first opener *after temporary-file `Sync()` and before `PublishExclusive`*; let second opener create, commit, and close; resumed opener loses with `os.ErrExist`, cleans up temp file, reopens winner under lock, and reads committed data.
    - Pause opener `A` *after seeing `<db>` absent and before stating `<db>-wal`*; let opener `B` create `<db>`, commit into `<db>-wal`, and remain open (or close); verify `A` retries locked open of `<db>` and observes `ErrDatabaseLocked` (or reads `B`'s committed data) rather than spuriously returning `ErrCorruption`.
    - Separately open a cleanly closed DB read-only with `<db>-wal` absent, verify read-only open on a read-only volume leaves files byte-identical, and verify WAL stat/read `EIO` fails `Open`.
  - **Batch marker cut & recovery**: Cut a two-member WAL write after A's commit marker and inside B. Assert neither A nor B becomes visible in the live engine, both A and B receive `ErrCommitIndeterminate`, a queued unsealed C receives known `ErrFailStop`, and reopening twice yields a legal committed prefix (every acknowledged `SyncFull` commit survives).
  - **Torn-header & tail-repair boundaries**: Test a WAL-frame-valid but internally corrupt Page 1 (bad magic/CRC/padding → `ErrCorruption`), a WAL-frame-valid checksum-valid candidate Page 1 requiring a newer reader (`ErrUnsupportedVersion`), an intact unsupported 32B WAL header with zero frames (`ErrUnsupportedVersion`), and a new commit after tail repair followed by another reopen verifying rolling CRC continuity.
  - **Durability Barrier Failure Table**: Inject failures at every row of §3.4 (including pre-frame new-WAL header `Sync()` and `SyncDir` failures in `SyncFull`/`SyncNormal`/`SyncOff`, background `SyncNormal` sync failure, and Checkpoint Steps 1–4) and verify Fail-Stop, WAL preservation, and watermark invariants.
  - **Physical batch rollover vs `ErrTxTooLarge` & Freelist audit**: Test two individually admissible members that exceed the staging budget together (`ErrBatchFull` rollover into two batches), one member exceeding the budget alone (`ErrTxTooLarge` before append), and `VerifyFreelist()` rejecting Page 1 or Page 2 inside any Freelist trunk/leaf.
  - **Close & lock lifetime**: Keep an old `Snapshot` across a new commit and `Close` (partial backfill retains WAL); block an in-flight `Sync()` past `CloseTimeout` and verify a second `Open` remains locked (`ErrDatabaseLocked`) until it exits.
  - **Reader vs WAL rotation (explicit pause point)**: Pause a reader after WAL offset resolution via a blocking test `vfs.FS.ReadAt` AND before cache insertion via an unexported nil-by-default function field on `Pager` (`testBeforeCacheInsert func()`, zero cost when nil, settable only by `internal/pager` package tests — never exported in `Pager`'s public interface); verify `CheckpointPassive` immediately retains WAL and returns without stalling concurrent writers while `genRefCount > 0`, and drive `CheckpointRestart`/`Truncate` with concurrent snapshot admission to prove deadlock-free generation retirement and exact old/new page isolation. Run `CheckpointPassive` repeatedly with and without a pinned reader and measure watermark progress and WAL reuse.
- **Invariants Enforced**: `INV-WAL-01`–`06`, `INV-STR-01`–`02`, `INV-TX-05`.
