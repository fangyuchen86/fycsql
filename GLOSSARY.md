# fycsql

An embedded pure-Go relational database engine with multi-writer ACID transactions, B+Tree storage, and crash recovery.

## Language

### Transactions & Concurrency

**Transaction**:
An atomic unit of relational reads, writes, or schema changes that executes against a single **Snapshot** and either commits all mutations together or rolls back completely.
_Avoid_: Session, unit of work

**Snapshot**:
An immutable, point-in-time view of all committed database state up to a specific **Commit Timestamp**, acquired when a **Transaction** begins.
_Avoid_: Read view, version view

**Commit Timestamp**:
A monotonically increasing logical version number assigned to a read-write **Transaction** when it passes validation at the **Commit Gate**, defining total commit order and **Snapshot** visibility.
_Avoid_: LSN, sequence number, txid (when referring to visibility order)

**Write Set**:
The private collection of uncommitted row and index mutations accumulated by a read-write **Transaction** during execution before entering the **Commit Gate**.
_Avoid_: Dirty buffer, staging log, undo buffer

**Conflict**:
A First-Committer-Wins collision detected at the **Commit Gate** when a committing **Transaction** modifies a primary key, unique index key, or schema object already modified by another **Transaction** committed after its **Snapshot**.
_Avoid_: Lock timeout, busy error

**Commit Gate**:
The serialized critical section that validates a **Write Set** for **Conflicts**, assigns the next **Commit Timestamp**, applies mutations durably, and publishes the new state to future **Snapshots**.
_Avoid_: Global write lock, big database lock

**Schema Barrier**:
An exclusive serialization boundary acquired at the **Commit Gate** by a **Transaction** containing DDL, ensuring catalog changes never interleave with concurrent writes while active **Snapshots** continue reading their prior catalog version.
_Avoid_: Metadata lock, table lock
