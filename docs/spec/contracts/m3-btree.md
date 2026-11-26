# M3 Contract — `internal/btree`: Slotted-Page B+Tree, Codecs, Overflow Chains, and Single-Tree Verification

> **Parent Index**: [v1-module-contracts.md](../v1-module-contracts.md) · **Handoff**: [v1-architecture-handoff.md](../v1-architecture-handoff.md) · **Invariants**: [v1-invariants.md](../v1-invariants.md) · **Glossary**: [GLOSSARY.md](../../../GLOSSARY.md)
> **Related ADRs**: [ADR-0002](../../adr/0002-strict-typing-and-sql-dialect.md), [ADR-0005](../../adr/0005-clustered-btree-slotted-page-and-memcomparable-keys.md), [ADR-0006](../../adr/0006-query-pipeline-catalog-volcano-and-spill.md)
> **Navigation**: ← [M2 — `internal/pager`](./m2-pager.md) · Next → [M4 — `internal/catalog` & `internal/tx`](./m4-catalog-and-tx.md)

---

## 1. Depth & Responsibility

`internal/btree` hides all physical B+Tree and binary codec complexity behind a narrow, catalog-agnostic interface:

- 16-byte common header & slotted-page layout (including 64 KiB `FreeEnd == 0 → 65536` widened arithmetic and lazy `FragBytes` compaction).
- `MaxKeySize = 900B` and `MaxLeafCellBytes = 1000B` (`inlineCap = min(900, 1000 - 12 - keyLen)`) with singly-linked Overflow Page chains and bounded chunked streaming (`StreamValue`).
- Byte-midpoint splits (90/10 right-most monotonic leaf split, 50/50 byte-midpoint leaf/internal split) and 25%-hysteresis merge/borrow rebalancing (including parent separator growth triggering parent splits).
- Stable root PageIDs (root splits and collapses mutate the root page in place without changing its `PageID`).
- Depth-safe forward and reverse cursors (singly-linked `RightSibling` forward + dynamic ancestor stack reverse).
- Zero-allocation order-preserving memcomparable key encoding and compact row record encoding/projection.
- Single-tree structural verification (including separator routing and disjoint child key ranges) and corruption-safe decoding.

- **Package Dependencies**: `internal/errs`, `internal/val`, Go stdlib only (**never imports `internal/pager` or `internal/catalog`**).
- **CRC Ownership**: `internal/pager` owns Page 2+ `[0..3] PageCRC32C` verification on disk load and sealing on `WriteSession.CommitMember()`, as well as empty Page 2 initialization on brand-new database creation; `internal/btree` writes bytes `[4..PageSize-1]` cleanly without duplicating CRC logic.

---

## 2. Interface Surface

> Signatures below define the behavioral contract, not production code.

```text
// Inbound seams (3 real adapters: snapshotPageReader in tx/root, stagePageWriter in tx, memPageStore in M3 tests)
// Uses Go builtin types so internal/btree has zero import dependency on internal/pager AND zero closure allocation (0 B/op).
type PageID = uint32
type ReadHint = uint8 // 0 = Random, 1 = SeqScan

type PageReader interface {
    PageSize() int
    PinPageToken(id PageID, hint ReadHint) (data []byte, token uint64, err error)
    UnpinToken(token uint64) // Idempotent single-release
}

type PageWriter interface {
    PageReader
    MutatePage(id PageID) ([]byte, error)
    AllocPage(pageType uint8) (PageID, []byte, error)
    FreePage(id PageID) error
}

// Pure memcomparable key & compact row record codecs (zero side effects)
type KeyColSpec struct { Desc bool; NullsLast bool }
func EncodeKey(dst []byte, specs []KeyColSpec, vals []val.Value) ([]byte, error)
func EncodeUniquePrefix(dst []byte, specs []KeyColSpec, vals []val.Value) (prefix []byte, hasNull bool, err error)
func EncodeRow(dst []byte, vals []val.Value, totalPhysicalSlots int) ([]byte, error)
// DecodeRow / ProjectRow validate record envelope strictly; TEXT/BLOB slices borrow from `raw`
// (caller must copy if retaining past `raw`'s lifetime).
func DecodeRow(raw []byte, dst []val.Value) (colCount int, err error)
func ProjectRow(raw []byte, slots []int, dst []val.Value) error

// Tree mutations & point lookups
func InitRoot(w PageWriter) (PageID, error)
// Get appends/copies the value into `dst` (or allocates an owned slice if dst == nil) BEFORE unpinning;
// returned bytes are caller-owned and remain valid after all pins are released.
func Get(r PageReader, root PageID, key []byte, dst []byte) (val []byte, found bool, err error)
func Put(w PageWriter, root PageID, key, val []byte) error
func Delete(w PageWriter, root PageID, key []byte) (deleted bool, err error)
func DropTree(w PageWriter, root PageID) error

// Ordered cursor (forward & reverse, opaque tree values)
type Cursor struct { ... }
func NewCursor(r PageReader, root PageID) *Cursor
func (c *Cursor) SeekFirst() bool
func (c *Cursor) SeekLast() bool
func (c *Cursor) SeekGE(key []byte) bool
func (c *Cursor) SeekLE(key []byte) bool
func (c *Cursor) Next() bool
func (c *Cursor) Prev() bool
func (c *Cursor) Key() []byte                  // Borrowed; valid until next movement or Close()
func (c *Cursor) ValueLen() int                // Total byte length (inline + overflow) without materializing overflow chain
func (c *Cursor) Value(dst []byte) ([]byte, error) // Materializes value into dst (owned if copied)
func (c *Cursor) StreamValue(w io.Writer) error    // Streams inline + overflow pages one page at a time (bounded memory)
func (c *Cursor) Valid() bool
func (c *Cursor) Err() error
func (c *Cursor) Close()

// Single-tree structural verification (zero catalog dependency)
type TreeAudit struct {
    TreePages     []PageID
    OverflowPages []PageID
    LeafDepth     int
    EntryCount    uint64
}
func VerifyTree(r PageReader, root PageID, opts VerifyOpts) (TreeAudit, error)
```

---

## 3. Behavioral Contracts & Error Modes

1. **Order-Preserving Keys & Width Limits (`INV-CAT-02`, `ADR-0005`)**:
   - Encoded byte order under `bytes.Compare` matches component types (`INTEGER`, normalized finite `REAL` signed zero, binary `TEXT`/`BLOB`, `BOOLEAN`), per-component `ASC`/`DESC` across the full supported width, and explicit `NULLS FIRST`/`NULLS LAST`.
   - `MaxKeySize = 900B` is enforced on the **final escaped composite key** including Row Key suffix and null tags. Exceeding 900B returns `errs.ErrConstraint` (including during index builds).
2. **Capacity, Stable Roots & Rebalancing (`INV-STR-03`, `ADR-0005`)**:
   - Every table and index keeps its root `PageID` from `InitRoot` through `DropTree` (Catalog root stays at Page 2).
   - A root split copies root contents into newly allocated child pages and installs the internal root in place; a root collapse copies the remaining child's contents back into the root before freeing the child.
   - On 4 KiB pages (4080 usable bytes), four maximum 1000-byte leaf cells plus their 2-byte slot pointers fit. On 64 KiB slotted pages, stored `FreeEnd == 0` decodes to `65536` (widened before arithmetic); smaller pages require ordinary in-range offsets.
   - 25% deletion maintenance threshold: on internal underflow, merge if the parent separator fits; otherwise redistribute children and update the separator, splitting the parent if its replacement grows. Every non-root internal node retains ≥ 2 children.
3. **Layered Safe Decoding & Structural Verification (`INV-STR-03`, `ADR-0005`)**:
   - **Tree seam (`Get`, `Cursor`, `VerifyTree`)**: Treats leaf values as opaque bytes (including empty secondary-index values). Before slicing any cell or overflow chunk, validates slot-array bounds, disjoint live cells, cell lengths, `cellArea - liveCellBytes == FragBytes`, overflow chunk bounds, acyclic overflow chains, and exact residual length. `VerifyTree` additionally validates strictly increasing keys within every node, **internal separator routing and disjoint child key ranges**, uniform leaf depth, and `RightSibling` linkage matching DFS order. Malformed tree/overflow structure returns `errs.ErrCorruption`.
   - **Row codec seam (`DecodeRow`, `ProjectRow`)**: Validates `[ColumnCount][NullBitmap][VarintLenHeader][Payloads]` envelopes and exact declared payload consumption (including tombstoned slots); truncated or malformed record envelopes return `errs.ErrCorruption`. (Schema-level checks such as `ColumnCount <= PhysicalSlotCount`, visible `NOT NULL`, and index bijection belong to M4 `Verify()`.)
4. **Bounded Streaming (`ADR-0006`)**:
   - `Cursor.StreamValue(w)` writes the inline prefix and then walks the singly-linked Overflow Page chain pinning **at most one overflow page at a time**, allowing M5's spill engine to transfer multi-MB row payloads without exceeding `WorkMem`.
5. **Error Atomicity & Rollback Ownership**:
   - `Put`, `Delete`, and `DropTree` never panic and never leak pins on allocation or I/O errors.
   - Physical rollback of partial page/Freelist mutations on error is owned by the caller's staging transaction (`pager.WriteSession.DiscardMember()` in production, or `memPageStore.Discard()` in M3 tests).

---

## 4. Resource Ownership & Lifecycle

- `Cursor` holds **at most one pinned leaf page** at any time (plus at most one transient overflow page during `StreamValue`/`Value`); its ancestor stack stores only `(PageID, slotIndex)` entries and grows dynamically with tree depth.
- `Cursor.Close()` unconditionally unpins the active page (idempotent).
- `Cursor.Key()` returns a borrowed slice valid only until the next cursor movement or `Close()`.
- `Get` copies/appends into caller-owned `dst` (or allocates an owned slice) **before** calling `UnpinToken`, so returned bytes never alias a Buffer Pool slab after unpinning.
- `DecodeRow` / `ProjectRow` `TEXT`/`BLOB` slices borrow from `raw`; callers copy if retaining beyond `raw`'s lifetime.
- `Put`, `Delete`, `DropTree`, and `VerifyTree` unpin every `pinToken` before returning.

---

## 5. Test Surface & Verification Contract

- Pure M3 tests exercise `Encode*`/`Decode*`, `Get`/`Put`/`Delete`/`DropTree`, `NewCursor`, and `VerifyTree` over a transactional in-memory `memPageStore` adapter (supporting explicit `BeginStage()` / `Commit()` / `Discard()` and fault injection) with compile-time assertions `var _ btree.PageWriter = (*memPageStore)(nil)`:
  - **All supported page sizes (`4096`–`65536`, incl. `FreeEnd=0`)**: Round-trip empty page, first insert, split, and lazy compaction.
  - **Rebalancing stress**: Delete into an underfull internal node with a long parent separator forcing redistribution and parent split; assert separator routing, disjoint child ranges, uniform leaf depth, DFS `RightSibling` chain, and ≥ 2 children on non-root internals.
  - **Allocation failure & explicit rollback**: Inject allocation failures on an overflow page, split sibling, and root child in turn; call `memPageStore.Discard()` explicitly and assert old values, reachability, and Freelist ownership (repeated against the real `pager.WriteSession`/`MemberStage` adapter in M4).
  - **Layered corruption rejection**: Feed CRC-valid malformed slot geometry / `FragBytes` / cyclic overflow / misrouted separator pages to `VerifyTree`/`Get`/`Cursor` (`errs.ErrCorruption`); separately feed truncated/corrupt row envelopes to `DecodeRow`/`ProjectRow` (`errs.ErrCorruption`).
  - **Bounded streaming & hot-path allocation**: Verify `Cursor.StreamValue` on multi-page overflow payloads and benchmark `Get` + `EncodeKey` with the real `snapshotPageReader` adapter once M4 wires it.
- **Invariants Enforced**: `INV-STR-03`, `INV-CAT-02`.
