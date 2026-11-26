// PROTOTYPE (THROWAWAY) — validates ADR-0003/0004/0008 VFS seam, CrashFS, and ErrorFS.
package prototype_wal

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var (
	ErrLocked     = errors.New("prototype: database file is locked by another process")
	ErrInjectedIO = errors.New("prototype: injected VFS I/O error")
)

type OpKind uint8

const (
	OpRead OpKind = iota + 1
	OpWrite
	OpSync
	OpTruncate
	OpSyncDir
	OpRemove
	OpOpen
)

// File is the narrow internal VFS file seam.
type File interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Sync() error
	Truncate(size int64) error
	Size() (int64, error)
	Lock() error
	Unlock() error
	Close() error
}

// FS is the narrow internal VFS seam wrapping OS files, dir sync, and locks.
type FS interface {
	Open(path string, create bool) (File, error)
	Remove(path string) error
	Exists(path string) bool
	SyncDir(dir string) error
}

// ---------------- OSFS (Real Linux/macOS stdlib VFS) ----------------

type OSFS struct{}

type osFile struct {
	f      *os.File
	locked bool
}

func (OSFS) Open(path string, create bool) (File, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	return &osFile{f: f}, nil
}

func (OSFS) Remove(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (OSFS) Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (OSFS) SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (f *osFile) ReadAt(p []byte, off int64) (int, error)  { return f.f.ReadAt(p, off) }
func (f *osFile) WriteAt(p []byte, off int64) (int, error) { return f.f.WriteAt(p, off) }
func (f *osFile) Sync() error                              { return f.f.Sync() }
func (f *osFile) Truncate(size int64) error                { return f.f.Truncate(size) }
func (f *osFile) Size() (int64, error) {
	st, err := f.f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
func (f *osFile) Lock() error {
	if err := syscall.Flock(int(f.f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return ErrLocked
	}
	f.locked = true
	return nil
}
func (f *osFile) Unlock() error {
	if !f.locked {
		return nil
	}
	f.locked = false
	return syscall.Flock(int(f.f.Fd()), syscall.LOCK_UN)
}
func (f *osFile) Close() error {
	_ = f.Unlock()
	return f.f.Close()
}

// ---------------- CrashFS (Dual-Layer Durable/Pending In-Memory FS) ----------------

type pendingWrite struct {
	off  int64
	data []byte
}

type crashFileState struct {
	syncedData    []byte
	liveData      []byte
	pendingWrites []pendingWrite
	syncedExists  bool // true once parent dir SyncDir has run after creation
	liveExists    bool
	locked        bool
}

type CutPoint struct {
	Index    int
	Kind     OpKind
	Path     string
	Snapshot *CrashFS // deep copy right after this op
	LastOff  int64
	LastLen  int
}

// CrashFS simulates OS page cache vs durable disk, recording cut-points for exhaustive crash tests.
type CrashFS struct {
	mu        sync.Mutex
	files     map[string]*crashFileState
	record    bool
	cutPoints []CutPoint
	opSeq     int
}

func NewCrashFS(record bool) *CrashFS {
	return &CrashFS{
		files:  make(map[string]*crashFileState),
		record: record,
	}
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

func (c *CrashFS) deepCloneLocked() *CrashFS {
	cp := &CrashFS{files: make(map[string]*crashFileState, len(c.files))}
	for k, st := range c.files {
		pw := make([]pendingWrite, len(st.pendingWrites))
		for i, w := range st.pendingWrites {
			pw[i] = pendingWrite{off: w.off, data: cloneBytes(w.data)}
		}
		cp.files[k] = &crashFileState{
			syncedData:    cloneBytes(st.syncedData),
			liveData:      cloneBytes(st.liveData),
			pendingWrites: pw,
			syncedExists:  st.syncedExists,
			liveExists:    st.liveExists,
		}
	}
	return cp
}

func (c *CrashFS) recordCutLocked(kind OpKind, path string, off int64, n int) {
	c.opSeq++
	if !c.record {
		return
	}
	c.cutPoints = append(c.cutPoints, CutPoint{
		Index:    c.opSeq,
		Kind:     kind,
		Path:     path,
		Snapshot: c.deepCloneLocked(),
		LastOff:  off,
		LastLen:  n,
	})
}

func (c *CrashFS) CutPoints() []CutPoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CutPoint, len(c.cutPoints))
	copy(out, c.cutPoints)
	return out
}

// CloneCleanPowerLoss drops all unsynced file writes and unsynced directory entries.
func (c *CrashFS) CloneCleanPowerLoss() *CrashFS {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := &CrashFS{files: make(map[string]*crashFileState)}
	for k, st := range c.files {
		if !st.syncedExists {
			continue
		}
		d := cloneBytes(st.syncedData)
		out.files[k] = &crashFileState{
			syncedData:   d,
			liveData:     cloneBytes(d),
			syncedExists: true,
			liveExists:   true,
		}
	}
	return out
}

// CloneTornWrite starts from synced disk, then partially persists the latest pending write
// (e.g., 512-byte sector prefix + corrupted byte) on targetPath to simulate torn sector power loss.
func (c *CrashFS) CloneTornWrite(targetPath string, keepBytes int) *CrashFS {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := &CrashFS{files: make(map[string]*crashFileState)}
	for k, st := range c.files {
		if !st.syncedExists {
			continue
		}
		d := cloneBytes(st.syncedData)
		if k == targetPath && len(st.pendingWrites) > 0 {
			// Apply earlier pending writes cleanly, then tear the last one.
			for i := 0; i < len(st.pendingWrites)-1; i++ {
				d = applyWrite(d, st.pendingWrites[i].off, st.pendingWrites[i].data)
			}
			last := st.pendingWrites[len(st.pendingWrites)-1]
			n := keepBytes
			if n > len(last.data) {
				n = len(last.data) / 2
			}
			torn := make([]byte, len(last.data))
			copy(torn[:n], last.data[:n])
			for j := n; j < len(torn); j++ {
				torn[j] = 0xAA // garbage tail bytes
			}
			d = applyWrite(d, last.off, torn)
		}
		out.files[k] = &crashFileState{
			syncedData:   d,
			liveData:     cloneBytes(d),
			syncedExists: true,
			liveExists:   true,
		}
	}
	return out
}

// CloneOutOfOrder persists ONLY the LAST pending write (e.g. a commit frame) while dropping
// an earlier pending write in the same unsynced window, testing out-of-order disk persistence.
func (c *CrashFS) CloneOutOfOrder(targetPath string) *CrashFS {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := &CrashFS{files: make(map[string]*crashFileState)}
	for k, st := range c.files {
		if !st.syncedExists {
			continue
		}
		d := cloneBytes(st.syncedData)
		if k == targetPath && len(st.pendingWrites) >= 2 {
			// Drop pendingWrites[0], apply pendingWrites[1:]
			for i := 1; i < len(st.pendingWrites); i++ {
				d = applyWrite(d, st.pendingWrites[i].off, st.pendingWrites[i].data)
			}
		} else if k == targetPath && len(st.pendingWrites) == 1 && len(st.pendingWrites[0].data) >= 2*WALFrameStride {
			// Multi-frame contiguous WriteAt: drop first frame (leave zeroes/old bytes), persist later frames!
			w := st.pendingWrites[0]
			d = applyWrite(d, w.off+WALFrameStride, w.data[WALFrameStride:])
		}
		out.files[k] = &crashFileState{
			syncedData:   d,
			liveData:     cloneBytes(d),
			syncedExists: true,
			liveExists:   true,
		}
	}
	return out
}

func applyWrite(buf []byte, off int64, data []byte) []byte {
	end := int(off) + len(data)
	if end > len(buf) {
		nb := make([]byte, end)
		copy(nb, buf)
		buf = nb
	}
	copy(buf[int(off):end], data)
	return buf
}

func (c *CrashFS) Open(path string, create bool) (File, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	clean := filepath.Clean(path)
	st, ok := c.files[clean]
	if !ok || !st.liveExists {
		if !create {
			return nil, os.ErrNotExist
		}
		st = &crashFileState{liveExists: true}
		c.files[clean] = st
		c.recordCutLocked(OpOpen, clean, 0, 0)
	}
	return &crashFile{fs: c, path: clean}, nil
}

func (c *CrashFS) Remove(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	clean := filepath.Clean(path)
	if st, ok := c.files[clean]; ok && st.liveExists {
		st.liveExists = false
		c.recordCutLocked(OpRemove, clean, 0, 0)
	}
	return nil
}

func (c *CrashFS) Exists(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.files[filepath.Clean(path)]
	return ok && st.liveExists
}

func (c *CrashFS) SyncDir(dir string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cleanDir := filepath.Clean(dir)
	for k, st := range c.files {
		if filepath.Clean(filepath.Dir(k)) == cleanDir {
			st.syncedExists = st.liveExists
			if !st.syncedExists {
				st.syncedData = nil
				st.pendingWrites = nil
			}
		}
	}
	c.recordCutLocked(OpSyncDir, cleanDir, 0, 0)
	return nil
}

type crashFile struct {
	fs   *CrashFS
	path string
}

func (f *crashFile) ReadAt(p []byte, off int64) (int, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	st := f.fs.files[f.path]
	if st == nil {
		return 0, os.ErrNotExist
	}
	if off >= int64(len(st.liveData)) {
		return 0, io.EOF
	}
	n := copy(p, st.liveData[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *crashFile) WriteAt(p []byte, off int64) (int, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	st := f.fs.files[f.path]
	if st == nil {
		return 0, os.ErrNotExist
	}
	st.liveData = applyWrite(st.liveData, off, p)
	st.pendingWrites = append(st.pendingWrites, pendingWrite{off: off, data: cloneBytes(p)})
	f.fs.recordCutLocked(OpWrite, f.path, off, len(p))
	return len(p), nil
}

func (f *crashFile) Sync() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	st := f.fs.files[f.path]
	if st == nil {
		return os.ErrNotExist
	}
	st.syncedData = cloneBytes(st.liveData)
	st.pendingWrites = nil
	f.fs.recordCutLocked(OpSync, f.path, 0, len(st.syncedData))
	return nil
}

func (f *crashFile) Truncate(size int64) error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	st := f.fs.files[f.path]
	if st == nil {
		return os.ErrNotExist
	}
	if size < int64(len(st.liveData)) {
		st.liveData = cloneBytes(st.liveData[:size])
	} else if size > int64(len(st.liveData)) {
		nb := make([]byte, size)
		copy(nb, st.liveData)
		st.liveData = nb
	}
	f.fs.recordCutLocked(OpTruncate, f.path, size, 0)
	return nil
}

func (f *crashFile) Size() (int64, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	st := f.fs.files[f.path]
	if st == nil {
		return 0, os.ErrNotExist
	}
	return int64(len(st.liveData)), nil
}

func (f *crashFile) Lock() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	st := f.fs.files[f.path]
	if st.locked {
		return ErrLocked
	}
	st.locked = true
	return nil
}

func (f *crashFile) Unlock() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if st := f.fs.files[f.path]; st != nil {
		st.locked = false
	}
	return nil
}

func (f *crashFile) Close() error {
	_ = f.Unlock()
	return nil
}

// ---------------- ErrorFS (Deterministic OnIndex Fault Injector) ----------------

type ErrorFS struct {
	Base       FS
	mu         sync.Mutex
	TargetKind OpKind
	FailAt     int // 1-based count of TargetKind ops; 0 disables
	Count      int
}

func (e *ErrorFS) check(kind OpKind) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if kind != e.TargetKind {
		return nil
	}
	e.Count++
	if e.FailAt > 0 && e.Count == e.FailAt {
		return ErrInjectedIO
	}
	return nil
}

func (e *ErrorFS) Open(path string, create bool) (File, error) {
	if err := e.check(OpOpen); err != nil {
		return nil, err
	}
	f, err := e.Base.Open(path, create)
	if err != nil {
		return nil, err
	}
	return &errorFile{efs: e, f: f}, nil
}
func (e *ErrorFS) Remove(path string) error {
	if err := e.check(OpRemove); err != nil {
		return err
	}
	return e.Base.Remove(path)
}
func (e *ErrorFS) Exists(path string) bool { return e.Base.Exists(path) }
func (e *ErrorFS) SyncDir(dir string) error {
	if err := e.check(OpSyncDir); err != nil {
		return err
	}
	return e.Base.SyncDir(dir)
}

type errorFile struct {
	efs *ErrorFS
	f   File
}

func (ef *errorFile) ReadAt(p []byte, off int64) (int, error) {
	if err := ef.efs.check(OpRead); err != nil {
		return 0, err
	}
	return ef.f.ReadAt(p, off)
}
func (ef *errorFile) WriteAt(p []byte, off int64) (int, error) {
	if err := ef.efs.check(OpWrite); err != nil {
		return 0, err
	}
	return ef.f.WriteAt(p, off)
}
func (ef *errorFile) Sync() error {
	if err := ef.efs.check(OpSync); err != nil {
		return err
	}
	return ef.f.Sync()
}
func (ef *errorFile) Truncate(size int64) error {
	if err := ef.efs.check(OpTruncate); err != nil {
		return err
	}
	return ef.f.Truncate(size)
}
func (ef *errorFile) Size() (int64, error) { return ef.f.Size() }
func (ef *errorFile) Lock() error          { return ef.f.Lock() }
func (ef *errorFile) Unlock() error        { return ef.f.Unlock() }
func (ef *errorFile) Close() error         { return ef.f.Close() }
