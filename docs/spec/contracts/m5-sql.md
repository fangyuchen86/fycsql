# M5 Contract — `internal/sql`: Frontend, Binder, Rule-Based Planner, Volcano Executor, and Spill Engine

> **Parent Index**: [v1-module-contracts.md](../v1-module-contracts.md) · **Handoff**: [v1-architecture-handoff.md](../v1-architecture-handoff.md) · **Invariants**: [v1-invariants.md](../v1-invariants.md) · **Glossary**: [GLOSSARY.md](../../../GLOSSARY.md)
> **Related ADRs**: [ADR-0002](../../adr/0002-strict-typing-and-sql-dialect.md), [ADR-0006](../../adr/0006-query-pipeline-catalog-volcano-and-spill.md), [ADR-0009](../../adr/0009-metadata-only-alter-table-and-physical-slots.md)
> **Navigation**: ← [M4 — `internal/catalog` & `internal/tx`](./m4-catalog-and-tx.md) · Next → [M6 — Root `fycsql`, `driver`, `cmd/fycsql`](./m6-api-driver-cli.md)

---

## 1. Depth & Responsibility

`internal/sql` encapsulates the entire SQL compilation, planning, execution, and out-of-core spill pipeline behind a three-type interface:

- Hand-written Lexer, Recursive Descent statement parser, and Pratt expression parser.
- Strict 5-type Binder (`INTEGER`, `REAL`, `TEXT`, `BLOB`, `BOOLEAN`, `NULL`; `INTEGER → REAL` widening only; 3VL).
- `SchemaVersion`-keyed Physical Plan cache with snapshot-local binding and private-DDL bypass.
- Rule-Based Logical→Physical Planner (constant folding, predicate & projection pushdown, sargable & prefix-`LIKE` range extraction, covering index scan, sort elimination including reverse cursor scans, Top-N heap sort, Index-NLJ / Hash Join / NLJ).
- Volcano pull-iterator Executor over caller-owned reusable row buffers with periodic `context.Context` checks.
- Unified External Merge-Sort spill engine (`WorkMem` default 16MB) over an injected writable `tempFS` using bounded streaming for oversized rows.
- Deterministic, zero-side-effect `EXPLAIN` output.

Inside `internal/sql`, `operator` is an **unexported Go `interface`** (~15 concrete physical operators); it is strictly an internal seam and never leaks outside `internal/sql`.

- **Package Dependencies**: `internal/tx`, `internal/catalog`, `internal/btree`, `vfs`, `internal/errs`, `internal/val`, Go stdlib only.

---

## 2. Interface Surface

> Signatures below define the behavioral contract, not production code. All exported types are concrete structs.

```text
type Engine struct { ... }

func NewEngine(txMgr *tx.Manager, tempFS vfs.FS, opts Options) *Engine
func (e *Engine) Prepare(query string) (*PreparedStmt, error)

type StmtKind uint8 // KindQuery | KindDML | KindDDL | KindExplain | KindBegin | KindCommit | KindRollback

type PreparedStmt struct { ... } // Safe for concurrent reuse across transactions
func (s *PreparedStmt) Kind() StmtKind
func (s *PreparedStmt) ParamCount() int
func (s *PreparedStmt) Exec(ctx context.Context, tx *tx.Tx, args []val.Value) (ExecResult, error)
func (s *PreparedStmt) Query(ctx context.Context, tx *tx.Tx, args []val.Value) (*RowStream, error)

type ExecResult struct {
    RowsAffected  int64
    LastInsertID  int64
    HasLastInsert bool // True only for INSERT into single-col INTEGER PK or hidden Internal RowID
}

type RowStream struct { ... }
func (rs *RowStream) Columns() []ColumnMeta // Name, Type, Nullable
func (rs *RowStream) Next(ctx context.Context, dst []val.Value) (ok bool, err error)
func (rs *RowStream) Close() error
```

---

## 3. Behavioral Contracts & Error Modes

1. **Strict Parsing, Dialect Rules & `EXPLAIN` (`INV-API-01`, `INV-CAT-01`, `ADR-0002`, `ADR-0006`)**:
   - `Prepare` parses SQL into a schema-independent AST without needing a `Tx`. Multi-statement SQL strings fail with `errs.ErrSyntax`.
   - Transaction control statements (`BEGIN`, `COMMIT`, `ROLLBACK`) are classified as `KindBegin`/`KindCommit`/`KindRollback` so stateless `*fycsql.DB` and `database/sql` reject them with `errs.ErrSyntax` while stateful `*fycsql.Conn` executes them.
   - **Dialect Rules**:
     - Identifiers: unquoted identifiers fold ASCII case-insensitively (lowercased); double-quoted `"Ident"` identifiers preserve exact case.
     - Parameters: supports positional `?` or numbered `$1..$N`; mixing placeholder styles in one statement fails with `errs.ErrSyntax`.
     - `LIKE`: binary case-sensitive `%`/`_` matching (with optional `ESCAPE`), enabling prefix-`LIKE` sargable index range extraction.
     - `NULL` ordering: default ordering is `NULLS FIRST` for `ASC` and `NULLS LAST` for `DESC`, overridden by explicit `NULLS FIRST` / `NULLS LAST`.
     - Aggregates: `COUNT(*)` counts all rows; `COUNT(expr)`, `SUM`, `AVG`, `MIN`, `MAX` ignore `NULL` inputs (returning `0` for `COUNT` and `NULL` for `SUM`/`AVG`/`MIN`/`MAX` over zero non-`NULL` rows).
   - **`EXPLAIN <stmt>`**: Returns a `*RowStream` with a single deterministic column named `plan` of type `TEXT` containing the indented physical operator tree, with **zero side effects** (including when explaining `INSERT`/`UPDATE`/`DELETE` or DDL).
2. **Snapshot-Local Binding & Plan Cache Isolation (`INV-QRY-02`, `ADR-0006`, `ADR-0009`)**:
   - Shared plans are cached inside `PreparedStmt` keyed by committed `tx.Schema().Version` and bind to stable numeric `(TableID, IndexID, PhysicalSlot)` identifiers.
   - When `tx.HasPrivateDDL()` is true, `Exec`/`Query` binds and plans locally against `tx.Schema()`, **bypassing shared plan lookup and insertion**. Rolling back private DDL never publishes a private plan, and planning against a newer schema leaves active older readers' bindings valid.
   - Sort elimination applies **only** when index scan direction, `NULLS FIRST`/`LAST` placement, and comparison type match every required `ORDER BY` component.
   - Residual predicates are always retained when an extracted index range is a superset (including widened `INTEGER ↔ REAL` bounds), preserving SQL three-valued logic.
3. **Statement Atomicity & Context Cancellation (`INV-TX-04`, `ADR-0007`)**:
   - `Exec` automatically wraps DML/DDL execution in `tx.BeginStatement()` → `tx.CommitStatement()` (or `tx.DiscardStatement()` on any error or `ctx` cancellation).
   - Both blocking setup during `Query()` / first `Next()` and subsequent `RowStream.Next` iterations check `ctx.Err()` periodically (at least every 128 rows) and abort cleanly on cancellation.
4. **Bounded Blocking Memory, Oversized Key Comparison & Streaming Spill (`INV-QRY-01`, `ADR-0006`)**:
   - `ORDER BY`, `GROUP BY`, `DISTINCT`, and joins obey per-operator `WorkMem` (default 16MB), including merge input buffers, oversized key comparison buffers, and equal-key duplicate-group replay buffers, spilling via temporary runs written to the injected writable `tempFS` (completely separate from `<db>`'s directory so read-only database handles spill safely).
   - **Bounded Oversized Row & Sort-Key Streaming**:
     - When sorting or spilling rows whose payload or sort key exceeds `WorkMem`, operators project bounded columns via `RowIter.ProjectCurrentBounded` and stream oversized `TEXT`/`BLOB` columns/records via `RowIter.StreamColumnValue` / `StreamCurrentValue` into temporary runs using fixed bounded I/O buffers (e.g., 32 KiB) without materializing full payloads in RAM.
     - **Bounded Comparison of Oversized Sort Keys** (e.g., `ORDER BY big_blob`): Each run entry stores a fixed-size inline key prefix (e.g., 256B–4 KiB) alongside its run-file offset and total byte length. During sorting and K-way merging, comparisons check the inline prefix first; when two oversized keys share an identical prefix, the comparator streams fixed-size chunks (e.g., 4 KiB) from both runs and compares chunk-by-chunk in $O(\text{chunkSize})$ memory.
   - Hash Join chooses hash-to-sort fallback **before** emitting any output rows and preserves all consumed input; merge fan-in is bounded; equal-key duplicate groups exceeding `WorkMem` replay from temporary runs by file offset.
   - **Per-Stream Temp Run Ownership & Cleanup**: Ownership starts at the **first temporary-file creation**, including failures or `ctx` cancellation occurring during `Query()` before a `*RowStream` is returned. Cleanup removes only that stream's runs (safe for concurrent spilling streams). When `tempFS.Remove` succeeds, zero temporary files remain; if `tempFS.Remove` fails persistently, the error is propagated/wrapped to the caller and external/OS temp-dir cleanup retains responsibility for that unremoved path.

---

## 4. Resource Ownership & Lifecycle

- `*Engine` owns no file descriptors directly; it holds references to `*tx.Manager` and `tempFS`.
- `*PreparedStmt` owns its immutable AST and bounded `SchemaVersion → PhysicalPlan` cache; safe for concurrent reuse and GC-managed when closed/dropped.
- `*RowStream` owns its active Volcano `operator` tree, any underlying `*tx.RowIter` cursors, and all temporary spill runs created on `tempFS`. `RowStream.Close()` is idempotent, unpins all cursors, attempts removal of all stream-owned temp runs (leaving zero files when `tempFS.Remove` succeeds), and returns any persistent removal error.

---

## 5. Test Surface & Verification Contract

- Tested through `Engine.Prepare`, `PreparedStmt.Exec`, `PreparedStmt.Query`, and `EXPLAIN` with an `ErrorFS`-wrapped `tempFS`:
  - **Coupled Root-Split / Collapse / PageID-Reuse Schedule (`INV-TX-01 × INV-WAL-03 × INV-CAT-04`)**: Keep one old `Snapshot` open, reuse the same `PreparedStmt` point lookup, probe keys in **both** subtrees after a root split, then collapse the root and reuse the freed child `PageID` for new data; assert old and new `Snapshot`s both return exact expected rows.
  - **Schema Evolution & Plan Equivalence**: Compare full scans and index plans across private `ALTER TABLE`/rollback, absent physical slots (`AddDefault`), explicit `NULL`, and dropped/re-added column names.
  - **Dialect, Keys & `EXPLAIN`**: Test escaped keys near 900B (`ErrConstraint` above 900B), wide composite `ASC`/`DESC` directions, default & explicit `NULLS FIRST`/`LAST`, mixed `INTEGER`/`REAL` bounds + 3VL residual predicates, case-sensitive `LIKE`, identifier folding/quoting, mixed placeholder rejection (`ErrSyntax`), aggregate `NULL` semantics, and deterministic `plan TEXT` zero-side-effect `EXPLAIN` (including over DML).
  - **Streaming Spill, Oversized Keys, Concurrency & Cleanup**: Test a single row larger than `WorkMem`, **two `ORDER BY` keys larger than `WorkMem` sharing a common prefix exceeding `WorkMem/2`** (verifying exact ordering within `WorkMem` bounds), two simultaneous spilling streams (verifying isolated cleanup), a spilling join whose duplicate equal-key group exceeds `WorkMem` compared to a reference join, cancellation during blocking `Query`/first `Next`, read-only DB spilling on separate `tempFS`, and `ErrorFS` failures on `tempFS` (asserting zero remaining files whenever `tempFS.Remove` succeeds, and verifying removal errors are reported when `Remove` persistently fails).
- **Invariants Enforced**: `INV-QRY-01`–`02`, `INV-CAT-01`–`02`.
