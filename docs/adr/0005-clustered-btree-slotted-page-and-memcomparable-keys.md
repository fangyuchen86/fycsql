---
status: accepted
---

# Clustered B+Tree Tables, Slotted Pages, Uniform Secondary Indexes, and Memcomparable Keys

fycsql v1 stores every table as a Clustered B+Tree keyed by its encoded Row Key (explicit PK or hidden Internal RowID), and every Secondary Index (both unique and non-unique) as a separate B+Tree with uniform key layout `encode(indexedCols..., RowKey)` and empty cell values (`UNIQUE` indexes enforce prefix uniqueness on `encode(indexedCols...)` whenever all indexed columns are non-`NULL`). All Page 2+ types share an exact 16-byte common header (`[0..3] PageCRC32C`, `[4] PageType`, `[5] Flags`, `[6..7] CellCount`, `[8..9] FreeEnd`, `[10..11] FragBytes` for lazy in-page compaction with implicit `FreeStart = 16 + 2*CellCount`, and `[12..15] LinkPage` serving as `RightSibling` on Leaf, `RightmostChild` on Internal, and `NextPage` on Overflow/Freelist), using sorted Slotted-Page cell pointers, singly-linked leaf `RightSibling` pointers (reverse scans use the B+Tree cursor's ancestor stack), order-preserving memcomparable keys compared via zero-alloc `bytes.Compare`, `MaxKeySize = 900B` coupled with `MaxLeafCellBytes = 1000B` (`inlineCap = min(900, 1000 - 12 - keyLen)` on overflow) and singly-linked Overflow Pages, byte-midpoint splitting (90/10 monotonic right-most Leaf split, 50/50 byte-midpoint Leaf/Internal split), and 25%-hysteresis borrow/merge rebalancing serialized inside the Commit Gate.

## Considered Options

- **Unordered Heap File + Separate PK & Secondary B+Trees (RID pointers)**: Avoids moving row payloads on B+Tree splits, but adds a separate heap page manager, forwarding pointers on row growth, and an extra indirection on every primary-key lookup.
- **Doubly-Linked Leaf Pages (`LeftSibling` + `RightSibling`)**: Simplifies reverse leaf scans without a cursor stack, but dirties an extra neighboring 4KB page in the WAL on every leaf split/merge.
- **Separate Unique Index Encoding (`key = indexedCols`, `val = RowKey`) vs Non-Unique Encoding**: Slightly smaller non-NULL unique entries, but splits secondary index codepaths and still requires appending `RowKey` when `NULL` values are present under SQL's distinct-`NULL` uniqueness rule.

## Consequences

- **Zero-allocation key ordering & fast column slicing**: Memcomparable encoding (null tag `0x00`/`0x01`, sign-flipped 8B big-endian `int64` and normalized finite IEEE-754 `float64`, `0x00`-escaped `TEXT`/`BLOB`, bitwise-inverted bytes for `DESC` columns) reduces all key comparisons to `bytes.Compare`; row records store `[ColumnCount][NullBitmap][VarintLenHeader][Payloads]` so projection skips unused columns without decoding them.
- **Guaranteed B+Tree fanout ≥ 4 & zero writer latch crabbing**: Capping `MaxKeySize = 900B` and combined leaf cell size at `MaxLeafCellBytes = 1000B` (so `inlineCap = min(900, 1000 - 12 - keyLen)` when overflowing) guarantees ≥ 4 cells per 4KB leaf and ≥ 4 keys (5 children) per 4KB internal node even under worst-case 900B keys; byte-midpoint splitting on both Leaf and Internal nodes prevents right-half overflow when short and long variable-length keys mix in the same node, while serializing splits/merges in the Commit Gate over private staging buffers eliminates concurrent B+Tree latch crabbing.
- **Minimal-diff `UPDATE` & atomic PK mutation**: Non-indexed `UPDATE`s touch only the Clustered Table; indexed-column edits update only affected Secondary Indexes; PK updates execute as atomic `DELETE oldRowKey + INSERT newRowKey` with both keys recorded in the OCC Write Set.
- **First-class `Verify()` structural invariant**: Any Snapshot can be audited for Page CRC32C, key/separator ordering, uniform leaf depth & `RightSibling` linkage, 1-to-1 base-table/secondary-index bijection, and exact whole-file page accounting (`Page 1 ∪ BTrees ∪ Overflows ∪ Freelist == {1..TotalPageCount}` with zero overlaps or leaks).
