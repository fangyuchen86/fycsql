---
status: accepted
---

# Verification, Fault-Injection VFS, Isolation Testing, and Benchmark Architecture

fycsql continuously proves correctness and performance across four pure-Go `go test -race` harnesses within a single root `go.mod` (`go 1.27`), enforcing zero third-party runtime dependencies via CI (`go list -deps` over all non-test packages): (1) an in-repo ~300-LOC `sqllogictest` runner executing both curated upstream SQLite `.test` files and fycsql-native strict-typing/planner suites, paired with a `modernc.org/sqlite` differential fuzz oracle inside `integration/*_test.go`; (2) composable in-memory `CrashFS` (exhaustive 4-mode write/truncate/rename/sync cut-point enumeration) and `ErrorFS` (`OnIndex(k)` `EIO`/`ENOSPC` injection) VFS wrappers validated by `Verify()` and commit-prefix invariants; (3) a deterministic Hermitage SI anomaly matrix + Go 1.27 `testing/synctest` concurrency scheduling tests + randomized Elle `list-append` stress suite; and (4) a 5-category `benchstat` benchmark suite in `integration/*_test.go` comparing fycsql against `modernc.org/sqlite` (WAL + `STRICT`) and `go.etcd.io/bbolt` (plus optional `-tags cgo_bench` for `mattn/go-sqlite3`).

## Considered Options

- **Nested `integration/go.mod` Submodule for Test Dependencies**: Keeps the root `go.mod` free of `require` lines, but adds multi-module maintenance (`replace` directives, separate `go mod tidy` / `cd integration && go test` invocations, and split IDE/gopls workspaces). Because Go 1.17+ lazy module graph pruning prevents downstream consumers from downloading `*_test.go`-only dependencies, a single root `go.mod` paired with an automated `go list -deps` stdlib-only CI gate provides the same zero-runtime-dependency guarantee with far simpler ergonomics.
- **External `sqllogictest` Go Libraries (`dolthub/sqllogictest`, CockroachDB `logictest`)**: Avoids writing a ~300-LOC runner, but pulls heavy dependencies or MySQL/CockroachDB dialect coupling into the test layer.
- **`time.Sleep`-Based Concurrency & Real-Process `SIGKILL` Crash Tests**: Easy to write initially, but slow, non-deterministic, flaky under `-race`, and incapable of systematically hitting every torn-write or out-of-order `fsync` cut-point.

## Consequences

- **Zero-runtime-dependency core enforced in CI**: Fast PR gates (< 2 min) enforce `CGO_ENABLED=0 go build ./...`, `go vet ./...`, and a `go list -deps` check proving every non-test package (`.`, `./driver`, `./vfs`, `./cmd/...`, `./internal/...`) imports strictly Go stdlib, alongside single-command `go test -race ./...` covering unit tests, core `sqllogictest`, Hermitage, `testing/synctest`, fast `CrashFS`/`ErrorFS`, and differential smoke tests in `integration/`.
- **Deep exhaustive coverage in Extended/Nightly gates**: Scheduled or `-tags extended` runs execute the full curated `sqllogictest` corpus, exhaustive multi-fault `CrashFS` (clean power loss, partial/torn writes, out-of-order writes before `Sync()`, crash-during-recovery/checkpoint), long `modernc.org/sqlite` differential fuzzing (under ADR-0002's 5-point normalization contract), Elle stress testing, and `-count=10` `benchstat` regressions.
- **Explicit SI boundary verification**: Hermitage tests assert zero occurrences of `G0`, `G1a/b/c`, `OTV`, `PMP`, `P4`, and `G-single`, while explicitly asserting and documenting `G2-item` / `G2` write skew as the known Snapshot Isolation boundary from ADR-0001.
- **5-category benchmark visibility**: Benchmarks track ops/s, p50/p95/p99 latency, `B/op`, `allocs/op`, WAL bytes, and `fsync` count across point lookups (hot/cold), range/covering scans & external sort, single-writer txns (`SyncFull`/`SyncNormal`), concurrent multi-writer OLTP (disjoint vs hot-key contention), and cold crash-recovery / checkpoint tail latency.
