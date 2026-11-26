---
status: accepted
---

# Clustered B+Tree Tables, Slotted Pages, Uniform Secondary Indexes, and Memcomparable Keys

fycsql v1 stores every table as a Clustered B+Tree keyed by its encoded Row Key (explicit PK or hidden Internal RowID), and every Secondary Index (both unique and non-unique) as a separate B+Tree with uniform key layout `encode(indexedCols..., RowKey)` and empty cell values (`UNIQUE` indexes enforce prefix uniqueness on `encode(indexedCols...)` whenever all indexed columns are non-`NULL`). All Page 2+ types share a 16-byte common header (including 4-byte `PageCRC32C` and `RightSibling`/`NextPage`), using sorted Slotted-Page cell pointers, singly-linked leaf `RightSibling` pointers (reverse scans use the B+Tree cursor's ancestor stack), order-preserving memcomparable keys compared via zero-alloc `bytes.Compare`, ~900-byte key/inline limits with singly-linked Overflow Pages, 90/10 monotonic right-most split optimization, and 25%-hysteresis borrow/merge rebalancing serialized inside the Commit Gate.

## Considered Options

- **Unordered Heap File + Separate PK & Secondary B+Trees (RID pointers)**: Avoids moving row payloads on B+Tree splits, but adds a separate heap page manager, forwarding pointers on row growth, and an extra indirection on every primary-key lookup.
- **Doubly-Linked Leaf Pages (`LeftSibling` + `RightSibling`)**: Simplifies reverse leaf scans without a cursor stack, but dirties an extra neighboring 4KB page in the WAL on every leaf split/merge.
- **Separate Unique Index Encoding (`key = indexedCols`, `val = RowKey`) vs Non-Unique Encoding**: Slightly smaller non-NULL unique entries, but splits secondary index codepaths and still requires appending `RowKey` when `NULL` values are present under SQL's distinct-`NULL` uniqueness rule.

## Consequences

- **Zero-allocation key ordering & fast column slicing**: Memcomparable encoding (null tag `0x00`/`0x01`, sign-flipped 8B big-endian `int64` and normalized finite IEEE-754 `float64`, `0x00`-escaped `TEXT`/`BLOB`, bitwise-inverted bytes for `DESC` columns) reduces all key comparisons to `bytes.Compare`; row records store `[ColumnCount][NullBitmap][VarintLenHeader][Payloads]` so projection skips unused columns without decoding them.
- **Guaranteed B+Tree fanout ≥ 4 & zero writer latch crabbing**: Capping `MaxKeySize` and `MaxInlinePayload` at ~900 bytes (at 4KB pages) prevents degenerate trees while keeping large values in Freelist-managed Overflow chains; serializing splits/merges in the Commit Gate over private staging buffers eliminates concurrent B+Tree latch crabbing.
- **Minimal-diff `UPDATE` & atomic PK mutation**: Non-indexed `UPDATE`s touch only the Clustered Table; indexed-column edits update only affected Secondary Indexes; PK updates execute as atomic `DELETE oldRowKey + INSERT newRowKey` with both keys recorded in the OCC Write Set.
- **First-class `Verify()` structural invariant**: Any Snapshot can be audited for Page CRC32C, key/separator ordering, uniform leaf depth & `RightSibling` linkage, 1-to-1 base-table/secondary-index bijection, and exact whole-file page accounting (`Page 1 ∪ BTrees ∪ Overflows ∪ Freelist == {1..TotalPageCount}` with zero overlaps or leaks).
