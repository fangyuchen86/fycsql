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
- **Global lock hierarchy**: Enforces strict lock order `CommitGate → SnapshotRegistry → WAL/BufferPool → PageLatch` across the engine.
