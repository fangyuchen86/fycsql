package btreeproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

var (
	ErrKeyTooLarge = errors.New("key exceeds MaxKeySize (900 bytes)")
	ErrOutOfPages  = errors.New("slab page store capacity exceeded")
)

// SlabStore models ADR-0003's contiguous pointer-free noscan []byte slab
// with 1-based PageIDs (Page 1 reserved as DB header, Page 2+ allocated to B+Tree/Overflow/Freelist).
type SlabStore struct {
	slab       []byte   // single pointer-free noscan allocation: capacity * PageSize
	maxPages   uint32
	totalPages uint32   // highest allocated PageID (starts at 1; Page 1 is reserved DB header)
	freelist   []uint32 // in-memory freelist stack for prototype accounting
}

func NewSlabStore(maxPages int) *SlabStore {
	return &SlabStore{
		slab:       make([]byte, maxPages*PageSize),
		maxPages:   uint32(maxPages),
		totalPages: 1, // Page 1 reserved for DB header per ADR-0003
	}
}

func (s *SlabStore) Page(id uint32) []byte {
	off := int(id-1) * PageSize
	return s.slab[off : off+PageSize]
}

func (s *SlabStore) Alloc(typ PageType) (uint32, error) {
	var id uint32
	if n := len(s.freelist); n > 0 {
		id = s.freelist[n-1]
		s.freelist = s.freelist[:n-1]
	} else {
		if s.totalPages >= s.maxPages {
			return 0, ErrOutOfPages
		}
		s.totalPages++
		id = s.totalPages
	}
	p := s.Page(id)
	pageInit(p, typ)
	pageUpdateCRC(p)
	return id, nil
}

func (s *SlabStore) Free(id uint32) {
	p := s.Page(id)
	pageInit(p, PageTypeFreelist)
	pageUpdateCRC(p)
	s.freelist = append(s.freelist, id)
}

type pathEntry struct {
	pageID   uint32
	childIdx int
}

// BTree is a prototype of ADR-0005's Clustered Slotted-Page B+Tree with
// reusable scratch buffers to minimize per-operation Go heap allocations.
type BTree struct {
	store    *SlabStore
	rootID   uint32
	height   int
	count    int
	splits90 int
	splits50 int
	merges   int
	borrows  int

	// Reusable scratch buffers (Commit Gate serializes mutations per ADR-0001/0005).
	compactScratch [PageSize]byte
	cellScratch    [MaxLeafCellBytes + 16]byte
	splitTmp       [PageSize * 2]byte
	pathScratch    [16]pathEntry
}

func NewBTree(store *SlabStore) (*BTree, error) {
	rootID, err := store.Alloc(PageTypeLeaf)
	if err != nil {
		return nil, err
	}
	return &BTree{
		store:  store,
		rootID: rootID,
		height: 1,
	}, nil
}

// Get looks up key and appends the value into dst (zero alloc if dst has capacity and inline).
func (t *BTree) Get(key []byte, dst []byte) ([]byte, bool) {
	pid := t.rootID
	for {
		p := t.store.Page(pid)
		if pageType(p) == PageTypeLeaf {
			idx, found := leafSearch(p, key)
			if !found {
				return dst, false
			}
			_, inlineVal, hasOv, totalLen, firstOv, _ := leafCellInfo(p, slotOffset(p, idx))
			dst = append(dst[:0], inlineVal...)
			if hasOv {
				dst = t.readOverflow(firstOv, int(totalLen)-len(inlineVal), dst)
			}
			return dst, true
		}
		cIdx := internalChildIndex(p, key)
		pid = internalCellChild(p, cIdx)
	}
}

func (t *BTree) readOverflow(ovID uint32, remaining int, dst []byte) []byte {
	for ovID != 0 && remaining > 0 {
		p := t.store.Page(ovID)
		chunkLen := pageCellCount(p)
		if chunkLen > remaining {
			chunkLen = remaining
		}
		dst = append(dst, p[HeaderSize:HeaderSize+chunkLen]...)
		remaining -= chunkLen
		ovID = pageLink(p)
	}
	return dst
}

func (t *BTree) writeOverflow(tail []byte) (uint32, error) {
	var firstID, prevID uint32
	for len(tail) > 0 {
		id, err := t.store.Alloc(PageTypeOverflow)
		if err != nil {
			if firstID != 0 {
				t.freeOverflow(firstID)
			}
			return 0, err
		}
		p := t.store.Page(id)
		n := len(tail)
		if n > OverflowPayloadCap {
			n = OverflowPayloadCap
		}
		copy(p[HeaderSize:HeaderSize+n], tail[:n])
		setPageCellCount(p, n)
		setPageLink(p, 0)
		pageUpdateCRC(p)
		if firstID == 0 {
			firstID = id
		} else {
			prev := t.store.Page(prevID)
			setPageLink(prev, id)
			pageUpdateCRC(prev)
		}
		prevID = id
		tail = tail[n:]
	}
	return firstID, nil
}

func (t *BTree) freeOverflow(ovID uint32) {
	for ovID != 0 {
		p := t.store.Page(ovID)
		next := pageLink(p)
		t.store.Free(ovID)
		ovID = next
	}
}

// Put inserts or updates (key, val).
func (t *BTree) Put(key, val []byte) error {
	if len(key) > MaxKeySize {
		return ErrKeyTooLarge
	}

	path := t.pathScratch[:0]
	pid := t.rootID
	for {
		p := t.store.Page(pid)
		if pageType(p) == PageTypeLeaf {
			break
		}
		cIdx := internalChildIndex(p, key)
		path = append(path, pathEntry{pageID: pid, childIdx: cIdx})
		pid = internalCellChild(p, cIdx)
	}

	leaf := t.store.Page(pid)
	idx, found := leafSearch(leaf, key)

	// Prepare new cell (with Overflow allocation if needed).
	inlineLen, needOv := leafInlineLimit(len(key), len(val))
	var firstOv uint32
	if needOv {
		var err error
		firstOv, err = t.writeOverflow(val[inlineLen:])
		if err != nil {
			return err
		}
	}
	cellLen := encodeLeafCell(t.cellScratch[:], key, val[:inlineLen], needOv, uint32(len(val)), firstOv)
	newCell := t.cellScratch[:cellLen]

	if found {
		_, _, oldHasOv, _, oldOv, _ := leafCellInfo(leaf, slotOffset(leaf, idx))
		if oldHasOv {
			t.freeOverflow(oldOv)
		}
		pageDeleteCellAt(leaf, idx)
	} else {
		t.count++
	}

	if pageInsertCellAt(leaf, idx, newCell, &t.compactScratch) {
		pageUpdateCRC(leaf)
		return nil
	}

	// Leaf split needed.
	rightID, sepKey, err := t.splitLeaf(pid, idx, newCell)
	if err != nil {
		return err
	}
	return t.insertIntoParent(path, pid, sepKey, rightID)
}

func (t *BTree) splitLeaf(leftID uint32, insertIdx int, newCell []byte) (uint32, []byte, error) {
	rightID, err := t.store.Alloc(PageTypeLeaf)
	if err != nil {
		return 0, nil, err
	}
	left := t.store.Page(leftID)
	right := t.store.Page(rightID)

	oldN := pageCellCount(left)
	totalCells := oldN + 1

	// Gather all cell slices in order into t.splitTmp.
	type cellRef struct{ off, ln int }
	var refs [512]cellRef
	tmpPos := 0
	totalBytes := 0
	for i := 0; i < totalCells; i++ {
		var src []byte
		if i < insertIdx {
			off := slotOffset(left, i)
			sz := leafCellSize(left, off)
			src = left[off : off+sz]
		} else if i == insertIdx {
			src = newCell
		} else {
			off := slotOffset(left, i-1)
			sz := leafCellSize(left, off)
			src = left[off : off+sz]
		}
		copy(t.splitTmp[tmpPos:tmpPos+len(src)], src)
		refs[i] = cellRef{off: tmpPos, ln: len(src)}
		tmpPos += len(src)
		totalBytes += len(src) + SlotSize
	}

	// 90/10 split for monotonic rightmost inserts, 50/50 otherwise (ADR-0005).
	isRightmostAppend := pageLink(left) == 0 && insertIdx == oldN
	targetLeftBytes := totalBytes / 2
	if isRightmostAppend {
		targetLeftBytes = (totalBytes * 9) / 10
		t.splits90++
	} else {
		t.splits50++
	}

	splitIdx := 0
	acc := 0
	for i := 0; i < totalCells-1; i++ {
		nextAcc := acc + refs[i].ln + SlotSize
		if nextAcc > PageUsableBytes {
			break
		}
		acc = nextAcc
		splitIdx = i + 1
		if acc >= targetLeftBytes && totalBytes-acc <= PageUsableBytes {
			break
		}
	}
	if splitIdx < 1 {
		splitIdx = 1
	}
	if splitIdx >= totalCells {
		splitIdx = totalCells - 1
	}

	oldSibling := pageLink(left)
	pageInit(left, PageTypeLeaf)
	setPageLink(left, rightID)
	pageInit(right, PageTypeLeaf)
	setPageLink(right, oldSibling)

	for i := 0; i < splitIdx; i++ {
		c := t.splitTmp[refs[i].off : refs[i].off+refs[i].ln]
		if !pageInsertCellAt(left, i, c, &t.compactScratch) {
			panic("splitLeaf: left overflow")
		}
	}
	for i := splitIdx; i < totalCells; i++ {
		c := t.splitTmp[refs[i].off : refs[i].off+refs[i].ln]
		if !pageInsertCellAt(right, i-splitIdx, c, &t.compactScratch) {
			panic("splitLeaf: right overflow")
		}
	}
	pageUpdateCRC(left)
	pageUpdateCRC(right)

	// Separator key = first key of right leaf.
	firstRightKey := leafCellKey(right, 0)
	sepKey := make([]byte, len(firstRightKey))
	copy(sepKey, firstRightKey)
	return rightID, sepKey, nil
}

func (t *BTree) insertIntoParent(path []pathEntry, leftID uint32, sepKey []byte, rightID uint32) error {
	for len(path) > 0 {
		parentEntry := path[len(path)-1]
		path = path[:len(path)-1]
		parent := t.store.Page(parentEntry.pageID)
		cIdx := parentEntry.childIdx
		n := pageCellCount(parent)

		// Build new internal cell (leftID, sepKey).
		var iCell [internalCellHdr + MaxKeySize]byte
		binary.LittleEndian.PutUint32(iCell[0:4], leftID)
		binary.LittleEndian.PutUint16(iCell[4:6], uint16(len(sepKey)))
		copy(iCell[6:], sepKey)
		cellBytes := iCell[:internalCellHdr+len(sepKey)]

		if pageTotalFree(parent) >= len(cellBytes)+SlotSize {
			if cIdx < n {
				setInternalCellChild(parent, cIdx, rightID)
			} else {
				setPageLink(parent, rightID)
			}
			if !pageInsertCellAt(parent, cIdx, cellBytes, &t.compactScratch) {
				panic("insertIntoParent: unexpected no space")
			}
			pageUpdateCRC(parent)
			return nil
		}

		// Split internal node.
		newRightID, promoteKey, err := t.splitInternal(parentEntry.pageID, cIdx, leftID, sepKey, rightID)
		if err != nil {
			return err
		}
		leftID = parentEntry.pageID
		sepKey = promoteKey
		rightID = newRightID
	}

	// Create new root.
	newRootID, err := t.store.Alloc(PageTypeInternal)
	if err != nil {
		return err
	}
	root := t.store.Page(newRootID)
	setPageLink(root, rightID)
	var iCell [internalCellHdr + MaxKeySize]byte
	binary.LittleEndian.PutUint32(iCell[0:4], leftID)
	binary.LittleEndian.PutUint16(iCell[4:6], uint16(len(sepKey)))
	copy(iCell[6:], sepKey)
	if !pageInsertCellAt(root, 0, iCell[:internalCellHdr+len(sepKey)], &t.compactScratch) {
		panic("new root insert failed")
	}
	pageUpdateCRC(root)
	t.rootID = newRootID
	t.height++
	return nil
}

func (t *BTree) splitInternal(parentID uint32, cIdx int, leftChild uint32, sepKey []byte, rightChild uint32) (uint32, []byte, error) {
	rightID, err := t.store.Alloc(PageTypeInternal)
	if err != nil {
		return 0, nil, err
	}
	left := t.store.Page(parentID)
	right := t.store.Page(rightID)

	oldN := pageCellCount(left)
	totalKeys := oldN + 1

	type kRef struct {
		child uint32
		off   int
		ln    int
	}
	var refs [512]kRef
	tmpPos := 0
	for i := 0; i < totalKeys; i++ {
		if i < cIdx {
			ch, k, _ := internalCellInfo(left, slotOffset(left, i))
			copy(t.splitTmp[tmpPos:tmpPos+len(k)], k)
			refs[i] = kRef{child: ch, off: tmpPos, ln: len(k)}
			tmpPos += len(k)
		} else if i == cIdx {
			copy(t.splitTmp[tmpPos:tmpPos+len(sepKey)], sepKey)
			refs[i] = kRef{child: leftChild, off: tmpPos, ln: len(sepKey)}
			tmpPos += len(sepKey)
		} else {
			ch, k, _ := internalCellInfo(left, slotOffset(left, i-1))
			if i == cIdx+1 {
				ch = rightChild
			}
			copy(t.splitTmp[tmpPos:tmpPos+len(k)], k)
			refs[i] = kRef{child: ch, off: tmpPos, ln: len(k)}
			tmpPos += len(k)
		}
	}
	var lastChild uint32
	if cIdx == oldN {
		lastChild = rightChild
	} else {
		lastChild = pageLink(left)
	}

	// Variable-length keys MUST split by byte midpoint (not key count!), ensuring both halves fit <= PageUsableBytes.
	totalBytes := 0
	for i := 0; i < totalKeys; i++ {
		totalBytes += internalCellHdr + refs[i].ln + SlotSize
	}
	mid := 1
	leftBytes := 0
	for i := 0; i < totalKeys-1; i++ {
		cellBytes := internalCellHdr + refs[i].ln + SlotSize
		// If i becomes mid (promoted), left holds [0..i-1] (leftBytes), right holds [i+1..totalKeys-1].
		rightBytes := totalBytes - leftBytes - cellBytes
		if i >= 1 && leftBytes <= PageUsableBytes && rightBytes <= PageUsableBytes {
			mid = i
			if leftBytes >= totalBytes/2 {
				break
			}
		}
		leftBytes += cellBytes
	}
	promoteKey := make([]byte, refs[mid].ln)
	copy(promoteKey, t.splitTmp[refs[mid].off:refs[mid].off+refs[mid].ln])

	pageInit(left, PageTypeInternal)
	setPageLink(left, refs[mid].child)
	var iCell [internalCellHdr + MaxKeySize]byte
	for i := 0; i < mid; i++ {
		k := t.splitTmp[refs[i].off : refs[i].off+refs[i].ln]
		binary.LittleEndian.PutUint32(iCell[0:4], refs[i].child)
		binary.LittleEndian.PutUint16(iCell[4:6], uint16(len(k)))
		copy(iCell[6:], k)
		if !pageInsertCellAt(left, i, iCell[:internalCellHdr+len(k)], &t.compactScratch) {
			panic("splitInternal: left overflow")
		}
	}

	pageInit(right, PageTypeInternal)
	setPageLink(right, lastChild)
	for i := mid + 1; i < totalKeys; i++ {
		k := t.splitTmp[refs[i].off : refs[i].off+refs[i].ln]
		binary.LittleEndian.PutUint32(iCell[0:4], refs[i].child)
		binary.LittleEndian.PutUint16(iCell[4:6], uint16(len(k)))
		copy(iCell[6:], k)
		if !pageInsertCellAt(right, i-(mid+1), iCell[:internalCellHdr+len(k)], &t.compactScratch) {
			panic("splitInternal: right overflow")
		}
	}

	pageUpdateCRC(left)
	pageUpdateCRC(right)
	return rightID, promoteKey, nil
}

// Delete removes key if present, freeing Overflow pages and rebalancing under 25% hysteresis.
func (t *BTree) Delete(key []byte) bool {
	path := t.pathScratch[:0]
	pid := t.rootID
	for {
		p := t.store.Page(pid)
		if pageType(p) == PageTypeLeaf {
			break
		}
		cIdx := internalChildIndex(p, key)
		path = append(path, pathEntry{pageID: pid, childIdx: cIdx})
		pid = internalCellChild(p, cIdx)
	}

	leaf := t.store.Page(pid)
	idx, found := leafSearch(leaf, key)
	if !found {
		return false
	}
	_, _, hasOv, _, firstOv, _ := leafCellInfo(leaf, slotOffset(leaf, idx))
	if hasOv {
		t.freeOverflow(firstOv)
	}
	pageDeleteCellAt(leaf, idx)
	pageUpdateCRC(leaf)
	t.count--

	if len(path) == 0 {
		return true
	}
	t.rebalanceAfterDelete(path, pid)
	return true
}

func (t *BTree) rebalanceAfterDelete(path []pathEntry, nodeID uint32) {
	for len(path) > 0 {
		node := t.store.Page(nodeID)
		if pageUsedBytes(node) >= UnderflowBytes && pageCellCount(node) > 0 {
			return
		}
		parentEntry := path[len(path)-1]
		path = path[:len(path)-1]
		parent := t.store.Page(parentEntry.pageID)
		cIdx := parentEntry.childIdx
		pN := pageCellCount(parent)

		isLeaf := pageType(node) == PageTypeLeaf

		// Choose sibling (prefer right sibling, fallback to left).
		var leftIdx, rightIdx, sepIdx int
		var leftID, rightID uint32
		if cIdx < pN {
			leftIdx, rightIdx, sepIdx = cIdx, cIdx+1, cIdx
			leftID = nodeID
			rightID = internalCellChild(parent, rightIdx)
		} else {
			leftIdx, rightIdx, sepIdx = cIdx-1, cIdx, cIdx-1
			leftID = internalCellChild(parent, leftIdx)
			rightID = nodeID
		}
		left := t.store.Page(leftID)
		right := t.store.Page(rightID)

		if isLeaf {
			// Try merge right -> left if combined used bytes fit in one leaf.
			if pageUsedBytes(left)+pageUsedBytes(right) <= PageUsableBytes {
				t.mergeLeaves(left, right)
				t.store.Free(rightID)
				t.removeInternalSep(parent, sepIdx, leftID)
				pageUpdateCRC(left)
				pageUpdateCRC(parent)
				t.merges++
				nodeID = parentEntry.pageID
				continue
			}
			// Try borrow 1 cell from the fuller sibling.
			if leftID == nodeID && pageCellCount(right) > 1 {
				off := slotOffset(right, 0)
				sz := leafCellSize(right, off)
				cellCopy := t.cellScratch[:sz]
				copy(cellCopy, right[off:off+sz])
				if pageInsertCellAt(left, pageCellCount(left), cellCopy, &t.compactScratch) {
					pageDeleteCellAt(right, 0)
					newSep := leafCellKey(right, 0)
					if t.replaceInternalKey(parent, sepIdx, newSep) {
						pageUpdateCRC(left)
						pageUpdateCRC(right)
						pageUpdateCRC(parent)
						t.borrows++
						return
					}
					// Rollback borrow if parent had no room for larger separator key.
					pageDeleteCellAt(left, pageCellCount(left)-1)
					pageInsertCellAt(right, 0, cellCopy, &t.compactScratch)
				}
			} else if rightID == nodeID && pageCellCount(left) > 1 {
				lastIdx := pageCellCount(left) - 1
				off := slotOffset(left, lastIdx)
				sz := leafCellSize(left, off)
				cellCopy := t.cellScratch[:sz]
				copy(cellCopy, left[off:off+sz])
				if pageInsertCellAt(right, 0, cellCopy, &t.compactScratch) {
					pageDeleteCellAt(left, lastIdx)
					newSep := leafCellKey(right, 0)
					if t.replaceInternalKey(parent, sepIdx, newSep) {
						pageUpdateCRC(left)
						pageUpdateCRC(right)
						pageUpdateCRC(parent)
						t.borrows++
						return
					}
					pageDeleteCellAt(right, 0)
					pageInsertCellAt(left, lastIdx, cellCopy, &t.compactScratch)
				}
			}
			return
		} else {
			// Internal node merge (pulls separator down from parent).
			sepKey := internalCellKey(parent, sepIdx)
			pulledCellBytes := internalCellHdr + len(sepKey) + SlotSize
			if pageUsedBytes(left)+pageUsedBytes(right)+pulledCellBytes <= PageUsableBytes {
				t.mergeInternals(left, right, sepKey)
				t.store.Free(rightID)
				t.removeInternalSep(parent, sepIdx, leftID)
				pageUpdateCRC(left)
				pageUpdateCRC(parent)
				t.merges++
				nodeID = parentEntry.pageID
				continue
			}
			return
		}
	}

	// Collapse root if internal root became empty.
	root := t.store.Page(t.rootID)
	if pageType(root) == PageTypeInternal && pageCellCount(root) == 0 {
		newRoot := pageLink(root)
		t.store.Free(t.rootID)
		t.rootID = newRoot
		t.height--
	}
}

func (t *BTree) mergeLeaves(left, right []byte) {
	rN := pageCellCount(right)
	for i := 0; i < rN; i++ {
		off := slotOffset(right, i)
		sz := leafCellSize(right, off)
		if !pageInsertCellAt(left, pageCellCount(left), right[off:off+sz], &t.compactScratch) {
			panic("mergeLeaves: unexpected overflow")
		}
	}
	setPageLink(left, pageLink(right))
}

func (t *BTree) mergeInternals(left, right []byte, sepKey []byte) {
	var iCell [internalCellHdr + MaxKeySize]byte
	binary.LittleEndian.PutUint32(iCell[0:4], pageLink(left))
	binary.LittleEndian.PutUint16(iCell[4:6], uint16(len(sepKey)))
	copy(iCell[6:], sepKey)
	if !pageInsertCellAt(left, pageCellCount(left), iCell[:internalCellHdr+len(sepKey)], &t.compactScratch) {
		panic("mergeInternals: sep overflow")
	}
	rN := pageCellCount(right)
	for i := 0; i < rN; i++ {
		off := slotOffset(right, i)
		sz := internalCellSize(right, off)
		if !pageInsertCellAt(left, pageCellCount(left), right[off:off+sz], &t.compactScratch) {
			panic("mergeInternals: cell overflow")
		}
	}
	setPageLink(left, pageLink(right))
}

func (t *BTree) removeInternalSep(parent []byte, sepIdx int, mergedChild uint32) {
	pN := pageCellCount(parent)
	if sepIdx+1 == pN {
		setPageLink(parent, mergedChild)
	} else {
		setInternalCellChild(parent, sepIdx+1, mergedChild)
	}
	pageDeleteCellAt(parent, sepIdx)
}

func (t *BTree) replaceInternalKey(parent []byte, sepIdx int, newKey []byte) bool {
	leftChild, oldKey, _ := internalCellInfo(parent, slotOffset(parent, sepIdx))
	delta := len(newKey) - len(oldKey)
	if delta > pageTotalFree(parent) {
		return false
	}
	var iCell [internalCellHdr + MaxKeySize]byte
	binary.LittleEndian.PutUint32(iCell[0:4], leftChild)
	binary.LittleEndian.PutUint16(iCell[4:6], uint16(len(newKey)))
	copy(iCell[6:], newKey)
	pageDeleteCellAt(parent, sepIdx)
	if !pageInsertCellAt(parent, sepIdx, iCell[:internalCellHdr+len(newKey)], &t.compactScratch) {
		panic("replaceInternalKey: unexpected failure")
	}
	return true
}

// ScanForward walks all leaves via singly-linked RightSibling pointers (zero allocations for inline values).
func (t *BTree) ScanForward(valBuf []byte, fn func(key, val []byte) bool) {
	pid := t.rootID
	for {
		p := t.store.Page(pid)
		if pageType(p) == PageTypeLeaf {
			break
		}
		pid = internalCellChild(p, 0)
	}
	for pid != 0 {
		p := t.store.Page(pid)
		n := pageCellCount(p)
		for i := 0; i < n; i++ {
			k, inlineVal, hasOv, totalLen, firstOv, _ := leafCellInfo(p, slotOffset(p, i))
			var v []byte
			if hasOv {
				valBuf = append(valBuf[:0], inlineVal...)
				valBuf = t.readOverflow(firstOv, int(totalLen)-len(inlineVal), valBuf)
				v = valBuf
			} else {
				v = inlineVal
			}
			if !fn(k, v) {
				return
			}
		}
		pid = pageLink(p)
	}
}

// ScanReverse walks all entries in descending key order using only an ancestor cursor stack
// (validating ADR-0005's singly-linked leaf design with no LeftSibling pointer).
func (t *BTree) ScanReverse(valBuf []byte, fn func(key, val []byte) bool) {
	type frame struct {
		pid  uint32
		cIdx int
	}
	var stack [16]frame
	sp := 0

	// Descend to rightmost leaf.
	pid := t.rootID
	for {
		p := t.store.Page(pid)
		if pageType(p) == PageTypeLeaf {
			break
		}
		cIdx := pageCellCount(p)
		stack[sp] = frame{pid: pid, cIdx: cIdx}
		sp++
		pid = internalCellChild(p, cIdx)
	}

	for {
		p := t.store.Page(pid)
		for i := pageCellCount(p) - 1; i >= 0; i-- {
			k, inlineVal, hasOv, totalLen, firstOv, _ := leafCellInfo(p, slotOffset(p, i))
			var v []byte
			if hasOv {
				valBuf = append(valBuf[:0], inlineVal...)
				valBuf = t.readOverflow(firstOv, int(totalLen)-len(inlineVal), valBuf)
				v = valBuf
			} else {
				v = inlineVal
			}
			if !fn(k, v) {
				return
			}
		}

		// Step left via ancestor stack.
		for sp > 0 && stack[sp-1].cIdx == 0 {
			sp--
		}
		if sp == 0 {
			return
		}
		stack[sp-1].cIdx--
		parent := t.store.Page(stack[sp-1].pid)
		pid = internalCellChild(parent, stack[sp-1].cIdx)
		for {
			cur := t.store.Page(pid)
			if pageType(cur) == PageTypeLeaf {
				break
			}
			cIdx := pageCellCount(cur)
			stack[sp] = frame{pid: pid, cIdx: cIdx}
			sp++
			pid = internalCellChild(cur, cIdx)
		}
	}
}

// Verify checks ADR-0005's structural invariants:
// 1. CRC32C on every allocated page (B+Tree, Overflow, Freelist).
// 2. Strict key ordering inside every node and across parent separator bounds.
// 3. Uniform leaf depth.
// 4. Exact RightSibling chain matching DFS left-to-right leaf order.
// 5. Exact whole-file page accounting: BTrees ∪ Overflows ∪ Freelist == {2..totalPages} with 0 overlaps/leaks.
func (t *BTree) Verify() error {
	seen := make([]uint8, t.store.totalPages+1) // 0=unseen, 1=btree, 2=overflow, 3=freelist
	var leaves []uint32
	leafDepth := -1
	totalEntries := 0

	var walk func(pid uint32, depth int, minKey, maxKey []byte) error
	walk = func(pid uint32, depth int, minKey, maxKey []byte) error {
		if pid < 2 || pid > t.store.totalPages {
			return fmt.Errorf("page %d out of range [2..%d]", pid, t.store.totalPages)
		}
		if seen[pid] != 0 {
			return fmt.Errorf("page %d referenced multiple times", pid)
		}
		seen[pid] = 1
		p := t.store.Page(pid)
		if !pageVerifyCRC(p) {
			return fmt.Errorf("page %d CRC32C mismatch", pid)
		}
		n := pageCellCount(p)
		switch pageType(p) {
		case PageTypeLeaf:
			if leafDepth == -1 {
				leafDepth = depth
			} else if leafDepth != depth {
				return fmt.Errorf("non-uniform leaf depth: got %d want %d", depth, leafDepth)
			}
			leaves = append(leaves, pid)
			var prevKey []byte
			for i := 0; i < n; i++ {
				k, inlineVal, hasOv, totalLen, firstOv, _ := leafCellInfo(p, slotOffset(p, i))
				if minKey != nil && bytes.Compare(k, minKey) < 0 {
					return fmt.Errorf("leaf %d key < lower bound", pid)
				}
				if maxKey != nil && bytes.Compare(k, maxKey) >= 0 {
					return fmt.Errorf("leaf %d key >= upper bound", pid)
				}
				if prevKey != nil && bytes.Compare(prevKey, k) >= 0 {
					return fmt.Errorf("leaf %d keys out of order at slot %d", pid, i)
				}
				prevKey = k
				totalEntries++

				if hasOv {
					ovBytes := 0
					ov := firstOv
					for ov != 0 {
						if ov < 2 || ov > t.store.totalPages {
							return fmt.Errorf("overflow page %d out of range", ov)
						}
						if seen[ov] != 0 {
							return fmt.Errorf("overflow page %d overlap/cycle", ov)
						}
						seen[ov] = 2
						op := t.store.Page(ov)
						if !pageVerifyCRC(op) || pageType(op) != PageTypeOverflow {
							return fmt.Errorf("overflow page %d corrupt", ov)
						}
						ovBytes += pageCellCount(op)
						ov = pageLink(op)
					}
					if len(inlineVal)+ovBytes != int(totalLen) {
						return fmt.Errorf("leaf %d slot %d overflow length mismatch: %d+%d != %d",
							pid, i, len(inlineVal), ovBytes, totalLen)
					}
				}
			}
			return nil

		case PageTypeInternal:
			if n == 0 {
				return fmt.Errorf("empty internal page %d", pid)
			}
			var prevSep []byte
			for i := 0; i < n; i++ {
				child, sep, _ := internalCellInfo(p, slotOffset(p, i))
				if prevSep != nil && bytes.Compare(prevSep, sep) >= 0 {
					return fmt.Errorf("internal %d separators out of order", pid)
				}
				cMin := minKey
				if prevSep != nil {
					cMin = prevSep
				}
				if err := walk(child, depth+1, cMin, sep); err != nil {
					return err
				}
				prevSep = sep
			}
			return walk(pageLink(p), depth+1, prevSep, maxKey)

		default:
			return fmt.Errorf("unexpected page type %d on page %d", pageType(p), pid)
		}
	}

	if err := walk(t.rootID, 1, nil, nil); err != nil {
		return err
	}
	if totalEntries != t.count {
		return fmt.Errorf("entry count mismatch: walked %d != tracked %d", totalEntries, t.count)
	}

	// Verify RightSibling chain matches DFS leaf sequence exactly.
	for i, lid := range leaves {
		next := pageLink(t.store.Page(lid))
		if i+1 < len(leaves) {
			if next != leaves[i+1] {
				return fmt.Errorf("leaf %d RightSibling=%d want %d", lid, next, leaves[i+1])
			}
		} else if next != 0 {
			return fmt.Errorf("rightmost leaf %d has non-zero RightSibling=%d", lid, next)
		}
	}

	// Verify freelist pages and exact whole-file accounting.
	for _, fid := range t.store.freelist {
		if fid < 2 || fid > t.store.totalPages {
			return fmt.Errorf("freelist page %d out of range", fid)
		}
		if seen[fid] != 0 {
			return fmt.Errorf("freelist page %d overlaps live page", fid)
		}
		seen[fid] = 3
		fp := t.store.Page(fid)
		if !pageVerifyCRC(fp) || pageType(fp) != PageTypeFreelist {
			return fmt.Errorf("freelist page %d corrupt", fid)
		}
	}
	for pid := uint32(2); pid <= t.store.totalPages; pid++ {
		if seen[pid] == 0 {
			return fmt.Errorf("leaked page %d (neither BTree, Overflow, nor Freelist)", pid)
		}
	}
	return nil
}
