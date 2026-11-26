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

### SQL & Schema

**Catalog**:
The transactional metadata describing all tables, columns, declared types, constraints, and indexes in the database.
_Avoid_: Data dictionary, schema cache, sqlite_master

**Value**:
A typed scalar datum belonging to one of the five canonical types (`INTEGER`, `REAL`, `TEXT`, `BLOB`, `BOOLEAN`) or `NULL`.
_Avoid_: Datum, cell, variant

**Row Key**:
The unique clustering key identifying a row within a table — either its declared primary key or an engine-assigned **Internal RowID**.
_Avoid_: Tuple ID, RID, record key

**Internal RowID**:
A hidden, monotonically assigned 64-bit integer **Row Key** generated automatically for tables declared without an explicit primary key.
_Avoid_: OID, surrogate key

### Storage & Paging

**Page**:
A fixed-size block in the main database file addressed by a 1-based `PageID`, storing either the database file header, a B+Tree node, overflow payload, or a **Freelist** block.
_Avoid_: Block, sector, raw node

**WAL**:
The append-only write-ahead log file (`<db>-wal`) holding committed **Page** images as sequential **Frames** for durability, crash recovery, and multi-version **Snapshot** reads.
_Avoid_: Journal, redo log

**Frame**:
A single committed **Page** image paired with its **Commit Timestamp** and checksum, appended sequentially to the **WAL**.
_Avoid_: Log record, WAL entry

**Frame Index**:
The in-memory index mapping each `PageID` to its committed **WAL** **Frames** ordered by **Commit Timestamp**, allowing any **Snapshot** to resolve the exact **Page** version visible to it.
_Avoid_: Shm index, wal-index

**Buffer Pool**:
The bounded in-memory cache of immutable **Pages** and **Frames** backed by pointer-free byte slabs with pin-counted eviction.
_Avoid_: Page cache, LRU cache

**Freelist**:
The transactional chain of trunk and leaf **Pages** tracking freed `PageID`s for reuse before extending the database file.
_Avoid_: Free map, bitmap

**VFS**:
The narrow virtual filesystem seam wrapping all file I/O, directory sync, and single-process lock operations for OS portability and deterministic fault injection.
_Avoid_: OS layer, disk driver

**Checkpoint**:
The four-step barriered process that copies committed **WAL** **Frames** up to the oldest active **Snapshot** back into the main database file and reclaims **WAL** space.
_Avoid_: Flush, compaction

**Group Commit**:
The zero-delay batching of already-queued validated **Transactions** inside the **Commit Gate** into a single shared **WAL** sync operation.
_Avoid_: Delayed commit, write batching

**Clustered Table**:
The primary B+Tree storing a table's complete row records sorted directly by **Row Key**.
_Avoid_: Heap, data file, table file

**Secondary Index**:
An auxiliary B+Tree storing order-preserving composite keys ending in **Row Key** to accelerate lookups or enforce uniqueness.
_Avoid_: Non-clustered index, index file

**Slotted Page**:
A **Page** layout combining a sorted array of cell offsets growing downward from the header with variable-length cell payloads growing upward from the end.
_Avoid_: Packed page, record page

**Overflow Page**:
A singly-linked **Page** holding trailing bytes of a row payload exceeding the inline cell limit, returned to the **Freelist** when replaced or deleted.
_Avoid_: Spill page, blob page




