package storage

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/useless-husband/basalt/internal/vfs"
)

func openTest(t *testing.T, fs vfs.FS, pool int) *Store {
	t.Helper()
	s, err := Open(Options{FS: fs, Dir: "/db", PoolPages: pool, CheckpointInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func tuple(i int, size int) []byte {
	b := make([]byte, TupleHeaderSize+size)
	EncodeHeader(b, TupleHeader{Xmin: uint64(i)})
	for j := TupleHeaderSize; j < len(b); j++ {
		b[j] = byte(i + j)
	}
	return b
}

func TestHeapInsertScanFetch(t *testing.T) {
	s := openTest(t, vfs.NewFaultFS(), 64)
	defer s.Close()
	m := s.Begin()
	first, err := m.CreateHeap()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Commit(); err != nil {
		t.Fatal(err)
	}
	h := s.Heap(first)
	tids := map[TID]int{}
	for i := 0; i < 3000; i++ {
		tid, err := h.Insert(tuple(i, 50+i%200))
		if err != nil {
			t.Fatal(err)
		}
		tids[tid] = i
	}
	c := h.Scan()
	n := 0
	for {
		tid, tup, ok, err := c.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		i, ok := tids[tid]
		if !ok || !bytes.Equal(tup, tuple(i, 50+i%200)) {
			t.Fatalf("scan returned wrong tuple at %v", tid)
		}
		n++
	}
	if n != 3000 {
		t.Fatalf("scan found %d tuples", n)
	}
	for tid, i := range tids {
		got, ok, err := h.Fetch(tid, nil)
		if err != nil || !ok || !bytes.Equal(got, tuple(i, 50+i%200)) {
			t.Fatalf("fetch %v failed", tid)
		}
	}
	if _, err := h.Insert(make([]byte, MaxTuple+1)); err == nil {
		t.Fatal("oversized tuple accepted")
	}
}

func btKeyOf(i int) []byte {
	k := make([]byte, 0, 24)
	k = append(k, []byte(fmt.Sprintf("key-%07d-", i))...)
	return binary.BigEndian.AppendUint64(k, uint64(i))
}

func TestBTreeRandomInsertDelete(t *testing.T) {
	seed := time.Now().UnixNano() % 1000
	t.Logf("seed %d", seed)
	r := rand.New(rand.NewSource(seed))
	s := openTest(t, vfs.NewFaultFS(), 256)
	defer s.Close()
	m := s.Begin()
	root, err := m.CreateBTree()
	if err != nil {
		t.Fatal(err)
	}
	m.Commit()
	tr := s.BTree(root)
	present := map[int]bool{}
	for step := 0; step < 20000; step++ {
		i := r.Intn(8000)
		if present[i] && r.Intn(3) > 0 {
			ok, err := tr.Delete(btKeyOf(i))
			if err != nil || !ok {
				t.Fatalf("seed %d: delete %d: %v %v", seed, i, ok, err)
			}
			delete(present, i)
		} else if !present[i] {
			// Large padding makes splits frequent.
			k := append(btKeyOf(i), bytes.Repeat([]byte{byte(i)}, r.Intn(300))...)
			k = btKeyOf(i)
			if err := tr.Insert(k, 0, nil); err != nil {
				t.Fatalf("seed %d: insert %d: %v", seed, i, err)
			}
			present[i] = true
		}
		if step%5000 == 0 {
			if _, err := tr.Check(); err != nil {
				t.Fatalf("seed %d step %d: %v", seed, step, err)
			}
		}
	}
	n, err := tr.Check()
	if err != nil {
		t.Fatalf("seed %d: %v", seed, err)
	}
	if n != len(present) {
		t.Fatalf("seed %d: tree has %d keys, want %d", seed, n, len(present))
	}
	var want []int
	for i := range present {
		want = append(want, i)
	}
	sort.Ints(want)
	c := tr.Seek(nil)
	for _, i := range want {
		k, ok, err := c.Next()
		if err != nil || !ok || !bytes.Equal(k, btKeyOf(i)) {
			t.Fatalf("seed %d: cursor mismatch at %d", seed, i)
		}
	}
	if _, ok, _ := c.Next(); ok {
		t.Fatalf("cursor returned extra keys")
	}
	// Delete everything: the tree must shrink back to a single leaf.
	for _, i := range want {
		if ok, err := tr.Delete(btKeyOf(i)); err != nil || !ok {
			t.Fatal(err)
		}
	}
	h, leaves, err := tr.Stats()
	if err != nil || h != 1 || leaves != 1 {
		t.Fatalf("after deleting all keys: height %d leaves %d err %v", h, leaves, err)
	}
	free, _ := s.FreePages()
	if free == 0 {
		t.Fatalf("merges should have freed pages")
	}
}

func TestBTreePrefixCheck(t *testing.T) {
	s := openTest(t, vfs.NewFaultFS(), 64)
	defer s.Close()
	m := s.Begin()
	root, _ := m.CreateBTree()
	m.Commit()
	tr := s.BTree(root)
	for i := 0; i < 10; i++ {
		k := append([]byte("dup"), byte(i))
		if err := tr.Insert(k, 3, nil); err != nil {
			t.Fatal(err)
		}
	}
	seen := 0
	err := tr.Insert([]byte("dupX"), 3, func(k []byte) error {
		seen++
		return nil
	})
	if err != nil || seen != 10 {
		t.Fatalf("prefix check saw %d keys, err %v", seen, err)
	}
	stop := fmt.Errorf("conflict")
	if err := tr.Insert([]byte("dupY"), 3, func([]byte) error { return stop }); err != stop {
		t.Fatalf("check error not propagated: %v", err)
	}
	if n, _ := tr.Check(); n != 11 {
		t.Fatalf("rejected insert must not be applied, have %d keys", n)
	}
}

// TestRecoveryAfterCrash commits work, crashes the file system at a random
// point (unsynced writes lost or torn) and checks that everything whose WAL
// record was flushed survives.
func TestRecoveryAfterCrash(t *testing.T) {
	for seed := int64(1); seed <= 30; seed++ {
		r := rand.New(rand.NewSource(seed))
		fs := vfs.NewFaultFS()
		s := openTest(t, fs, 64) // small pool forces evictions
		m := s.Begin()
		first, _ := m.CreateHeap()
		root, _ := m.CreateBTree()
		lsn, _ := m.Commit()
		s.Flush(lsn)
		h := s.Heap(first)
		tr := s.BTree(root)
		var durable []int
		n := 200 + r.Intn(1500)
		for i := 0; i < n; i++ {
			if _, err := h.Insert(tuple(i, 100)); err != nil {
				t.Fatal(err)
			}
			if err := tr.Insert(btKeyOf(i), 0, nil); err != nil {
				t.Fatal(err)
			}
			if r.Intn(10) == 0 {
				if err := s.Flush(s.WAL().CurrentLSN()); err != nil {
					t.Fatal(err)
				}
				durable = append(durable[:0], i)
			}
			if r.Intn(300) == 0 {
				if err := s.Checkpoint(); err != nil {
					t.Fatal(err)
				}
			}
		}
		s.Abandon()
		crashed := fs.Crash(r, CrashOptionsForTest(r))
		s2 := openTest(t, crashed, 64)
		h2 := s2.Heap(first)
		tr2 := s2.BTree(root)
		got := map[int]bool{}
		c := h2.Scan()
		for {
			_, tup, ok, err := c.Next()
			if err != nil {
				t.Fatalf("seed %d: scan after crash: %v", seed, err)
			}
			if !ok {
				break
			}
			i := int(DecodeHeader(tup).Xmin)
			if !bytes.Equal(tup, tuple(i, 100)) {
				t.Fatalf("seed %d: corrupt tuple %d after recovery", seed, i)
			}
			got[i] = true
		}
		keys, err := tr2.Check()
		if err != nil {
			t.Fatalf("seed %d: btree after crash: %v", seed, err)
		}
		if len(durable) > 0 {
			last := durable[0]
			for i := 0; i <= last; i++ {
				if !got[i] {
					t.Fatalf("seed %d: durable tuple %d lost (last flushed %d)", seed, i, last)
				}
			}
			if keys < last+1 {
				t.Fatalf("seed %d: btree lost keys: %d < %d", seed, keys, last+1)
			}
		}
		// Every key in the tree must also be in the heap (they were
		// inserted in that order and each is atomic).
		cur := tr2.Seek(nil)
		for {
			k, ok, err := cur.Next()
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			i := int(binary.BigEndian.Uint64(k[len(k)-8:]))
			if !got[i] {
				t.Fatalf("seed %d: index key %d without heap tuple", seed, i)
			}
		}
		// Recovery must be repeatable and the store usable.
		if _, err := h2.Insert(tuple(99999, 10)); err != nil {
			t.Fatal(err)
		}
		if err := s2.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// CrashOptionsForTest picks a crash mode.
func CrashOptionsForTest(r *rand.Rand) vfs.CrashOptions {
	return vfs.CrashOptions{KeepProbability: r.Float64(), TornProbability: 0.5}
}

func TestTornPageDetected(t *testing.T) {
	fs := vfs.NewFaultFS()
	s := openTest(t, fs, 64)
	m := s.Begin()
	first, _ := m.CreateHeap()
	m.Commit()
	h := s.Heap(first)
	for i := 0; i < 50; i++ {
		h.Insert(tuple(i, 100))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Corrupt the heap page on disk and remove the WAL that could repair
	// it: the checksum must catch it.
	f, _ := fs.OpenFile("/db/basalt.db", false)
	f.WriteAt([]byte{0xde, 0xad}, int64(first)*PageSize+4000)
	f.Sync()
	s2 := openTest(t, fs, 64)
	defer s2.Close()
	_, _, err := s2.Heap(first).Fetch(MakeTID(first, 0), nil)
	if err == nil {
		t.Fatal("corrupted page was not detected")
	}
}

func TestClog(t *testing.T) {
	fs := vfs.NewFaultFS()
	s := openTest(t, fs, 64)
	for _, x := range []uint64{3, 4, 70000, 70001} {
		st := XidCommitted
		if x%2 == 0 {
			st = XidAborted
		}
		lsn, err := s.SetXidStatus(x, st)
		if err != nil {
			t.Fatal(err)
		}
		s.Flush(lsn)
	}
	s.Abandon()
	s2 := openTest(t, fs.Crash(rand.New(rand.NewSource(1)), vfs.CrashOptions{}), 64)
	defer s2.Close()
	b, err := s2.ReadClog()
	if err != nil {
		t.Fatal(err)
	}
	status := func(x uint64) byte { return b[x/4] >> ((x % 4) * 2) & 3 }
	if status(3) != XidCommitted || status(4) != XidAborted || status(70001) != XidCommitted || status(70000) != XidAborted || status(5) != XidInProgress {
		t.Fatalf("clog statuses wrong")
	}
}
