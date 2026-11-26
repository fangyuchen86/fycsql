package btreeproto

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"testing"
	"time"
)

func TestMemcomparableKeyOrdering(t *testing.T) {
	// 1. Integer ordering across full int64 spectrum including NULL.
	ints := []Value{
		NullValue(),
		IntValue(math.MinInt64),
		IntValue(-1_000_000),
		IntValue(-1),
		IntValue(0),
		IntValue(1),
		IntValue(1_000_000),
		IntValue(math.MaxInt64),
	}
	checkStrictOrder(t, "INT ASC", ints, 0, []ColType{TypeInt})
	checkReverseOrder(t, "INT DESC", ints, 1, []ColType{TypeInt})

	// 2. Float ordering including -0.0 == +0.0 normalization and subnormals.
	reals := []Value{
		NullValue(),
		RealValue(-math.MaxFloat64),
		RealValue(-1.5),
		RealValue(-math.SmallestNonzeroFloat64),
		RealValue(0.0),
		RealValue(math.SmallestNonzeroFloat64),
		RealValue(1.5),
		RealValue(math.MaxFloat64),
	}
	checkStrictOrder(t, "REAL ASC", reals, 0, []ColType{TypeReal})
	checkReverseOrder(t, "REAL DESC", reals, 1, []ColType{TypeReal})

	// Verify -0.0 and +0.0 produce identical keys.
	negZero, err := AppendMemKey(nil, []Value{RealValue(math.Copysign(0, -1))}, 0)
	if err != nil {
		t.Fatal(err)
	}
	posZero, err := AppendMemKey(nil, []Value{RealValue(0.0)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(negZero, posZero) {
		t.Fatalf("-0.0 and +0.0 keys differ: %x vs %x", negZero, posZero)
	}
	if _, err := AppendMemKey(nil, []Value{RealValue(math.NaN())}, 0); err != ErrInvalidFloat {
		t.Fatalf("expected ErrInvalidFloat for NaN, got %v", err)
	}

	// 3. TEXT / BLOB ordering including embedded 0x00 bytes.
	blobs := []Value{
		NullValue(),
		BlobValue([]byte{}),
		BlobValue([]byte{0x00}),
		BlobValue([]byte{0x00, 0x00}),
		BlobValue([]byte{0x00, 0x01}),
		BlobValue([]byte("a")),
		BlobValue([]byte("a\x00")),
		BlobValue([]byte("a\x00\x00")),
		BlobValue([]byte("a\x00b")),
		BlobValue([]byte("ab")),
		BlobValue([]byte{0xFF}),
	}
	checkStrictOrder(t, "BLOB ASC", blobs, 0, []ColType{TypeBlob})
	checkReverseOrder(t, "BLOB DESC", blobs, 1, []ColType{TypeBlob})

	// 4. Composite key round-trip (INT ASC, TEXT DESC, BOOL ASC) + zero-alloc decode.
	schema := []ColType{TypeInt, TypeText, TypeBool}
	var descMask uint64 = 1 << 1 // col 1 DESC
	k1, err := AppendMemKey(nil, []Value{IntValue(10), TextValue([]byte("b")), BoolValue(false)}, descMask)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := AppendMemKey(nil, []Value{IntValue(10), TextValue([]byte("a")), BoolValue(true)}, descMask)
	if err != nil {
		t.Fatal(err)
	}
	k3, err := AppendMemKey(nil, []Value{IntValue(10), NullValue(), BoolValue(false)}, descMask)
	if err != nil {
		t.Fatal(err)
	}
	// On col 1 DESC: "b" < "a" < NULL (NULLS LAST on DESC!).
	if !(bytes.Compare(k1, k2) < 0 && bytes.Compare(k2, k3) < 0) {
		t.Fatalf("composite DESC+NULLS LAST order broken")
	}
	var dst []Value
	var scratch []byte
	dst, scratch, err = DecodeMemKeyInto(k1, schema, descMask, dst, scratch)
	if err != nil || dst[0].AsInt() != 10 || string(dst[1].Raw) != "b" || dst[2].AsBool() != false {
		t.Fatalf("composite decode failed: err=%v dst=%+v", err, dst)
	}
}

func checkStrictOrder(t *testing.T, name string, vals []Value, descMask uint64, schema []ColType) {
	t.Helper()
	var prev []byte
	var dst []Value
	var scratch []byte
	for i, v := range vals {
		k, err := AppendMemKey(nil, []Value{v}, descMask)
		if err != nil {
			t.Fatalf("%s[%d]: %v", name, i, err)
		}
		if prev != nil && bytes.Compare(prev, k) >= 0 {
			t.Fatalf("%s: key[%d]=%x not > key[%d]=%x", name, i, k, i-1, prev)
		}
		dst, scratch, err = DecodeMemKeyInto(k, schema, descMask, dst, scratch)
		if err != nil || len(dst) != 1 || dst[0].Type != v.Type {
			t.Fatalf("%s[%d] round-trip mismatch: err=%v got=%+v want=%+v", name, i, err, dst, v)
		}
		prev = k
	}
}

func checkReverseOrder(t *testing.T, name string, vals []Value, descMask uint64, schema []ColType) {
	t.Helper()
	rev := make([]Value, len(vals))
	for i := range vals {
		rev[i] = vals[len(vals)-1-i]
	}
	checkStrictOrder(t, name, rev, descMask, schema)
}

func TestRecordCodecAndProjection(t *testing.T) {
	schema := []ColType{TypeInt, TypeText, TypeReal, TypeBool, TypeBlob, TypeText}
	row := []Value{
		IntValue(42),
		TextValue([]byte("hello world")),
		NullValue(),
		BoolValue(true),
		BlobValue([]byte{0xDE, 0xAD, 0xBE, 0xEF}),
		TextValue([]byte("projected-col-5")),
	}
	raw := AppendRecord(nil, row)
	got, err := DecodeRecordInto(raw, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(row) || got[0].AsInt() != 42 || string(got[1].Raw) != "hello world" ||
		got[2].Type != TypeNull || !got[3].AsBool() || !bytes.Equal(got[4].Raw, row[4].Raw) ||
		string(got[5].Raw) != "projected-col-5" {
		t.Fatalf("record decode mismatch: %+v", got)
	}

	col5, err := DecodeColumn(raw, 5, TypeText)
	if err != nil || string(col5.Raw) != "projected-col-5" {
		t.Fatalf("projected col 5 mismatch: %v %+v", err, col5)
	}
	col2, err := DecodeColumn(raw, 2, TypeReal)
	if err != nil || col2.Type != TypeNull {
		t.Fatalf("projected NULL col 2 mismatch: %v %+v", err, col2)
	}
}

func TestBTreeMonotonicRandomOverflowAndRebalance(t *testing.T) {
	store := NewSlabStore(4096)
	bt, err := NewBTree(store)
	if err != nil {
		t.Fatal(err)
	}
	ref := make(map[string][]byte)
	var keyBuf [16]byte
	var valBuf []byte

	// 1. Monotonic inserts (triggers 90/10 splits).
	for i := int64(0); i < 5000; i++ {
		k, _ := AppendMemKey(keyBuf[:0], []Value{IntValue(i)}, 0)
		v := fmt.Appendf(nil, "row-%06d-payload-padding-0123456789", i)
		if err := bt.Put(k, v); err != nil {
			t.Fatalf("monotonic put %d: %v", i, err)
		}
		ref[string(k)] = v
	}
	if err := bt.Verify(); err != nil {
		t.Fatalf("verify after monotonic: %v", err)
	}
	if bt.splits90 == 0 {
		t.Fatalf("expected 90/10 splits on monotonic load, got 0")
	}
	// Measure monotonic 90/10 fill factor across all leaves.
	monoLeaves, monoUsed := 0, 0
	for pid := uint32(2); pid <= store.totalPages; pid++ {
		p := store.Page(pid)
		if pageType(p) == PageTypeLeaf {
			monoLeaves++
			monoUsed += pageUsedBytes(p)
		}
	}
	monoFill := 100.0 * float64(monoUsed) / float64(monoLeaves*PageUsableBytes)
	t.Logf("Monotonic 5000 rows: height=%d leaves=%d avgFill=%.1f%% splits90=%d", bt.height, monoLeaves, monoFill, bt.splits90)

	// 2. Large keys (900B) + large overflow values (10KB) verifying >= 4 cells/leaf invariant.
	for i := 0; i < 30; i++ {
		bigKey := make([]byte, MaxKeySize)
		copy(bigKey, []byte("bigkey-"))
		binary.BigEndian.PutUint64(bigKey[len(bigKey)-8:], uint64(i))
		bigVal := bytes.Repeat([]byte{byte('A' + i%26)}, 10_000+i*137)
		if err := bt.Put(bigKey, bigVal); err != nil {
			t.Fatalf("big put %d: %v", i, err)
		}
		ref[string(bigKey)] = bigVal
	}
	if err := bt.Verify(); err != nil {
		t.Fatalf("verify after overflow inserts: %v", err)
	}

	// 3. Random updates, inserts, and deletes exercising 50/50 splits, lazy compaction, overflow replacement, and 25% merge/borrow.
	rng := rand.New(rand.NewPCG(42, 99))
	for step := 0; step < 6000; step++ {
		id := rng.Int64N(8000)
		k, _ := AppendMemKey(keyBuf[:0], []Value{IntValue(id)}, 0)
		op := rng.IntN(10)
		if op < 5 {
			sz := 32 + rng.IntN(200)
			if op == 0 && rng.IntN(20) == 0 {
				sz = 5000 + rng.IntN(4000) // occasional overflow update
			}
			v := make([]byte, sz)
			binary.LittleEndian.PutUint64(v, uint64(step))
			if err := bt.Put(k, v); err != nil {
				t.Fatalf("step %d put: %v", step, err)
			}
			ref[string(k)] = v
		} else {
			gotDel := bt.Delete(k)
			_, wantDel := ref[string(k)]
			if gotDel != wantDel {
				t.Fatalf("step %d delete(%d) got=%v want=%v", step, id, gotDel, wantDel)
			}
			delete(ref, string(k))
		}
		if step%1000 == 999 {
			if err := bt.Verify(); err != nil {
				t.Fatalf("verify at step %d: %v", step, err)
			}
		}
	}

	// 4. Verify every remaining key + forward vs reverse scan equivalence.
	for ks, want := range ref {
		got, ok := bt.Get([]byte(ks), valBuf)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("get mismatch for key %x: ok=%v len=%d wantLen=%d", ks, ok, len(got), len(want))
		}
	}
	var fwdKeys [][]byte
	bt.ScanForward(valBuf, func(k, v []byte) bool {
		kc := append([]byte(nil), k...)
		fwdKeys = append(fwdKeys, kc)
		if !bytes.Equal(v, ref[string(k)]) {
			t.Fatalf("forward scan value mismatch")
		}
		return true
	})
	var revKeys [][]byte
	bt.ScanReverse(valBuf, func(k, v []byte) bool {
		kc := append([]byte(nil), k...)
		revKeys = append(revKeys, kc)
		if !bytes.Equal(v, ref[string(k)]) {
			t.Fatalf("reverse scan value mismatch")
		}
		return true
	})
	if len(fwdKeys) != len(ref) || len(revKeys) != len(ref) {
		t.Fatalf("scan count mismatch: fwd=%d rev=%d ref=%d", len(fwdKeys), len(revKeys), len(ref))
	}
	for i := range fwdKeys {
		if !bytes.Equal(fwdKeys[i], revKeys[len(revKeys)-1-i]) {
			t.Fatalf("reverse scan mismatch at %d", i)
		}
	}

	// 5. Delete almost everything to force deep merges and root collapse.
	for ks := range ref {
		if !bt.Delete([]byte(ks)) {
			t.Fatalf("final delete failed")
		}
	}
	if err := bt.Verify(); err != nil {
		t.Fatalf("verify after draining tree: %v", err)
	}
	if bt.count != 0 || bt.height != 1 {
		t.Fatalf("expected empty height-1 tree, got count=%d height=%d", bt.count, bt.height)
	}
	t.Logf("Verified! totalPages=%d freelist=%d splits90=%d splits50=%d merges=%d borrows=%d",
		store.totalPages, len(store.freelist), bt.splits90, bt.splits50, bt.merges, bt.borrows)
}

// TestGCPauseProfileBufferPool compares Go 1.27 GC pause & mark time across
// three 64MB Buffer Pool representations (16,384 x 4KB pages).
func TestGCPauseProfileBufferPool(t *testing.T) {
	const numPages = 16384 // 64MB

	measureGC := func(label string, hold any) {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		for i := 0; i < 10; i++ {
			runtime.GC()
		}
		elapsed := time.Since(start)
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(hold)
		avgTotal := elapsed / 10
		avgPause := time.Duration(after.PauseTotalNs-before.PauseTotalNs) / 10
		t.Logf("%-30s | 10-GC avg wall: %8v | avg STW pause: %8v | HeapAlloc: %5.1f MB",
			label, avgTotal, avgPause, float64(after.HeapAlloc)/(1024*1024))
	}

	// Variant A: Contiguous noscan []byte slab (ADR-0003 recommended)
	slab := make([]byte, numPages*PageSize)
	for i := 0; i < len(slab); i += PageSize {
		slab[i] = 1
	}
	measureGC("Contiguous noscan []byte", slab)
	slab = nil

	// Variant B: Slice of pointers to 4KB arrays []*[4096]byte
	ptrPages := make([]*[PageSize]byte, numPages)
	for i := range ptrPages {
		ptrPages[i] = new([PageSize]byte)
		ptrPages[i][0] = 1
	}
	measureGC("[]*[4096]byte (16k ptrs)", ptrPages)
	ptrPages = nil

	// Variant C: Slice of independent []byte slices ([][]byte)
	slicePages := make([][]byte, numPages)
	for i := range slicePages {
		slicePages[i] = make([]byte, PageSize)
		slicePages[i][0] = 1
	}
	measureGC("[][]byte (16k slices)", slicePages)
	slicePages = nil

	// Variant D: Pointer-rich Go structs (e.g., decoded in-memory B+Tree nodes with []*Node)
	type naiveCell struct {
		key []byte
		val []byte
	}
	naivePool := make([][]naiveCell, numPages)
	for i := range naivePool {
		cells := make([]naiveCell, 32)
		for j := range cells {
			cells[j] = naiveCell{key: make([]byte, 16), val: make([]byte, 64)}
		}
		naivePool[i] = cells
	}
	measureGC("Naive decoded [][]cell (~1M ptrs)", naivePool)
	naivePool = nil
}

// --- Benchmarks (-benchmem) ---

func BenchmarkMemKeyEncodeDecode(b *testing.B) {
	schema := []ColType{TypeInt, TypeText, TypeReal, TypeBool}
	vals := []Value{IntValue(123456789), TextValue([]byte("users/alice@example.com")), RealValue(99.95), BoolValue(true)}
	var descMask uint64 = 1 << 1
	buf := make([]byte, 0, 128)
	dst := make([]Value, 0, 4)
	scratch := make([]byte, 0, 128)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		buf, err = AppendMemKey(buf[:0], vals, descMask)
		if err != nil {
			b.Fatal(err)
		}
		dst, scratch, err = DecodeMemKeyInto(buf, schema, descMask, dst, scratch)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRecordEncodeDecodeAll(b *testing.B) {
	schema := []ColType{TypeInt, TypeText, TypeText, TypeReal, TypeBool, TypeInt, TypeBlob, TypeText}
	vals := []Value{
		IntValue(1001), TextValue([]byte("alice")), TextValue([]byte("alice@example.com")),
		RealValue(1234.56), BoolValue(true), IntValue(1700000000),
		BlobValue([]byte("0123456789abcdef")), TextValue([]byte("active-member")),
	}
	buf := make([]byte, 0, 256)
	dst := make([]Value, 0, 8)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = AppendRecord(buf[:0], vals)
		var err error
		dst, err = DecodeRecordInto(buf, schema, dst)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRecordProjectSingleCol(b *testing.B) {
	vals := []Value{
		IntValue(1001), TextValue([]byte("alice")), TextValue([]byte("alice@example.com")),
		RealValue(1234.56), BoolValue(true), IntValue(1700000000),
		BlobValue([]byte("0123456789abcdef")), TextValue([]byte("active-member")),
	}
	raw := AppendRecord(nil, vals)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v, err := DecodeColumn(raw, 7, TypeText)
		if err != nil || len(v.Raw) == 0 {
			b.Fatal(err)
		}
	}
}

func BenchmarkBTreePointGet100k(b *testing.B) {
	store := NewSlabStore(8192)
	bt, err := NewBTree(store)
	if err != nil {
		b.Fatal(err)
	}
	var keyBuf [16]byte
	val := bytes.Repeat([]byte("x"), 96)
	const N = 100_000
	for i := int64(0); i < N; i++ {
		k, _ := AppendMemKey(keyBuf[:0], []Value{IntValue(i)}, 0)
		if err := bt.Put(k, val); err != nil {
			b.Fatal(err)
		}
	}
	rng := rand.New(rand.NewPCG(1, 2))
	dst := make([]byte, 0, 128)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k, _ := AppendMemKey(keyBuf[:0], []Value{IntValue(rng.Int64N(N))}, 0)
		var ok bool
		dst, ok = bt.Get(k, dst)
		if !ok {
			b.Fatal("missing key")
		}
	}
}

func BenchmarkBTreeMonotonicPut(b *testing.B) {
	store := NewSlabStore(16384)
	bt, err := NewBTree(store)
	if err != nil {
		b.Fatal(err)
	}
	var keyBuf [16]byte
	val := bytes.Repeat([]byte("v"), 96)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i > 0 && i%100_000 == 0 {
			b.StopTimer()
			store = NewSlabStore(16384)
			bt, _ = NewBTree(store)
			b.StartTimer()
		}
		k, _ := AppendMemKey(keyBuf[:0], []Value{IntValue(int64(i))}, 0)
		if err := bt.Put(k, val); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBTreeForwardScan100k(b *testing.B) {
	store := NewSlabStore(8192)
	bt, err := NewBTree(store)
	if err != nil {
		b.Fatal(err)
	}
	var keyBuf [16]byte
	val := bytes.Repeat([]byte("s"), 96)
	for i := int64(0); i < 100_000; i++ {
		k, _ := AppendMemKey(keyBuf[:0], []Value{IntValue(i)}, 0)
		if err := bt.Put(k, val); err != nil {
			b.Fatal(err)
		}
	}
	valBuf := make([]byte, 0, 128)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cnt := 0
		bt.ScanForward(valBuf, func(k, v []byte) bool {
			cnt++
			return true
		})
		if cnt != 100_000 {
			b.Fatalf("got %d", cnt)
		}
	}
}

func BenchmarkBTreeReverseScan100k(b *testing.B) {
	store := NewSlabStore(8192)
	bt, err := NewBTree(store)
	if err != nil {
		b.Fatal(err)
	}
	var keyBuf [16]byte
	val := bytes.Repeat([]byte("s"), 96)
	for i := int64(0); i < 100_000; i++ {
		k, _ := AppendMemKey(keyBuf[:0], []Value{IntValue(i)}, 0)
		if err := bt.Put(k, val); err != nil {
			b.Fatal(err)
		}
	}
	valBuf := make([]byte, 0, 128)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cnt := 0
		bt.ScanReverse(valBuf, func(k, v []byte) bool {
			cnt++
			return true
		})
		if cnt != 100_000 {
			b.Fatalf("got %d", cnt)
		}
	}
}

func BenchmarkBTreeRandomPut(b *testing.B) {
	store := NewSlabStore(16384)
	bt, err := NewBTree(store)
	if err != nil {
		b.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(7, 13))
	var keyBuf [16]byte
	val := bytes.Repeat([]byte("r"), 96)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i > 0 && i%100_000 == 0 {
			b.StopTimer()
			store = NewSlabStore(16384)
			bt, _ = NewBTree(store)
			b.StartTimer()
		}
		k, _ := AppendMemKey(keyBuf[:0], []Value{IntValue(rng.Int64())}, 0)
		if err := bt.Put(k, val); err != nil {
			b.Fatal(err)
		}
	}
}
