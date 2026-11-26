---
status: accepted
---

# WAL Wire Format, Rolling CRC32C Checksums, Group Commit, and Four-Step Checkpointing

fycsql v1 guarantees ACID atomicity and durability using a deterministic 32-byte WAL file header (`FYCWAL\x00\x00`, version, page size, checkpoint generation, `Salt1`/`Salt2`, header CRC32C) followed by fixed-size frames (32-byte frame header with `PageID`, `DBPageCount` commit marker `>0` on the final frame of a transaction, `CommitTS`, `Salt1`/`Salt2`, rolling `PrevCRC`, and `FrameCRC`, plus full page payload). Every Page 2+ in `<db>` also carries a 4-byte CRC32C in its common page header verified on read into the Buffer Pool. Commits support `SyncFull` (default), `SyncNormal`, and `SyncOff`, with zero-delay opportunistic Group Commit coalescing already-queued validated commits at the Commit Gate into one `walFile.Sync()`. Checkpointing runs in `Passive` (default async at 1,000 frames), `Restart` (salt rotation), and `Truncate` modes under a strict four-step barrier (`WAL.Sync → DB.Write → DB.Sync → WAL.Reset/Truncate+Sync`), never copying frames newer than `minActiveSnapshotTS`.

## Considered Options

- **Per-Frame Independent Checksums Without Chaining or Salts**: Simpler to compute per frame, but fails to detect out-of-order disk persistence (commit frame hitting disk before an earlier data frame) and cannot distinguish stale pre-reset frames without truncating the WAL on every reset.
- **No Checksums on Main DB Pages (WAL CRC Only)**: Saves 4 bytes per 4KB page (0.1%), but leaves the database blind to silent disk bit-rot or external corruption once the WAL is checkpointed and removed on clean close.
- **Synchronous Inline Checkpointing on Commit**: Simpler than a background checkpointer goroutine, but injects periodic multi-millisecond tail-latency spikes into write commits.

## Consequences

- **Idempotent crash recovery across any byte cut-point**: `Open()` performs a read-only single-pass WAL scan, loading only complete transactions ending in a valid commit marker (`DBPageCount > 0`) into the in-memory Frame Index. Crashing during `Open()` or during any step of Checkpointing converges identically; torn WAL tails and torn `<32B` initial WAL headers (with intact `<db>`) recover cleanly, while corrupt Page 1 headers, page-size mismatches, or bad `<db>` page CRC32Cs fail loudly with `ErrCorruption`.
- **Single-file cleanliness on clean `Close()`**: File creation and WAL removal pair `File.Sync()` with Unix parent-directory `Sync()`; clean `Close()` drains transactions, runs `CheckpointTruncate`, syncs `<db>`, removes `<db>-wal` + syncs the parent directory, and releases the process lock.
- **Checkpoint starvation visibility**: Long-running read Snapshots bound `safeMaxTS = minActiveSnapshotTS`; WAL growth past a configurable warning watermark (default 10,000 frames) emits observability alerts and integrates with ADR-0001's optional `MaxSnapshotAge`.
- **Uniform Page 2+ checksum contract**: The B+Tree and Freelist page layouts (next tickets) reserve a common 4-byte CRC32C field at a fixed page header offset.
