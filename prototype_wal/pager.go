// PROTOTYPE (THROWAWAY) — validates ADR-0003/0004 Pager, Immutable Buffer Pool, WAL, Group Commit, and Checkpointing.
package prototype_wal

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"path/filepath"
	"sync"
	"sync/atomic"
)

const (
	PageSize       = 4096
	DBMagic        = "FYCSQL\x00\x00"
	WALMagic       = "FYCWAL\x00\x00"
	FormatVersion  = uint16(1)
	WALHeaderSize  = 32
	WALFrameHdrSz  = 32
	WALFrameStride = WALFrameHdrSz + PageSize
	Page2HdrSize   = 16
)

var (
	crc32cTable   = crc32.MakeTable(crc32.Castagnoli)
	ErrCorruption = errors.New("prototype: database corruption detected")
	ErrFailStop   = errors.New("prototype: engine in terminal Fail-Stop state after Sync error")
)

type SyncMode uint8

const (
	SyncFull SyncMode = iota
	SyncNormal
	SyncOff
)

type CheckpointMode uint8

const (
	CheckpointPassive CheckpointMode = iota
	CheckpointRestart
	CheckpointTruncate
)

// ---------------- Immutable Buffer Pool (noscan flat []byte slab + atomic CLOCK) ----------------

type FileKind uint8

const (
	FileDB  FileKind = 1
	FileWAL FileKind = 2
)

type CacheKey struct {
	Kind FileKind
	ID   uint32 // PageID for FileDB, FrameNo (0-based) for FileWAL
}

type poolSlot struct {
	key    CacheKey
	valid  bool
	pins   atomic.Int32
	refBit atomic.Uint32
	data   []byte // slice into flat noscan slab
}

type BufferPool struct {
	mu      sync.Mutex
	slab    []byte
	slots   []poolSlot
	index   map[CacheKey]int
	hand    int
	hits    atomic.Int64
	misses  atomic.Int64
}

func NewBufferPool(numFrames int) *BufferPool {
	slab := make([]byte, numFrames*PageSize) // single pointer-free noscan slab
	slots := make([]poolSlot, numFrames)
	for i := range slots {
		slots[i].data = slab[i*PageSize : (i+1)*PageSize]
	}
	return &BufferPool{
		slab:  slab,
		slots: slots,
		index: make(map[CacheKey]int, numFrames),
	}
}

type PinnedFrame struct {
	slot *poolSlot
}

func (p PinnedFrame) Bytes() []byte { return p.slot.data }
func (p PinnedFrame) Unpin()        { p.slot.pins.Add(-1) }

func (bp *BufferPool) Get(key CacheKey, seqScan bool) (PinnedFrame, bool) {
	bp.mu.Lock()
	idx, ok := bp.index[key]
	if !ok {
		bp.mu.Unlock()
		bp.misses.Add(1)
		return PinnedFrame{}, false
	}
	slot := &bp.slots[idx]
	slot.pins.Add(1)
	bp.mu.Unlock()
	if !seqScan {
		slot.refBit.Store(1)
	}
	bp.hits.Add(1)
	return PinnedFrame{slot: slot}, true
}

func (bp *BufferPool) Put(key CacheKey, page []byte, pin bool, seqScan bool) PinnedFrame {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	if idx, ok := bp.index[key]; ok {
		slot := &bp.slots[idx]
		if pin {
			slot.pins.Add(1)
		}
		if !seqScan {
			slot.refBit.Store(1)
		}
		return PinnedFrame{slot: slot}
	}
	// CLOCK eviction scan
	n := len(bp.slots)
	for attempts := 0; attempts < 2*n; attempts++ {
		slot := &bp.slots[bp.hand]
		idx := bp.hand
		bp.hand = (bp.hand + 1) % n
		if !slot.valid {
			copy(slot.data, page)
			slot.key = key
			slot.valid = true
			if !seqScan {
				slot.refBit.Store(1)
			}
			bp.index[key] = idx
			if pin {
				slot.pins.Add(1)
			}
			return PinnedFrame{slot: slot}
		}
		if slot.pins.Load() > 0 {
			continue
		}
		if slot.refBit.CompareAndSwap(1, 0) {
			continue
		}
		delete(bp.index, slot.key)
		copy(slot.data, page)
		slot.key = key
		if !seqScan {
			slot.refBit.Store(1)
		}
		bp.index[key] = idx
		if pin {
			slot.pins.Add(1)
		}
		return PinnedFrame{slot: slot}
	}
	panic("prototype buffer pool exhausted: all frames pinned")
}

func (bp *BufferPool) Invalidate(key CacheKey) {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	if idx, ok := bp.index[key]; ok {
		slot := &bp.slots[idx]
		// Only remove from index; any active pinned reader still holds its immutable slice safely!
		delete(bp.index, key)
		if slot.pins.Load() == 0 {
			slot.valid = false
		}
	}
}

func (bp *BufferPool) InvalidateAllWAL() {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	for k, idx := range bp.index {
		if k.Kind == FileWAL {
			delete(bp.index, k)
			if bp.slots[idx].pins.Load() == 0 {
				bp.slots[idx].valid = false
			}
		}
	}
}

func (bp *BufferPool) PinnedCount() int32 {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	var total int32
	for i := range bp.slots {
		total += bp.slots[i].pins.Load()
	}
	return total
}

// ---------------- Page & WAL Wire Codecs ----------------

type DBHeader struct {
	PageSize         uint32
	TotalPages       uint32
	CatalogRoot      uint32
	FreelistHead     uint32
	FreePageCount    uint32
	SchemaVersion    uint64
	LastCheckpointTS uint64
}

func encodeDBHeader(h DBHeader) []byte {
	buf := make([]byte, PageSize)
	copy(buf[0:8], DBMagic)
	binary.BigEndian.PutUint16(buf[8:10], FormatVersion)
	binary.BigEndian.PutUint32(buf[12:16], h.PageSize)
	binary.BigEndian.PutUint32(buf[16:20], h.TotalPages)
	binary.BigEndian.PutUint32(buf[20:24], h.CatalogRoot)
	binary.BigEndian.PutUint32(buf[24:28], h.FreelistHead)
	binary.BigEndian.PutUint32(buf[28:32], h.FreePageCount)
	binary.BigEndian.PutUint64(buf[32:40], h.SchemaVersion)
	binary.BigEndian.PutUint64(buf[40:48], h.LastCheckpointTS)
	crc := crc32.Checksum(buf[0:124], crc32cTable)
	binary.BigEndian.PutUint32(buf[124:128], crc)
	return buf
}

func decodeDBHeader(buf []byte) (DBHeader, error) {
	if len(buf) < PageSize || string(buf[0:8]) != DBMagic {
		return DBHeader{}, fmt.Errorf("%w: bad Page 1 magic", ErrCorruption)
	}
	if binary.BigEndian.Uint16(buf[8:10]) != FormatVersion {
		return DBHeader{}, fmt.Errorf("%w: unsupported format version", ErrCorruption)
	}
	wantCRC := binary.BigEndian.Uint32(buf[124:128])
	gotCRC := crc32.Checksum(buf[0:124], crc32cTable)
	if wantCRC != gotCRC {
		return DBHeader{}, fmt.Errorf("%w: Page 1 CRC mismatch", ErrCorruption)
	}
	return DBHeader{
		PageSize:         binary.BigEndian.Uint32(buf[12:16]),
		TotalPages:       binary.BigEndian.Uint32(buf[16:20]),
		CatalogRoot:      binary.BigEndian.Uint32(buf[20:24]),
		FreelistHead:     binary.BigEndian.Uint32(buf[24:28]),
		FreePageCount:    binary.BigEndian.Uint32(buf[28:32]),
		SchemaVersion:    binary.BigEndian.Uint64(buf[32:40]),
		LastCheckpointTS: binary.BigEndian.Uint64(buf[40:48]),
	}, nil
}

// MakeDataPage encodes a Page 2+ payload with the ADR-0004/0005 16-byte common header + CRC32C.
func MakeDataPage(pageType uint8, payload []byte) []byte {
	buf := make([]byte, PageSize)
	buf[0] = pageType
	copy(buf[Page2HdrSize:], payload)
	// CRC32C covers bytes [0..11] + [16..4096]
	h := crc32.New(crc32cTable)
	_, _ = h.Write(buf[0:12])
	_, _ = h.Write(buf[16:PageSize])
	binary.BigEndian.PutUint32(buf[12:16], h.Sum32())
	return buf
}

func VerifyDataPage(buf []byte) error {
	if len(buf) != PageSize {
		return fmt.Errorf("%w: short data page", ErrCorruption)
	}
	want := binary.BigEndian.Uint32(buf[12:16])
	h := crc32.New(crc32cTable)
	_, _ = h.Write(buf[0:12])
	_, _ = h.Write(buf[16:PageSize])
	if want != h.Sum32() {
		return fmt.Errorf("%w: Page 2+ CRC32C mismatch", ErrCorruption)
	}
	return nil
}

type WALHeader struct {
	PageSize      uint32
	CheckpointGen uint32
	Salt1         uint32
	Salt2         uint32
	CRC           uint32
}

func encodeWALHeader(h *WALHeader) []byte {
	buf := make([]byte, WALHeaderSize)
	copy(buf[0:8], WALMagic)
	binary.BigEndian.PutUint16(buf[8:10], FormatVersion)
	binary.BigEndian.PutUint32(buf[12:16], h.PageSize)
	binary.BigEndian.PutUint32(buf[16:20], h.CheckpointGen)
	binary.BigEndian.PutUint32(buf[20:24], h.Salt1)
	binary.BigEndian.PutUint32(buf[24:28], h.Salt2)
	h.CRC = crc32.Checksum(buf[0:28], crc32cTable)
	binary.BigEndian.PutUint32(buf[28:32], h.CRC)
	return buf
}

func decodeWALHeader(buf []byte) (WALHeader, error) {
	if len(buf) < WALHeaderSize || string(buf[0:8]) != WALMagic {
		return WALHeader{}, errors.New("invalid wal header")
	}
	want := binary.BigEndian.Uint32(buf[28:32])
	got := crc32.Checksum(buf[0:28], crc32cTable)
	if want != got {
		return WALHeader{}, errors.New("wal header crc mismatch")
	}
	return WALHeader{
		PageSize:      binary.BigEndian.Uint32(buf[12:16]),
		CheckpointGen: binary.BigEndian.Uint32(buf[16:20]),
		Salt1:         binary.BigEndian.Uint32(buf[20:24]),
		Salt2:         binary.BigEndian.Uint32(buf[24:28]),
		CRC:           want,
	}, nil
}

func randomSalt() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

// ---------------- Pager + WAL + Frame Index + Group Commit + Checkpointer ----------------

type FrameLoc struct {
	CommitTS uint64
	FrameNo  uint32
}

type commitReq struct {
	pages map[uint32][]byte // staged dirty pages (PageID -> 4KB)
	resp  chan commitResp
}

type commitResp struct {
	commitTS uint64
	err      error
}

type Pager struct {
	fs       FS
	dbPath   string
	walPath  string
	dbFile   File
	walFile  File
	pool     *BufferPool
	syncMode SyncMode

	// Lock order per ADR-0001: commitMu -> metaMu
	commitMu sync.Mutex
	metaMu   sync.RWMutex

	dbPages        uint32
	latestCommitTS uint64
	checkpointTS   uint64

	walHdr     WALHeader
	walFrames  uint32
	walLastCRC uint32
	frameIndex map[uint32][]FrameLoc // PageID -> ascending CommitTS frames

	activeSnapshots map[uint64]int // startTS -> refcount
	failStop        atomic.Bool
	failErr         error

	// Group commit queue
	gcMu      sync.Mutex
	gcQueue   []*commitReq
	gcRunning bool

	// Stats
	WALSyncCount atomic.Int64
	WALBytesOut  atomic.Int64
}

func OpenPager(fs FS, dbPath string, poolFrames int, syncMode SyncMode) (*Pager, error) {
	walPath := dbPath + "-wal"
	isNew := !fs.Exists(dbPath)
	dbFile, err := fs.Open(dbPath, true)
	if err != nil {
		return nil, err
	}
	if err := dbFile.Lock(); err != nil {
		_ = dbFile.Close()
		return nil, err
	}

	p := &Pager{
		fs:              fs,
		dbPath:          dbPath,
		walPath:         walPath,
		dbFile:          dbFile,
		pool:            NewBufferPool(poolFrames),
		syncMode:        syncMode,
		frameIndex:      make(map[uint32][]FrameLoc),
		activeSnapshots: make(map[uint64]int),
	}

	if isNew {
		hdr := DBHeader{PageSize: PageSize, TotalPages: 2, CatalogRoot: 2}
		p1 := encodeDBHeader(hdr)
		p2 := MakeDataPage(2, []byte("empty-catalog"))
		if _, err := dbFile.WriteAt(p1, 0); err != nil {
			_ = dbFile.Close()
			return nil, err
		}
		if _, err := dbFile.WriteAt(p2, PageSize); err != nil {
			_ = dbFile.Close()
			return nil, err
		}
		if syncMode != SyncOff {
			if err := dbFile.Sync(); err != nil {
				_ = dbFile.Close()
				return nil, err
			}
			if err := fs.SyncDir(filepath.Dir(dbPath)); err != nil {
				_ = dbFile.Close()
				return nil, err
			}
		}
		p.dbPages = 2
		return p, nil
	}

	// Existing DB: read & verify Page 1
	p1 := make([]byte, PageSize)
	if _, err := dbFile.ReadAt(p1, 0); err != nil {
		_ = dbFile.Close()
		return nil, fmt.Errorf("%w: reading Page 1: %v", ErrCorruption, err)
	}
	hdr, err := decodeDBHeader(p1)
	if err != nil {
		_ = dbFile.Close()
		return nil, err
	}
	if hdr.PageSize != PageSize {
		_ = dbFile.Close()
		return nil, fmt.Errorf("%w: unexpected page size %d", ErrCorruption, hdr.PageSize)
	}
	p.dbPages = hdr.TotalPages
	p.latestCommitTS = hdr.LastCheckpointTS
	p.checkpointTS = hdr.LastCheckpointTS

	// Single-pass read-only WAL recovery if <db>-wal exists
	if fs.Exists(walPath) {
		if err := p.recoverWAL(); err != nil {
			_ = dbFile.Close()
			return nil, err
		}
	}
	return p, nil
}

func (p *Pager) recoverWAL() error {
	wf, err := p.fs.Open(p.walPath, false)
	if err != nil {
		return err
	}
	size, err := wf.Size()
	if err != nil {
		_ = wf.Close()
		return err
	}
	// ADR-0004: <32B or torn initial WAL header with intact <db> = empty uncommitted WAL.
	if size < WALHeaderSize {
		_ = wf.Close()
		return nil
	}
	hdrBuf := make([]byte, WALHeaderSize)
	if _, err := wf.ReadAt(hdrBuf, 0); err != nil {
		_ = wf.Close()
		return nil
	}
	wh, err := decodeWALHeader(hdrBuf)
	if err != nil {
		// Torn WAL header (crash during first WAL creation or step-4 reset after DB sync)
		_ = wf.Close()
		return nil
	}
	if wh.PageSize != PageSize {
		_ = wf.Close()
		return fmt.Errorf("%w: WAL page size %d != DB page size %d", ErrCorruption, wh.PageSize, PageSize)
	}

	p.walFile = wf
	p.walHdr = wh
	prevCRC := wh.CRC
	frameBuf := make([]byte, WALFrameStride)

	type pendingRec struct {
		pageID  uint32
		frameNo uint32
	}
	var pending []pendingRec
	var lastValidFrames uint32
	var lastValidCRC = wh.CRC

	maxFrames := uint32((size - WALHeaderSize) / WALFrameStride)
	for fno := uint32(0); fno < maxFrames; fno++ {
		off := int64(WALHeaderSize) + int64(fno)*WALFrameStride
		if _, err := wf.ReadAt(frameBuf, off); err != nil {
			break
		}
		pageID := binary.BigEndian.Uint32(frameBuf[0:4])
		dbPageCount := binary.BigEndian.Uint32(frameBuf[4:8])
		commitTS := binary.BigEndian.Uint64(frameBuf[8:16])
		salt1 := binary.BigEndian.Uint32(frameBuf[16:20])
		salt2 := binary.BigEndian.Uint32(frameBuf[20:24])
		recPrevCRC := binary.BigEndian.Uint32(frameBuf[24:28])
		recFrameCRC := binary.BigEndian.Uint32(frameBuf[28:32])

		if salt1 != wh.Salt1 || salt2 != wh.Salt2 || recPrevCRC != prevCRC {
			break // old generation or out-of-order break
		}
		h := crc32.New(crc32cTable)
		_, _ = h.Write(frameBuf[0:28])
		_, _ = h.Write(frameBuf[WALFrameHdrSz:])
		if h.Sum32() != recFrameCRC {
			break // torn frame
		}

		prevCRC = recFrameCRC
		pending = append(pending, pendingRec{pageID: pageID, frameNo: fno})
		if dbPageCount > 0 {
			// Commit marker! Publish this transaction's frames into Frame Index.
			for _, rec := range pending {
				p.frameIndex[rec.pageID] = append(p.frameIndex[rec.pageID], FrameLoc{
					CommitTS: commitTS,
					FrameNo:  rec.frameNo,
				})
			}
			pending = pending[:0]
			p.dbPages = dbPageCount
			if commitTS > p.latestCommitTS {
				p.latestCommitTS = commitTS
			}
			lastValidFrames = fno + 1
			lastValidCRC = recFrameCRC
		}
	}

	p.walFrames = lastValidFrames
	p.walLastCRC = lastValidCRC
	return nil
}

func (p *Pager) ensureWALOpenLocked() error {
	if p.walFile != nil {
		return nil
	}
	wf, err := p.fs.Open(p.walPath, true)
	if err != nil {
		return err
	}
	wh := WALHeader{
		PageSize:      PageSize,
		CheckpointGen: 1,
		Salt1:         1,
		Salt2:         randomSalt(),
	}
	hdrBytes := encodeWALHeader(&wh)
	if _, err := wf.WriteAt(hdrBytes, 0); err != nil {
		_ = wf.Close()
		return err
	}
	if p.syncMode != SyncOff {
		if err := wf.Sync(); err != nil {
			_ = wf.Close()
			p.enterFailStop(err)
			return err
		}
		if err := p.fs.SyncDir(filepath.Dir(p.walPath)); err != nil {
			_ = wf.Close()
			p.enterFailStop(err)
			return err
		}
	}
	p.walFile = wf
	p.walHdr = wh
	p.walFrames = 0
	p.walLastCRC = wh.CRC
	return nil
}

func (p *Pager) enterFailStop(err error) {
	p.failErr = err
	p.failStop.Store(true)
}

func (p *Pager) IsFailStop() bool { return p.failStop.Load() }

// BeginSnapshot pins the current CommitTS so Checkpoint will not overwrite needed pages.
func (p *Pager) BeginSnapshot() uint64 {
	p.metaMu.Lock()
	defer p.metaMu.Unlock()
	ts := p.latestCommitTS
	p.activeSnapshots[ts]++
	return ts
}

func (p *Pager) EndSnapshot(ts uint64) {
	p.metaMu.Lock()
	defer p.metaMu.Unlock()
	if n := p.activeSnapshots[ts]; n <= 1 {
		delete(p.activeSnapshots, ts)
	} else {
		p.activeSnapshots[ts] = n - 1
	}
}

func (p *Pager) LatestCommitTS() uint64 {
	p.metaMu.RLock()
	defer p.metaMu.RUnlock()
	return p.latestCommitTS
}

func (p *Pager) DBPages() uint32 {
	p.metaMu.RLock()
	defer p.metaMu.RUnlock()
	return p.dbPages
}

// ReadPage resolves pageID at startTS via Frame Index -> Buffer Pool -> WAL/DB File.
func (p *Pager) ReadPage(startTS uint64, pageID uint32) (PinnedFrame, error) {
	p.metaMu.RLock()
	var walFrameNo uint32
	var inWAL bool
	if locs := p.frameIndex[pageID]; len(locs) > 0 {
		for i := len(locs) - 1; i >= 0; i-- {
			if locs[i].CommitTS <= startTS {
				walFrameNo = locs[i].FrameNo
				inWAL = true
				break
			}
		}
	}
	wf := p.walFile
	p.metaMu.RUnlock()

	if inWAL {
		key := CacheKey{Kind: FileWAL, ID: walFrameNo}
		if pf, ok := p.pool.Get(key, false); ok {
			return pf, nil
		}
		buf := make([]byte, PageSize)
		off := int64(WALHeaderSize) + int64(walFrameNo)*WALFrameStride + WALFrameHdrSz
		if _, err := wf.ReadAt(buf, off); err != nil {
			return PinnedFrame{}, err
		}
		if pageID >= 2 {
			if err := VerifyDataPage(buf); err != nil {
				return PinnedFrame{}, err
			}
		}
		return p.pool.Put(key, buf, true, false), nil
	}

	key := CacheKey{Kind: FileDB, ID: pageID}
	if pf, ok := p.pool.Get(key, false); ok {
		return pf, nil
	}
	buf := make([]byte, PageSize)
	off := int64(pageID-1) * PageSize
	if _, err := p.dbFile.ReadAt(buf, off); err != nil {
		return PinnedFrame{}, err
	}
	if pageID >= 2 {
		if err := VerifyDataPage(buf); err != nil {
			return PinnedFrame{}, err
		}
	}
	return p.pool.Put(key, buf, true, false), nil
}

// Commit submits staged dirty pages through the Commit Gate with zero-delay opportunistic Group Commit.
func (p *Pager) Commit(ctx context.Context, staged map[uint32][]byte) (uint64, error) {
	if p.failStop.Load() {
		return 0, fmt.Errorf("%w: %v", ErrFailStop, p.failErr)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	req := &commitReq{pages: staged, resp: make(chan commitResp, 1)}

	p.gcMu.Lock()
	p.gcQueue = append(p.gcQueue, req)
	isLeader := !p.gcRunning
	if isLeader {
		p.gcRunning = true
	}
	p.gcMu.Unlock()

	if isLeader {
		p.runGroupCommitLoop()
	}

	res := <-req.resp
	return res.commitTS, res.err
}

func (p *Pager) runGroupCommitLoop() {
	for {
		p.gcMu.Lock()
		if len(p.gcQueue) == 0 {
			p.gcRunning = false
			p.gcMu.Unlock()
			return
		}
		// Drain up to 32 already-queued waiters (zero artificial delay!).
		n := len(p.gcQueue)
		if n > 32 {
			n = 32
		}
		batch := make([]*commitReq, n)
		copy(batch, p.gcQueue[:n])
		p.gcQueue = p.gcQueue[n:]
		p.gcMu.Unlock()

		p.applyBatch(batch)
	}
}

func (p *Pager) applyBatch(batch []*commitReq) {
	p.commitMu.Lock()
	defer p.commitMu.Unlock()

	if p.failStop.Load() {
		err := fmt.Errorf("%w: %v", ErrFailStop, p.failErr)
		for _, r := range batch {
			r.resp <- commitResp{err: err}
		}
		return
	}
	if err := p.ensureWALOpenLocked(); err != nil {
		for _, r := range batch {
			r.resp <- commitResp{err: err}
		}
		return
	}

	// Stage all frames into WAL sequentially; publish only after single shared Sync()!
	type stagedFrame struct {
		pageID   uint32
		commitTS uint64
		frameNo  uint32
		data     []byte
	}
	var toPublish []stagedFrame
	curTS := p.latestCommitTS
	curPages := p.dbPages
	curFrameNo := p.walFrames
	curCRC := p.walLastCRC
	assignedTS := make([]uint64, len(batch))

	startFrameNo := p.walFrames
	totalFrames := 0
	for _, req := range batch {
		totalFrames += len(req.pages)
	}
	batchBuf := make([]byte, totalFrames*WALFrameStride)
	bufPos := 0

	for bi, req := range batch {
		curTS++
		assignedTS[bi] = curTS
		// Deterministic page order
		pids := make([]uint32, 0, len(req.pages))
		for pid := range req.pages {
			pids = append(pids, pid)
			if pid > curPages {
				curPages = pid
			}
		}
		// Sort pids for deterministic WAL ordering
		for i := 1; i < len(pids); i++ {
			for j := i; j > 0 && pids[j] < pids[j-1]; j-- {
				pids[j], pids[j-1] = pids[j-1], pids[j]
			}
		}
		for idx, pid := range pids {
			payload := req.pages[pid]
			dbCount := uint32(0)
			if idx == len(pids)-1 {
				dbCount = curPages // commit marker on final frame of txn
			}
			frameSlice := batchBuf[bufPos : bufPos+WALFrameStride]
			binary.BigEndian.PutUint32(frameSlice[0:4], pid)
			binary.BigEndian.PutUint32(frameSlice[4:8], dbCount)
			binary.BigEndian.PutUint64(frameSlice[8:16], curTS)
			binary.BigEndian.PutUint32(frameSlice[16:20], p.walHdr.Salt1)
			binary.BigEndian.PutUint32(frameSlice[20:24], p.walHdr.Salt2)
			binary.BigEndian.PutUint32(frameSlice[24:28], curCRC)
			copy(frameSlice[WALFrameHdrSz:], payload)
			h := crc32.New(crc32cTable)
			_, _ = h.Write(frameSlice[0:28])
			_, _ = h.Write(frameSlice[WALFrameHdrSz:])
			fcrc := h.Sum32()
			binary.BigEndian.PutUint32(frameSlice[28:32], fcrc)

			toPublish = append(toPublish, stagedFrame{
				pageID:   pid,
				commitTS: curTS,
				frameNo:  curFrameNo,
				data:     payload,
			})
			curCRC = fcrc
			curFrameNo++
			bufPos += WALFrameStride
		}
	}

	// Single contiguous WriteAt for the entire batch (fewer syscalls, no dangling mid-batch commit frames on WriteAt error)
	batchOff := int64(WALHeaderSize) + int64(startFrameNo)*WALFrameStride
	if _, err := p.walFile.WriteAt(batchBuf, batchOff); err != nil {
		for _, r := range batch {
			r.resp <- commitResp{err: err}
		}
		return
	}
	p.WALBytesOut.Add(int64(len(batchBuf)))

	if p.syncMode == SyncFull {
		if err := p.walFile.Sync(); err != nil {
			p.enterFailStop(err)
			for _, r := range batch {
				r.resp <- commitResp{err: err}
			}
			return
		}
		p.WALSyncCount.Add(1)
	}

	// Publish into Frame Index + warm immutable WAL frames into Buffer Pool
	p.metaMu.Lock()
	for _, sf := range toPublish {
		p.frameIndex[sf.pageID] = append(p.frameIndex[sf.pageID], FrameLoc{
			CommitTS: sf.commitTS,
			FrameNo:  sf.frameNo,
		})
		p.pool.Put(CacheKey{Kind: FileWAL, ID: sf.frameNo}, sf.data, false, false)
	}
	p.latestCommitTS = curTS
	p.dbPages = curPages
	p.walFrames = curFrameNo
	p.walLastCRC = curCRC
	p.metaMu.Unlock()

	for bi, r := range batch {
		r.resp <- commitResp{commitTS: assignedTS[bi]}
	}
}

// Checkpoint executes the ADR-0004 four-step barriered checkpoint:
// 1. WAL.Sync -> 2. DB.Write (<= safeMaxTS) -> 3. DB.Sync -> 4. WAL.Reset/Truncate + Sync.
func (p *Pager) Checkpoint(mode CheckpointMode) (checkpointedPages int, resetWAL bool, err error) {
	p.commitMu.Lock()
	defer p.commitMu.Unlock()

	if p.failStop.Load() {
		return 0, false, fmt.Errorf("%w: %v", ErrFailStop, p.failErr)
	}
	if p.walFile == nil || p.walFrames == 0 {
		return 0, false, nil
	}

	// Compute safeMaxTS = minActiveSnapshotTS (or latestCommitTS if no active snapshots)
	p.metaMu.RLock()
	safeMaxTS := p.latestCommitTS
	hasActiveSnapshots := len(p.activeSnapshots) > 0
	for ts := range p.activeSnapshots {
		if ts < safeMaxTS {
			safeMaxTS = ts
		}
	}
	// Deduplicate latest frame <= safeMaxTS per PageID
	toCopy := make(map[uint32]uint32) // PageID -> FrameNo
	for pid, locs := range p.frameIndex {
		for i := len(locs) - 1; i >= 0; i-- {
			if locs[i].CommitTS <= safeMaxTS {
				toCopy[pid] = locs[i].FrameNo
				break
			}
		}
	}
	canReset := !hasActiveSnapshots && safeMaxTS == p.latestCommitTS
	latestTS := p.latestCommitTS
	dbPages := p.dbPages
	p.metaMu.RUnlock()

	if len(toCopy) == 0 && !canReset {
		return 0, false, nil
	}

	// Step 1: WAL.Sync (guarantee WAL frames durable before touching <db>)
	if p.syncMode != SyncOff {
		if err := p.walFile.Sync(); err != nil {
			p.enterFailStop(err)
			return 0, false, err
		}
	}

	// Step 2: Write deduplicated pages to <db>
	buf := make([]byte, PageSize)
	for pid, fno := range toCopy {
		walOff := int64(WALHeaderSize) + int64(fno)*WALFrameStride + WALFrameHdrSz
		if _, err := p.walFile.ReadAt(buf, walOff); err != nil {
			return 0, false, err
		}
		dbOff := int64(pid-1) * PageSize
		if _, err := p.dbFile.WriteAt(buf, dbOff); err != nil {
			return 0, false, err
		}
		p.pool.Invalidate(CacheKey{Kind: FileDB, ID: pid})
		checkpointedPages++
	}

	// Update Page 1 header with new TotalPages and LastCheckpointTS
	p1 := encodeDBHeader(DBHeader{
		PageSize:         PageSize,
		TotalPages:       dbPages,
		CatalogRoot:      2,
		LastCheckpointTS: safeMaxTS,
	})
	if _, err := p.dbFile.WriteAt(p1, 0); err != nil {
		return 0, false, err
	}
	p.pool.Invalidate(CacheKey{Kind: FileDB, ID: 1})

	// Step 3: DB.Sync (guarantee <db> durable before touching WAL reset/truncate)
	if p.syncMode != SyncOff {
		if err := p.dbFile.Sync(); err != nil {
			p.enterFailStop(err)
			return 0, false, err
		}
	}

	p.metaMu.Lock()
	p.checkpointTS = safeMaxTS
	p.metaMu.Unlock()

	// Step 4: Reset/Truncate WAL only when ALL committed frames are checkpointed & zero readers pin WAL
	if canReset && (mode == CheckpointRestart || mode == CheckpointTruncate) && safeMaxTS == latestTS {
		newHdr := WALHeader{
			PageSize:      PageSize,
			CheckpointGen: p.walHdr.CheckpointGen + 1,
			Salt1:         p.walHdr.Salt1 + 1,
			Salt2:         randomSalt(),
		}
		if mode == CheckpointTruncate {
			if err := p.walFile.Truncate(0); err != nil {
				return checkpointedPages, false, err
			}
		}
		hdrBytes := encodeWALHeader(&newHdr)
		if _, err := p.walFile.WriteAt(hdrBytes, 0); err != nil {
			return checkpointedPages, false, err
		}
		if p.syncMode != SyncOff {
			if err := p.walFile.Sync(); err != nil {
				p.enterFailStop(err)
				return checkpointedPages, false, err
			}
		}
		p.metaMu.Lock()
		p.walHdr = newHdr
		p.walFrames = 0
		p.walLastCRC = newHdr.CRC
		p.frameIndex = make(map[uint32][]FrameLoc)
		p.pool.InvalidateAllWAL()
		p.metaMu.Unlock()
		resetWAL = true
	}

	return checkpointedPages, resetWAL, nil
}

// Close runs CheckpointTruncate and removes <db>-wal on clean close; preserves WAL on Fail-Stop.
func (p *Pager) Close() error {
	if !p.failStop.Load() && p.walFile != nil {
		_, _, err := p.Checkpoint(CheckpointTruncate)
		if err == nil {
			_ = p.walFile.Close()
			p.walFile = nil
			_ = p.fs.Remove(p.walPath)
			if p.syncMode != SyncOff {
				_ = p.fs.SyncDir(filepath.Dir(p.dbPath))
			}
		}
	} else if p.walFile != nil {
		_ = p.walFile.Close()
		p.walFile = nil
	}
	return p.dbFile.Close()
}

// CheckPageEquals is a helper for prototype tests to inspect a data page's string prefix.
func ReadPageString(p *Pager, ts uint64, pid uint32) (string, error) {
	pf, err := p.ReadPage(ts, pid)
	if err != nil {
		return "", err
	}
	defer pf.Unpin()
	raw := pf.Bytes()[Page2HdrSize:]
	if idx := bytes.IndexByte(raw, 0); idx >= 0 {
		return string(raw[:idx]), nil
	}
	return string(raw), nil
}
