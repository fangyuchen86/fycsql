# Go `database/sql/driver` Contracts and Embedded Connection Pooling

Research findings for **fycsql** on Go's `database/sql` and `database/sql/driver` contracts, connection pool mechanics with embedded single-writer / multi-reader databases, `TxOptions` and `context.Context` cancellation semantics, type conversion rules, and architectural lessons from `mattn/go-sqlite3` and `modernc.org/sqlite`.

All claims below are verified against primary sources: Go standard library source (`golang/go` `src/database/sql/`), official Go package docs (`pkg.go.dev/database/sql` and `pkg.go.dev/database/sql/driver`), official SQLite documentation (`sqlite.org`), and primary source code of `mattn/go-sqlite3` and `modernc.org/sqlite`.

---

## 1. Required vs. Optional `database/sql/driver` Interfaces

`database/sql` assumes a `driver.Conn` and its `driver.Stmt`s are stateful and **never used concurrently by multiple goroutines** ([driver.go#L94-L95](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L94-L95), [driver.go#L231-L234](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L231-L234), [driver.go#L329-L330](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L329-L330)). `sql.DB` enforces this per connection with `driverConn.Mutex`.

### Core Required Interfaces

| Interface | Implemented By | Methods | Contract & Notes |
| --- | --- | --- | --- |
| `driver.Driver` | Driver | `Open(name string) (Conn, error)` | Required for `sql.Register` + `sql.Open`. Must **never** return `driver.ErrBadConn` ([driver.go#L12](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L12), [driver.go#L86-L97](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L86-L97)). |
| `driver.Conn` | Connection | `Prepare(query string) (Stmt, error)`<br>`Close() error`<br>`Begin() (Tx, error)` | `Begin()` is deprecated in favor of `ConnBeginTx` ([driver.go#L235-L256](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L235-L256)). |
| `driver.Stmt` | Statement | `Close() error`<br>`NumInput() int`<br>`Exec([]Value) (Result, error)`<br>`Query([]Value) (Rows, error)` | `Exec`/`Query` are deprecated in favor of `StmtExecContext`/`StmtQueryContext`. Return `-1` from `NumInput()` to skip `database/sql` placeholder-count sanity checks ([driver.go#L331-L363](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L331-L363)). |
| `driver.Tx` | Transaction | `Commit() error`<br>`Rollback() error` | Must leave the connection out of any active transaction upon return, even if `Commit()` fails ([driver.go#L541-L544](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L541-L544)). |
| `driver.Result` | Exec result | `LastInsertId() (int64, error)`<br>`RowsAffected() (int64, error)` | Stdlib provides helpers `driver.RowsAffected(n)` and `driver.ResultNoRows` for DDL ([driver.go#L318-L327](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L318-L327), [driver.go#L546-L575](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L546-L575)). |
| `driver.Rows` | Result cursor | `Columns() []string`<br>`Close() error`<br>`Next(dest []Value) error` | `Next` populates `dest` (length == `len(Columns())`) and returns `io.EOF` when exhausted ([driver.go#L425-L441](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L425-L441)). |

### Recommended / Optional Interfaces

The package doc explicitly recommends modern drivers implement **`DriverContext`, `Connector`, `Pinger`, `SessionResetter`, `Validator`, `ExecerContext`, `QueryerContext`, `ConnPrepareContext`, `ConnBeginTx`**, and **`NamedValueChecker`** ([driver.go#L10-L31](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L10-L31)).

| Interface | Implemented By | Methods | Why It Matters for `fycsql` |
| --- | --- | --- | --- |
| `driver.DriverContext` | Driver | `OpenConnector(name string) (Connector, error)` | Called once by `sql.Open` so DSN parsing and engine initialization happen **once per `*sql.DB`**, not per pooled connection ([driver.go#L99-L109](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L99-L109), [sql.go#L876-L882](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L876-L882)). |
| `driver.Connector` (+ `io.Closer`) | Connector | `Connect(context.Context) (Conn, error)`<br>`Driver() Driver`<br>*(optional)* `Close() error` | Can be passed directly to `sql.OpenDB(c)` without global `sql.Register`. **Crucially**, if `Connector` implements `io.Closer`, `sql.DB.Close()` invokes `Connector.Close()` ([driver.go#L111-L143](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L111-L143), [sql.go#L956-L961](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L956-L961)). Ideal owner for an embedded DB engine handle. Must **never** return `ErrBadConn`. |
| `driver.ConnBeginTx` | Conn | `BeginTx(ctx, TxOptions) (Tx, error)` | Required to support `context.Context`, `TxOptions.Isolation`, and `TxOptions.ReadOnly`. Without it, `database/sql` errors on any non-default `Isolation` or `ReadOnly: true` ([ctxutil.go#L97-L119](https://github.com/golang/go/blob/master/src/database/sql/ctxutil.go#L97-L119)). |
| `driver.ConnPrepareContext` | Conn | `PrepareContext(ctx, query) (Stmt, error)` | Context-aware prepare. `ctx` is strictly for preparation and **must not** be stored on the returned `Stmt` ([driver.go#L259-L264](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L259-L264)). |
| `driver.ExecerContext` | Conn | `ExecContext(ctx, query, []NamedValue) (Result, error)` | Fast path avoiding prepare+exec+close round-trip; supports named args and context cancellation. May return `driver.ErrSkip` to fall back to prepare+exec ([driver.go#L190-L202](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L190-L202), [sql.go#L1705-L1737](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1705-L1737)). |
| `driver.QueryerContext` | Conn | `QueryContext(ctx, query, []NamedValue) (Rows, error)` | Fast path for queries; supports named args and context cancellation. May return `driver.ErrSkip` to fall back to prepare+query ([driver.go#L217-L229](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L217-L229), [sql.go#L1779-L1839](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1779-L1839)). |
| `driver.StmtExecContext` / `driver.StmtQueryContext` | Stmt | `ExecContext(ctx, []NamedValue)`<br>`QueryContext(ctx, []NamedValue)` | Context-aware and named-parameter-capable execution of prepared statements ([driver.go#L365-L381](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L365-L381), [ctxutil.go#L63-L95](https://github.com/golang/go/blob/master/src/database/sql/ctxutil.go#L63-L95)). Without these, named parameters fail with `"driver does not support the use of Named Parameters"` ([ctxutil.go#L137-L146](https://github.com/golang/go/blob/master/src/database/sql/ctxutil.go#L137-L146)). |
| `driver.Pinger` | Conn | `Ping(ctx) error` | Invoked by `DB.PingContext`. Returning `ErrBadConn` removes the connection from the pool ([driver.go#L166-L175](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L166-L175), [sql.go#L887-L896](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L887-L896)). |
| `driver.SessionResetter` | Conn | `ResetSession(ctx) error` | Called before reusing a previously used connection; return `ErrBadConn` to discard it ([driver.go#L297-L304](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L297-L304), [sql.go#L601-L612](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L601-L612)). |
| `driver.Validator` | Conn | `IsValid() bool` | Called before returning a connection to the pool (`putConn`); returning `false` discards it ([driver.go#L306-L315](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L306-L315), [sql.go#L616-L627](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L616-L627), [sql.go#L1486-L1491](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1486-L1491)). **Implementing both `SessionResetter` and `Validator` is required to prevent `database/sql` from discarding connections on canceled transactions** (see §3). |
| `driver.NamedValueChecker` | Stmt or Conn | `CheckNamedValue(*NamedValue) error` | Intercepts parameter validation/conversion before `DefaultParameterConverter`; supports custom Go types and `ErrRemoveArgument` / `ErrSkip` ([driver.go#L389-L409](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L389-L409)). |
| `driver.RowsColumnType*` | Rows | `ColumnTypeScanType`, `ColumnTypeDatabaseTypeName`, `ColumnTypeLength`, `ColumnTypeNullable`, `ColumnTypePrecisionScale` | Powers `(*sql.Rows).ColumnTypes()` metadata introspection ([driver.go#L484-L538](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L484-L538), [sql.go#L3355-L3371](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L3355-L3371)). |
| `driver.RowsNextResultSet` | Rows | `HasNextResultSet() bool`<br>`NextResultSet() error` | Supports multi-result-set queries ([driver.go#L468-L482](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L468-L482)). |
| `driver.RowsColumnScanner` *(Go 1.27+)* | Rows | `NextRow() error`<br>`ScanColumn(ScanContext, index int, dest any) error` | Optional zero-boxing direct scanning introduced in Go 1.27; supersedes `Rows.Next` when implemented ([driver.go#L447-L466](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L447-L466), [sql.go#L3128-L3135](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L3128-L3135), [sql.go#L3480-L3493](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L3480-L3493)). |

### Error Sentinel Contract (`ErrBadConn`, `ErrSkip`, `ErrRemoveArgument`)

- **`driver.ErrBadConn`**: Checked via `errors.Is` ([driver.go#L162-L163](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L162-L163)). Signals a broken connection so `sql.DB` discards it and automatically retries up to `maxBadConnRetries` (2 times with `cachedOrNewConn`, then 1 final attempt with `alwaysNewConn` — 3 total attempts) ([sql.go#L1577-L1589](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1577-L1589)). **Never** return `ErrBadConn` from `Open`/`Connect` or if the operation might already have executed, to avoid silent duplicate writes ([driver.go#L12-L14](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L12-L14), [driver.go#L157-L160](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L157-L160)).
- **`driver.ErrSkip`**: Supported only where documented (`ExecerContext`/`Execer`, `QueryerContext`/`Queryer`, `NamedValueChecker`) to fall back to the default path ([driver.go#L145-L150](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L145-L150)).
- **`driver.ErrRemoveArgument`**: Returned from `NamedValueChecker.CheckNamedValue` to strip non-SQL option arguments before `NumInput` verification and execution ([driver.go#L383-L387](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L383-L387), [convert.go#L186-L188](https://github.com/golang/go/blob/master/src/database/sql/convert.go#L186-L188), [convert.go#L204-L206](https://github.com/golang/go/blob/master/src/database/sql/convert.go#L204-L206)).

---

## 2. `database/sql` Connection Pool Semantics & Embedded DB Pitfalls

### Exact Built-In Pool Mechanics (`sql.DB`)

1. **Lazy opening & defaults**: `sql.Open` / `sql.OpenDB` does not open any connections eagerly ([sql.go#L837-L885](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L837-L885)). Defaults are:
   - `MaxOpenConns = 0` (unlimited open connections) ([sql.go#L1032-L1039](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1032-L1039))
   - `MaxIdleConns = 2` (`defaultMaxIdleConns = 2`) ([sql.go#L965-L978](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L965-L978))
   - `ConnMaxLifetime = 0`, `ConnMaxIdleTime = 0` (no time-based eviction) ([sql.go#L1047-L1074](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1047-L1074))
   - Reducing `MaxOpenConns` below `MaxIdleConns` automatically lowers `MaxIdleConns` to match ([sql.go#L1040-L1044](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1040-L1044)).
2. **LIFO idle reuse**: `db.conn()` pops the **most recently returned** idle connection (`db.freeConn[len-1]`) so idle-time eviction can close older unused connections ([sql.go#L1337-L1343](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1337-L1343)).
3. **Random-order wait queue at `MaxOpenConns`**: When `db.maxOpen > 0 && db.numOpen >= db.maxOpen`, callers enqueue a buffered channel into `db.connRequests` and block in `select` on `<-ctx.Done()` vs `<-req` ([sql.go#L1363-L1432](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1363-L1432)). On return (`putConnDBLocked`), `sql.DB` wakes a **random** waiter (`db.connRequests.TakeRandom()`), **not FIFO** ([sql.go#L1554-L1562](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1554-L1562), [sql.go#L3764-L3772](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L3764-L3772)).
4. **Validation & session reset lifecycle**:
   - On `putConn(dc, err, resetSession)`: `dc.validateConnection(resetSession)` marks `dc.needReset = true` (when `resetSession` is true) and calls `Validator.IsValid()`. If `false`, `dc` is closed and discarded ([sql.go#L616-L627](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L616-L627), [sql.go#L1486-L1525](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1486-L1525)).
   - On checkout (`conn()` from `freeConn` or waiter handoff): if `dc.needReset` is true, `SessionResetter.ResetSession(ctx)` is called; returning `ErrBadConn` discards the connection ([sql.go#L601-L612](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L601-L612), [sql.go#L1352-L1356](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1352-L1356), [sql.go#L1425-L1429](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1425-L1429)). Note that `IsValid` is **not** called while a connection stays pinned (e.g., inside `*sql.Conn` or active `*sql.Tx`), so errors that poison a connection (such as a failed `Rollback`) should also wrap `driver.ErrBadConn` ([driver.go#L33-L38](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L33-L38), [mattn/go-sqlite3/sqlite3.go#L767-L777](https://github.com/mattn/go-sqlite3/blob/master/sqlite3.go#L767-L777)).
5. **Pinning rules**:
   - `*sql.Tx` pins one `driverConn` from `BeginTx` until `Commit`/`Rollback` ([sql.go#L1897-L1934](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1897-L1934), [sql.go#L2279-L2283](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L2279-L2283)).
   - `*sql.Rows` from `db.QueryContext` pins one `driverConn` until `Rows.Close()` or EOF ([sql.go#L1800-L1808](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1800-L1808), [sql.go#L3104-L3106](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L3104-L3106)). (Inside a `*sql.Tx`, `Rows` borrows `tx.closemu.RLock()`, preventing `Commit`/`Rollback` until `Rows` closes.)
   - **Self-deadlock trap with `SetMaxOpenConns(1)`**: If a goroutine iterates `*sql.Rows` from `db.Query` and calls `db.Exec` inside the loop while `MaxOpenConns == 1`, it deadlocks waiting for the single connection held by `Rows`!

### Why SQLite Drivers Hit `SQLITE_BUSY` and Resort to `SetMaxOpenConns(1)` or Dual Pools

In `mattn/go-sqlite3` and `modernc.org/sqlite`, each `driver.Conn` opens an independent SQLite `sqlite3*` handle. This collides with `database/sql`'s default multi-connection pool in four concrete ways:

1. **Multi-connection write contention**: `sql.DB` opens unlimited connections by default and dispatches concurrent goroutines across them. SQLite permits only **one writer at a time** ([SQLite WAL docs](https://www.sqlite.org/wal.html)); with default `busy_timeout = 0`, concurrent writes on separate connections immediately fail with `SQLITE_BUSY` ([mattn/go-sqlite3#209](https://github.com/mattn/go-sqlite3/issues/209), [mattn/go-sqlite3#274](https://github.com/mattn/go-sqlite3/issues/274)).
2. **Deferred-transaction upgrade trap (bypasses `busy_timeout`)**: SQLite's default `BEGIN` is `BEGIN DEFERRED`, acquiring no write lock until the first DML statement ([SQLite Transaction docs](https://www.sqlite.org/lang_transaction.html)). If two connections start deferred transactions, both read (taking SHARED/snapshot locks), and then both attempt to write, SQLite **cannot wait via `busy_timeout`** without risking deadlock ("deadly embrace") and immediately fails the upgrader with `SQLITE_BUSY` / `SQLITE_BUSY_SNAPSHOT` ([SQLite Transaction docs](https://www.sqlite.org/lang_transaction.html), [SQLite WAL docs](https://www.sqlite.org/wal.html)). Using `BEGIN IMMEDIATE` (`_txlock=immediate` in both [mattn/go-sqlite3/sqlite3.go#L1482-L1491](https://github.com/mattn/go-sqlite3/blob/master/sqlite3.go#L1482-L1491) and [modernc.org/sqlite/sqlite.go#L426-L432](https://gitlab.com/cznic/sqlite/-/blob/master/sqlite.go#L426-L432)) acquires the RESERVED lock at `BEGIN` so `busy_timeout` is respected.
3. **Why `SetMaxOpenConns(1)` vs. Dual Pools**:
   - **Single-connection pool (`db.SetMaxOpenConns(1)`)**: Recommended in `mattn/go-sqlite3`'s FAQ ([mattn/go-sqlite3 README](https://github.com/mattn/go-sqlite3/blob/master/README.md#L555-L572)) to serialize all access inside Go's `sql.DB` pool and avoid `SQLITE_BUSY`. Downside: **all reads are also serialized behind writes** (and nested `db.Exec` during open `db.Query` deadlocks).
   - **Dual pools**: For read-heavy workloads, applications open two separate `*sql.DB` handles to the same WAL-mode database — a **writer `*sql.DB`** (`SetMaxOpenConns(1)`, `_txlock=immediate`, `busy_timeout`) and a **reader `*sql.DB`** (`mode=ro`, `SetMaxOpenConns(N)`) ([mattn/go-sqlite3#1022](https://github.com/mattn/go-sqlite3/issues/1022), [mattn/go-sqlite3#1179](https://github.com/mattn/go-sqlite3/issues/1179)).
   - **Avoid `cache=shared`**: Although `mattn/go-sqlite3`'s legacy FAQ mentions `cache=shared`, SQLite's own documentation explicitly discourages shared-cache mode in favor of WAL ([SQLite Shared-Cache docs](https://www.sqlite.org/sharedcache.html)).
   - **PRAGMA ordering**: Because `sql.DB` opens new connections lazily on demand, connection-scoped pragmas (`busy_timeout`, `foreign_keys`, `synchronous`) must be applied in the DSN or `Connector`/`RegisterConnectionHook` — and `busy_timeout` must be applied *first* so subsequent write-capable pragmas (like `journal_mode=WAL`) don't fail with `SQLITE_BUSY` ([modernc.org/sqlite/sqlite.go#L455-L497](https://gitlab.com/cznic/sqlite/-/blob/master/sqlite.go#L455-L497)).
4. **In-memory (`:memory:`) pool hazards**:
   - With `sql.Open("sqlite3", ":memory:")`, opening a second pooled connection creates a **brand-new empty database**, causing racy `"no such table"` errors; closing the last idle connection destroys the database ([mattn/go-sqlite3 README](https://github.com/mattn/go-sqlite3/blob/master/README.md#L522-L538), [SQLite In-Memory DB docs](https://www.sqlite.org/inmemorydb.html)).
   - In `modernc.org/sqlite`, `conn.usable()` explicitly special-cases `c.inMemory` to return `true` even after an interrupt so `database/sql` won't discard the connection and wipe the in-memory database ([modernc.org/sqlite/conn.go#L1143-L1156](https://gitlab.com/cznic/sqlite/-/blob/master/conn.go#L1143-L1156)).

### How a Pure-Go Custom Embedded Driver (`fycsql`) Can Avoid These Pitfalls on a Single `*sql.DB`

Because `fycsql` controls both the Go driver and the storage/transaction engine in-process, it does **not** need to inherit SQLite's C/file-lock pooling limitations:

1. **Own the shared engine instance in `driver.Connector` (implementing `io.Closer`)**:
   - `DriverContext.OpenConnector(dsn)` (and a public `fycsql.NewConnector(cfg)` for `sql.OpenDB`) creates or attaches to a single shared in-process `Engine` (holding page cache, WAL, catalog, and lock manager).
   - Each `Connector.Connect(ctx)` returns a lightweight `driver.Conn` session (holding per-session state, active tx pointer, optional stmt cache) backed by the shared `Engine`.
   - `sql.DB.Close()` invokes `Connector.Close()` ([sql.go#L956-L961](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L956-L961)), cleanly checkpointing/closing files.
   - **Bonus for `:memory:`**: Because the `Engine` lives on the `Connector`, multiple pooled connections see the **same** in-memory database, and idle connection eviction or canceled-tx discard never wipes data until `sql.DB.Close()`.
2. **In-process, context-aware writer serialization + lock-free concurrent readers**:
   - Keep `sql.DB`'s normal multi-connection pool (no `SetMaxOpenConns(1)`, no dual-pool split needed).
   - **Readers** (autocommit `SELECT`s and `BeginTx(ctx, &sql.TxOptions{ReadOnly: true})`) acquire an immutable WAL/MVCC read snapshot concurrently without touching the write lock.
   - **Writers** acquire a single per-engine **channel-based write semaphore** (`writeGate chan struct{}` of cap 1) with `select` on `ctx.Done()`:
     ```go
     select {
     case e.writeGate <- struct{}{}:
     case <-ctx.Done():
         return ctx.Err()
     }
     ```
     This replaces SQLite's sleeping `busy_timeout` polling loop ([modernc.org/sqlite/busy.go#L23-L89](https://gitlab.com/cznic/sqlite/-/blob/master/busy.go#L23-L89)) with zero-spin Go scheduler queueing and instant context cancellation — eliminating spurious `SQLITE_BUSY` errors inside a single process.
3. **Eliminate mid-transaction read-to-write upgrade deadlocks**:
   - `BeginTx(ctx, TxOptions{ReadOnly: true})`: acquire read snapshot only; reject any write statement immediately with a read-only error.
   - `BeginTx(ctx, TxOptions{ReadOnly: false})`: eagerly acquire `writeGate` in `BeginTx` (`BEGIN IMMEDIATE` semantics), respecting `ctx.Done()`. Writers queue cleanly and never fail mid-transaction on lock upgrade.
   - Autocommit `ExecContext` / DML acquires `writeGate` only around the single statement; autocommit `QueryContext` of read-only queries takes only a read snapshot.
   - *(Note on multi-process access)*: If `fycsql` ever supports concurrent OS processes on the same file, OS file locking (`flock`/OFD) only needs to wrap the single in-process `writeGate` holder, not every goroutine.

---

## 3. `sql.TxOptions` and `context.Context` Cancellation Contracts

### `sql.TxOptions` (`Isolation` and `ReadOnly`)

`sql.IsolationLevel` defines 8 values: `LevelDefault` (0), `LevelReadUncommitted`, `LevelReadCommitted`, `LevelWriteCommitted`, `LevelRepeatableRead`, `LevelSnapshot`, `LevelSerializable`, and `LevelLinearizable` ([sql.go#L124-L139](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L124-L139)).

`driver.ConnBeginTx.BeginTx(ctx, opts)` carries a **strict rejection contract** ([driver.go#L286-L294](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L286-L294)):
- **Must check `opts.Isolation`**: If a non-default isolation level is requested and not supported by the engine, `BeginTx` **must return an error** (not silently downgrade).
- **Must check `opts.ReadOnly`**: If `true`, either enforce read-only semantics or return an error if unsupported.

> **Lesson from SQLite drivers**: Neither `mattn/go-sqlite3` ([sqlite3_sql.go#L57-L59](https://github.com/mattn/go-sqlite3/blob/master/sqlite3_sql.go#L57-L59)) nor `modernc.org/sqlite` ([tx.go#L23-L35](https://gitlab.com/cznic/sqlite/-/blob/master/tx.go#L23-L35)) validates `opts.Isolation` (and `mattn/go-sqlite3` ignores `opts.ReadOnly` as well, whereas `modernc.org/sqlite` uses `!opts.ReadOnly` only to decide whether to apply `_txlock`). `fycsql` should strictly validate `opts.Isolation` (accepting `LevelDefault` and the exact levels `fycsql` guarantees, e.g., `LevelSerializable` / `LevelSnapshot`, rejecting others) and enforce `opts.ReadOnly`.

### `context.Context` Semantics Across Operations

| Operation | Context Lifetime & Cancellation Contract | Subtle Pitfalls Verified in Stdlib / Drivers |
| --- | --- | --- |
| `Connector.Connect(ctx)` | Dial/open only; **do not store** `ctx` on `Conn` ([driver.go#L129-L133](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L129-L133)). | Pool may call `Connect` asynchronously via `connectionOpener` with a background context ([sql.go#L846](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L846)). |
| `ConnPrepareContext.PrepareContext(ctx, q)` | Prepare only; **must not store** `ctx` on `Stmt` ([driver.go#L260-L263](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L260-L263)). | If unimplemented, `ctxDriverPrepare` falls back to `Prepare`, then checks `ctx.Done()` *after* it finishes and closes the `Stmt` ([ctxutil.go#L13-L27](https://github.com/golang/go/blob/master/src/database/sql/ctxutil.go#L13-L27)). |
| `ExecContext` / `QueryContext` (Conn & Stmt) | Must honor timeout/cancellation promptly ([driver.go#L199](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L199), [driver.go#L226](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L226), [driver.go#L370](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L370), [driver.go#L379](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L379)). | Both `mattn/go-sqlite3` and `modernc.org/sqlite` must carefully guard `sqlite3_interrupt` with a mutex/flag so a late cancellation cannot interrupt a *subsequent* statement on the same connection ([modernc.org/sqlite/sqlite.go#L83-L140](https://gitlab.com/cznic/sqlite/-/blob/master/sqlite.go#L83-L140), [mattn/go-sqlite3/sqlite3.go#L550-L552](https://github.com/mattn/go-sqlite3/blob/master/sqlite3.go#L550-L552), [L2789-L2803](https://github.com/mattn/go-sqlite3/blob/master/sqlite3.go#L2789-L2803)). In pure-Go `fycsql`, pass `ctx` directly into query operators and check `ctx.Err()` periodically (e.g., every N rows/pages) — zero race with future queries. |
| `ConnBeginTx.BeginTx(ctx, opts)` | `ctx` stays active for the **entire lifetime of the transaction** until `Commit` or `Rollback` ([sql.go#L1868-L1871](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1868-L1871)). | 1. `beginDC` spawns `go tx.awaitDone()`. When `ctx` cancels, `awaitDone` calls `tx.rollback(discardConnection)` where `discardConnection = !(hasSessionResetter && hasValidator)` ([sql.go#L1908-L1933](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L1908-L1933), [sql.go#L2253-L2266](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L2253-L2266)). **Implement both `SessionResetter` and `Validator` to avoid discarding healthy connections on tx cancel.**<br>2. `Tx.Commit()` checks `tx.ctx.Done()` first and returns `tx.ctx.Err()` if canceled ([sql.go#L2335-L2342](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L2335-L2342)).<br>3. `driver.Tx.Commit()` may itself observe `txCtx` (as `modernc.org/sqlite` does in [tx.go#L45-L64](https://gitlab.com/cznic/sqlite/-/blob/master/tx.go#L45-L64)), whereas `driver.Tx.Rollback()` **must NOT** fail due to `txCtx` being canceled — it is called *because* `txCtx` was canceled ([tx.go#L66-L73](https://gitlab.com/cznic/sqlite/-/blob/master/tx.go#L66-L73)).<br>4. If `Commit()` returns an error, `database/sql` assumes the tx is over and returns the `Conn` to the pool ([sql.go#L2356-L2363](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L2356-L2363)). The driver **must** roll back any lingering tx state before returning from a failed `Commit()` (see [mattn/go-sqlite3/sqlite3.go#L751-L764](https://github.com/mattn/go-sqlite3/blob/master/sqlite3.go#L751-L764), [modernc.org/sqlite/tx.go#L51-L62](https://gitlab.com/cznic/sqlite/-/blob/master/tx.go#L51-L62)). |
| `driver.Rows.Next(dest)` | `driver.Rows.Next` takes **no `context.Context`**, yet queries must still cancel during row iteration. | `queryDC` spawns `go rs.awaitDone(ctx, txctx, closectx)` which calls `rs.close(ctx.Err())` on cancellation ([sql.go#L3046-L3080](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L3046-L3080)). **Critical**: `sql.Rows.Next()` holds `rs.closemu.RLock()` and `rs.dc.Lock()` while calling `rowsi.Next(...)` ([sql.go#L3099-L3135](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L3099-L3135)), so `awaitDone` **blocks** waiting for `rs.closemu.Lock()` until `rowsi.Next` returns! Therefore, the driver's `Rows` **must store the query `ctx`** from `QueryContext` and check `ctx.Err()` / `ctx.Done()` inside `Next` (just as `mattn/go-sqlite3` does with `context.AfterFunc` in [sqlite3.go#L639](https://github.com/mattn/go-sqlite3/blob/master/sqlite3.go#L639), [L3020-L3039](https://github.com/mattn/go-sqlite3/blob/master/sqlite3.go#L3020-L3039) and `modernc.org/sqlite` does in [rows.go#L52](https://gitlab.com/cznic/sqlite/-/blob/master/rows.go#L52), [L265-L275](https://gitlab.com/cznic/sqlite/-/blob/master/rows.go#L265-L275)). |

---

## 4. Type Conversion Rules (`driver.Value` vs. `NamedValueChecker`)

### Canonical `driver.Value` Types

`driver.Value` is `any`, restricted to `nil`, a type accepted by a driver's `NamedValueChecker`, or one of these core types ([driver.go#L48-L63](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L48-L63), [types.go#L180-L192](https://github.com/golang/go/blob/master/src/database/sql/driver/types.go#L180-L192)):
- `int64`
- `float64`
- `bool`
- `[]byte`
- `string`
- `time.Time`
- `nil` (SQL `NULL`)
- *(special)* `driver.Rows` (returned cursor value) or an internal `decimalDecompose` interface.

### Parameter Conversion Pipeline (`driverArgsConnLocked`)

Before invoking `ExecContext`/`QueryContext`, `database/sql` runs every argument through a strict precedence chain ([driver.go#L393-L403](https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go#L393-L403), [convert.go#L111-L209](https://github.com/golang/go/blob/master/src/database/sql/convert.go#L111-L209)):

1. Unwraps `sql.NamedArg` into `driver.NamedValue{Name, Ordinal (1-based), Value}` ([convert.go#L149-L157](https://github.com/golang/go/blob/master/src/database/sql/convert.go#L149-L157)).
2. Selects the first available checker:
   1. `Stmt.(driver.NamedValueChecker)`
   2. `Conn.(driver.NamedValueChecker)`
   3. `Stmt.(driver.ColumnConverter)` *(deprecated)*
   4. `driver.DefaultParameterConverter`
3. Control flow on return from `CheckNamedValue(nv)`:
   - `nil`: argument accepted as-is (even if `nv.Value` is a custom Go type not in `driver.IsValue`).
   - `driver.ErrRemoveArgument`: argument dropped from `[]NamedValue` before `NumInput` count validation.
   - `driver.ErrSkip`: falls through to `ColumnConverter` (if on `Stmt`) and then `DefaultParameterConverter`.
   - Any other error: wrapped and returned to caller.

### What `DefaultParameterConverter` Handles (and Rejects)

From [types.go#L242-L299](https://github.com/golang/go/blob/master/src/database/sql/driver/types.go#L242-L299):
- Passes `driver.IsValue(v)` unchanged.
- Calls `driver.Valuer.Value()` (safely converting a typed nil pointer whose value-receiver type implements `Valuer` into `nil` instead of panicking — Issue 8415, [types.go#L222-L240](https://github.com/golang/go/blob/master/src/database/sql/driver/types.go#L222-L240)) and verifies `IsValue(sv)`.
- Handles `uuid.UUID` -> `string` *(Go 1.27+)* ([types.go#L262-L263](https://github.com/golang/go/blob/master/src/database/sql/driver/types.go#L262-L263)).
- Dereferences pointers (`nil` pointer -> `nil`; non-nil pointer recurses).
- Converts signed ints (`int`, `int8`, `int16`, `int32`, `int64`) and small unsigned ints (`uint`, `uint8`, `uint16`, `uint32`) to `int64`.
- **`uint64` caveat**: Converts to `int64` **only if `< 1<<63`**; values with the high bit set (`>= 1<<63`) **fail with an error** ([types.go#L279-L284](https://github.com/golang/go/blob/master/src/database/sql/driver/types.go#L279-L284)). Supporting full-range `uint64`, `uint` on 64-bit platforms with high bit set, or custom types (e.g., decimals, JSON, arrays) requires implementing `NamedValueChecker`.
- Converts `float32`/`float64` -> `float64`, named `bool` -> `bool`, named `string` -> `string`, named `[]uint8` -> `[]byte`. All other types error.

### Result Scanning Rules (`Rows.Scan`)

- Drivers populate `dest[i]` in `Rows.Next` with canonical `driver.Value` types (`nil`, `int64`, `float64`, `bool`, `[]byte`, `string`, `time.Time`).
- `sql.Rows.Scan` converts between numeric/string/bool/`time.Time`/`*[]byte`/`*sql.RawBytes`/`sql.Scanner` with overflow checks ([sql.go#L3376-L3441](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L3376-L3441)).
- **`[]byte` ownership**: `Scan` into `*[]byte` or `*any` clones the slice; `Scan` into `*sql.RawBytes` borrows driver memory directly and holds `rs.closemu.RLock()` (`closemuScanHold = true`) until the next `Rows.Next()` or `Rows.Close()` ([sql.go#L3089-L3092](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L3089-L3092), [sql.go#L3410-L3419](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L3410-L3419), [sql.go#L3449-L3456](https://github.com/golang/go/blob/master/src/database/sql/sql.go#L3449-L3456)). Driver `Rows.Next` must keep any `[]byte` it places into `dest` valid until the next `Next()` or `Close()` call.

---

## 5. Recommended `fycsql` Driver Contract Checklist

1. **Driver & Connector**: Implement `Driver` + `DriverContext` (`OpenConnector`) + exported `NewConnector(...)` for `sql.OpenDB`. Implement `io.Closer` on `Connector` so `sql.DB.Close()` shuts down the shared in-process engine cleanly. Never return `ErrBadConn` from `Open`/`Connect`.
2. **Conn**: Implement `Conn`, `ConnBeginTx`, `ConnPrepareContext`, `ExecerContext`, `QueryerContext`, `Pinger`, `SessionResetter`, `Validator`, and `NamedValueChecker` (handling custom types and returning `driver.ErrSkip` for standard fallback).
3. **Stmt**: Implement `Stmt`, `StmtExecContext`, `StmtQueryContext`. Do not store `PrepareContext`'s `ctx` on `Stmt`.
4. **Tx**: Strictly validate `opts.Isolation` and enforce `opts.ReadOnly`. Acquire read snapshot for `ReadOnly: true`; eagerly acquire the context-aware write gate for `ReadOnly: false`. Always clean up tx state on failed `Commit()`; never fail `Rollback()` due to canceled `txCtx`.
5. **Rows**: Store query `ctx` from `QueryContext` on `Rows` and check `ctx.Err()` inside `Next()` so `sql.Rows.awaitDone` is never blocked indefinitely. Implement `RowsColumnTypeScanType`, `RowsColumnTypeDatabaseTypeName`, `RowsColumnTypeLength`, and `RowsColumnTypeNullable`.
6. **Concurrency & Pooling**: Share one `Engine` per `Connector` with concurrent MVCC/WAL read snapshots and a single context-aware Go channel write gate. Users get full reader concurrency + serialized writers on a single standard `*sql.DB` with no `SetMaxOpenConns(1)` bottleneck, no dual pools, and no `:memory:` eviction hazard.

---

## Primary Sources

- Go `database/sql` package documentation: https://pkg.go.dev/database/sql
- Go `database/sql/driver` package documentation: https://pkg.go.dev/database/sql/driver
- Go stdlib `src/database/sql/driver/driver.go`: https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go
- Go stdlib `src/database/sql/driver/types.go`: https://github.com/golang/go/blob/master/src/database/sql/driver/types.go
- Go stdlib `src/database/sql/sql.go`: https://github.com/golang/go/blob/master/src/database/sql/sql.go
- Go stdlib `src/database/sql/ctxutil.go`: https://github.com/golang/go/blob/master/src/database/sql/ctxutil.go
- Go stdlib `src/database/sql/convert.go`: https://github.com/golang/go/blob/master/src/database/sql/convert.go
- SQLite Write-Ahead Logging: https://www.sqlite.org/wal.html
- SQLite Transactions (`DEFERRED` / `IMMEDIATE` / `EXCLUSIVE`): https://www.sqlite.org/lang_transaction.html
- SQLite Shared-Cache Mode (discouraged): https://www.sqlite.org/sharedcache.html
- SQLite In-Memory Databases: https://www.sqlite.org/inmemorydb.html
- `mattn/go-sqlite3` source & FAQ: https://github.com/mattn/go-sqlite3/blob/master/README.md, https://github.com/mattn/go-sqlite3/blob/master/sqlite3.go, https://github.com/mattn/go-sqlite3/blob/master/sqlite3_sql.go
- `mattn/go-sqlite3` pooling/concurrency issues: [#209](https://github.com/mattn/go-sqlite3/issues/209), [#274](https://github.com/mattn/go-sqlite3/issues/274), [#1022](https://github.com/mattn/go-sqlite3/issues/1022), [#1179](https://github.com/mattn/go-sqlite3/issues/1179)
- `modernc.org/sqlite` docs & source: https://pkg.go.dev/modernc.org/sqlite, [connector.go](https://gitlab.com/cznic/sqlite/-/blob/master/connector.go), [conn.go](https://gitlab.com/cznic/sqlite/-/blob/master/conn.go), [tx.go](https://gitlab.com/cznic/sqlite/-/blob/master/tx.go), [rows.go](https://gitlab.com/cznic/sqlite/-/blob/master/rows.go), [busy.go](https://gitlab.com/cznic/sqlite/-/blob/master/busy.go), [sqlite.go](https://gitlab.com/cznic/sqlite/-/blob/master/sqlite.go)
