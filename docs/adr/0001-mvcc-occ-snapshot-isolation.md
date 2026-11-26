---
status: accepted
---

# Multi-Writer MVCC + OCC with Snapshot Isolation and Serialized Commit Gate

fycsql v1 uses multi-writer MVCC with Optimistic Concurrency Control (OCC) and Snapshot Isolation (First-Committer-Wins) rather than SQLite-style single-writer serialization (SWMR) or pessimistic row-level 2PL. Read-only and read-write transactions execute concurrently across goroutines against immutable point-in-time Snapshots (acquired immediately at `BEGIN`), buffering uncommitted writes privately in a bounded in-memory Write Set with Read-Your-Own-Writes overlay; at `COMMIT`, a context-aware FIFO Commit Gate serializes row/key-level conflict validation, Commit Timestamp assignment, B+Tree/WAL application, and publication.

## Considered Options

- **SWMR (Single-Writer, Multiple-Readers)**: Simpler and naturally serializable with eager write locking, but serializes all write-transaction execution (including query planning, scans, and application think-time between statements), limiting multi-goroutine write concurrency.
- **MVCC + Pessimistic Row-Level 2PL**: Queues writers on hot rows instead of aborting, but requires a lock manager, deadlock detection, concurrent B+Tree latch crabbing, and uncommitted-write Undo/CLR crash recovery.
- **Serializable via OCC Read-Set & Range Validation**: Eliminates write skew (`G2-item`) and phantom write skew (`G2`), but adds read-range predicate tracking and higher abort rates on overlapping range scans; deferred beyond v1 in favor of Snapshot Isolation.

## Consequences

- **Zero deadlocks & clean crash recovery**: Uncommitted and aborted transactions never dirty on-disk B+Tree pages, and B+Tree structural modifications (splits/merges) execute exclusively inside the Commit Gate, keeping page-level WAL recovery viable without ARIES Undo/CLR complexity.
- **No false hot-leaf collisions**: Row/key-level conflict checks on `(tableID, primaryKey)` and unique index keys allow concurrent monotonic inserts on the same right-most B+Tree leaf page to succeed.
- **Explicit retry contract & write-set ceiling**: Explicit transactions return retryable `ErrConflict` (or `ErrSchemaChanged` on DDL barrier conflicts) while standalone autocommit statements auto-retry within `context`/timeout budgets; single-transaction Write Sets are capped (default 64MB, returning `ErrTxTooLarge`).
- **Known SI anomaly boundary**: Prevents `G0`, `G1a/b/c`, `P4`, and `G-single`; write skew (`G2-item`) and phantom write skew (`G2`) remain possible under Snapshot Isolation and are documented as explicit v1 behavior.
- **Global lock hierarchy**: Enforces strict lock order `CommitGate → WriteSerializeMu → TxRegistry → SnapshotRegistry → WAL/BufferPool` (where `WriteSerializeMu` serializes physical WAL appends and Checkpoint coordination outside `SnapshotRegistry`, and `WAL/BufferPool` covers `FrameIndex` and Buffer Pool shard locks with zero `PageLatch` on immutable published frames).
- **Batch validation and rollback.** In FIFO order, validate logical keys against committed history and earlier successful members. Apply each member to that cumulative page state. Freeze its page images at its own Commit Timestamp. On a preparation error discard only that member's changes, including splits, overflow chains, Page 1 and Freelist allocations. If cancellation withdraws a prepared member before append, rebuild and revalidate its dependent successors before writing the batch.
- **Conflict history.** Remember the last mutation TS for each (TableID, Row Key) and (IndexID, non-NULL unique prefix), including deletes and both sides of a key change. Keep entries newer than the oldest active read-write Snapshot. An intervening write that restores the original value still returns retryable `ErrConflict`. A duplicate already present in the transaction's own view is a constraint error.
- **Schema Barrier in both directions.** Reject a writer bound to an obsolete schema with `ErrSchemaChanged`. A DDL transaction that scanned a table, including `CREATE UNIQUE INDEX`, validates the table's last data-change TS against its Snapshot under the Gate, including earlier batch members. A concurrent source change yields `ErrConflict`. Build from the transaction's logical rows including its own DML, check uniqueness and publish the complete index and Catalog change together. Charge staged index entries to the write budget and discard the build on overflow.
- **Memory admission.** Charge retained row/key bytes, mutation bookkeeping and the growing Statement Delta against the same 64 MB transaction budget before allocation. Failure leaves the previous Write Set intact. The WAL batch has a separate physical-byte budget under ADR-0004.
