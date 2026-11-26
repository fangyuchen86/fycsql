# Research: SQL Differential Testing, Isolation Suites, and Crash-Injection Harnesses

> **Ticket**: [fangyuchen86/fycsql#5](https://github.com/fangyuchen86/fycsql/issues/5)  
> **Scope**: Primary-source verification methodologies for an embedded RDBMS written in Go: (1) SQLite's `sqllogictest` corpus & Go runners, (2) `modernc.org/sqlite` as a pure-Go differential oracle and benchmark baseline, (3) Hermitage & Elle transaction isolation suites encoded deterministically in Go (`-race`), and (4) deterministic fault-injection VFS harnesses modeled on SQLite's `test_vfs`/crash harness and CockroachDB Pebble's `vfs.MemFS`/`vfs/errorfs`.

---

## 1. SQLite `sqllogictest`: Specification, Corpus, and Go Integration

### 1.1 Design Philosophy & Format Specification

Created by D. Richard Hipp for SQLite, `sqllogictest` is a plain-text, engine-agnostic test format designed to answer a single question: *"Does the database engine compute the correct answer?"* It intentionally excludes performance, locking, concurrency, and transactional recovery ([SQLite `sqllogictest` Design & Spec — `about.wiki`](https://sqlite.org/sqllogictest/doc/trunk/about.wiki)).

A `.test` script consists of blank-line-separated records of the following types ([`about.wiki`](https://sqlite.org/sqllogictest/doc/trunk/about.wiki)):

- **Statements** (DDL/DML with no result rows):
  - `statement ok` — the following SQL statement must succeed.
  - `statement error` — the following SQL statement must return an error.
- **Queries**:
  ```text
  query <type-string> <sort-mode> [<label>]
  <SQL query spanning one or more lines>
  ----
  <expected values, one per line, OR hash line>
  ```
  - **Column type string** (`<type-string>`): one character per result column:
    - `I` — integer (rendered as decimal integer).
    - `T` — text (rendered as string; empty string `''` is serialized literally as `(empty)`; non-printable/control characters are replaced by `@`).
    - `R` — floating-point real (normalized via `%.3f` to absorb cross-platform rounding drift).
    - Any SQL `NULL` value in any column type is serialized literally as `NULL`.
  - **Sort modes** (`<sort-mode>`):
    - `nosort` — compare results in the exact order returned by the engine (used with `ORDER BY` or single-row queries).
    - `rowsort` — sort result rows lexicographically by their rendered text representation before comparison.
    - `valuesort` — flatten all values across all rows and columns into a single list and sort lexicographically.
  - **Result hashing (`hash-threshold <N>`)**: When a query returns more than `N` total values, expected output is compacted to `<count> values hashing to <32-char-md5-hex>`, where the MD5 digest is computed over each serialized value followed by `\n` (after applying the sort mode).
  - **Labels (`<label>`)**: Optional identifier attached to a query; if multiple queries share the same label, the runner verifies they produce identical output hashes (useful for verifying index vs. full-scan equivalence or query rewrites).
- **Dialect & control directives**:
  - `onlyif <db>` / `skipif <db>` — conditionally include or skip the immediately following statement/query based on the target engine identifier (upstream tags: `sqlite`, `postgresql`, `mysql`, `mssql`, `oracle`).
  - `halt` — unconditionally stop executing the remainder of the script (used for incremental enablement or bisecting).

### 1.2 Upstream Corpus Structure

The official SQLite `sqllogictest` repository ([`sqlite.org/sqllogictest`](https://sqlite.org/sqllogictest/dir)) contains **622 test files** totaling **over 7.2 million queries**, organized into four primary groups:

| Corpus Directory / Files | Focus |
| :--- | :--- |
| `test/select1.test` – `test/select5.test` | Foundational `SELECT`, expressions, `JOIN`s, `GROUP BY`, subqueries, `UNION`/`INTERSECT`/`EXCEPT`. |
| `test/evidence/` | Direct tests of SQL language specification requirements and edge-case semantics (`slt_lang_*.test`). |
| `test/index/` | Large suites verifying that adding/using indexes (`BETWEEN`, `IN`, `ORDER BY`, `VIEW`, `COMMUTE`, `RANDOM`) produces identical results to unindexed scans. |
| `test/random/` | Stochastically generated expression, aggregate, join, and subquery tests that exercise deep planner/evaluator edge cases. |

### 1.3 Go Runners & Integration Patterns

Two prominent production Go implementations exist in the ecosystem:

1. **DoltHub `sqllogictest` Go library** ([`github.com/dolthub/sqllogictest`](https://github.com/dolthub/sqllogictest), package `github.com/dolthub/sqllogictest/go/logictest`):
   - Standalone Go parser/runner built for `go-mysql-server`, `dolt`, and `doltgresql`.
   - Exposes a `Harness` interface (`EngineStr`, `Init`, `ExecuteStatement`, `ExecuteQuery`, etc.) that bridges any Go SQL engine to the runner.
   - *Practical caveat*: `pkg.go.dev` hides documentation for `github.com/dolthub/sqllogictest/go/logictest` because the upstream SQLite corpus repository lacks a standard SPDX license file at repo root ([`pkg.go.dev`](https://pkg.go.dev/github.com/dolthub/sqllogictest/go/logictest)), and the runner's `Harness` has MySQL/Dolt-oriented quirks.
2. **CockroachDB `pkg/sql/logictest`** ([`github.com/cockroachdb/cockroach/tree/master/pkg/sql/logictest`](https://github.com/cockroachdb/cockroach/tree/master/pkg/sql/logictest)):
   - Deeply integrated internal Go runner executed inside standard `go test` suites, extending `sqllogictest` with multi-session, user-switching, and cluster directives. Not packaged as a standalone external library.

**Recommendation for `fycsql`:** Write a compact, zero-dependency `internal/testutil/sqllogictest` package (~300 LOC) implementing the exact `about.wiki` spec directly against `fycsql` (and `database/sql.DB` so the same runner can validate against SQLite). Map each `.test` file to a `t.Run` subtest, support `onlyif`/`skipif` with both `sqlite` (for upstream corpus compatibility) and `fycsql` (for engine-specific scripts), and maintain a curated fast tier (`select1..5`, `evidence/`, and custom regression `.test` files) in PR CI while running the full 622-file corpus (`index/`, `random/`) in nightly CI.

---

## 2. `modernc.org/sqlite` as an In-Process Differential Oracle & Benchmark Baseline

### 2.1 Architecture & Pure-Go Properties

`modernc.org/sqlite` ([`pkg.go.dev/modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite), upstream source [`gitlab.com/cznic/sqlite`](https://gitlab.com/cznic/sqlite)) is a **CGo-free** pure-Go SQLite3 driver produced by mechanically transpiling the official C SQLite amalgamation into Go via `ccgo` ([`gitlab.com/cznic/ccgo`](https://gitlab.com/cznic/ccgo)).

Key properties relevant to `fycsql` testing:

- **Zero CGo / zero C toolchain**: Compiles natively under `CGO_ENABLED=0` and cross-compiles across `GOOS`/`GOARCH` targets without `gcc` or `clang`.
- **Full `go test -race` compatibility**: Unlike CGo-based `mattn/go-sqlite3`—where Go's race detector cannot inspect C memory and CGo pointer-passing checks can complicate testing—`modernc.org/sqlite` executes completely within Go's memory model and runs cleanly under `go test -race`.
- **Upstream SQLite semantic parity**: Because it is mechanically translated from upstream SQLite C source rather than reimplemented from scratch, it faithfully matches upstream SQLite SQL semantics.

### 2.2 Driver Configuration & Connection-Pool Pitfalls

`modernc.org/sqlite` registers the `database/sql` driver name `"sqlite"` ([`pkg.go.dev/modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite)):

- **Per-connection PRAGMAs via DSN**: Because `database/sql.DB` pools independent physical connections, executing `PRAGMA ...` once via `db.Exec` only configures a single connection. Always pass pragmas in the DSN via `_pragma` parameters (or shorthand flags like `_busy_timeout`, `_foreign_keys`, `_journal_mode`) or via `sqlite.RegisterConnectionHook`:
  ```go
  dsn := "file:oracle.db?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
  db, err := sql.Open("sqlite", dsn)
  ```
- **In-memory database pooling trap**: Opening `":memory:"` creates a distinct private database per physical connection. For single-session differential tests, call `db.SetMaxOpenConns(1)` on `":memory:"`; for multi-connection tests, use a unique named shared-memory URI (`file:test_<name>?mode=memory&cache=shared`) or a `t.TempDir()` file path.

### 2.3 Differential Oracle Pattern in `go test`

Use `modernc.org/sqlite` as a **dual-execution oracle** inside both property-based tests and Go native fuzzing (`go test -fuzz`):

1. Generate deterministic or fuzzed DDL + DML + query workloads (random schemas, inserts/updates/deletes, expressions, joins, aggregates, `ORDER BY`/`LIMIT`, `NULL` edge cases).
2. Execute the exact SQL sequence against both `fycsql` and `modernc.org/sqlite` in-process.
3. Compare outcomes using a canonical normalizer:
   - Error vs. success parity (optionally allowing an explicit allowlist for intentionally unsupported SQLite extensions).
   - Value normalization matching `sqllogictest` (`NULL`, integer/real tolerance, multiset row comparison when no `ORDER BY` is present).
4. **Known SQLite oracle caveats**: SQLite uses dynamic typing / type affinity (e.g., inserting `'abc'` into an `INT` column succeeds unless `STRICT` tables are declared) and allows non-standard `GROUP BY` bare columns. If `fycsql` enforces strict typing, either emit `STRICT` tables in the generator or restrict generated SQL to the strict common subset so oracle mismatches always indicate real bugs.

### 2.4 Benchmark Baseline Role & Caveats

Because `modernc.org/sqlite` executes transpiled Go rather than native C, community benchmarks (e.g., `go-sqlite-bench`) show it is measurably slower than CGo `mattn/go-sqlite3` on CPU-intensive queries and bulk write transactions, while avoiding CGo call-crossing overhead on tiny calls.

- **Use `modernc.org/sqlite`** as the **always-on pure-Go CI baseline** in `go test -bench` (relative latency/throughput tracking, memory allocation profiling with `-benchmem`, and verifying `fycsql` performance trends without CGo dependencies).
- **Keep an optional CGo benchmark build tag** (`//go:build cgo_bench`) wrapping `mattn/go-sqlite3` if/when absolute comparison against native C SQLite is desired on developer/benchmark machines.

---

## 3. Transaction Isolation Suites: Hermitage, Elle, and Deterministic Go (`-race`) Encoding

### 3.1 Formal Anomaly Taxonomy & Hermitage Test Matrix

Martin Kleppmann's **Hermitage** suite ([`github.com/ept/hermitage`](https://github.com/ept/hermitage)) operationalizes the formal anomaly taxonomy from Atul Adya's PhD thesis (MIT, 1999; Adya, Liskov, O'Neil, ICDE 2000) and Berenson et al. (SIGMOD 1995) into concrete, portable SQL schedules over a minimal schema:

```sql
CREATE TABLE test (id INT PRIMARY KEY, value INT);
INSERT INTO test (id, value) VALUES (1, 10), (2, 20);
```

Adya models committed transaction histories as a **Direct Serialization Graph (DSG)** with three dependency edge types between transactions $T_i, T_j$:
- **Write-dependency (`ww`)**: $T_j$ installs a version overwriting a version installed by $T_i$.
- **Read-dependency (`wr`)**: $T_j$ reads a version installed by $T_i$.
- **Anti-dependency (`rw`)**: $T_i$ reads a version that is later overwritten by $T_j$ (or matches a predicate changed by $T_j$).

Anomalies correspond to forbidden cycles or dirty-data patterns in the DSG ([`ept/hermitage`](https://github.com/ept/hermitage); [Kingsbury & Alvaro, VLDB 2020](https://www.vldb.org/pvldb/vol13/p3046-kingsbury.pdf)):

| Code | Anomaly Name | DSG / Structural Pattern | Concrete Hermitage Schedule (on `test(1->10, 2->20)`) | Forbidden Starting At |
| :--- | :--- | :--- | :--- | :--- |
| **G0** | Write Cycles (Dirty Write) | Cycle of `ww` edges only | $T_1$ writes `id=1 -> 11`; $T_2$ writes `id=1 -> 12`, `id=2 -> 22`; $T_1$ writes `id=2 -> 21`; both commit (final state mixes `1->12, 2->21`). | Read Uncommitted (PL-1) |
| **G1a** | Aborted Reads (Dirty Read) | $T_2$ reads a version written by $T_1$, then $T_1$ aborts | $T_1$ updates `id=1 -> 101`; $T_2$ reads `id=1` (sees `101`); $T_1$ `ROLLBACK`; $T_2$ `COMMIT`. | Read Committed (PL-2) |
| **G1b** | Intermediate Reads | $T_2$ reads an intermediate uncommitted version of $T_1$ | $T_1$ updates `id=1 -> 101`; $T_2$ reads `id=1` (sees `101`); $T_1$ updates `id=1 -> 102`, `COMMIT`; $T_2$ `COMMIT`. | Read Committed (PL-2) |
| **G1c** | Circular Information Flow | Cycle of `ww`/`wr` edges | $T_1$ updates `id=1 -> 11`; $T_2$ updates `id=2 -> 22`; $T_1$ reads `id=2` (sees `22`); $T_2$ reads `id=1` (sees `11`); both commit. | Read Committed (PL-2) |
| **OTV** | Observed Transaction Vanishes | $T_3$ sees $T_2$'s effect on one key and $T_1$'s earlier state on another (`ww`+`wr`+`rw` chain) | $T_1$ updates `1->11, 2->19`; $T_2$ updates `1->12, 2->18`; $T_3$ reads `id=1` (`12`) then `id=2` (`19`). | Repeatable Read / Snapshot Isolation |
| **PMP** | Predicate-Many-Preceders | Predicate read sees inconsistent set across repeated predicate scans | $T_1$ selects `WHERE value=30` (empty); $T_2$ inserts `(3, 30)` and commits; $T_1$ re-runs `WHERE value=30` (sees `(3, 30)`). | Snapshot Isolation / Serializable |
| **P4** | Lost Update | Read-modify-write overwrite (`T1` & `T2` read `1->10`, both write `1->11`) | $T_1$ reads `1` (`10`); $T_2$ reads `1` (`10`); $T_1$ updates `1 -> 11` and commits; $T_2$ updates `1 -> 11` and commits ($T_1$'s update lost). | Snapshot Isolation (first-committer-wins) / Repeatable Read (locking) |
| **G-single** | Read Skew (Single Anti-Dependency Cycle) | Cycle with **one** `rw` edge + one or more `ww`/`wr` edges | $T_1$ reads `id=1` (`10`); $T_2$ updates `1->12, 2->18` and commits; $T_1$ reads `id=2` (sees `18` instead of `20`). | Snapshot Isolation / Repeatable Read |
| **G2-item** | Item Write Skew (Item Anti-Dependency Cycle) | Cycle with $\ge 2$ item `rw` edges | $T_1$ reads `1, 2`; $T_2$ reads `1, 2`; $T_1$ updates `1 -> 11`; $T_2$ updates `2 -> 21`; both commit (disjoint writes based on stale shared reads). | Serializable (allowed by Snapshot Isolation!) |
| **G2** | Predicate Write Skew / Phantoms | Cycle with $\ge 2$ `rw` edges involving predicate reads | $T_1$ selects `WHERE value % 3 = 0`; $T_2$ selects `WHERE value % 3 = 0`; $T_1$ inserts `(3, 30)`; $T_2$ inserts `(4, 42)`; both commit. | Serializable (allowed by Snapshot Isolation!) |

*Reference point — SQLite isolation* ([`sqlite.org/isolation.html`](https://sqlite.org/isolation.html)): SQLite enforces `SERIALIZABLE` by default through single-writer database locking (only one writer executes at a time, eliminating concurrent write-skew `G2-item`/`G2`). In `WAL` mode, concurrent readers run under **Snapshot Isolation** against a fixed WAL frame snapshot while writers remain serialized (a `BEGIN DEFERRED` transaction that upgrades from read to write after another writer commits fails with `SQLITE_BUSY`). `READ UNCOMMITTED` is only available with shared cache (`PRAGMA read_uncommitted = ON`).

### 3.2 Complementing Hermitage with Elle-Style Black-Box Cycle Detection

Hermitage provides **deterministic unit witnesses** for known anomalies, whereas **Elle** ([`github.com/jepsen-io/elle`](https://github.com/jepsen-io/elle); [Kingsbury & Alvaro, *Elle: Inferring Isolation Anomalies from Experimental Observations*, PVLDB 2020](https://www.vldb.org/pvldb/vol13/p3046-kingsbury.pdf)) catches unexpected concurrency bugs under arbitrary interleavings:

- Clients execute randomized concurrent transactions performing `append(k, unique_val)` and `read(k)` over append-only lists (encoded in SQL as comma-delimited strings or child rows).
- Because every appended element is globally unique and list order exposes the exact version chain, a black-box checker can directly reconstruct `ww`, `wr`, and `rw` edges of the DSG from the observed history and run cycle detection (Tarjan's SCC / BFS) to flag `G0`, `G1a/b/c`, `G-single`, and `G2-item` violations with minimal counterexamples.
- Implementing a focused Go `list-append` Elle checker takes ~250–350 LOC and pairs well with multi-goroutine randomized stress tests under `go test -race`.

### 3.3 Encoding Deterministic Isolation Tests in Go (`-race`)

Flaky `time.Sleep`-based concurrency tests mask real bugs and fail sporadically in CI. In Go, encode Hermitage schedules deterministically using three techniques:

1. **Dedicated connections per transaction**: Never execute concurrent transactions through a bare `*sql.DB` without pinning connections; acquire explicit `*sql.Conn` (via `db.Conn(ctx)`) or direct engine `Tx`/`Session` handles for `T1`, `T2`, `T3` so connection pooling cannot silently serialize or reuse connections.
2. **Channel-driven step sequencer (lockstep interleaving)**:
   - Model each Hermitage test as an ordered slice of steps: `[]Step{{Tx: "T1", SQL: "..."}, {Tx: "T2", SQL: "..."}, ...}`.
   - Spawn one goroutine per transaction (`T1`, `T2`, ...). Each goroutine waits on its own `cmdCh`, executes one SQL operation, and reports `{rows, err}` on `resCh`.
   - For non-blocking steps (e.g., MVCC snapshot reads, optimistic writes, or engines returning immediate conflict errors), the coordinator sends the step and waits synchronously on `resCh`—giving **100% deterministic interleaving** with zero sleeps.
3. **Deterministic blocking verification via `testing/synctest`** ([`pkg.go.dev/testing/synctest`](https://pkg.go.dev/testing/synctest); [Go Blog: *Testing Time and Concurrency with `testing/synctest`*](https://go.dev/blog/synctest)):
   - In pessimistic locking / 2PL or lock-waiting engines, steps such as `T2` updating a row locked by `T1` are *expected to block* until `T1` commits or aborts.
   - Standard `testing/synctest` (experimental in Go 1.24 as `synctest.Run`, standard library in Go 1.25 as `synctest.Test(t, func(t *testing.T) { ... })`) runs goroutines inside an isolated **bubble** with a synthetic clock.
   - After dispatching a step expected to block on `T2`, call `synctest.Wait()`: it blocks until every other goroutine in the bubble is **durably blocked** (waiting on a Go channel, mutex/cond, or timer inside the bubble). Once `synctest.Wait()` returns, the test asserts `T2` has *not* completed yet, then advances `T1` to `COMMIT`/`ROLLBACK` and asserts `T2` unblocks with the expected outcome (success, serialization failure, or deadlock error).
   - `testing/synctest` is fully understood by Go's race detector (`go test -race`), so running the Hermitage matrix + Elle stress tests under `-race` simultaneously verifies **isolation semantics** and **Go memory-model race freedom** across the lock manager, page cache, and MVCC version chains.
   - *Design note for `fycsql`*: Ensure internal lock waits use bubble-compatible Go synchronization primitives (channels / `context` / timers rather than raw OS syscalls or `sync.Mutex` outside bubble awareness—note Go's `synctest` documentation specifies which primitives count as durably blocking, notably channel ops, `select`, `sync.WaitGroup.Wait`, `sync.Cond.Wait`, and `time.Sleep`).

---

## 4. Deterministic Fault-Injection VFS Design & Crash-Point Enumeration

### 4.1 Primary-Source Precedents

#### SQLite: `testing.html`, `atomiccommit.html`, `test_vfs.c`, and `crash.test`

SQLite achieves its crash-safety record through two complementary VFS-level mechanisms ([*How SQLite Is Tested*](https://sqlite.org/testing.html), [*Atomic Commit In SQLite*](https://sqlite.org/atomiccommit.html), [`src/test_vfs.c`](https://sqlite.org/src/file?name=src/test_vfs.c)):

- **Exhaustive I/O Error Injection (`test_vfs.c`)**: A VFS shim intercepts `xRead`, `xWrite`, `xSync`, `xTruncate`, `xOpen`, etc., counting operations and injecting an error (`SQLITE_IOERR`, `SQLITE_FULL`) on the $N$-th I/O operation. Tests loop $N = 1, 2, 3, \dots$ for both **single-failure** and **continuous-failure** modes, then disable fault injection and run `PRAGMA integrity_check` to guarantee zero corruption ([`sqlite.org/testing.html`](https://sqlite.org/testing.html)).
- **Power-Loss & Crash Simulation (`crash.test` & TH3)**:
  - **TCL harness (`test/crash.test`)**: Spawns a child process to run transactions, kills it mid-write, and uses a crash VFS that randomly **reorders and corrupts unsynchronized writes** between `xSync` calls, modeling real storage buffers and non-atomic sector writes (`xSectorSize`, `SQLITE_IOCAP_POWERSAFE_OVERWRITE`).
  - **TH3 embedded harness**: Uses an **in-memory VFS** that snapshots the entire filesystem after $N$ I/O operations, advancing $N$ iteratively across the workload, then reverts to each snapshot and introduces simulated power-loss sector damage before verifying atomic recovery ([`sqlite.org/testing.html`](https://sqlite.org/testing.html)).
  - **Sync-order auditing (`test_journal.c`)**: A shim observes writes and `xSync` calls across rollback journal and database files to verify durability ordering invariants directly (e.g., journal synced before database page overwrite).

#### CockroachDB Pebble: `vfs.MemFS`, `CrashClone`, and `vfs/errorfs`

CockroachDB's pure-Go Pebble storage engine demonstrates how to translate these VFS fault-injection patterns idiomatically into Go ([`pkg.go.dev/github.com/cockroachdb/pebble/vfs`](https://pkg.go.dev/github.com/cockroachdb/pebble/vfs), [`vfs/mem_fs.go`](https://github.com/cockroachdb/pebble/blob/master/vfs/mem_fs.go), [`pkg.go.dev/github.com/cockroachdb/pebble/vfs/errorfs`](https://pkg.go.dev/github.com/cockroachdb/pebble/vfs/errorfs)):

- **Narrow `vfs.FS` / `vfs.File` abstraction**: Every file and directory operation (`Create`, `Open`, `Remove`, `Rename`, `ReadAt`, `WriteAt`, `Sync`, and **directory `Sync`**) goes through `vfs.FS`.
- **Crashable in-memory filesystem (`vfs.NewCrashableMem` / `vfs.NewStrictMem`)**:
  - Maintains separate **live (buffered)** state and **synced (durable)** state for both file contents and directory entries. Reads see live writes, but mutations only promote to durable state upon explicit `File.Sync()` / `Dir.Sync()`.
  - `MemFS.ResetToSyncedState()` instantly discards all unsynced file writes, truncations, creations, and renames, simulating clean power loss.
  - `MemFS.CrashClone(vfs.CrashCloneCfg{UnsyncedDataPercent, RNG})` forks a crashed clone of the filesystem without mutating the active instance, selectively retaining a configurable fraction of unsynced data to test partial persistence and recovery.
- **Composable error & latency injector (`vfs/errorfs`)**:
  - Wraps any `vfs.FS` with declarative `Injector` rules combining predicates such as `errorfs.OnIndex(n)` (deterministic failure on the exact $n$-th matching operation), `errorfs.OpKind` matchers (`OpFileWrite`, `OpFileSync`, `OpFileRead`, `OpCreate`, etc.), filename/extension filters, custom error returns (`errorfs.ErrInjected`, `ENOSPC`), and `errorfs.RandomLatency` for I/O jitter in stress/metamorphic tests.

### 4.2 Recommended Fault-Injection VFS Architecture for `fycsql`

To get SQLite-grade crash verification with Pebble-grade Go ergonomics, `fycsql` should enforce a VFS boundary from the first storage commit:

#### 1. Minimal Storage Seam (`vfs.FS` & `vfs.File`)
Every storage component (pager, WAL, freelist, checkpoint, manifest) must perform all I/O through `vfs.FS`—never direct `os.OpenFile`/`os.Rename`—including explicit **`Dir.Sync()`** calls after creating, renaming, or unlinking WAL/DB files (POSIX does not guarantee directory entry durability without syncing the parent directory).

#### 2. `CrashFS` Dual-State + Pending-Write-Log Model
Implement an in-memory `CrashFS` maintaining:
- **Durable layer**: committed file byte slices + committed directory entry map (updated *only* on `File.Sync()` / `Dir.Sync()`).
- **Pending write log** (per file since its last `File.Sync()`): ordered slice of atomic write records `{opIdx, fileID, offset, oldData, newData}` plus `Truncate` records and pending directory operations `{Create, Rename, Remove}` since last `Dir.Sync()`.
- Live reads consult the live layer (durable + all pending ops); crash materialization constructs a fresh read-write clone from the durable layer plus a deterministic perturbation of the pending log.

#### 3. Four Mandatory Fault Models

| Fault Mode | What `CrashFS` / `ErrorFS` Simulates | Bugs Caught |
| :--- | :--- | :--- |
| **1. Dropped unsynced writes & dir ops** | Revert to `synced` layer (`ResetToSyncedState`), discarding all unsynced `WriteAt`, `Truncate`, `Create`, `Rename`, `Remove`. | Missing `File.Sync()` on WAL commit/checkpoint; missing `Dir.Sync()` after WAL creation or atomic file rename; premature ACK before `fsync`. |
| **2. Partial / torn writes** | Apply only a prefix of a pending write (e.g., split at 512-byte sector or mid-4KB page boundary), optionally zeroing or leaving stale/corrupt bytes in the unwritten tail of touched sectors (modeling non-PSOW hardware). | Missing page/WAL-frame checksums; relying on multi-sector write atomicity; partial WAL commit frame misinterpreted as valid. |
| **3. Out-of-order writes before `fsync`** | Apply a non-prefix subset of pending writes since the last `Sync()` (e.g., apply write $j$ while dropping earlier write $i < j$, or persist DB page overwrite while dropping unsynced WAL/journal frame). | Writing commit marker before payload without an intervening `fsync` or chained checksum; checkpoint overwriting DB pages before WAL is synced. |
| **4. Deterministic I/O errors (`ErrorFS`)** | Wrap `CrashFS` with `OnIndex(k)` returning `ENOSPC` / `EIO` on the $k$-th `Write`/`Sync`/`Read`/`Open` (single-shot and persistent modes). | Corrupted in-memory page cache after failed flush; leaked locks or file handles; broken rollback on disk-full mid-transaction. |

#### 4. Deterministic Crash-Point Enumeration Algorithm in `go test`

Instead of relying solely on random `kill -9` tests, enumerate every crash point deterministically in two passes:

```text
Pass 1 (Trace):
  Run workload W (e.g., multi-statement txn, rollback, WAL checkpoint, B-tree split/merge)
  on CrashFS with tracing enabled.
  Record:
    - Total crash-relevant I/O ops M (WriteAt, Truncate, Sync, Rename, Remove)
    - Logical oracle snapshots S_0, S_1, ..., S_T after each committed transaction t
    - The op index commitOp[t] at which Commit() returned nil to the caller

Pass 2 (Exhaustive Crash Enumeration):
  For each cut-point k in 0..M-1:
    Replay W cleanly up to op k (or snapshot at op k during Pass 1).
    Materialize deterministic crash images from state(k):
      (a) Clean power loss: durable state only (drop all pending ops)
      (b) Torn last write: durable + pending[0..last-1] + half-sector prefix of pending[last]
      (c) Reordered unsynced subset: durable + drop pending[0], keep pending[last] (when len(pending) >= 2)
    For each crash image:
      1. Open a fresh fycsql DB instance on the crashed CrashFS clone (triggers WAL/journal recovery).
      2. Run structural integrity verification (B-tree key order, page linkage, freelist accounting, checksums).
      3. Assert durability + atomicity invariant:
           - Recovered logical DB state MUST equal S_t for some t.
           - For every transaction t with commitOp[t] <= k, recovered state MUST include t (durability: committed txns never lost).
           - No partial transaction state is ever visible (atomicity).
      4. Execute one follow-up write transaction + commit on the recovered DB to verify the engine remains writable post-recovery.
```

Because `CrashFS` operates entirely in RAM with zero real `fsync` syscalls, enumerating hundreds of crash points across B-tree splits, WAL rollovers, and checkpoints completes in milliseconds inside `go test -race` and produces a 100% deterministic reproducer `(workload, cutPoint, faultMode, seed)` on failure.

---

## 5. Summary Verification Matrix for `fycsql`

| Verification Tier | Mechanism / Tooling | Primary Invariants Verified | CI Cadence |
| :--- | :--- | :--- | :--- |
| **SQL Logic Conformance** | In-repo Go `sqllogictest` runner over curated + upstream 622-file corpus | Query semantics, expressions, joins, aggregates, index-vs-scan equivalence | Curated subset on PR; full 7.2M corpus nightly |
| **Differential Fuzzing & Bench** | `modernc.org/sqlite` (pure-Go) in `go test -fuzz` & `go test -bench` | Planner/evaluator parity with SQLite on random SQL; pure-Go regression benchmarks | Short fuzz & bench on PR; extended fuzzing nightly |
| **Isolation & Concurrency** | Hermitage matrix (`G0`–`G2`) with `testing/synctest` + Go Elle DSG checker under `-race` | Formal isolation guarantees (SI / Serializable), deadlock/lock-wait behavior, zero Go data races | Every PR (`go test -race`) |
| **Crash & Fault Injection** | `CrashFS` (dual-layer + pending write log) + `ErrorFS` (`OnIndex`) exhaustive cut-point enumeration | Atomic commit, durability after `Commit()`, torn-write & reordered-write resilience, `ENOSPC`/`EIO` recovery | Every PR (core workloads); randomized `CrashClone` nightly |

---

## References (Primary Sources)

1. **SQLite `sqllogictest` Specification & Corpus**: [SQLite `sqllogictest` Design (`about.wiki`)](https://sqlite.org/sqllogictest/doc/trunk/about.wiki); [SQLite `sqllogictest` Fossil Repository](https://sqlite.org/sqllogictest/dir).
2. **Go `sqllogictest` Runners**: [DoltHub `sqllogictest`](https://github.com/dolthub/sqllogictest) (`github.com/dolthub/sqllogictest/go/logictest`); [CockroachDB `pkg/sql/logictest`](https://github.com/cockroachdb/cockroach/tree/master/pkg/sql/logictest).
3. **Pure-Go SQLite Oracle**: [`pkg.go.dev/modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite); [`gitlab.com/cznic/sqlite`](https://gitlab.com/cznic/sqlite); [`gitlab.com/cznic/ccgo`](https://gitlab.com/cznic/ccgo).
4. **Transaction Isolation & Concurrency Testing**: [Martin Kleppmann — Hermitage (`github.com/ept/hermitage`)](https://github.com/ept/hermitage); [SQLite Transaction Isolation (`sqlite.org/isolation.html`)](https://sqlite.org/isolation.html); [Kyle Kingsbury & Peter Alvaro — *Elle: Inferring Isolation Anomalies from Experimental Observations* (PVLDB 2020)](https://www.vldb.org/pvldb/vol13/p3046-kingsbury.pdf) & [`github.com/jepsen-io/elle`](https://github.com/jepsen-io/elle); [Go `testing/synctest` Package Docs](https://pkg.go.dev/testing/synctest) & [Go Blog: Testing Time and Concurrency with `testing/synctest`](https://go.dev/blog/synctest).
5. **Fault-Injection VFS & Crash Recovery Testing**: [SQLite — *How SQLite Is Tested* (`sqlite.org/testing.html`)](https://sqlite.org/testing.html); [SQLite — *Atomic Commit In SQLite* (`sqlite.org/atomiccommit.html`)](https://sqlite.org/atomiccommit.html); [SQLite `src/test_vfs.c`](https://sqlite.org/src/file?name=src/test_vfs.c); [CockroachDB Pebble `vfs` Package Docs](https://pkg.go.dev/github.com/cockroachdb/pebble/vfs) & [`vfs/mem_fs.go`](https://github.com/cockroachdb/pebble/blob/master/vfs/mem_fs.go); [CockroachDB Pebble `vfs/errorfs` Package Docs](https://pkg.go.dev/github.com/cockroachdb/pebble/vfs/errorfs).
