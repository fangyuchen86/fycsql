# M1 Contract — `vfs`: Portable I/O, Process Locking, and Fault-Injection Seam

> **Parent Index**: [v1-module-contracts.md](../v1-module-contracts.md) · **Handoff**: [v1-architecture-handoff.md](../v1-architecture-handoff.md) · **Invariants**: [v1-invariants.md](../v1-invariants.md) · **Glossary**: [GLOSSARY.md](../../../GLOSSARY.md)
> **Related ADRs**: [ADR-0003](../../adr/0003-page-level-wal-and-immutable-buffer-pool.md), [ADR-0004](../../adr/0004-wal-format-checkpointing-and-recovery.md), [ADR-0008](../../adr/0008-verification-fault-injection-and-benchmark-architecture.md)
> **Navigation**: Next → [M2 — `internal/pager`](./m2-pager.md)

---

## 1. Depth & Responsibility

`vfs` is the public, zero-internal-dependency virtual filesystem seam (`github.com/fangyuchen86/fycsql/vfs`). It hides OS-specific file creation, atomic non-clobbering file publication, exclusive single-process locking (Unix `flock`, Windows exclusive `CreateFile`), parent-directory durability (`SyncDir`), isolated in-memory storage (`:memory:`), exhaustive 4-mode crash simulation, and deterministic I/O error/short-write injection behind two narrow Go interfaces (`FS` and `File`) plus concrete configuration/enumeration APIs on `CrashFS` and `ErrorFS`.

- **Dependency Category**: Local-substitutable / OS seam (4 real adapters: `OSFS`, `MemFS`, `CrashFS`, `ErrorFS`, plus optional custom caller adapters).
- **Package Dependencies**: Strictly Go stdlib only (`io`, `os`, `errors`, `sync`, and build-tagged OS locking syscalls). Never imports `internal/*` or root `fycsql`.

---

## 2. Interface Surface

> Signatures below define the behavioral contract, not production code.

```text
type OpenFlags uint32
  OpenReadOnly      // Read-only descriptor; never creates or modifies the target file
  OpenReadWrite     // Read-write descriptor; fails with os.ErrNotExist if absent unless OpenCreate/OpenCreateExcl set
  OpenCreate        // Create file if missing
  OpenCreateExcl    // Atomic O_CREAT|O_EXCL; fails with os.ErrExist if path already exists
  OpenLockExclusive // Acquire non-blocking single-process exclusive lock; fails with ErrLocked if held

type FS interface {
    Open(name string, flags OpenFlags) (File, error)
    Stat(name string) (FileInfo, error)
    Remove(name string) error
    Rename(oldName, newName string) error             // Overwrite-permitted rename
    PublishExclusive(tmpName, dstName string) error   // Atomic non-clobbering publish; os.ErrExist if dstName exists
    SyncDir(dir string) error
}

type File interface {
    ReadAt(p []byte, off int64) (int, error)
    WriteAt(p []byte, off int64) (int, error)
    Sync() error
    Truncate(size int64) error
    Size() (int64, error)
    Close() error
}

var ErrLocked = errors.New("vfs: file locked")

// Concrete fault-injection control surfaces (never part of FS/File)
type CrashMode uint8 // CrashPowerLoss | CrashTornWrite | CrashOutOfOrder | CrashDuringRecovery
type CrashFS struct { ... }
func NewCrashFS(base *MemFS) *CrashFS
func (c *CrashFS) EnableMode(modes ...CrashMode)
func (c *CrashFS) CutPoints() []CrashCutPoint
func (c *CrashFS) Materialize(cp CrashCutPoint) (*MemFS, error) // Independent reopenable snapshot at cut-point

type ErrorFS struct { ... }
func NewErrorFS(inner FS) *ErrorFS
func (e *ErrorFS) OnIndex(k int, err error)
func (e *ErrorFS) OnShortWrite(k int, written int, err error)
func (e *ErrorFS) OnMatch(pred FaultPredicate, inject FaultInjection)
func (e *ErrorFS) Reset()
```

---

## 3. Behavioral Contracts & Error Modes

1. **Positional, Stateless I/O (`ADR-0003`)**:
   - `ReadAt` and `WriteAt` carry explicit byte offsets and MUST be safe for concurrent `ReadAt` calls across goroutines without any shared file cursor.
   - No `mmap` is exposed or used anywhere in the seam.
2. **Distinguishing Absence from I/O Failure (`INV-WAL-06`)**:
   - Only an error matching `os.ErrNotExist` means a file is absent.
   - Permission errors, `EIO`, broken paths, or other OS failures MUST propagate untouched so `internal/pager` never mistakes an I/O failure for a missing file.
3. **Atomic Non-Clobbering Publication (`PublishExclusive`, `ADR-0003`)**:
   - `PublishExclusive(tmpName, dstName)` publishes an already-initialized, synced temporary file at `tmpName` under `dstName` **only if `dstName` does not exist**:
     - **Unix**: `os.Link(tmpName, dstName)` followed by best-effort `os.Remove(tmpName)` on success (POSIX `link(2)` fails atomically with `EEXIST` without replacing `dstName`).
     - **Windows**: `MoveFileExW(tmpName, dstName, 0)` without `MOVEFILE_REPLACE_EXISTING` (fails atomically with `ERROR_ALREADY_EXISTS` / `ERROR_FILE_EXISTS` without replacing `dstName`).
     - **`MemFS`**: Atomic directory-map check-and-insert under mutex.
   - If `dstName` already exists, `PublishExclusive` returns an error matching `os.ErrExist`, leaves `dstName` completely untouched, and leaves `tmpName` for loser cleanup (`Remove(tmpName)`).
   - Callers MUST follow a successful `PublishExclusive` with `SyncDir(dir)` to make the new directory entry crash-durable, and then reopen `dstName` under `OpenLockExclusive`.
4. **Exclusive Creation & Non-Blocking Lock (`ADR-0003`, `ADR-0007`)**:
   - `OpenCreateExcl` MUST fail with `os.ErrExist` if the target path already exists and MUST never overwrite or truncate an existing file.
   - `OpenLockExclusive` MUST be non-blocking and return `vfs.ErrLocked` on both same-process and cross-process contention.
5. **Short Writes & Partial Tears (`INV-WAL-05`, `ADR-0008`)**:
   - Callers MUST NOT assume `WriteAt` is all-or-nothing.
   - `ErrorFS` and `CrashFS` explicitly support returning `(n, err)` where `0 < n < len(p)` and `err != nil`, representing a partial write whose prefix may have landed on disk.
6. **Four Concrete Adapters**:
   - **`OSFS`**: Real filesystem over Go stdlib + build-tagged Unix (`flock` + directory fd `SyncDir` + `os.Link`) and Windows (exclusive `CreateFile` + `MoveFileExW` non-replacing).
   - **`MemFS`**: Thread-safe, fully isolated in-memory filesystem supporting exclusive locks, `OpenCreateExcl`, `PublishExclusive`, `Rename`, `Remove`, and `SyncDir`; used for `:memory:` databases and fast tests.
   - **`CrashFS`**: Wraps an in-memory base and logs every `OpenCreate`/`OpenCreateExcl`, `WriteAt`, `Truncate`, `Remove`, `Rename`, `PublishExclusive`, `Sync`, and `SyncDir` operation (tracking file-creation and directory-entry durability distinctly from file-data durability) to deterministically materialize crashed `*MemFS` images across all 4 fault modes:
     1. Clean power loss at any sync barrier (losing unsynced file data and unsynced directory entries).
     2. Partial/torn byte cut-point on any unsynced write.
     3. Out-of-order subset of unsynced writes before `Sync()`.
     4. Crash during recovery or checkpoint replay (by wrapping a materialized image in a second `CrashFS`).
   - **`ErrorFS`**: Wraps any `FS` and injects `EIO` / `ENOSPC` / short-write `(n > 0, err != nil)` / read-within-length errors on the `k`-th operation (`OnIndex(k)`) or matched predicate (`OpKind`, path, offset).

---

## 4. Resource Ownership & Lifecycle

- The caller (`internal/pager` or `internal/sql`) owns every opened `File` handle and MUST call `Close()`.
- `OSFS` retains the OS file descriptor and exclusive process lock until `File.Close()` completes.
- `CrashFS.Materialize` returns a fresh, independent `*MemFS` copy owned by the test caller; `ErrorFS` borrows its wrapped `FS` without assuming ownership of its external lifecycle.
- Every `MemFS` instance is completely isolated (no process-wide path registry).

---

## 5. Test Surface & Verification Contract

- **Shared Adapter Contract Suite**: Run identically against `OSFS` and `MemFS` through `FS` and `File` — covering `OpenCreateExcl` races, `PublishExclusive` non-clobbering races (winner succeeds, loser gets `os.ErrExist` and cleans up `tmpName`, `dstName` stays byte-identical), same-process `OpenLockExclusive` contention, concurrent `ReadAt`, `OpenReadOnly` write rejection, `Truncate`, `Rename`, `Remove`, and `SyncDir`.
- **Cross-Process OS Lock Test**: Spawn a real child helper process (`os/exec`) holding `OSFS` `OpenLockExclusive` on a file and assert the parent receives `vfs.ErrLocked` until the child closes/exits.
- **Fault-Injection Verification**: Exercise `CrashFS.CutPoints()` / `Materialize()` across all 4 modes (including unsynced file creation and unsynced `PublishExclusive`/`SyncDir` directory entries) and `ErrorFS.OnIndex` / `OnShortWrite` / `OnMatch` (including read-within-measured-length `EIO`).
- **Invariants Enforced**: `INV-WAL-06`, `INV-API-02`.
