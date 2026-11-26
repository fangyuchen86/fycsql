---
status: accepted
---

# Verification, Fault-Injection VFS, Isolation Testing, and Nested Benchmark Module

fycsql continuously proves correctness and performance across four pure-Go `go test -race` harnesses while keeping the root `go.mod` strictly zero-dependency: (1) an in-repo ~300-LOC `sqllogictest` runner executing both curated upstream SQLite `.test` files and fycsql-native strict-typing/planner suites, paired with a `modernc.org/sqlite` differential fuzz oracle hosted inside a nested `integration/go.mod` submodule; (2) composable in-memory `CrashFS` (exhaustive 4-mode write/truncate/rename/sync cut-point enumeration) and `ErrorFS` (`OnIndex(k)` `EIO`/`ENOSPC` injection) VFS wrappers validated by `Verify()` and commit-prefix invariants; (3) a deterministic Hermitage SI anomaly matrix + Go 1.27 `testing/synctest` concurrency scheduling tests + randomized Elle `list-append` stress suite; and (4) a 5-category `benchstat` benchmark suite in `integration/` comparing fycsql against `modernc.org/sqlite` (WAL + `STRICT`) and `go.etcd.io/bbolt` (plus optional `-tags cgo_bench` for `mattn/go-sqlite3`).

## Considered Options

- **Declaring `modernc.org/sqlite` and `bbolt` in the Root `go.mod`**: Simpler single-module layout, but leaks third-party test dependencies into the root `go.mod` and downstream consumers' Go module graph, violating fycsql's strict stdlib-only core boundary.
- **External `sqllogictest` Go Libraries (`dolthub/sqllogictest`, CockroachDB `logictest`)**: Avoids writing a ~300-LOC runner, but pulls heavy dependencies or MySQL/CockroachDB dialect coupling into the test layer.
- **`time.Sleep`-Based Concurrency & Real-Process `SIGKILL` Crash Tests**: Easy to write initially, but slow, non-deterministic, flaky under `-race`, and incapable of systematically hitting every torn-write or out-of-order `fsync` cut-point.

## Consequences

- **Zero-dependency root module enforced in CI**: Fast PR gates (< 2 min) enforce `CGO_ENABLED=0 go build ./...` + `go vet ./...` on the root module alongside `-race` unit tests, core `sqllogictest`, Hermitage, `testing/synctest`, fast `CrashFS`/`ErrorFS`, and differential smoke tests in `integration/`.
- **Deep exhaustive coverage in Extended/Nightly gates**: Scheduled or `-tags extended` runs execute the full curated `sqllogictest` corpus, exhaustive multi-fault `CrashFS` (clean power loss, partial/torn writes, out-of-order writes before `Sync()`, crash-during-recovery/checkpoint), long `modernc.org/sqlite` differential fuzzing (under ADR-0002's 5-point normalization contract), Elle stress testing, and `-count=10` `benchstat` regressions.
- **Explicit SI boundary verification**: Hermitage tests assert zero occurrences of `G0`, `G1a/b/c`, `OTV`, `PMP`, `P4`, and `G-single`, while explicitly asserting and documenting `G2-item` / `G2` write skew as the known Snapshot Isolation boundary from ADR-0001.
- **5-category benchmark visibility**: Benchmarks track ops/s, p50/p95/p99 latency, `B/op`, `allocs/op`, WAL bytes, and `fsync` count across point lookups (hot/cold), range/covering scans & external sort, single-writer txns (`SyncFull`/`SyncNormal`), concurrent multi-writer OLTP (disjoint vs hot-key contention), and cold crash-recovery / checkpoint tail latency.
