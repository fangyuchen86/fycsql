---
status: accepted
---

# WAL Wire Format, Rolling CRC32C Checksums, Group Commit, and Four-Step Checkpointing

fycsql v1 records transactions using a deterministic 32-byte WAL file header (`FYCWAL\x00\x00`, version, page size, checkpoint generation, `Salt1`/`Salt2`, header CRC32C) followed by fixed-size frames (32-byte frame header with `PageID`, `DBPageCount` commit marker `>0` on the final frame of a transaction, `CommitTS`, `Salt1`/`Salt2`, rolling `PrevCRC`, and `FrameCRC`, plus full page payload). Every live Page 2+ in `<db>` also carries a 4-byte CRC32C in its common page header verified on read into the Buffer Pool. Commits support `SyncFull` (default), `SyncNormal`, and `SyncOff`, with zero-delay opportunistic Group Commit sharing one WAL sync among queued commits under `SyncFull`. Checkpointing runs in `Passive` (default async at 1,000 frames), `Restart` (salt rotation), and `Truncate` modes under a strict four-step barrier (`WAL.Sync → DB.Write → DB.Sync → WAL.Reset/Truncate+Sync`), never copying frames newer than `minActiveSnapshotTS`.

## Considered Options

- **Per-Frame Independent Checksums Without Chaining or Salts**: Simpler to compute per frame, but fails to detect out-of-order disk persistence (commit frame hitting disk before an earlier data frame) and cannot distinguish stale pre-reset frames without truncating the WAL on every reset.
- **No Checksums on Main DB Pages (WAL CRC Only)**: Saves 4 bytes per 4KB page (0.1%), but leaves the database blind to silent disk bit-rot or external corruption once the WAL is checkpointed and removed on clean close.
- **Synchronous Inline Checkpointing on Commit**: Simpler than a background checkpointer goroutine, but injects periodic multi-millisecond tail-latency spikes into write commits.

## Commit and recovery

Each nonempty modifying transaction includes a full Page 1 frame with its own `LastCommitTS`, roots, allocator state and page count. Its final frame carries the commit marker. An empty write transaction returns without allocating a Commit Timestamp. An ordinary Group Commit uses one contiguous write when it fits the physical batch budget. Budget physical staging separately from the logical Write Set. Stop admitting batch members when their page images would exceed the staging budget. Process a large transaction alone and stream its WAL encoding in bounded chunks with its marker last. If its private page images alone exceed the staging budget, discard them and return `ErrTxTooLarge` before append. This adds one header-page image per modifying transaction and makes every Checkpoint watermark recoverable.

Under `SyncFull`, acknowledge and publish only after the shared WAL sync succeeds. `SyncNormal` may publish after append and coalesce background WAL syncs. `SyncOff` omits commit and background syncs. The latter two modes can lose an unsynced suffix on power loss. Before acknowledging the first commit, make the WAL name durable with directory sync on filesystems that require it. File creation and the Checkpoint barriers below apply in every mode so that a partial backfill cannot destroy an earlier durable prefix.

A write can stop after a complete member's marker. On an uncertain append (including a short write), or a commit sync error, publish none of the affected batch in memory, enter Fail-Stop and preserve the WAL. Return non-retryable `ErrCommitIndeterminate` for affected members. A restart with or without power loss can recover a prefix of complete members. Requests still queued before append have a known failure. A later background sync failure enters Fail-Stop and preserves the log. Earlier Normal-mode acknowledgements retain their documented durability semantics.

Under the database lock, scan the WAL read-only. Validate its header, rolling CRC chain, generation salts, transaction TS and final marker before indexing the transaction. Retain the offset and rolling CRC of the last complete transaction. Ignore an incomplete or torn append tail. An I/O error while reading bytes within the measured file length aborts Open with `ErrIO`. A read-write opener removes any invalid tail and syncs that repair before appending. A read-only opener leaves both files untouched. An empty WAL or an incomplete initial header (file length at most 32 bytes) can be ignored with a valid main database because WAL header sync precedes all frame writes. Reject an invalid header followed by frame data. Propagate an intact unsupported version. Header validation and torn Page 1 bootstrap follow ADR-0010.

## Checkpoint and close

1. Under writer serialization choose the latest committed TS bounded by the oldest registered Snapshot. Sync the WAL before any database write.
2. Copy the latest changed image of each page in the interval since the previous successful backfill through that TS. Copy its exact Page 1 image, including its historical page count. After a restart, resolve reads against the full valid WAL overlay. The first writable Checkpoint recopies the latest image per changed page from that generation before retirement. A newly written main-file header alone cannot prove that other pages reached disk.
3. Sync the database. Only now publish the backfill watermark, redirect reads to the synced base and retire covered Frame Index entries. A partial backfill retains the WAL. Passive Checkpoint skips repeat work at an unchanged watermark.
4. With snapshot registration and reader admission serialized, recheck that all commits are backfilled and all old physical references have drained. Switch read sources and retire the old generation before reusing offsets. Passive may rotate when these conditions already hold. Restart seeks a fresh generation and Truncate reclaims the file length. Establish a durable empty WAL before writing a new salted header, then sync that header before any new frames. A crash during rotation therefore leaves either the old generation, an empty WAL, or a possibly incomplete new header with no frames yet.

Clean Close drains work and removes the WAL only after successful full backfill and retirement of the final generation. Propagate removal and directory-sync errors. Preserve the WAL on partial backfill, Fail-Stop or drain timeout. Keep the process lock until physical operations finish (ADR-0007). Long readers can delay reuse. Report the configured warning watermark and oldest Snapshot age.
