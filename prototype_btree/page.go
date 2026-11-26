package btreeproto

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
)

const (
	PageSize   = 4096
	HeaderSize = 16
	SlotSize   = 2

	// Usable bytes per page after the 16-byte common header.
	PageUsableBytes = PageSize - HeaderSize // 4080

	// Fanout guarantee:
	// Internal cell: 4B LeftChild + 2B KeyLen + MaxKeySize (900B) + 2B slot = 908B -> 4 keys (5 children) per 4KB page.
	// Leaf cell: capped at MaxLeafCellBytes (1000B) + 2B slot = 1002B -> >= 4 cells per 4KB leaf even with 900B key + overflow!
	MaxKeySize       = 900
	MaxLeafCellBytes = 1000
	MaxInlineCap     = 900

	// Overflow payload per page (4096 - 16 = 4080 bytes).
	OverflowPayloadCap = PageUsableBytes

	// 25% fill-factor hysteresis for borrow/merge rebalancing (ADR-0005).
	UnderflowBytes = PageUsableBytes / 4 // 1020 bytes

	// Leaf inline cell header: 2B keyLen + 2B inlineLen (high bit 0x8000 = hasOverflow).
	leafCellBaseHdr     = 4
	leafCellOverflowExt = 8 // 4B totalValLen + 4B firstOverflowPageID
	overflowFlag        = uint16(0x8000)

	// Internal cell header: 4B leftChild + 2B keyLen.
	internalCellHdr = 6
)

type PageType uint8

const (
	PageTypeLeaf     PageType = 1
	PageTypeInternal PageType = 2
	PageTypeOverflow PageType = 3
	PageTypeFreelist PageType = 4
)

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// Uniform 16-byte Page 2+ Common Header:
//   [0..3]   PageCRC32C (uint32 LE, over bytes [4..PageSize])
//   [4]      PageType   (uint8)
//   [5]      Flags      (uint8)
//   [6..7]   CellCount  (uint16 LE; for Overflow = payload byte len)
//   [8..9]   FreeEnd    (uint16 LE; lowest byte offset of cell region, starts at 4096)
//   [10..11] FragBytes  (uint16 LE; dead bytes inside cell region awaiting lazy compaction)
//   [12..15] LinkPage   (uint32 LE; Leaf=RightSibling, Internal=RightmostChild, Overflow/Freelist=NextPage)

func pageInit(p []byte, typ PageType) {
	clear(p[:HeaderSize])
	p[4] = byte(typ)
	binary.LittleEndian.PutUint16(p[8:10], PageSize)
}

func pageType(p []byte) PageType        { return PageType(p[4]) }
func pageCellCount(p []byte) int        { return int(binary.LittleEndian.Uint16(p[6:8])) }
func setPageCellCount(p []byte, n int)  { binary.LittleEndian.PutUint16(p[6:8], uint16(n)) }
func pageFreeEnd(p []byte) int          { return int(binary.LittleEndian.Uint16(p[8:10])) }
func setPageFreeEnd(p []byte, off int)  { binary.LittleEndian.PutUint16(p[8:10], uint16(off)) }
func pageFragBytes(p []byte) int        { return int(binary.LittleEndian.Uint16(p[10:12])) }
func setPageFragBytes(p []byte, n int)  { binary.LittleEndian.PutUint16(p[10:12], uint16(n)) }
func pageLink(p []byte) uint32          { return binary.LittleEndian.Uint32(p[12:16]) }
func setPageLink(p []byte, id uint32)   { binary.LittleEndian.PutUint32(p[12:16], id) }

func pageUpdateCRC(p []byte) {
	sum := crc32.Checksum(p[4:PageSize], crc32cTable)
	binary.LittleEndian.PutUint32(p[0:4], sum)
}

func pageVerifyCRC(p []byte) bool {
	want := binary.LittleEndian.Uint32(p[0:4])
	got := crc32.Checksum(p[4:PageSize], crc32cTable)
	return want == got
}

func pageFreeStart(p []byte) int {
	return HeaderSize + pageCellCount(p)*SlotSize
}

func pageContiguousFree(p []byte) int {
	return pageFreeEnd(p) - pageFreeStart(p)
}

func pageTotalFree(p []byte) int {
	return pageContiguousFree(p) + pageFragBytes(p)
}

func pageUsedBytes(p []byte) int {
	return PageUsableBytes - pageTotalFree(p)
}

func slotOffset(p []byte, idx int) int {
	pos := HeaderSize + idx*SlotSize
	return int(binary.LittleEndian.Uint16(p[pos : pos+2]))
}

func setSlotOffset(p []byte, idx, off int) {
	pos := HeaderSize + idx*SlotSize
	binary.LittleEndian.PutUint16(p[pos:pos+2], uint16(off))
}

// Leaf cell view helpers (zero-alloc slices directly into page slab).

func leafCellInfo(p []byte, off int) (key []byte, inlineVal []byte, hasOverflow bool, totalValLen uint32, firstOvPage uint32, cellSize int) {
	kLen := int(binary.LittleEndian.Uint16(p[off : off+2]))
	vField := binary.LittleEndian.Uint16(p[off+2 : off+4])
	hasOverflow = (vField & overflowFlag) != 0
	iLen := int(vField & ^overflowFlag)
	kStart := off + leafCellBaseHdr
	vStart := kStart + kLen
	key = p[kStart : kStart+kLen]
	inlineVal = p[vStart : vStart+iLen]
	cellSize = leafCellBaseHdr + kLen + iLen
	if hasOverflow {
		extOff := vStart + iLen
		totalValLen = binary.LittleEndian.Uint32(p[extOff : extOff+4])
		firstOvPage = binary.LittleEndian.Uint32(p[extOff+4 : extOff+8])
		cellSize += leafCellOverflowExt
	}
	return
}

func leafCellKey(p []byte, idx int) []byte {
	off := slotOffset(p, idx)
	kLen := int(binary.LittleEndian.Uint16(p[off : off+2]))
	return p[off+leafCellBaseHdr : off+leafCellBaseHdr+kLen]
}

func leafCellSize(p []byte, off int) int {
	kLen := int(binary.LittleEndian.Uint16(p[off : off+2]))
	vField := binary.LittleEndian.Uint16(p[off+2 : off+4])
	iLen := int(vField & ^overflowFlag)
	sz := leafCellBaseHdr + kLen + iLen
	if (vField & overflowFlag) != 0 {
		sz += leafCellOverflowExt
	}
	return sz
}

// leafInlineLimit calculates how many payload bytes stay inline for a given keyLen
// so every leaf cell stays <= MaxLeafCellBytes (guaranteeing >= 4 cells per 4KB leaf).
func leafInlineLimit(keyLen, valLen int) (inlineLen int, needOverflow bool) {
	if leafCellBaseHdr+keyLen+valLen <= MaxLeafCellBytes && valLen <= MaxInlineCap {
		return valLen, false
	}
	capBytes := MaxLeafCellBytes - leafCellBaseHdr - leafCellOverflowExt - keyLen
	if capBytes > MaxInlineCap {
		capBytes = MaxInlineCap
	}
	if capBytes < 0 {
		capBytes = 0
	}
	return capBytes, true
}

func encodeLeafCell(dst []byte, key, inlineVal []byte, hasOverflow bool, totalValLen, firstOv uint32) int {
	binary.LittleEndian.PutUint16(dst[0:2], uint16(len(key)))
	vField := uint16(len(inlineVal))
	if hasOverflow {
		vField |= overflowFlag
	}
	binary.LittleEndian.PutUint16(dst[2:4], vField)
	pos := leafCellBaseHdr
	copy(dst[pos:], key)
	pos += len(key)
	copy(dst[pos:], inlineVal)
	pos += len(inlineVal)
	if hasOverflow {
		binary.LittleEndian.PutUint32(dst[pos:pos+4], totalValLen)
		binary.LittleEndian.PutUint32(dst[pos+4:pos+8], firstOv)
		pos += leafCellOverflowExt
	}
	return pos
}

func leafSearch(p []byte, key []byte) (idx int, found bool) {
	lo, hi := 0, pageCellCount(p)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		cmp := bytes.Compare(leafCellKey(p, mid), key)
		if cmp < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < pageCellCount(p) && bytes.Equal(leafCellKey(p, lo), key) {
		return lo, true
	}
	return lo, false
}

// Internal cell helpers.

func internalCellInfo(p []byte, off int) (leftChild uint32, key []byte, cellSize int) {
	leftChild = binary.LittleEndian.Uint32(p[off : off+4])
	kLen := int(binary.LittleEndian.Uint16(p[off+4 : off+6]))
	key = p[off+internalCellHdr : off+internalCellHdr+kLen]
	cellSize = internalCellHdr + kLen
	return
}

func internalCellKey(p []byte, idx int) []byte {
	off := slotOffset(p, idx)
	kLen := int(binary.LittleEndian.Uint16(p[off+4 : off+6]))
	return p[off+internalCellHdr : off+internalCellHdr+kLen]
}

func internalCellChild(p []byte, idx int) uint32 {
	if idx == pageCellCount(p) {
		return pageLink(p)
	}
	off := slotOffset(p, idx)
	return binary.LittleEndian.Uint32(p[off : off+4])
}

func setInternalCellChild(p []byte, idx int, child uint32) {
	if idx == pageCellCount(p) {
		setPageLink(p, child)
		return
	}
	off := slotOffset(p, idx)
	binary.LittleEndian.PutUint32(p[off:off+4], child)
}

func internalCellSize(p []byte, off int) int {
	kLen := int(binary.LittleEndian.Uint16(p[off+4 : off+6]))
	return internalCellHdr + kLen
}

// internalChildIndex finds child index i in [0..CellCount] for key:
// Child[0] < K[0], Child[i] in [K[i-1], K[i]), Child[N] >= K[N-1].
func internalChildIndex(p []byte, key []byte) int {
	lo, hi := 0, pageCellCount(p)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if bytes.Compare(internalCellKey(p, mid), key) <= 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// pageCompact repacks all live cells tightly at the end of p using caller scratch (4KB).
func pageCompact(p []byte, scratch *[PageSize]byte) {
	n := pageCellCount(p)
	typ := pageType(p)
	copy(scratch[:HeaderSize], p[:HeaderSize])
	writeOff := PageSize
	for i := 0; i < n; i++ {
		off := slotOffset(p, i)
		var sz int
		if typ == PageTypeLeaf {
			sz = leafCellSize(p, off)
		} else {
			sz = internalCellSize(p, off)
		}
		writeOff -= sz
		copy(scratch[writeOff:writeOff+sz], p[off:off+sz])
		setSlotOffset(scratch[:], i, writeOff)
	}
	setPageFreeEnd(scratch[:], writeOff)
	setPageFragBytes(scratch[:], 0)
	copy(p, scratch[:])
}

func pageInsertCellAt(p []byte, idx int, cellBytes []byte, scratch *[PageSize]byte) bool {
	needed := len(cellBytes) + SlotSize
	if pageTotalFree(p) < needed {
		return false
	}
	if pageContiguousFree(p) < needed {
		pageCompact(p, scratch)
	}
	n := pageCellCount(p)
	newOff := pageFreeEnd(p) - len(cellBytes)
	copy(p[newOff:newOff+len(cellBytes)], cellBytes)
	setPageFreeEnd(p, newOff)

	// Shift slots [idx..n-1] right by 1.
	slotPos := HeaderSize + idx*SlotSize
	if idx < n {
		copy(p[slotPos+SlotSize:HeaderSize+(n+1)*SlotSize], p[slotPos:HeaderSize+n*SlotSize])
	}
	setSlotOffset(p, idx, newOff)
	setPageCellCount(p, n+1)
	return true
}

func pageDeleteCellAt(p []byte, idx int) {
	n := pageCellCount(p)
	off := slotOffset(p, idx)
	var sz int
	if pageType(p) == PageTypeLeaf {
		sz = leafCellSize(p, off)
	} else {
		sz = internalCellSize(p, off)
	}
	// Shift slots [idx+1..n-1] left by 1.
	slotPos := HeaderSize + idx*SlotSize
	if idx+1 < n {
		copy(p[slotPos:HeaderSize+(n-1)*SlotSize], p[slotPos+SlotSize:HeaderSize+n*SlotSize])
	}
	setPageCellCount(p, n-1)
	if off == pageFreeEnd(p) {
		setPageFreeEnd(p, off+sz)
	} else {
		setPageFragBytes(p, pageFragBytes(p)+sz)
	}
}
