---
status: accepted
---

# Hand-Written Query Pipeline, Self-Hosted Catalog, Volcano Executor, and Unified External Merge-Sort Spill

fycsql v1 implements its query pipeline entirely in pure Go stdlib: a hand-written lexer + recursive descent statement parser + Pratt expression parser, an explicit strict-typing Binder against a self-hosted Clustered B+Tree `_fycsql_catalog` (cached in RAM by immutable `SchemaVersion`), a rule-based Logical→Physical Planner, and a Volcano pull-iterator Executor using caller-owned reusable `*Row` buffers with periodic `context.Context` checks. `Internal RowID` and `AUTOINCREMENT` sequences allocate monotonically via per-table `atomic.Int64` counters to avoid false OCC collisions under multi-writer inserts; every DML statement stages mutations in a Statement Delta before atomically merging into the transaction's Write Set (solving both the Halloween Problem and statement-level atomicity); and all out-of-RAM blocking operators (`ORDER BY`, `GROUP BY`, `DISTINCT`, `Hash Join`) share one unified External Merge-Sort spill engine under a configurable `WorkMem` budget (default `16MB`).

## Considered Options

- **Transactional Catalog Counters for `AUTOINCREMENT` / `Internal RowID`**: Simpler persistence, but causes concurrent multi-writer OCC transactions reading the same Snapshot to allocate identical Row Keys and abort on false write-write conflicts.
- **Chunked / Vectorized Execution Engine**: Higher throughput on wide analytical scans, but adds columnar batch and expression-eval complexity mismatched with v1's row-oriented OLTP B+Tree focus.
- **Separate Grace Hash Partition Spill for `GROUP BY` and `Hash Join`**: Avoids sorting on hash spill, but duplicates spill file format, buffering, and fault-testing surfaces compared to falling back to the single External Merge-Sort engine (`SortAggregate` / `SortMergeJoin`).
- **Cost-Based Optimizer with Histograms (`ANALYZE`) in v1**: Better multi-way join ordering on skewed data, but requires statistics collection, maintenance, and cardinality estimation; deferred to later fog resolution.

## Consequences

- **High-impact OLTP plan quality without statistics**: Rule-based planner handles constant folding, predicate & projection pushdown, sargable key/prefix-`LIKE` range extraction, covering index detection, `ORDER BY` sort elimination (including reverse cursor scans), Top-N heap sort for `LIMIT`, and Index-NLJ / Hash Join / NLJ selection.
- **Transparent Prepared Statement caching & deterministic `EXPLAIN`**: Prepared statements cache the schema-independent AST plus a `SchemaVersion`-tagged Physical Plan (transparently re-binding/re-planning after DDL); `EXPLAIN <stmt>` outputs a deterministic indented physical operator tree (`plan` `TEXT` column) with zero side effects.
- **Bounded memory & clean cancellation**: Every blocking operator respects `WorkMem` via `vfs.FS`-backed external merge-sort temp files cleaned up on `Close()`, and Volcano operators check `ctx.Err()` periodically (e.g., every 128 rows) alongside RYOW sorted merge iterators (`MergeIter(SnapshotBTreeIter, WriteSetIter)`).
- **Gaps allowed in generated IDs**: Rolling back a transaction that allocated an `atomic.Int64` RowID or `AUTOINCREMENT` value leaves a numeric gap, matching standard PostgreSQL `SERIAL` and SQLite `AUTOINCREMENT` semantics.
