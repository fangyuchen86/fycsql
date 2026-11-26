---
status: accepted
---

# Page-Level WAL, Dedicated Page 1 Header, and Immutable Buffer Pool

fycsql v1 persists data in a two-file layout (`<db>` + `<db>-wal`, plus `:memory:` via `MemFS`) using SQLite-style page-level WAL with an in-memory Frame Index (no `-shm` file) over pure-stdlib `(*os.File).ReadAt`/`WriteAt` and `Sync()` behind a narrow `vfs.FS` seam (no `mmap`). Database pages are 1-based `uint32` `PageID`s (4KB default), with Page 1 dedicated solely to the CRC32C-protected file header so Page 2+ have uniform offset-0 layout; free pages are tracked via a transactional Freelist trunk/leaf chain. The bounded `noscan` Buffer Pool (64MB default) caches immutable frames keyed by physical source (including generation and frame number for WAL reads) using sharded atomic-CLOCK eviction with `HintSeqScan`, while the Commit Gate mutates private copy-on-modify staging buffers before appending committed pages to the WAL.

## Considered Options

- **Copy-on-Write (CoW) B+Tree (bbolt / LMDB style)**: Offers instant recovery and lock-free reads, but incurs $O(h)$ root-to-leaf write amplification across primary and secondary indexes, requires two `fsync`s per commit, bloats the main DB file under long-running readers, and relies heavily on `mmap`.
- **In-Place B+Tree + ARIES Physiological WAL**: Decouples checkpoints from long readers and supports concurrent physical page mutations, but adds 5x–10x complexity (3-pass Analysis/Redo/Undo, CLRs, full-page-write/doublewrite torn-page mitigation, and a separate MVCC subsystem) unnecessary under ADR-0001's serialized Commit Gate.
- **`mmap`-Backed Paging**: Avoids user-space copy overhead, but introduces uncatchable `SIGBUS` risks on I/O error or truncation, off-heap RSS invisible to `GOMEMLIMIT`, remap stalls, and poor eviction control when the dataset exceeds RAM.
- **SQLite-Style Shared Page 1 (Header at 0..99 + Catalog Root at 100..4095)**: Saves one 4KB page per database at the cost of special-casing offset math across B+Tree page code.

## Consequences

- **Lock-free immutable reader frames**: Because the Commit Gate never mutates Buffer Pool frames in place (copy-on-modify staging → append immutable WAL frames → write-through cache), a reader may access a pinned frame without a page latch. Source selection and pin acquisition follow the read-source lifetime rule below.
- **Low Go GC overhead & scan resistance**: Backing the Buffer Pool with contiguous pointer-free `[]byte` slabs keeps GC scan cost $O(1)$, while sharded atomic-CLOCK + `HintSeqScan` avoids mutex contention on cache hits and prevents sequential scans from evicting hot internal B+Tree nodes.
- **Fail-stop durability & portable locking**: Any `vfs.File.Sync()` error transitions the engine into a fail-stop error state (accounting for Linux `errseq_t`); single-process exclusivity uses build-tagged `syscall.Flock` on Unix and exclusive `CreateFile` on Windows directly on `<db>`.
- **Checkpointing responsibility**: Multi-version WAL frames are reclaimed by Checkpointing (specified in the WAL & Crash Recovery decision), while freed main-DB pages are reused via the transactional Freelist; physical file shrinking (`VACUUM`) remains deferred to later fog resolution.
- **Opening the file pair.** Resolve the database and its sidecar under one stable name. The VFS distinguishes read-only, read-write and exclusive creation. Only an explicit file-not-found result means absent. Acquire the database lock before interpreting its contents. For first creation, publish a fully initialized, synced file without replacing an existing path, sync the directory and re-open the winner under lock. A delayed opener always validates that file. An existing file always uses the recovery rules in ADR-0010. Reject a missing database with an orphan WAL before publishing a new database. Read-only opens use read-only descriptors for both files.
- **Read-source lifetime.** Coordinate snapshot registration with Checkpoint. Resolve and pin the frame or acquire a read reference to its physical generation before releasing metadata protection. Hold it through I/O and cache insertion. A main-file overwrite also excludes old in-flight cache fills and invalidates the old lookup while preserving already pinned bytes. Cache warming is optional when all slots are pinned.
- **Freelist verification.** A trunk is a live, checksummed Freelist page. Its free leaf PageIDs represent reusable storage. Verify their range, uniqueness, disjointness and total count including trunks. Reuse installs a complete new version before interpreting that page's bytes.
