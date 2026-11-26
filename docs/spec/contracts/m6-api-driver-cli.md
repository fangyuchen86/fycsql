# M6 Contract — Root `fycsql`, `fycsql/driver`, and `cmd/fycsql`: Public Go API, Driver Adapter, and CLI

> **Parent Index**: [v1-module-contracts.md](../v1-module-contracts.md) · **Handoff**: [v1-architecture-handoff.md](../v1-architecture-handoff.md) · **Invariants**: [v1-invariants.md](../v1-invariants.md) · **Glossary**: [GLOSSARY.md](../../../GLOSSARY.md)
> **Related ADRs**: [ADR-0007](../../adr/0007-go-public-api-sql-driver-and-error-semantics.md), [ADR-0010](../../adr/0010-page1-header-versioning-stats-cli-and-package-layout.md)
> **Navigation**: ← [M5 — `internal/sql`](./m5-sql.md)

---

## 1. Depth & Responsibility

M6 exposes fycsql to external Go callers, `database/sql` users, and interactive CLI users across three cleanly layered packages:

- **Root `fycsql` (`github.com/fangyuchen86/fycsql`)**: Deep public facade owning the 1:1 engine assembly (`dbFS` + `tempFS` → `pager.Pager` → `catalog.Manager` [bootstrapped via a zero-alloc `pager.Snapshot → btree.PageReader` adapter] → `tx.Manager` → `sql.Engine`), native Go ergonomics (`Open`, `DB`, `Conn`, `Tx`, `Stmt`, `Rows`, `Result`, `View`, `Update`, `Checkpoint`, `Verify`, `Stats`, `WithWarningHook`, `CloseContext`, `NewConnector`), 3-layer `context.Context` cancellation, OCC retry loops, unified shutdown draining, and public `*fycsql.Error` + canonical sentinel re-exports.
- **`fycsql/driver` (`github.com/fangyuchen86/fycsql/driver`)**: Thin opt-in registration adapter (`init() { sql.Register("fycsql", ...) }`) implementing `driver.DriverContext.OpenConnector` and strict `file:<path>?...` DSN parsing delegating to `fycsql.NewConnector`.
- **`cmd/fycsql`**: Minimal pure-stdlib interactive & batch CLI built solely on top of root `fycsql` (`db.Conn(ctx)`), with an injectable `runCLI(ctx, db, in, out, errOut, cfg)` test seam so CLI tests can inject deterministic `ErrorFS` failures without spawning external hacks.

- **Package Dependencies**:
  - Root `fycsql` imports `database/sql/driver` (pure stdlib interfaces, zero pool registration), `vfs`, and `internal/{errs,val,pager,catalog,tx,sql}`.
  - `fycsql/driver` imports `database/sql`, `database/sql/driver`, and root `fycsql` (strictly one-way).
  - `cmd/fycsql` imports root `fycsql` and Go stdlib only.

---

## 2. Interface Surface

> Signatures below define the behavioral contract, not production code. All exported API types are concrete structs.

```text
// Root package fycsql
func Open(path string, opts ...Option) (*DB, error)
func NewConnector(path string, opts ...Option) (*Connector, error) // Implements driver.Connector & io.Closer

// Sentinel errors (exact re-exports of internal/errs sentinels) + structured *fycsql.Error
func IsRetryable(err error) bool // True ONLY for ErrConflict and ErrSchemaChanged; NEVER ErrCommitIndeterminate

// *DB (stateless, goroutine-safe; rejects raw SQL BEGIN/COMMIT/ROLLBACK & multi-statement SQL)
func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (Result, error)
func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error)
func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *Row
func (db *DB) PrepareContext(ctx context.Context, query string) (*Stmt, error)
func (db *DB) BeginTx(ctx context.Context, opts *TxOptions) (*Tx, error)
func (db *DB) View(ctx context.Context, fn func(*Tx) error) error
func (db *DB) Update(ctx context.Context, fn func(*Tx) error) error // Auto-retries IsRetryable up to MaxUpdateRetries (default 5)
func (db *DB) Conn(ctx context.Context) (*Conn, error)              // Stateful single-goroutine handle (supports SQL tx control)
func (db *DB) Checkpoint(ctx context.Context, mode CheckpointMode) (CheckpointStats, error)
func (db *DB) Verify(ctx context.Context) error
func (db *DB) Stats() Stats                                         // Zero-allocation point-in-time snapshot
func (db *DB) Close() error                                         // Delegates to CloseContext with CloseTimeout (default 5s)
func (db *DB) CloseContext(ctx context.Context) error

// *Conn (stateful, single-goroutine; holds at most one active *Tx; supports SQL BEGIN/COMMIT/ROLLBACK)
func (c *Conn) ExecContext(ctx context.Context, query string, args ...any) (Result, error)
func (c *Conn) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error)
func (c *Conn) PrepareContext(ctx context.Context, query string) (*Stmt, error)
func (c *Conn) Tables(ctx context.Context) ([]TableInfo, error)            // Snapshot-visible metadata for CLI .tables/.schema
func (c *Conn) TableSchema(ctx context.Context, name string) (string, error)
func (c *Conn) Close() error // Closes active rows and rolls back any uncommitted tx

// *Tx (explicit transaction; supports 3-layer context cancellation including explicit commit ctx)
func (tx *Tx) ExecContext(ctx context.Context, query string, args ...any) (Result, error)
func (tx *Tx) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error)
func (tx *Tx) PrepareContext(ctx context.Context, query string) (*Stmt, error)
func (tx *Tx) CommitContext(ctx context.Context) error // Honors ctx until atomic WAL append transition
func (tx *Tx) Commit() error                           // Delegates to CommitContext(tx.txCtx)
func (tx *Tx) Rollback() error

// *Stmt (bound to DB/Conn/Tx)
func (s *Stmt) ExecContext(ctx context.Context, args ...any) (Result, error)
func (s *Stmt) QueryContext(ctx context.Context, args ...any) (*Rows, error)
func (s *Stmt) Close() error

// *Connector
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error)
func (c *Connector) Driver() driver.Driver
func (c *Connector) DB() *DB
func (c *Connector) Close() error

// *Rows
func (r *Rows) Columns() []string
func (r *Rows) Next() bool
func (r *Rows) Scan(dest ...any) error // Safe-copies []byte by default
func (r *Rows) Values() []Value        // Borrowed zero-copy; valid until next Next()/Close()
func (r *Rows) Err() error
func (r *Rows) Close() error

// Observability (zero-allocation value struct per ADR-0010)
type Stats struct {
    BufferPoolHits, BufferPoolMisses, BufferPoolEvictions, PinnedPages uint64
    WALFrames, WALBytes                                                uint64
    WALWarnWatermarkExceeded                                           bool
    CheckpointCount                                                    uint64
    CheckpointTotalDuration                                            time.Duration
    ActiveSnapshots                                                    int64
    OldestSnapshotAge                                                  time.Duration
    CommitCount, ConflictCount                                         uint64
    SchemaVersion                                                      uint64
    TotalPages, FreelistPages                                          uint32
    FailStop                                                           bool
}
```

---

## 3. Behavioral Contracts & Error Modes

1. **1:1 Engine Ownership & Registered Driver Pooling (`ADR-0007`)**:
   - Every `Open` or `NewConnector` owns a dedicated engine instance (`:memory:` allocates an isolated private `MemFS`; duplicate same-process opens of the same file path fail with `ErrDatabaseLocked`).
   - `Open` wires two `vfs.FS` dependencies: `dbFS` (for `<db>` and `<db>-wal`) and a separate writable `tempFS` (for `internal/sql` spill runs, e.g., `OSFS` over `os.TempDir()` or a private `MemFS` for `:memory:`).
   - **Registered Driver Pooling (`driver.DriverContext`)**: `fycsql/driver` implements `driver.DriverContext.OpenConnector(dsn string) (driver.Connector, error)` delegating strict `file:<path>?...` DSN parsing to `fycsql.NewConnector`. Therefore, `sql.Open("fycsql", dsn)` creates ONE `*fycsql.Connector` per `*sql.DB` pool so all pooled connections share the same engine without hitting `ErrDatabaseLocked`, each `:memory:` `*sql.DB` pool stays isolated, and `(*sql.DB).Close()` closes the `Connector` via `io.Closer`. (`Driver.Open` is retained only for legacy interface satisfaction and delegates via a single-use connector closed on `Conn.Close`.)
2. **Three-Layer `context.Context`, Retries & Strict No-Replay (`ADR-0007`, `INV-WAL-05`)**:
   - `stmtCtx` cancellation discards only the active `StatementDelta` (leaving the enclosing `Tx` valid).
   - `txCtx` cancellation aborts the enclosing `Tx`.
   - `Tx.CommitContext(ctx)` cancels queued commits up to the atomic transition into WAL append; once append starts, it waits and returns the actual outcome.
   - `DB.Update` and standalone autocommit retry `IsRetryable(err)` (`ErrConflict` / `ErrSchemaChanged` only) up to `MaxUpdateRetries` / `AutocommitMaxRetries` (default `5`) with jittered exponential backoff respecting `ctx`; `Update` callbacks are documented as requiring idempotency.
   - Native helpers (`DB.Update`, autocommit) and `database/sql/driver` **strictly prohibit** retrying or replaying `ErrCommitIndeterminate`. `driver.Conn` MUST NEVER translate `ErrCommitIndeterminate` into `driver.ErrBadConn`.
3. **Honest `database/sql/driver` Contract & `io.EOF` Exception (`INV-API-01`, `ADR-0007`)**:
   - `Connector` implements `io.Closer`; `driver.Conn` implements `SessionResetter`, `Validator`, `Pinger`, `ConnBeginTx`, `ConnPrepareContext`, `ExecerContext`, `QueryerContext`, and `NamedValueChecker`.
   - `ConnBeginTx` accepts only `sql.LevelDefault`, `sql.LevelSnapshot`, and `sql.LevelRepeatableRead`, strictly enforces `opts.ReadOnly`, and rejects `time.Time`, `NaN`/`±Inf`, `uint64 > math.MaxInt64`, raw SQL `BEGIN`/`COMMIT`/`ROLLBACK`, multi-statement SQL, and unknown DSN parameters loudly.
   - `driver.Rows.Next` checks query `ctx.Err()` and returns **raw `io.EOF`** (never wrapped in `*fycsql.Error`) on end-of-rows per Go's `database/sql/driver` contract; implements `RowsColumnTypeScanType`/`DatabaseTypeName`/`Nullable`.
4. **Observability & Warning Hook (`ADR-0010`)**:
   - `DB.Stats()` returns a zero-allocation `Stats` value struct populated from atomic/lock-free counters.
   - `WithWarningHook(func(Warning))` is always invoked **outside all internal engine locks** (never while holding `CommitGate`, `WriteSerializeMu`, `TxRegistry`, `SnapshotRegistry`, or `BufferPool` locks) so callbacks may safely call `db.Stats()` or log without deadlocking; hook callbacks must be non-blocking.
5. **Unified Shutdown Drain Sequence (`ADR-0007`, `ADR-0010`)**:
   1. **Single Admission Cutoff**: `DB.CloseContext(ctx)` atomically marks `DB` closing (rejecting new operations with `ErrDatabaseClosed`) and calls `txMgr.StopAdmission()` (non-blocking).
   2. **Logical Drain**: Waits for active `Conn`/`Tx`/`Rows` and `txMgr.Drain(ctx)` within `ctx`; if `ctx` expires, force-cancels all tracked logical handle contexts (closing active `Rows`/`RowStream`/`RowIter` pins before releasing `Tx` `Snapshot`s) and aborts unsealed queued commits.
   3. **Physical Finalization (`pager.Close(ctx)`)**: Runs `CheckpointTruncate` + WAL removal + `SyncDir` on clean read-write drain; skips Checkpoint and preserves `<db>-wal` on `ReadOnly`, `FailStop`, or `CloseTimeout` while retaining FDs and process lock until the last physical operation exits.
   4. **Idempotency**: Repeated `Close`/`CloseContext` calls are idempotent and never double-close resources. Partial `Open` failures unwind all acquired resources in reverse order.
6. **Result & CLI Contract (`ADR-0007`, `ADR-0010`)**:
   - `Result.LastInsertId()` returns the `int64` Row Key for single-column `INTEGER` PKs and hidden Internal RowIDs, and `(0, ErrNoLastInsertId)` otherwise.
   - `cmd/fycsql` supports `-readonly`, `-sync`, `-format table|csv|json`, `-c`, multi-line SQL + `BEGIN`/`COMMIT`/`ROLLBACK`, and 8 dot-commands (`.help`, `.tables`, `.schema`, `.checkpoint`, `.verify`, `.stats`, `.mode`, `.quit`), using `Conn.Tables`/`Conn.TableSchema` for snapshot-visible metadata.
   - On EOF or `.quit`, `cmd/fycsql` explicitly calls `conn.Close()` (closing active rows and rolling back any open transaction on its `*Conn`) **before** calling `db.CloseContext` so shutdown does not stall waiting on the CLI's own active connection. Batch mode (`-c`) reports `ErrCommitIndeterminate` explicitly and exits nonzero.

---

## 4. Resource Ownership & Lifecycle

- `*DB` (or `*Connector`) is the root owner of the database engine and tracks active `Conn`, `Tx`, and `Rows` handles for logical cancellation on `CloseContext` timeout.
- `*Conn` is single-goroutine stateful and holds at most one active `*Tx`; `Conn.Close()` closes open rows and rolls back any uncommitted transaction.
- `*Rows` (and `driver.Rows`) owns one `*sql.RowStream` **and, for every autocommit query path (`DB.QueryContext`, `Conn.QueryContext` outside an explicit tx, `Stmt.QueryContext`, and `driver.Conn.QueryContext`), owns the implicit read-only `*tx.Tx`**, releasing both deterministically on EOF, iteration error, `ctx` cancellation, or early `Close()`. If query setup/binding/planning fails **before** `*Rows`/`driver.Rows` is returned, the query entry point MUST immediately release the implicit `*tx.Tx` before returning the error.
- `Rows.Scan` safe-copies `[]byte` by default; `RawBytes` / `Values()` return borrowed slices valid only until the next `Next()` or `Close()`.

---

## 5. Test Surface & Verification Contract

- Verified strictly through public `fycsql`, `database/sql` + `fycsql/driver`, and `cmd/fycsql`:
  - **Commit cancellation & exact attempt counting**:
    - Cancel one queued commit before append (known cancellation).
    - Cancel a second commit **after** WAL append starts when `Sync()` **succeeds** (verifying the real commit succeeds rather than returning a spurious `ErrCommitIndeterminate`).
    - Cancel/fail a third commit after WAL append starts on `ErrorFS` append/sync failure (`ErrCommitIndeterminate`): count execution attempts and assert **exact count `== 1`** (zero replay) across native `DB.Update`/autocommit, `database/sql` driver (no `ErrBadConn` replay), and `cmd/fycsql` batch mode (via `runCLI` over an `ErrorFS`-backed `*DB`, asserting nonzero exit).
  - **Driver pooling & `io.EOF` seam**: Test both `sql.Open("fycsql", dsn)` (`OpenConnector`) and `sql.OpenDB(NewConnector(...))` with two simultaneously checked-out connections on a file database (no `ErrDatabaseLocked`) plus two independent `:memory:` pools; verify `driver.Rows.Next` returns raw `io.EOF`, isolation gating, `opts.ReadOnly`, `time.Time`/`NaN` rejection, unknown DSN param rejection, and `Rows.Next` `ctx` cancellation.
  - **Autocommit `Rows` & Close lifecycle**: Verify early `Close()`, EOF, iteration error, and pre-`Rows` setup error all release the implicit read-only `Tx` (`Stats().ActiveSnapshots == 0`) across `DB.QueryContext`, `Conn.QueryContext`, `Stmt.QueryContext`, and `driver.Conn.QueryContext`; verify repeated `Close()`, partial `Open` unwind, and re-entrant `db.Stats()` inside `WithWarningHook`.
  - **CLI lifecycle**: Exit CLI via EOF and `.quit` with an active uncommitted transaction and open rows (verifying `conn.Close()` completes before `db.CloseContext` drains); test `.tables` and `.schema` against snapshot-visible metadata.
  - **Dependency gate**: CI `go list -deps` across `.`, `./driver`, `./vfs`, `./cmd/...`, `./internal/...` confirms zero third-party runtime dependencies (`INV-API-02`).
- **Invariants Enforced**: `INV-API-01`–`02`, `INV-WAL-05`.
