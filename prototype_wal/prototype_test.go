// PROTOTYPE (THROWAWAY) — validates CrashFS exhaustive cut-points, ErrorFS Fail-Stop, Snapshot watermark, and real OS fsync latency.
package prototype_wal

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSnapshotIsolationAndCheckpointWatermark(t *testing.T) {
	fs := NewCrashFS(false)
	p, err := OpenPager(fs, "/db/test.db", 32, SyncFull)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer p.Close()

	// Tx1 writes page 3 = "v1"
	ts1, err := p.Commit(context.Background(), map[uint32][]byte{
		3: MakeDataPage(2, []byte("v1")),
	})
	if err != nil {
		t.Fatalf("commit1: %v", err)
	}

	// Reader pins snapshot at ts1
	snap1 := p.BeginSnapshot()
	if snap1 != ts1 {
		t.Fatalf("expected snap1=%d, got %d", ts1, snap1)
	}

	// Tx2 overwrites page 3 = "v2", Tx3 overwrites page 3 = "v3"
	if _, err := p.Commit(context.Background(), map[uint32][]byte{3: MakeDataPage(2, []byte("v2"))}); err != nil {
		t.Fatalf("commit2: %v", err)
	}
	ts3, err := p.Commit(context.Background(), map[uint32][]byte{3: MakeDataPage(2, []byte("v3"))})
	if err != nil {
		t.Fatalf("commit3: %v", err)
	}

	// CheckpointPassive while snap1 is pinned: MUST NOT overwrite page 3 in <db> past snap1 (v1), MUST NOT reset WAL
	_, reset, err := p.Checkpoint(CheckpointPassive)
	if err != nil {
		t.Fatalf("checkpoint passive: %v", err)
	}
	if reset {
		t.Fatalf("expected WAL NOT to reset while snapshot is pinned")
	}

	// snap1 still sees "v1", latest sees "v3"
	v1, err := ReadPageString(p, snap1, 3)
	if err != nil || v1 != "v1" {
		t.Fatalf("snap1 expected v1, got %q (err=%v)", v1, err)
	}
	v3, err := ReadPageString(p, ts3, 3)
	if err != nil || v3 != "v3" {
		t.Fatalf("latest expected v3, got %q (err=%v)", v3, err)
	}

	// Release snap1 -> CheckpointRestart resets WAL and rotates salts cleanly
	p.EndSnapshot(snap1)
	_, reset, err = p.Checkpoint(CheckpointRestart)
	if err != nil || !reset {
		t.Fatalf("expected WAL reset after snapshot release, got reset=%v err=%v", reset, err)
	}

	// Post-reset read from <db> still sees "v3" and zero leaked pins
	v3After, err := ReadPageString(p, ts3, 3)
	if err != nil || v3After != "v3" {
		t.Fatalf("post-reset expected v3, got %q (err=%v)", v3After, err)
	}
	if pins := p.pool.PinnedCount(); pins != 0 {
		t.Fatalf("buffer pool pin leak: %d", pins)
	}
}

func TestExhaustiveCrashCutPoints(t *testing.T) {
	fs := NewCrashFS(true)
	dbPath := "/db/crash.db"
	walPath := dbPath + "-wal"

	// Record expected committed states (state[k] = map[pageID]string after tx k)
	type expectedState struct {
		txNum int
		pages map[uint32]string
	}
	history := []expectedState{
		{txNum: 0, pages: map[uint32]string{2: "empty-catalog"}},
	}

	p, err := OpenPager(fs, dbPath, 16, SyncFull)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Track minimum acknowledged tx at each cut-point index (Durability invariant!)
	type ackPoint struct {
		cutIdx int
		minTx  int
	}
	var acks []ackPoint

	runTx := func(txNum int, updates map[uint32]string) {
		staged := make(map[uint32][]byte, len(updates))
		nextPages := make(map[uint32]string, len(history[len(history)-1].pages))
		for k, v := range history[len(history)-1].pages {
			nextPages[k] = v
		}
		for pid, val := range updates {
			staged[pid] = MakeDataPage(2, []byte(val))
			nextPages[pid] = val
		}
		if _, err := p.Commit(context.Background(), staged); err != nil {
			t.Fatalf("tx %d commit: %v", txNum, err)
		}
		history = append(history, expectedState{txNum: txNum, pages: nextPages})
		acks = append(acks, ackPoint{cutIdx: len(fs.CutPoints()), minTx: txNum})
	}

	// 1. Multi-page commits
	runTx(1, map[uint32]string{3: "tx1-p3", 4: "tx1-p4"})
	runTx(2, map[uint32]string{3: "tx2-p3", 5: "tx2-p5"})
	runTx(3, map[uint32]string{4: "tx3-p4", 6: "tx3-p6"})

	// 2. CheckpointPassive + more commits
	if _, _, err := p.Checkpoint(CheckpointPassive); err != nil {
		t.Fatalf("checkpoint passive: %v", err)
	}
	runTx(4, map[uint32]string{3: "tx4-p3", 5: "tx4-p5"})

	// 3. CheckpointRestart (salt rotation + wrap to Frame 0) + more commits
	if _, reset, err := p.Checkpoint(CheckpointRestart); err != nil || !reset {
		t.Fatalf("checkpoint restart: reset=%v err=%v", reset, err)
	}
	runTx(5, map[uint32]string{3: "tx5-p3", 6: "tx5-p6"})
	runTx(6, map[uint32]string{4: "tx6-p4", 7: "tx6-p7"})

	// 4. Clean Close (CheckpointTruncate + remove WAL)
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	cuts := fs.CutPoints()
	t.Logf("Enumerated %d VFS cut-points across 6 multi-page txns + Passive/Restart/Truncate checkpoints + Close", len(cuts))

	verifyCrashImage := func(label string, img *CrashFS, minAckTx int) {
		if !img.Exists(dbPath) {
			// Crash happened before initial DB creation SyncDir completed: valid empty state.
			if minAckTx > 0 {
				t.Fatalf("%s: DB vanished after tx %d was acknowledged!", label, minAckTx)
			}
			return
		}
		rp, err := OpenPager(img, dbPath, 16, SyncFull)
		if err != nil {
			t.Fatalf("%s: OpenPager failed after crash: %v", label, err)
		}
		defer rp.Close()

		ts := rp.LatestCommitTS()
		// Verify every page 2..DBPages passes CRC32C and matches EXACTLY ONE historical committed prefix >= minAckTx
		got := make(map[uint32]string)
		for pid := uint32(2); pid <= rp.DBPages(); pid++ {
			s, err := ReadPageString(rp, ts, pid)
			if err != nil {
				t.Fatalf("%s: ReadPage(%d) failed: %v", label, pid, err)
			}
			got[pid] = s
		}
		if pins := rp.pool.PinnedCount(); pins != 0 {
			t.Fatalf("%s: pin leak after recovery: %d", label, pins)
		}

		matchedTx := -1
		for _, h := range history {
			if len(h.pages) != len(got) {
				continue
			}
			ok := true
			for pid, want := range h.pages {
				if got[pid] != want {
					ok = false
					break
				}
			}
			if ok {
				matchedTx = h.txNum
				break
			}
		}
		if matchedTx < 0 {
			t.Fatalf("%s: TORN/CORRUPT STATE! got=%v did not match any committed prefix", label, got)
		}
		if matchedTx < minAckTx {
			t.Fatalf("%s: DURABILITY VIOLATION! acknowledged tx %d lost (recovered tx %d)", label, minAckTx, matchedTx)
		}
	}

	checkedImages := 0
	for i, cut := range cuts {
		minAck := 0
		for _, a := range acks {
			if i+1 >= a.cutIdx {
				minAck = a.minTx
			}
		}
		// Mode 1: Clean power loss (drop unsynced writes & dir entries)
		verifyCrashImage(fmt.Sprintf("cut#%d(%v:%s)/clean", cut.Index, cut.Kind, filepath.Base(cut.Path)), cut.Snapshot.CloneCleanPowerLoss(), minAck)
		checkedImages++

		// Mode 2: Torn sector write on WAL (512B prefix + garbage tail)
		if cut.Kind == OpWrite && cut.Path == walPath {
			verifyCrashImage(fmt.Sprintf("cut#%d/torn-512B", cut.Index), cut.Snapshot.CloneTornWrite(walPath, 512), minAck)
			verifyCrashImage(fmt.Sprintf("cut#%d/torn-16B", cut.Index), cut.Snapshot.CloneTornWrite(walPath, 16), minAck)
			checkedImages += 2

			// Mode 3: Out-of-order persistence (commit frame hits disk, earlier data frame dropped)
			verifyCrashImage(fmt.Sprintf("cut#%d/out-of-order", cut.Index), cut.Snapshot.CloneOutOfOrder(walPath), minAck)
			checkedImages++
		}
	}
	t.Logf("PASSED %d simulated power-loss recovery images (clean, torn-512B, torn-16B, out-of-order) with 0 torn transactions and 0 lost commits!", checkedImages)
}

func TestErrorFSFailStopAndZeroDirtyPages(t *testing.T) {
	// 1. Pre-Sync WriteAt failure -> zero dirty frames, engine stays healthy, retry succeeds
	base := NewCrashFS(false)
	efs := &ErrorFS{Base: base, TargetKind: OpWrite}
	p, err := OpenPager(efs, "/db/err.db", 16, SyncFull)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// First commit creates WAL header (1 write) + frame (2nd write); fail on 2nd write
	efs.Count = 0
	efs.FailAt = 2
	_, err = p.Commit(context.Background(), map[uint32][]byte{3: MakeDataPage(2, []byte("should-not-stick"))})
	if !errors.Is(err, ErrInjectedIO) {
		t.Fatalf("expected ErrInjectedIO on WriteAt, got %v", err)
	}
	if p.IsFailStop() {
		t.Fatalf("pre-Sync WriteAt error should NOT trigger Fail-Stop")
	}
	if p.LatestCommitTS() != 0 {
		t.Fatalf("failed pre-Sync commit published commitTS=%d", p.LatestCommitTS())
	}

	// Retry succeeds cleanly once fault cleared
	efs.FailAt = 0
	ts1, err := p.Commit(context.Background(), map[uint32][]byte{3: MakeDataPage(2, []byte("good-v1"))})
	if err != nil || ts1 != 1 {
		t.Fatalf("retry commit failed: ts=%d err=%v", ts1, err)
	}

	// 2. Sync() failure -> MUST enter terminal Fail-Stop and preserve <db>-wal on Close()
	efs.TargetKind = OpSync
	efs.Count = 0
	efs.FailAt = 1
	_, err = p.Commit(context.Background(), map[uint32][]byte{3: MakeDataPage(2, []byte("sync-fail"))})
	if !errors.Is(err, ErrInjectedIO) {
		t.Fatalf("expected ErrInjectedIO on Sync, got %v", err)
	}
	if !p.IsFailStop() {
		t.Fatalf("Sync() error MUST trigger terminal Fail-Stop")
	}
	// Subsequent commit MUST immediately fail with ErrFailStop
	efs.FailAt = 0
	if _, err := p.Commit(context.Background(), map[uint32][]byte{3: MakeDataPage(2, []byte("after-failstop"))}); !errors.Is(err, ErrFailStop) {
		t.Fatalf("expected ErrFailStop after Fail-Stop, got %v", err)
	}
	_ = p.Close()
	if !base.Exists("/db/err.db-wal") {
		t.Fatalf("Fail-Stop Close() MUST preserve <db>-wal!")
	}
	// Reopen after simulated power loss (unsynced page-cache dropped) -> MUST recover exact durable "good-v1"
	powerLossImg := base.CloneCleanPowerLoss()
	rpPL, err := OpenPager(powerLossImg, "/db/err.db", 16, SyncFull)
	if err != nil {
		t.Fatalf("reopen after Fail-Stop power loss: %v", err)
	}
	gotPL, err := ReadPageString(rpPL, rpPL.LatestCommitTS(), 3)
	_ = rpPL.Close()
	if err != nil || gotPL != "good-v1" {
		t.Fatalf("expected good-v1 after Fail-Stop power-loss reopen, got %q (err=%v)", gotPL, err)
	}

	// Reopen WITHOUT power loss (OS page cache retained): Sync()-failed commit outcome is indeterminate
	// ("good-v1" OR "sync-fail"), NEVER corrupt or torn!
	rpLive, err := OpenPager(base, "/db/err.db", 16, SyncFull)
	if err != nil {
		t.Fatalf("reopen after Fail-Stop (live cache): %v", err)
	}
	defer rpLive.Close()
	gotLive, err := ReadPageString(rpLive, rpLive.LatestCommitTS(), 3)
	if err != nil || (gotLive != "good-v1" && gotLive != "sync-fail") {
		t.Fatalf("expected good-v1 or sync-fail on live-cache reopen, got %q (err=%v)", gotLive, err)
	}
}

func TestRealOSFsyncAndRecoveryProfile(t *testing.T) {
	dir := t.TempDir()

	// 1. Single-goroutine SyncFull latency (50 commits)
	pFull, err := OpenPager(OSFS{}, filepath.Join(dir, "full.db"), 256, SyncFull)
	if err != nil {
		t.Fatalf("open full: %v", err)
	}
	const singleN = 50
	t0 := time.Now()
	for i := 0; i < singleN; i++ {
		if _, err := pFull.Commit(context.Background(), map[uint32][]byte{
			3: MakeDataPage(2, []byte(fmt.Sprintf("val-%d", i))),
		}); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	singleDur := time.Since(t0)
	singleSyncs := pFull.WALSyncCount.Load()
	_ = pFull.Close()

	// 2. Concurrent Group Commit (16 goroutines × 25 commits = 400 commits, SyncFull)
	pGC, err := OpenPager(OSFS{}, filepath.Join(dir, "gc.db"), 256, SyncFull)
	if err != nil {
		t.Fatalf("open gc: %v", err)
	}
	const workers, perWorker = 16, 25
	var wg sync.WaitGroup
	t1 := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				pid := uint32(3 + id)
				if _, err := pGC.Commit(context.Background(), map[uint32][]byte{
					pid: MakeDataPage(2, []byte(fmt.Sprintf("w%d-%d", id, j))),
				}); err != nil {
					t.Errorf("worker %d commit %d: %v", id, j, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	gcDur := time.Since(t1)
	gcSyncs := pGC.WALSyncCount.Load()
	_ = pGC.Close()

	// 3. SyncNormal throughput (1,000 commits) + Cold WAL Recovery (1,000 & 10,000 frames)
	benchRecovery := func(frames int) (writeDur, recoverDur, ckptDur time.Duration) {
		dbp := filepath.Join(dir, fmt.Sprintf("rec_%d.db", frames))
		p, err := OpenPager(OSFS{}, dbp, 512, SyncNormal)
		if err != nil {
			t.Fatalf("open rec: %v", err)
		}
		tw := time.Now()
		for i := 0; i < frames; i++ {
			pid := uint32(3 + (i % 500))
			if _, err := p.Commit(context.Background(), map[uint32][]byte{
				pid: MakeDataPage(2, []byte(fmt.Sprintf("f-%d", i))),
			}); err != nil {
				t.Fatalf("commit %d: %v", i, err)
			}
		}
		_ = p.walFile.Sync()
		writeDur = time.Since(tw)
		// Simulate abrupt crash (close raw handles WITHOUT clean CheckpointTruncate)
		_ = p.walFile.Close()
		_ = p.dbFile.Close()

		tr := time.Now()
		rp, err := OpenPager(OSFS{}, dbp, 512, SyncFull)
		if err != nil {
			t.Fatalf("recover %d frames: %v", frames, err)
		}
		recoverDur = time.Since(tr)
		if rp.LatestCommitTS() != uint64(frames) {
			t.Fatalf("recovered ts=%d, want %d", rp.LatestCommitTS(), frames)
		}

		tc := time.Now()
		if _, _, err := rp.Checkpoint(CheckpointTruncate); err != nil {
			t.Fatalf("checkpoint truncate: %v", err)
		}
		ckptDur = time.Since(tc)
		_ = rp.Close()
		return
	}

	w1k, r1k, c1k := benchRecovery(1000)
	w10k, r10k, c10k := benchRecovery(10000)

	t.Logf("\n=== Real WSL2 ext4 Durability & Recovery Profile ===\n"+
		"Single-goroutine SyncFull (%d txns): %v total | %.2f ms/tx | %.0f tx/s | %d fsyncs\n"+
		"Group Commit 16g SyncFull (%d txns): %v total | %.2f ms/tx | %.0f tx/s | %d fsyncs (%.1fx coalescing)\n"+
		"SyncNormal 1,000 frames: write=%v (%.0f tx/s) | cold-recover=%v | CheckpointTruncate=%v\n"+
		"SyncNormal 10,000 frames (40MB WAL): write=%v (%.0f tx/s) | cold-recover=%v | CheckpointTruncate=%v",
		singleN, singleDur, float64(singleDur.Microseconds())/1000/singleN, float64(singleN)/singleDur.Seconds(), singleSyncs,
		workers*perWorker, gcDur, float64(gcDur.Microseconds())/1000/float64(workers*perWorker), float64(workers*perWorker)/gcDur.Seconds(), gcSyncs, float64(workers*perWorker)/float64(gcSyncs),
		w1k, 1000/w1k.Seconds(), r1k, c1k,
		w10k, 10000/w10k.Seconds(), r10k, c10k,
	)
}
