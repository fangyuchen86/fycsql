# Research: Embedded Storage Engines — Page-Level WAL vs Copy-on-Write B+Tree vs ARIES

**Issue**: [#3](https://github.com/fangyuchen86/fycsql/issues/3)  
**Scope**: Compare the three primary embedded B+Tree persistence and recovery architectures against primary sources for `fycsql` v1 (pure-Go stdlib-only, single-process multi-goroutine embedded relational DB, MBs–tens of GBs where working set may exceed RAM, Linux/macOS primary + Windows VFS portability, ms–100ms crash recovery).

---

## Executive Summary

| Dimension | 1. Page-Level WAL (SQLite-style) | 2. Copy-on-Write B+Tree (LMDB / bbolt) | 3. In-Place B+Tree + ARIES WAL (PG / InnoDB) |
|---|---|---|---|
| **Core Mechanism** | Append full modified pages sequentially to `-wal`; in-memory frame index maps `(page, snapshot)` -> frame; async checkpoint backfills main DB. | Never overwrite live pages; copy modified leaf + ancestors up to root ($O(h)$); atomically flip between 2 alternating meta pages. | Update pages in-place in buffer pool (steal/no-force); log physiological redo/undo records + per-page LSN; 3-pass crash recovery (Analysis/Redo/Undo). |
| **Read/Write Concurrency** | Single writer + unlimited concurrent snapshot readers; readers never block writers (except checkpoint reset). | Single writer + unlimited lock-free snapshot readers; readers never block writers (except `mmap` remap in bbolt). | Supports concurrent writers (with 2PL/row locks + B+Tree latch coupling) and snapshot readers **only if paired with a separate MVCC layer**. |
| **Write Amplification** | ~2× full-page writes (WAL append + checkpoint backfill); multiple writes to same page coalesce before checkpoint. | $O(h)$ full pages per modified tree (3–5+ pages per table + every updated secondary index) + freelist + meta page; **2 `fsync`s/commit**. | Lowest log bytes for small warm-page updates, **plus** torn-page cost (PostgreSQL `full_page_writes` after checkpoint, or InnoDB doublewrite buffer). |
| **Long-Running Readers** | **Checkpoint starvation**: WAL cannot reset/truncate past oldest reader mark; `-wal` grows until a reader gap occurs (shrinks on `TRUNCATE`). | **Freelist starvation**: old pages remain pending; main `.db` file balloons **permanently** until offline compaction/VACUUM. | **Physical WAL unaffected** (fuzzy checkpoints advance `RedoLSN`); MVCC layer still bloats (dead tuples / undo history lag). |
| **Space Reclamation** | Standard in-DB freelist (trunk/leaf pages), $O(1)$ alloc/free; `-wal` reclaimed on `TRUNCATE` checkpoint. | Txid-indexed delayed freelist (`FREE_DBI` / `pending`); either O(freelist) write overhead per commit or full-DB scan on open (`NoFreelistSync`). | Free-space maps / extent bitmaps (must be WAL-logged) + background tuple vacuum/purge. |
| **Crash Recovery** | **Simple**, $O(\text{WAL frames})$ single forward scan verifying rolling checksums + salts (ms–100ms when WAL bounded). Inherently torn-page safe. | **$O(1)$ instant** (pick highest valid checksummed meta page), unless `NoFreelistSync` forces full-tree scan. Requires strict 2-sync ordering. | **Very complex**: 3-pass recovery, CLRs, `UndoNxtLSN`, Nested Top Actions (ARIES/IM) for B+Tree splits/merges, plus FPI/doublewrite for torn pages. |
| **Pure-Go Stdlib Fit** | **Excellent**: works cleanly over `os.File.ReadAt`/`WriteAt` + pre-allocated `noscan` `[]byte` buffer pool; single-process eliminates `-shm` file; 1 sync/commit. | **Friction**: idioms assume `mmap` (OS-specific `syscall`, uncatchable `SIGBUS`, remap locks, uncontrolled page faults on working sets > RAM) and 2 `F_FULLFSYNC`s on macOS. | **High cost**: clean over `ReadAt`/`WriteAt` + buffer pool, but implementation & fault-injection verification surface is an order of magnitude larger. |

**Recommendation for `fycsql` v1**: Adopt **SQLite-style Page-Level WAL with an in-memory frame index** (dropping SQLite's multi-process `-shm` file and 5-slot `aReadMark` cap) over a portable `os.File.ReadAt`/`WriteAt` VFS and a fixed-slab `noscan` Go buffer pool. It delivers native snapshot isolation, simple deterministic ms-scale crash recovery, inherent torn-page safety, single-`fsync` commits (critical under macOS `F_FULLFSYNC`), and avoids both CoW's multi-index path-copying / `mmap` hazards and ARIES's multi-subsystem complexity cliff.

---

## 1. Architecture Overviews (Primary-Source Mechanics)

### 1.1 SQLite-Style Page-Level WAL with Frame Index & Checkpointing

In SQLite WAL mode ([SQLite WAL Documentation](https://www.sqlite.org/wal.html), [WAL File Format](https://www.sqlite.org/walformat.html)):
- **Append-only page frames**: Writers leave the main database file untouched and append modified full pages sequentially to a companion `-wal` file. The WAL begins with a 32-byte header containing two random 32-bit salts (`Salt-1`, `Salt-2`) that change whenever the WAL resets. Each frame has a 24-byte header (`pgno`, `dbSize` in pages, `Salt-1`, `Salt-2`, cumulative rolling `Checksum-1`/`Checksum-2`) followed by the raw page bytes.
- **Atomic commit**: Intermediate frames store `dbSize = 0`. A transaction commits atomically when its final frame records a non-zero `dbSize` and is flushed to stable storage. Rollback simply discards uncommitted frames without touching the main database.
- **WAL-index lookup**: To avoid linear WAL scans on reads, SQLite maintains a hash-indexed WAL-index mapping page numbers to the newest frame index $\le$ a reader's snapshot mark (`mxFrame`), stored in a `-shm` shared-memory file across processes ([WAL Format Specification](https://www.sqlite.org/walformat.html)).
- **Checkpointing**: A checkpointer copies committed frames from `-wal` back into the main database file (`nBackfill` tracks progress) in four modes ([SQLite WAL Documentation](https://www.sqlite.org/wal.html)): `PASSIVE` (non-blocking, default for autocheckpoint), `FULL` (blocks writers until all frames are copied), `RESTART` (also waits for old readers so next writer wraps to frame 1), and `TRUNCATE` (resets and truncates `-wal` to 0 bytes).

### 1.2 Copy-on-Write (CoW) Shadow-Paged B+Tree (LMDB / bbolt)

In LMDB ([LMDB Technical Docs](https://lmdb.tech/doc/)) and Go's `etcd-io/bbolt` ([bbolt README](https://github.com/etcd-io/bbolt/blob/main/README.md), [`tx.go`](https://github.com/etcd-io/bbolt/blob/main/tx.go), [`db.go`](https://github.com/etcd-io/bbolt/blob/main/db.go)):
- **Shadow paging (no WAL)**: Live pages visible to any active reader are strictly immutable. Modifying a leaf page allocates new copies of the leaf and every ancestor branch page up to the tree root (path copying).
- **Two alternating meta pages**: Pages 0 and 1 store root pointers, monotonically increasing `txid`, freelist metadata, and integrity checksums. On commit, the writer (1) writes all new data/branch/freelist pages to free or newly extended file space, (2) executes the first `fsync`/`fdatasync`, (3) writes the older of the two meta pages with `txid + 1` pointing to the new root, and (4) executes a second `fsync`/`fdatasync` ([bbolt `tx.go`](https://github.com/etcd-io/bbolt/blob/main/tx.go); [LMDB Docs](https://lmdb.tech/doc/)).
- **Single-level store via `mmap`**: Both LMDB and bbolt map the database file directly into virtual memory so readers traverse B+Tree pages zero-copy from the OS page cache without a user-space buffer pool.

### 1.3 In-Place B+Tree with ARIES-Style Physiological Redo/Undo WAL

Defined by Mohan et al. (*"ARIES: A Transaction Recovery Method Supporting Fine-Granularity Locking and Partial Rollbacks Using Write-Ahead Logging"*, ACM TODS 1992, [doi:10.1145/128765.128770](https://doi.org/10.1145/128765.128770)) and extended to B+Trees in Mohan & Levine (*"ARIES/IM"*, SIGMOD 1992, [doi:10.1145/130283.130338](https://doi.org/10.1145/130283.130338)), and adapted in production engines such as PostgreSQL ([PostgreSQL WAL Intro](https://www.postgresql.org/docs/current/wal-intro.html), [WAL Internals](https://www.postgresql.org/docs/current/wal-internals.html)) and MySQL InnoDB ([InnoDB Doublewrite](https://dev.mysql.com/doc/refman/8.4/en/innodb-doublewrite-buffer.html), [InnoDB Multi-Versioning](https://dev.mysql.com/doc/refman/8.4/en/innodb-multi-versioning.html)):
- **Steal / No-Force buffer pool**: Dirty uncommitted pages may be evicted to disk (`steal`), and committed pages need not be flushed before commit returns (`no-force`). Every page header carries a `pageLSN`; the WAL rule mandates flushing log records up to `pageLSN` before writing that page to disk.
- **Physiological logging**: Log records identify a physical page but describe a logical intra-page action (e.g., insert/delete tuple in slot $k$), shrinking log volume and decoupling physical byte offsets during recovery.
- **Three-pass restart recovery**: (1) **Analysis** scans forward from the last fuzzy checkpoint to rebuild the Active Transaction Table and Dirty Page Table (`RedoLSN`); (2) **Redo** ("repeating history") replays all logged updates (committed and uncommitted) from `RedoLSN` where `pageLSN < record.LSN`; (3) **Undo** walks loser transactions backward via `prevLSN`, writing redo-only **Compensation Log Records (CLRs)** with `UndoNxtLSN` so crashes during recovery remain idempotent.
- **B+Tree structural modifications (SMOs)**: Page splits/merges cannot be undone when a single user transaction aborts if other transactions have already traversed or updated the split pages; ARIES/IM wraps SMOs in **Nested Top Actions (NTAs)** anchored by dummy CLRs ([Mohan & Levine 1992](https://doi.org/10.1145/130283.130338)).

---

## 2. Detailed Trade-Off Evaluation

### 2.1 Read/Write Concurrency & Snapshot Isolation

- **Page-Level WAL**: Naturally provides **Snapshot Isolation** at page granularity with **1 concurrent writer and $N$ concurrent readers**. A reader captures `mxFrame` at transaction start and resolves each page lookup to the latest WAL frame $\le \text{mxFrame}$ (falling back to the main DB file if absent). Writers append strictly beyond `mxFrame` and never block readers; readers never block writers (only checkpoint wrap-around). Because `fycsql` v1 is single-process, it does not inherit SQLite's fixed 5-slot `-shm` `aReadMark` array limit ([WAL Format](https://www.sqlite.org/walformat.html))—every active read goroutine can hold an exact frame mark in memory.
- **CoW B+Tree**: Naturally provides **lock-free Snapshot Isolation** with **1 concurrent writer and $N$ concurrent readers**. A reader reads the active meta page's `txid` and root pointer, registers its `txid` in a reader table (cache-line-aligned slots in LMDB; `rwlock` / slice in bbolt), and traverses an immutable tree ([LMDB Docs](https://lmdb.tech/doc/), [bbolt README](https://github.com/etcd-io/bbolt/blob/main/README.md)). One Go caveat in bbolt: when the DB file grows, `db.mmap()` must acquire `mmaplock` exclusively to unmap/remap the virtual address space, briefly stalling new read transactions until active readers finish ([bbolt `db.go`](https://github.com/etcd-io/bbolt/blob/main/db.go)).
- **ARIES**: **Recovery alone does not provide Snapshot Isolation or MVCC.** ARIES enables **concurrent writers** (multiple transactions modifying different rows/pages simultaneously via 2PL or optimistic row locks + B+Tree latch coupling / B-link trees), which neither SQLite WAL nor CoW supports natively. However, achieving non-blocking snapshot readers requires building a **second full concurrency subsystem** on top: either tuple-level version chains inside heap/index pages (PostgreSQL MVCC) or rollback-segment undo logs with read-view reconstruction (InnoDB MVCC, [MySQL 8.4 Docs](https://dev.mysql.com/doc/refman/8.4/en/innodb-multi-versioning.html)).

### 2.2 Write Amplification & Durability Sync Cost

Assume 4 KiB or 8 KiB pages, an OLTP workload updating 1 row in a table B+Tree of height $h = 4$ with $k = 2$ secondary indexes:

- **Page-Level WAL**:
  - *Bytes written*: Writes 1 leaf page for the table + 1 leaf page per updated secondary index (typically 3 pages total, unless a split touches a parent) sequentially to `-wal`, then writes those same pages once to the main DB during checkpointing (~**2× page write amplification**). Crucially, if 50 transactions update the same hot leaf page before a checkpoint, the WAL writes 50 frames sequentially, but the checkpoint backfills only the **single newest frame** to the main DB—coalescing random I/O ([SQLite WAL](https://www.sqlite.org/wal.html)).
  - *Syncs per commit*: **1 `fsync`** under full durability (`PRAGMA synchronous=FULL`), or **0 per commit** under `NORMAL` (syncing only on checkpoint, trading last-commit power-loss durability for crash-consistency) ([SQLite WAL](https://www.sqlite.org/wal.html)). Multiple waiting writers can also be batched via **group commit** into a single WAL `fsync`.
- **CoW B+Tree**:
  - *Bytes written*: Every single-row update must clone the entire root-to-leaf path in the table B+Tree ($h$ pages) **plus** the root-to-leaf path in every updated secondary index ($k \cdot h$ pages) **plus** any root-catalog / bucket-directory page **plus** freelist pages **plus** the meta page. For $h = 4, k = 2$, a 1-row update writes **12–15+ full pages** (48–120 KiB) of random-offset writes per commit—an order of magnitude worse write amplification than page-level WAL on multi-index relational schemas. Furthermore, bbolt's default `NoFreelistSync: false` serializes the *entire* freelist on every commit ([bbolt `tx.go`](https://github.com/etcd-io/bbolt/blob/main/tx.go)).
  - *Syncs per commit*: **2 `fsync`s** (data pages first, then meta page) to prevent the meta page from pointing to unpersisted data pages after a power loss ([bbolt `tx.go`](https://github.com/etcd-io/bbolt/blob/main/tx.go); [LMDB Docs](https://lmdb.tech/doc/)). On macOS, where Go's `os.File.Sync()` invokes `F_FULLFSYNC` ([Go Issue #26650](https://github.com/golang/go/issues/26650)), doubling durable syncs per commit halves unbatched commit throughput.
- **ARIES Physiological WAL**:
  - *Bytes written*: Smallest WAL footprint on warm pages (tens–hundreds of bytes per row delta) plus asynchronous dirty-page writes at checkpoint. **However**, to survive torn pages on 512 B / 4 KiB storage sectors when DB pages are larger (e.g., 8–16 KiB), ARIES engines must add torn-page protection:
    - PostgreSQL's `full_page_writes = on` writes the **full 8 KiB page image** to WAL on the *first* modification to each page after every checkpoint ([PostgreSQL WAL Internals](https://www.postgresql.org/docs/current/wal-internals.html), [WAL Configuration](https://www.postgresql.org/docs/current/wal-configuration.html)).
    - InnoDB's **doublewrite buffer** writes every dirty page twice during flush (first sequentially to the doublewrite area + flush, then to its real tablespace offset) ([MySQL 8.4 Doublewrite Buffer](https://dev.mysql.com/doc/refman/8.4/en/innodb-doublewrite-buffer.html)).
  - *Syncs per commit*: **1 WAL `fsync`** (highly amenable to group commit).

### 2.3 Checkpoint Starvation & Long-Running Readers

- **Page-Level WAL**: A checkpointer cannot copy WAL frames beyond the oldest active reader's end-mark, because doing so would overwrite main-DB pages that the reader still needs ([SQLite WAL](https://www.sqlite.org/wal.html)). Under a long-running read query—or overlapping short readers with no "reader gap"—`nBackfill` stalls below `mxFrame`, preventing WAL reset. The `-wal` file grows continuously until the blocking reader closes.
  - *Mitigation advantage*: Once the reader closes, a `RESTART` or `TRUNCATE` checkpoint immediately reclaims 100% of the `-wal` space back to the OS. In a single-process Go engine, `fycsql` can also track exact per-reader marks, expose WAL-growth metrics, support reader-gap scheduling (`RESTART` mode throttling new readers briefly once WAL exceeds a high-water mark), or bound read transaction lifetimes via `context.Context`.
- **CoW B+Tree**: No WAL checkpoint exists, but long-running readers cause **freelist reclamation starvation**—arguably a harsher failure mode. As bbolt's README explicitly warns, *"old pages cannot be reclaimed while an old transaction is using them. A long running read transaction can cause the database to quickly grow"* ([bbolt README](https://github.com/etcd-io/bbolt/blob/main/README.md); [LMDB Docs](https://lmdb.tech/doc/)). Because freed pages stay `pending`, every write allocates fresh pages at EOF, inflating the **main database file**. Unlike a `-wal` file, that main `.db` file bloat does **not** shrink when the reader finishes; reclaiming disk space requires an expensive copy/compaction (`VACUUM`).
- **ARIES + MVCC**: Completely decouples **physical WAL truncation** from long-running readers: fuzzy checkpoints flush dirty pages to disk and advance `RedoLSN` even while hour-long queries run, bounding WAL size and crash-recovery time ([PostgreSQL WAL Configuration](https://www.postgresql.org/docs/current/wal-configuration.html)). However, the **MVCC layer** still suffers logical bloat under long readers—PostgreSQL `VACUUM` cannot reclaim dead tuples newer than `oldestXmin`, and InnoDB's purge thread stalls while the undo history list grows ([MySQL 8.4 Multi-Versioning](https://dev.mysql.com/doc/refman/8.4/en/innodb-multi-versioning.html)).

### 2.4 Free-Page Tracking & Space Reclamation

- **Page-Level WAL**: Uses a straightforward in-DB freelist—e.g., SQLite's linked list of **freelist trunk pages**, each holding an array of 4-byte **freelist leaf page** pointers ([SQLite File Format](https://www.sqlite.org/fileformat.html)). Free leaf pages are never read or written while unallocated, making allocation/deallocation $O(1)$ with minimal WAL frames. Because the freelist lives inside the same paged database, freeing/reusing pages is automatically atomic with the transaction's WAL commit; old page versions are protected by the WAL snapshot boundary rather than a txid-deferred freelist. File truncation uses explicit `VACUUM` or incremental auto-vacuum (pointer-map pages).
- **CoW B+Tree**: Must defer page reuse by freeing transaction ID until `freeTxID < minActiveReaderTxID`.
  - LMDB stores freed page lists in a hidden B+Tree (`FREE_DBI`) keyed by `txnid`, which itself undergoes CoW updates during commit ([LMDB Docs](https://lmdb.tech/doc/)).
  - bbolt maintains an in-memory freelist (`FreelistArrayType` with $O(N)$ contiguous-run search or `FreelistMapType` hash-based allocator) and either writes the entire serialized freelist on every commit or skips persisting it via `NoFreelistSync`, which forces a full B+Tree traversal on every database open to rediscover free pages ([bbolt `tx.go`](https://github.com/etcd-io/bbolt/blob/main/tx.go), [`db.go`](https://github.com/etcd-io/bbolt/blob/main/db.go)). Contiguous multi-page allocations (for overflow/BLOB values) also suffer external fragmentation under CoW.
- **ARIES**: Requires crash-safe page-allocation bitmaps / Free Space Maps (logged via physiological/physical records or mini-transactions) **plus** intra-page slot compaction and background MVCC garbage collection (VACUUM/purge) to turn deleted/superseded tuple versions back into reusable space.

### 2.5 Crash Recovery Complexity & Torn-Write Safety

- **Page-Level WAL**:
  - *Recovery*: Single forward pass over `-wal` verifying header salts and cumulative rolling checksums frame-by-frame until EOF or the first mismatch; sets `mxFrame` to the last verified commit frame and rebuilds the in-memory frame index ([WAL Format](https://www.sqlite.org/walformat.html)). With a bounded WAL (e.g., autocheckpoint at 1,000–4,000 pages = 4–16 MiB), sequential scan recovery finishes in **a few milliseconds to tens of milliseconds**, meeting `fycsql`'s ms–100ms target comfortably.
  - *Torn-write safety*: Inherently immune to torn pages without extra mechanisms:
    1. A torn write during WAL append breaks the frame's rolling checksum and causes recovery to ignore that frame and any uncommitted tail cleanly.
    2. A torn write during checkpointing corrupts a page in the main DB file, **but** the complete, checksummed page still exists in `-wal` (because the WAL is never reset until the main DB is `fsync`ed); the next checkpoint simply overwrites the torn page cleanly ([SQLite WAL](https://www.sqlite.org/wal.html)).
- **CoW B+Tree**:
  - *Recovery*: **$O(1)$ instant**—read meta page 0 and meta page 1, verify magic + checksum, pick the valid meta page with the higher `txid`. (Exception: bbolt with `NoFreelistSync: true` degrades open time to $O(\text{DB size})$ by walking every page in the tree.)
  - *Torn-write safety*: Torn writes to newly allocated data/branch pages are unreachable because the meta page was not yet flipped. Torn writes to the alternate meta page fail its checksum, falling back cleanly to the previous meta page. Critical subtlety: correctness depends strictly on the OS/disk honoring the barrier between data `fsync` and meta-page write; skipping the first `fsync` or relying on non-checksummed hardware sector atomicity risks silent tree corruption.
- **ARIES**:
  - *Recovery*: **High implementation and testing complexity**. Must implement exact `pageLSN` monotonicity, Analysis/Redo/Undo passes, CLR chaining (`UndoNxtLSN`), fuzzy checkpoint records (`begin_checkpoint` / `end_checkpoint`), crash-during-recovery idempotency, and B+Tree SMO recovery via Nested Top Actions ([Mohan et al. 1992](https://doi.org/10.1145/128765.128770); [Mohan & Levine 1992](https://doi.org/10.1145/130283.130338)). Recovery time is bounded by checkpoint interval (`RedoLSN` to end of WAL) plus undo of long uncommitted writers.
  - *Torn-write safety*: **Not inherent** to delta/physiological logging; requires either `full_page_writes` (PostgreSQL) or a doublewrite buffer (InnoDB), adding another persistence invariant to verify under power-loss fault injection.

### 2.6 Go-Specific Ergonomics (Memory, GC, Pure-Go Stdlib, VFS Portability)

`fycsql` v1 imposes strict Go engineering constraints: **zero CGo, zero third-party runtime dependencies (Go stdlib only)**, working sets larger than RAM (MBs–tens of GBs), and clean VFS portability across Linux, macOS, and Windows.

1. **Buffer Pool Memory Management & Go GC Pressure**:
   - **Pointer-free `noscan` slabs**: Go's concurrent mark-sweep GC only traces objects containing pointers. A raw `[]byte` backing array is allocated in a `noscan` span and incurs **zero GC mark scanning cost** regardless of size ([A Guide to the Go Garbage Collector](https://go.dev/doc/gc-guide)). By storing buffer pool frames in a single pre-allocated `[]byte` slab (or fixed-size chunk slabs) and referencing frame descriptors via integer indices (`uint32`) rather than pointer-rich graphs, `fycsql` avoids GC mark CPU spikes completely—without needing CGo.
   - **The `GOGC` RSS Multiplier Trap (Pebble Lesson)**: CockroachDB's Pebble documented that keeping multi-GB caches on the Go heap inflated RSS by ~2× because default `GOGC=100` waits for new allocations equal to the entire live heap before running GC ([Pebble `docs/memory.md`](https://github.com/cockroachdb/pebble/blob/master/docs/memory.md)). Pebble solved this via CGo-backed `C.malloc` (`internal/manual`), which is unavailable to pure-Go `fycsql`. In modern Go (≥ 1.19), pure-Go engines mitigate this trap cleanly by:
     - Configuring **`GOMEMLIMIT` / `runtime/debug.SetMemoryLimit`** alongside `GOGC` so GC pacing accounts for the fixed buffer pool without doubling RSS ([Go GC Guide](https://go.dev/doc/gc-guide));
     - Pooling transient scratch buffers (`sync.Pool`) and avoiding per-row heap escapes on hot scan paths so allocation rate stays low even when live `noscan` pool bytes are large.
2. **Why `mmap` (LMDB/bbolt's Idiom) Is Poorly Suited to Pure-Go `fycsql`**:
   - **Uncatchable `SIGBUS` crashes**: As documented by both SQLite ([SQLite Memory-Mapped I/O](https://www.sqlite.org/mmap.html)) and LMDB, disk I/O errors, ENOSPC on sparse files, or external truncation on a memory-mapped region raise `SIGBUS`, which kills the Go process instead of returning an `error`.
   - **Non-portable stdlib**: `mmap` is absent from Go's `os` package; supporting Linux, macOS, and Windows requires platform-specific `syscall` / Win32 wrappers and `unsafe.Slice` conversions, undermining a clean portable VFS.
   - **Go scheduler stalls when DB > RAM**: When the working set exceeds RAM, reading an `mmap` byte slice triggers synchronous OS major page faults inside the kernel, blocking the underlying OS thread (`M`) without cooperating with Go's netpoller/scheduler (unlike `ReadAt` syscalls that park cleanly via `entersyscall`), and relinquishing eviction policy to the kernel LRU (where table scans evict hot B+Tree interior pages).
   - **Remap synchronization**: Growing an `mmap`ped file requires either pre-reserving huge virtual address space (LMDB's `mapsize`) or pausing readers to unmap/remap (bbolt's `mmaplock`, [`db.go`](https://github.com/etcd-io/bbolt/blob/main/db.go)).
   - *Note*: CoW *can* be built over `ReadAt`/`WriteAt` + a user-space buffer pool instead of `mmap`, but doing so eliminates CoW's main read-path performance advantage while keeping all of CoW's write-amplification, 2-`fsync`, and freelist-bloat downsides.
3. **macOS `F_FULLFSYNC` Amplification**:
   - Since Go 1.12, `os.File.Sync()` on macOS invokes `fcntl(fd, F_FULLFSYNC)` to guarantee true non-volatile durability ([Go Issue #26650](https://github.com/golang/go/issues/26650)). Because `F_FULLFSYNC` flushes drive write caches and is substantially slower than Linux `fsync`/`fdatasync`, an architecture requiring **1 sync per commit** (Page-Level WAL, plus easy group commit) has a major real-world latency advantage over CoW's **2 syncs per commit**.
4. **Implementation & Verification Complexity**:
   - **Page-Level WAL** (single-process Go): ~1,000–2,000 LOC for pager + WAL + in-memory frame index + checkpointer + freelist. Easy to test exhaustively with a fault-injecting VFS (simulating torn writes, dropped syncs, and crashes at every I/O boundary).
   - **CoW B+Tree**: Moderate core LOC, but secondary-index write amplification, overflow-page contiguous allocation, and txid-deferred freelist persistence/compaction add subtle edge cases.
   - **ARIES + MVCC**: Easily 5×–10× the state-machine complexity (LSN bookkeeping across every page mutator, physiological redo/undo handlers for every B+Tree/catalog mutation, NTAs for splits/merges, torn-page FPI/doublewrite, plus tuple MVCC and background vacuum/purge).

---

## 3. Architectural Recommendation & Open Design Implications for `fycsql` v1

**Recommended Storage Architecture**: **SQLite-style Page-Level WAL with an In-Memory Frame Index**, built over a portable `ReadAt`/`WriteAt`/`Sync` VFS and a pre-allocated pointer-free (`noscan`) buffer pool.

### Why this fits `fycsql` v1 best
1. **Matches single-process Go strengths**: Eliminating multi-process coordination removes SQLite's trickiest WAL component (the `-shm` mmap file, cross-process POSIX locks, and the 5-slot `aReadMark` cap). The frame index becomes a clean in-memory Go structure rebuilt deterministically on startup/crash recovery.
2. **Fast, provably correct crash recovery**: Single forward WAL scan with salts + rolling checksums hits the ms–100ms recovery target with inherent torn-page safety and zero crash-time undo.
3. **Strong relational OLTP economics**: Updates touching a primary B+Tree + multiple secondary indexes write only modified leaves (not $O(h)$ paths per tree), coalesce hot-page writes at checkpoint time, and require at most **1 `os.File.Sync()` per commit** (amortizable via group commit—crucial on macOS `F_FULLFSYNC`).
4. **Clean working-set > RAM behavior**: User-space buffer pool over `os.File.ReadAt`/`WriteAt` avoids `SIGBUS`, avoids `mmap` remap pauses, gives full control over scan-resistant page replacement, and keeps Go GC scanning overhead near zero via `noscan` byte slabs.

### Risks to address explicitly in downstream ADRs
- **Checkpoint starvation**: Define explicit reader-mark tracking, `PASSIVE`/`RESTART`/`TRUNCATE` checkpoint policies, a configurable WAL high-water mark (e.g., autocheckpoint threshold + optional soft writer/reader backpressure or diagnostic warning), and metrics for oldest-reader lag.
- **Single-writer serialization ceiling**: Document that v1 uses a single writer (with group commit to maximize concurrent goroutine throughput) and confirm whether fine-grained concurrent writers are deferred to v2+.
- **Page size & write amplification**: Choose page size (4 KiB vs 8 KiB) balancing B+Tree fan-out against per-commit WAL frame size.
- **Freelist & offline/online compaction**: Specify trunk/leaf freelist format and `VACUUM` strategy to reclaim space in the main DB file after bulk deletes.

---

## 4. Primary Sources

1. **SQLite — Write-Ahead Logging**: https://www.sqlite.org/wal.html
2. **SQLite — WAL File Format & WAL-Index Specification**: https://www.sqlite.org/walformat.html
3. **SQLite — Database File Format (B-trees & Freelist Trunk/Leaf Pages)**: https://www.sqlite.org/fileformat.html
4. **SQLite — Memory-Mapped I/O (`SIGBUS` and error handling)**: https://www.sqlite.org/mmap.html
5. **LMDB — Official Technical Documentation**: https://lmdb.tech/doc/
6. **bbolt (`etcd-io/bbolt`) — README (CoW, mmap, long-running read txns)**: https://github.com/etcd-io/bbolt/blob/main/README.md
7. **bbolt (`etcd-io/bbolt`) — Transaction Commit & `writeMeta` (`tx.go`)**: https://github.com/etcd-io/bbolt/blob/main/tx.go
8. **bbolt (`etcd-io/bbolt`) — `mmap`, `mmaplock`, and Freelist Options (`db.go`)**: https://github.com/etcd-io/bbolt/blob/main/db.go
9. **Mohan, Haderle, Lindsay, Pirahesh, Schwarz (1992) — ARIES (ACM TODS, Vol. 17, No. 1)**: https://doi.org/10.1145/128765.128770
10. **Mohan & Levine (1992) — ARIES/IM (ACM SIGMOD '92)**: https://doi.org/10.1145/130283.130338
11. **PostgreSQL — Write-Ahead Logging (WAL) Introduction**: https://www.postgresql.org/docs/current/wal-intro.html
12. **PostgreSQL — WAL Internals (LSNs, `full_page_writes`, recovery)**: https://www.postgresql.org/docs/current/wal-internals.html
13. **PostgreSQL — WAL Configuration & Checkpoints**: https://www.postgresql.org/docs/current/wal-configuration.html
14. **MySQL 8.4 Reference Manual — InnoDB Doublewrite Buffer**: https://dev.mysql.com/doc/refman/8.4/en/innodb-doublewrite-buffer.html
15. **MySQL 8.4 Reference Manual — InnoDB Multi-Versioning & Purge**: https://dev.mysql.com/doc/refman/8.4/en/innodb-multi-versioning.html
16. **CockroachDB Pebble — Memory Management & Go GC Notes (`docs/memory.md`)**: https://github.com/cockroachdb/pebble/blob/master/docs/memory.md
17. **Go Documentation — A Guide to the Go Garbage Collector (`GOGC`, `GOMEMLIMIT`, scanning)**: https://go.dev/doc/gc-guide
18. **Go Issue #26650 — `os: File.Sync()` on Darwin uses `F_FULLFSYNC`**: https://github.com/golang/go/issues/26650
