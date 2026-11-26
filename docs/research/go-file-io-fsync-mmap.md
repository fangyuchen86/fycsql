# Research: Go File I/O, `fsync` Durability, and `mmap` Trade-offs Across OSes

**Issue**: [#2 — Research: Go File I/O, fsync Durability, and mmap Trade-offs Across OSes](https://github.com/fangyuchen86/fycsql/issues/2)  
**Scope**: Pure Go standard library (zero third-party runtime dependencies, no CGo) across Linux (ext4 / WSL2), macOS (APFS), and Windows (NTFS).

---

## Executive Summary

| Decision Area | Verified Finding | Downstream Implication for `fycsql` |
|---|---|---|
| **Page / Log I/O** | `(*os.File).ReadAt` / `WriteAt` map to lock-free `pread`/`pwrite` on Unix and `OVERLAPPED` I/O on Windows; errors return as Go `error` values. | Use `ReadAt`/`WriteAt` behind a small internal VFS interface (modeled on Pebble's `vfs`) to support deterministic fault-injection testing. |
| **Durability (`Sync`)** | Go's `(*os.File).Sync()` calls `fsync` on Linux, `fcntl(F_FULLFSYNC)` (with `ENOTSUP` fallback to `fsync`) on macOS since Go 1.12, and `FlushFileBuffers` on Windows. | `(*os.File).Sync()` is already crash-safe on macOS/APFS without extra syscalls. Optional Linux-only `syscall.Fdatasync` optimization only pays off on in-place overwrites of preallocated files. |
| **Directory Sync** | POSIX `fsync` on a file does **not** persist parent directory entries (create/rename/unlink). Windows NTFS does not require (and rejects) directory `FlushFileBuffers`. | Explicitly `Sync()` the parent directory after file creation, rename, or unlink on Unix; make dir `Sync()` a no-op on Windows. |
| **`fsync` Failure Policy** | On Linux, failed writeback marks dirty page-cache pages clean; retrying `fsync` returns `0` even though data was lost. | **Never retry `fsync` and proceed.** Treat any `Sync()` error as fatal to the transaction/writer (halt writes and require recovery). |
| **Torn Writes** | Multi-sector pages (e.g., 4–16 KiB) are not written atomically on power loss. | Never overwrite live database pages in place without WAL or rollback-journal protection plus checksums. |
| **Exclusive Locking** | POSIX `fcntl(F_SETLK)` silently drops *all* process locks when *any* fd for the same file is closed. `syscall.Flock` (Unix) and `syscall.CreateFile(..., sharemode=0)` (Windows) are pure stdlib and avoid this trap. | Use build-tagged pure-stdlib locking: `syscall.Flock(LOCK_EX\|LOCK_NB)` on Unix and `syscall.CreateFile` (`sharemode=0`) on a lock file (or `LockFileEx` via `syscall.NewLazyDLL`) on Windows. |
| **`mmap` vs `pread`/`pwrite`** | `mmap` introduces `SIGBUS` crash hazards, remap/reader stalls, off-heap memory invisible to `GOMEMLIMIT` (cgroup OOM risk), OS-driven page eviction, and `-d=checkptr` friction. | **Avoid `mmap`.** Use `ReadAt`/`WriteAt` with an explicit, bounded user-space buffer pool. |

---

## 1. Go Standard-Library File I/O (Zero Dependencies, No CGo)

### 1.1 `*os.File` Primitives and Underlying OS Calls

Go's `os.File` wraps an `internal/poll.FD`, isolating platform differences behind build tags ([`pkg.go.dev/os`](https://pkg.go.dev/os), [`src/os/file.go`](https://github.com/golang/go/blob/master/src/os/file.go)):

- **`ReadAt(b []byte, off int64) (n int, err error)`** and **`WriteAt(b []byte, off int64) (n int, err error)`**:
  - Loop over the underlying positional syscall until `len(b)` bytes are transferred or an error (`io.EOF` on short read) occurs ([`src/os/file.go`](https://github.com/golang/go/blob/master/src/os/file.go)).
  - Do **not** read or modify the file descriptor's seek offset.
  - **Append-mode restriction**: `WriteAt` explicitly fails with `os: invalid use of WriteAt on file opened with O_APPEND` (`errWriteAtInAppendMode` in [`src/os/file.go`](https://github.com/golang/go/blob/master/src/os/file.go)). Do not open database or WAL files with `O_APPEND` if positional writes are used.
  - **Unix (`internal/poll/fd_unix.go`)**: Calls `syscall.Pread` / `syscall.Pwrite` under `fd.incref()` only (no mutex serialization), enabling true concurrent positional reads/writes from multiple goroutines on the same `*os.File` ([`src/internal/poll/fd_unix.go`](https://github.com/golang/go/blob/master/src/internal/poll/fd_unix.go), [`pread(2)`](https://man7.org/linux/man-pages/man2/pread.2.html)). Regular files bypass the runtime network poller (`epoll`/`kqueue`), so blocking disk I/O parks the calling OS thread (`M`) and Go's scheduler (`sysmon`) hands off other goroutines.
  - **Windows (`internal/poll/fd_windows.go`)**: Populates `syscall.Overlapped` (`Offset`/`OffsetHigh`) and routes through `execIO` with `readWriteLock()` coordination ([`src/internal/poll/fd_windows.go`](https://github.com/golang/go/blob/master/src/internal/poll/fd_windows.go)).
- **`Truncate(size int64) error`**: Maps to `ftruncate` (Unix) / `SetEndOfFile` (Windows). Does not change the seek offset; extending zero-fills (sparsely where supported), shrinking discards bytes beyond `size` ([`pkg.go.dev/os#File.Truncate`](https://pkg.go.dev/os#File.Truncate)).
- **`Sync() error`**: Delegates to `internal/poll.(*FD).Fsync()` (details in Section 2).

### 1.2 Frozen `syscall` Package Constraints (No `golang.org/x/sys`)

Since Go 1.4, the stdlib `syscall` package is **frozen**—it does not receive new OS syscalls, and Go recommends `golang.org/x/sys` for expanded syscall coverage ([`pkg.go.dev/syscall`](https://pkg.go.dev/syscall)). Under a **zero third-party dependency** constraint, only symbols already present in stdlib `syscall` are available:

| Capability | Linux (`syscall`) | macOS/Darwin (`syscall`) | Windows (`syscall`) |
|---|---|---|---|
| Full durability sync | `(*os.File).Sync()` (`Fsync`) | `(*os.File).Sync()` (`F_FULLFSYNC`) | `(*os.File).Sync()` (`FlushFileBuffers`) |
| Data-only sync | `syscall.Fdatasync(fd)` | **Unavailable** (XNU has no `fdatasync` syscall) | **Unavailable** |
| Whole-file advisory lock | `syscall.Flock(fd, how)` | `syscall.Flock(fd, how)` | **Unavailable** (`CreateFile` `sharemode=0` or `NewLazyDLL("kernel32.dll")` for `LockFileEx`) |
| Linux OFD locks (`F_OFD_SETLK`) | **Unavailable** in frozen `syscall` | N/A | N/A |
| `fallocate` / `posix_fallocate` | **Unavailable** in frozen `syscall` | **Unavailable** | N/A |
| Memory mapping | `syscall.Mmap` / `Munmap` | `syscall.Mmap` / `Munmap` | `CreateFileMapping` / `MapViewOfFile` |

> **Note on `f.Fd()`**: Prefer `(*os.File).SyscallConn()` (`syscall.RawConn.Control`) over raw `f.Fd()` when invoking `syscall.*` functions so Go's runtime keeps the file descriptor referenced and cannot concurrently close/recycle the fd number while a syscall runs ([`pkg.go.dev/os#File.SyscallConn`](https://pkg.go.dev/os#File.SyscallConn)).

---

## 2. OS-Level Durability Guarantees and Failure Modes

### 2.1 What `(*os.File).Sync()` Actually Invokes

Verified directly in Go's `src/internal/poll` implementation:

1. **Linux & other POSIX** ([`src/internal/poll/fd_fsync_posix.go`](https://github.com/golang/go/blob/master/src/internal/poll/fd_fsync_posix.go)): Calls `syscall.Fsync(fd.Sysfd)` wrapped in `ignoringEINTR`. Flushes dirty file data and all modified inode metadata to stable storage ([`fsync(2)`](https://man7.org/linux/man-pages/man2/fsync.2.html)).
2. **macOS / Darwin** ([`src/internal/poll/fd_fsync_darwin.go`](https://github.com/golang/go/blob/master/src/internal/poll/fd_fsync_darwin.go)):
   - Since **Go 1.12** ([golang/go#26650](https://github.com/golang/go/issues/26650)), calls `fcntl(fd, F_FULLFSYNC, 0)` instead of `fsync(2)`.
   - On macOS, POSIX `fsync(2)` only pushes data out of the kernel to the drive's volatile write-back cache, which can be lost on power failure; Apple documents `F_FULLFSYNC` as the command that asks the drive to flush all buffered data to permanent storage ([Apple Developer: *Reducing Disk Writes*](https://developer.apple.com/documentation/xcode/reducing-disk-writes), [SQLite *Atomic Commit*](https://www.sqlite.org/atomiccommit.html)).
   - If `F_FULLFSYNC` returns `ENOTSUP` (e.g., SMB/network mounts), Go falls back to `syscall.Fsync(fd.Sysfd)` ([golang/go#64215](https://github.com/golang/go/issues/64215)).
   - Apple also provides `F_BARRIERFSYNC` (ordering barrier without forcing a full hardware cache flush—faster than `F_FULLFSYNC`, weaker against power loss; [Apple Developer: *Reducing Disk Writes*](https://developer.apple.com/documentation/xcode/reducing-disk-writes)). Go's `(*os.File).Sync()` deliberately uses `F_FULLFSYNC` for full durability.
3. **Windows** ([`src/internal/poll/fd_fsync_windows.go`](https://github.com/golang/go/blob/master/src/internal/poll/fd_fsync_windows.go), [`src/syscall/syscall_windows.go`](https://github.com/golang/go/blob/master/src/syscall/syscall_windows.go)): Calls Win32 `FlushFileBuffers(handle)`, which flushes OS and device write buffers for the file handle ([Microsoft Docs: `FlushFileBuffers`](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-flushfilebuffers)).

### 2.2 `fsync` vs `fdatasync`

- According to Linux [`fsync(2)` / `fdatasync(2)`](https://man7.org/linux/man-pages/man2/fsync.2.html), `fdatasync()` flushes file data plus only the metadata required to retrieve that data (notably `st_size` and extent mappings), skipping non-essential metadata such as `st_atime` and `st_mtime`.
- **When `fdatasync` actually helps (ext4)**: On Linux ext4 (`data=ordered`, default), overwriting already-allocated blocks without changing `st_size` allows `fdatasync` to avoid committing an extra JBD2 metadata transaction for `mtime` ([Linux Kernel ext4 Docs](https://docs.kernel.org/filesystems/ext4/journal.html)). However, when **appending** and growing `st_size` (typical for a growing WAL unless preallocated/recycled), `fdatasync` must still flush size metadata, largely erasing the difference from `fsync`.
- **Cross-platform precedent**: Both `bbolt` ([`bolt_linux.go`](https://github.com/etcd-io/bbolt/blob/main/bolt_linux.go), [`boltsync_unix.go`](https://github.com/etcd-io/bbolt/blob/main/boltsync_unix.go)) and CockroachDB Pebble ([`vfs/default_linux.go`](https://github.com/cockroachdb/pebble/blob/master/vfs/default_linux.go), [`vfs/default_unix.go`](https://github.com/cockroachdb/pebble/blob/master/vfs/default_unix.go)) call `Fdatasync` on Linux and fall back to `(*os.File).Sync()` on macOS/other Unixes (where `fdatasync` does not exist) and Windows.

### 2.3 Directory `fsync` on Create, Rename, and Unlink

Linux [`fsync(2)`](https://man7.org/linux/man-pages/man2/fsync.2.html) explicitly warns:

> *"Calling `fsync()` does not necessarily ensure that the entry in the directory containing the file has also reached disk. For that an explicit `fsync()` on a file descriptor for the directory is also needed."*

- **Unix (Linux & macOS)**: After creating a new file (e.g., database file, WAL segment, rollback journal), atomically renaming a file (`os.Rename`), or unlinking a file, the engine **must** open the parent directory read-only (`os.Open(dirPath)`) and call `dir.Sync()`, then `dir.Close()`. Without this, a power loss after committing a new WAL/journal or rename can leave the file unlinked/missing on recovery ([SQLite *Atomic Commit*](https://www.sqlite.org/atomiccommit.html)).
- **Windows**: `FlushFileBuffers` is not used on directories; NTFS journals directory namespace operations automatically. Following RocksDB and Pebble ([`vfs/default_windows.go`](https://github.com/cockroachdb/pebble/blob/master/vfs/default_windows.go)), directory `Sync()` should be a no-op on Windows.

### 2.4 Failed `fsync` Semantics ("fsyncgate" / Linux `errseq_t`)

As documented during PostgreSQL's 2018 investigation ([LWN: *PostgreSQL's fsync() surprise*](https://lwn.net/Articles/752063/)) and Linux [`fsync(2)`](https://man7.org/linux/man-pages/man2/fsync.2.html):

- When asynchronous page-cache writeback fails (e.g., `EIO`, `ENOSPC`), Linux marks the affected page-cache pages **clean** (or discards them) and records the writeback error in the inode's `errseq_t` counter (introduced in Linux 4.13 so each open fd observes the error at least once).
- Once `fsync()` reports that `EIO`/`ENOSPC` on a file descriptor, the fd's `errseq_t` cursor advances. **A second `fsync()` call on the same fd returns `nil` (`0`) even though the previously dirty data was never written to disk.**
- **Rule for `fycsql`**: Never blindly retry `Sync()` after an error and assume durability succeeded. Abort the commit, stop advancing checkpoints/truncating WAL, and require crash recovery or process restart (matching PostgreSQL's fail-stop policy).

### 2.5 Torn Writes, Lying Hardware, and OS/FS Nuances

- **Torn writes**: Storage devices write in 512-byte or 4 KiB physical sectors, not atomically across multi-sector database pages (or even across a single 4 KiB page on 512e drives or interrupted flashes). On sudden power loss, a page written in place can contain a mix of old and new sectors ([SQLite *Atomic Commit*](https://www.sqlite.org/atomiccommit.html)). Crash recovery requires either a **WAL** or **rollback journal** plus **checksums** (e.g., CRC32C) on log records/pages so torn frames are reliably detected and discarded on replay.
- **Lying drives**: Consumer USB sticks, SD cards, and some SSDs ignore cache-flush commands and acknowledge `fsync` while data remains in volatile DRAM, permitting write reordering across sync barriers ([SQLite *How To Corrupt An SQLite Database File*](https://www.sqlite.org/howtocorrupt.html)). Software cannot prevent hardware lies, only detect corruption via checksums.
- **Linux ext4**: Default `data=ordered` guarantees file data blocks reach disk before metadata referencing them is committed to the journal, preventing uninitialized/stale block exposure after a crash ([Linux Kernel ext4 Docs](https://docs.kernel.org/filesystems/ext4/journal.html)).
- **WSL2**: Runs a real Linux kernel over an `ext4` filesystem inside a Hyper-V virtual disk (`ext4.vhdx`) hosted on Windows NTFS ([Microsoft WSL Docs](https://learn.microsoft.com/en-us/windows/wsl/compare-versions)).
  - Every `fsync` crosses the VM boundary (`ext4` -> Hyper-V storage -> Windows host NTFS/VHDX), yielding noticeably higher per-sync latency than bare-metal Linux.
  - Exhausting Windows host disk space while a dynamic `ext4.vhdx` expands under heavy writes is a known vector for guest `ext4` corruption ([microsoft/WSL](https://github.com/microsoft/WSL)).
  - Never place database files on `/mnt/c` (DrvFS / 9P bridge): it has severe I/O overhead and non-POSIX locking/rename/sync quirks.
- **macOS APFS**: Copy-on-write filesystem with strong crash consistency, but because Go's `(*os.File).Sync()` issues `F_FULLFSYNC`, every single-transaction commit forces a full SSD hardware cache flush—often 5–20x slower per sync than Linux `fsync`. Batching / group commit (or explicit transactions) is essential for acceptable write throughput on macOS.

---

## 3. Single-Process Exclusive File Locking in Pure Go Stdlib

Go's `os` package provides no portable file-locking API, and `os.OpenFile` does **not** lock files on any OS (on Windows, [`src/syscall/syscall_windows.go`](https://github.com/golang/go/blob/master/src/syscall/syscall_windows.go) explicitly opens files with `FILE_SHARE_READ | FILE_SHARE_WRITE`).

### 3.1 Why POSIX `fcntl(F_SETLK)` Is Hazardous

Per Linux [`fcntl(2)`](https://man7.org/linux/man-pages/man2/fcntl.2.html), [`close(2)`](https://man7.org/linux/man-pages/man2/close.2.html), and [SQLite *How To Corrupt An SQLite Database File* §2.2](https://www.sqlite.org/howtocorrupt.html):

1. **Silent release on unrelated `close()`**: POSIX `fcntl` record locks are owned by the **process**, not the open file description. If *any* code in the same process opens the same file (via a separate `open()`) and calls `close()`, the kernel silently drops **all** `fcntl` locks held by the process on that inode.
2. **No intra-process exclusion**: Two independent opens in the same process do not block each other on `F_SETLK`; engines that use `F_SETLK` (like Pebble in [`vfs/file_lock_unix.go`](https://github.com/cockroachdb/pebble/blob/master/vfs/file_lock_unix.go)) must pair it with a dedicated `LOCK` file and a process-wide Go mutex/map.
3. Linux's Open File Description locks (`F_OFD_SETLK`) fix both problems, but they are Linux-specific and absent from Go's frozen stdlib `syscall`.

### 3.2 Pure-Stdlib Locking Strategy (Build Tags, Zero Dependencies)

| Platform | Pure-Stdlib Mechanism | Semantics & Caveats |
|---|---|---|
| **Unix** (`linux`, `darwin`, BSDs) | `syscall.Flock(int(fd), syscall.LOCK_EX\|syscall.LOCK_NB)` (same mechanism as [`bbolt/bolt_unix.go`](https://github.com/etcd-io/bbolt/blob/main/bolt_unix.go)) | **Per-open-file-description** ([`flock(2)`](https://man7.org/linux/man-pages/man2/flock.2.html)): closing an unrelated fd to the same file does **not** drop the lock; a second `open()` in the same process **does** fail with `EWOULDBLOCK`; kernel auto-releases on process exit/crash. Advisory only (non-cooperating processes are not blocked) and unreliable on NFSv3/network filesystems. |
| **Windows — Option A (Recommended with sidecar `.lock` file)** | `syscall.CreateFile` with `sharemode = 0` (`FILE_SHARE_NONE`), `GENERIC_READ\|GENERIC_WRITE`, `CREATE_ALWAYS` (pure-stdlib equivalent of [`pebble/vfs/file_lock_windows.go`](https://github.com/cockroachdb/pebble/blob/master/vfs/file_lock_windows.go)) | Enforces both cross-process and intra-process exclusivity via Windows mandatory sharing (`ERROR_SHARING_VIOLATION`); zero `NewLazyDLL` boilerplate; auto-released on handle close or process crash. |
| **Windows — Option B (Single-file DB, no sidecar)** | Either open the DB file itself via `syscall.CreateFile` (`sharemode = 0`) wrapped in `os.NewFile`, or invoke `LockFileEx` via `syscall.NewLazyDLL("kernel32.dll")` on byte range `[0xFFFFFFFF_FFFFFFFF, +1]` (pattern from [`bbolt/bolt_windows.go`](https://github.com/etcd-io/bbolt/blob/main/bolt_windows.go)) | Locking an out-of-band offset avoids conflicting with normal `ReadFile`/`WriteFile` calls on the DB file. Requires careful `unsafe.Pointer(&overlapped)` handling. |

**Recommendation**: Use a clean two-file build-tag split (`lock_unix.go` with `//go:build unix` using `syscall.Flock`, and `lock_windows.go` with `//go:build windows` using `syscall.CreateFile` with `sharemode = 0`—or `LockFileEx` if strict single-file storage with no `.lock` file is chosen).

---

## 4. `mmap` vs `pread`/`pwrite` Trade-offs in Go

As analyzed in Crotty, Leis, and Pavlo's CIDR 2022 paper [*Are You Sure You Want to Use MMAP in Your Database Management System?*](https://db.cs.cmu.edu/papers/2022/cidr2022-p13-crotty.pdf) and reinforced by Go-specific runtime behavior:

### 4.1 Comparison Summary

| Dimension | `mmap` (`syscall.Mmap`) | `ReadAt` / `WriteAt` (`pread` / `pwrite`) + Buffer Pool |
|---|---|---|
| **I/O & Truncation Errors** | Kernel delivers **`SIGBUS`**, crashing the Go process ([`mmap(2)`](https://man7.org/linux/man-pages/man2/mmap.2.html)). | Returns standard Go `error` (`io.EOF`, `syscall.EIO`); recoverable per query/tx. |
| **Write Ordering / Recovery** | Writable `mmap` allows the OS to flush dirty pages at arbitrary times, violating WAL-before-data ordering. | Engine controls exact write and `Sync()` order before updating checkpoints. |
| **File Growth** | Must `munmap` and `mmap` a larger region while blocking all concurrent readers (`db.mmaplock` in `bbolt`). | Immediate—just `WriteAt` beyond EOF (or `Truncate` to extend); zero reader stalls. |
| **Dataset > RAM** | Kernel page cache evicts blindly (no DBMS pinning/clock/LRU-K), triggers synchronous page faults blocking Go OS threads, and suffers OS eviction lock / TLB shootdown bottlenecks (CIDR 2022). | Bounded user-space buffer pool with database-aware eviction (e.g., LRU-K / 2Q / Clock) and predictable memory footprint. |
| **Go GC & `GOMEMLIMIT`** | Off-heap: invisible to `GOMEMLIMIT` / `GOGC` ([Go GC Guide](https://go.dev/doc/gc-guide)), yet file-backed RSS counts toward Linux cgroup limits—risking container OOM kills. | Buffer pool slices live on the Go heap (bounded, tracked by `GOMEMLIMIT`, pointer-free `[]byte` arrays impose near-zero GC scan cost). |
| **Type Safety / `-race`** | Casting `mmap` bytes to structs via `unsafe.Pointer` clashes with Go's `-d=checkptr` (enabled automatically under `-race`). | Standard safe Go decoding from `[]byte` buffers; 100% `-race` and `checkptr` clean. |
| **Portability (Stdlib Only)** | Unix (`syscall.Mmap`) and Windows (`CreateFileMapping`/`MapViewOfFile`) require completely different code paths. | Single portable `*os.File` implementation across Linux, macOS, and Windows. |

### 4.2 Go-Specific `mmap` Hazards Worth Highlighting

1. **`SIGBUS` and `debug.SetPanicOnFault`**: Reading past a truncated file or hitting an underlying disk read error on a mapped page raises `SIGBUS`, which fatally terminates a Go process by default. [`runtime/debug.SetPanicOnFault(true)`](https://pkg.go.dev/runtime/debug#SetPanicOnFault) converts `SIGBUS`/`SIGSEGV` into a catchable panic, **but** it is goroutine-local (every reader goroutine must enable/restore it) and does not provide clean error context or prevent mid-operation invariant corruption.
2. **Remap vs. Concurrent Readers**: Even engines that restrict `mmap` to read-only (`PROT_READ`) and perform writes via `pwrite`—as `bbolt` does in [`bolt_unix.go`](https://github.com/etcd-io/bbolt/blob/main/bolt_unix.go)—must hold an exclusive reader-writer lock (`db.mmaplock`) to unmap and remap whenever the file grows. Any slice referencing the old mapping that escapes a transaction causes an immediate `SIGSEGV` (use-after-unmap).
3. **Go Heap / GC Interaction**: A `[]byte` slice contains no Go pointers, so a fixed-size buffer pool of preallocated `[]byte` page frames adds negligible GC mark/scan overhead while staying fully visible to `GOMEMLIMIT` and `runtime/metrics`. By contrast, `mmap` hides working-set memory from Go's memory limit while still consuming cgroup memory ([Go GC Guide](https://go.dev/doc/gc-guide)).

### 4.3 Recommendation for `fycsql`

Use **`(*os.File).ReadAt` / `(*os.File).WriteAt`** paired with an explicit, fixed-capacity **user-space buffer pool** of `[]byte` page frames. Wrap file, directory, and lock operations behind a narrow internal VFS interface (inspired by Pebble's `vfs`) so the storage engine stays 100% stdlib-only in production while allowing in-memory and crash/fault-injection VFS implementations in tests.

---

## References (Primary Sources)

1. Go Standard Library `os` Package Documentation: https://pkg.go.dev/os
2. Go Standard Library `syscall` Package Documentation (frozen notice): https://pkg.go.dev/syscall
3. Go Standard Library `runtime/debug` (`SetPanicOnFault`, `SetMemoryLimit`): https://pkg.go.dev/runtime/debug
4. Go Garbage Collector Guide (`GOMEMLIMIT` & off-heap memory): https://go.dev/doc/gc-guide
5. Go Source — `src/os/file.go` (`ReadAt`, `WriteAt`, `errWriteAtInAppendMode`): https://github.com/golang/go/blob/master/src/os/file.go
6. Go Source — `src/internal/poll/fd_fsync_darwin.go` (`F_FULLFSYNC` & `ENOTSUP` fallback, Issues [#26650](https://github.com/golang/go/issues/26650) & [#64215](https://github.com/golang/go/issues/64215)): https://github.com/golang/go/blob/master/src/internal/poll/fd_fsync_darwin.go
7. Go Source — `src/internal/poll/fd_fsync_posix.go` & `fd_fsync_windows.go`: https://github.com/golang/go/blob/master/src/internal/poll/fd_fsync_posix.go , https://github.com/golang/go/blob/master/src/internal/poll/fd_fsync_windows.go
8. Go Source — `src/internal/poll/fd_unix.go` & `fd_windows.go` (`Pread`/`Pwrite`): https://github.com/golang/go/blob/master/src/internal/poll/fd_unix.go , https://github.com/golang/go/blob/master/src/internal/poll/fd_windows.go
9. Go Source — `src/syscall/syscall_windows.go` (`Open` `FILE_SHARE_READ|FILE_SHARE_WRITE`, `Fsync` -> `FlushFileBuffers`): https://github.com/golang/go/blob/master/src/syscall/syscall_windows.go
10. Linux Manual Pages — `fsync(2)` / `fdatasync(2)`: https://man7.org/linux/man-pages/man2/fsync.2.html
11. Linux Manual Pages — `flock(2)`: https://man7.org/linux/man-pages/man2/flock.2.html
12. Linux Manual Pages — `fcntl(2)` & `close(2)`: https://man7.org/linux/man-pages/man2/fcntl.2.html , https://man7.org/linux/man-pages/man2/close.2.html
13. Linux Manual Pages — `pread(2)` & `mmap(2)`: https://man7.org/linux/man-pages/man2/pread.2.html , https://man7.org/linux/man-pages/man2/mmap.2.html
14. Linux Kernel Documentation — ext4 Journaling (`data=ordered`): https://docs.kernel.org/filesystems/ext4/journal.html
15. LWN — *PostgreSQL's fsync() surprise* (Linux page cache clean-on-error & `errseq_t`): https://lwn.net/Articles/752063/
16. Apple Developer Documentation — *Reducing Disk Writes* (`F_FULLFSYNC` vs `F_BARRIERFSYNC`): https://developer.apple.com/documentation/xcode/reducing-disk-writes
17. Microsoft Win32 Docs — `FlushFileBuffers` & WSL2 Architecture: https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-flushfilebuffers , https://learn.microsoft.com/en-us/windows/wsl/compare-versions
18. SQLite Official Docs — *Atomic Commit In SQLite*: https://www.sqlite.org/atomiccommit.html
19. SQLite Official Docs — *How To Corrupt An SQLite Database File* (§2.2 POSIX `fcntl` close trap, lying `fsync`): https://www.sqlite.org/howtocorrupt.html
20. Crotty, Leis, Pavlo (CIDR 2022) — *Are You Sure You Want to Use MMAP in Your Database Management System?*: https://db.cs.cmu.edu/papers/2022/cidr2022-p13-crotty.pdf
21. etcd `bbolt` Source — `bolt_unix.go`, `bolt_linux.go`, `boltsync_unix.go`, `bolt_windows.go`: https://github.com/etcd-io/bbolt/blob/main/bolt_unix.go , https://github.com/etcd-io/bbolt/blob/main/bolt_linux.go , https://github.com/etcd-io/bbolt/blob/main/bolt_windows.go
22. CockroachDB Pebble VFS Source — `default_linux.go`, `default_unix.go`, `default_windows.go`, `file_lock_unix.go`, `file_lock_windows.go`: https://github.com/cockroachdb/pebble/tree/master/vfs
