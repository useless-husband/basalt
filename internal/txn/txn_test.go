package txn

import (
	"context"
	"testing"
	"time"

	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/storage"
	"github.com/useless-husband/basalt/internal/vfs"
)

func TestLockCompatibility(t *testing.T) {
	lm := NewLockManager()
	ctx := context.Background()
	tag := Tag{Kind: TagRelation, ID: 1}
	if err := lm.Acquire(ctx, 1, tag, AccessShare); err != nil {
		t.Fatal(err)
	}
	if err := lm.Acquire(ctx, 2, tag, RowExclusive); err != nil {
		t.Fatal("AccessShare and RowExclusive are compatible")
	}
	got := make(chan error, 1)
	go func() { got <- lm.Acquire(ctx, 3, tag, AccessExclusive) }()
	select {
	case <-got:
		t.Fatal("AccessExclusive must wait for the other holders")
	case <-time.After(30 * time.Millisecond):
	}
	lm.ReleaseAll(1)
	lm.ReleaseAll(2)
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	// A queued exclusive request blocks later shared requests (FIFO).
	got2 := make(chan error, 1)
	go func() { got2 <- lm.Acquire(ctx, 4, tag, AccessShare) }()
	select {
	case <-got2:
		t.Fatal("shared request must wait behind the exclusive holder")
	case <-time.After(30 * time.Millisecond):
	}
	lm.ReleaseAll(3)
	if err := <-got2; err != nil {
		t.Fatal(err)
	}
}

func TestDeadlockDetection(t *testing.T) {
	lm := NewLockManager()
	ctx := context.Background()
	a, b := Tag{Kind: TagKey, ID: 1, Key: "a"}, Tag{Kind: TagKey, ID: 1, Key: "b"}
	lm.Acquire(ctx, 1, a, Exclusive)
	lm.Acquire(ctx, 2, b, Exclusive)
	first := make(chan error, 1)
	go func() { first <- lm.Acquire(ctx, 1, b, Exclusive) }() // 1 waits for 2
	time.Sleep(30 * time.Millisecond)
	err := lm.Acquire(ctx, 2, a, Exclusive) // closes the cycle
	if pgerr.Code(err) != pgerr.DeadlockDetected {
		t.Fatalf("want 40P01, got %v", err)
	}
	lm.ReleaseAll(2)
	if err := <-first; err != nil {
		t.Fatalf("the survivor should get the lock: %v", err)
	}
	// A three-party cycle is found as well.
	lm2 := NewLockManager()
	x, y, z := Tag{Kind: TagKey, Key: "x"}, Tag{Kind: TagKey, Key: "y"}, Tag{Kind: TagKey, Key: "z"}
	lm2.Acquire(ctx, 1, x, Exclusive)
	lm2.Acquire(ctx, 2, y, Exclusive)
	lm2.Acquire(ctx, 3, z, Exclusive)
	go lm2.Acquire(ctx, 1, y, Exclusive)
	go lm2.Acquire(ctx, 2, z, Exclusive)
	time.Sleep(30 * time.Millisecond)
	if err := lm2.Acquire(ctx, 3, x, Exclusive); pgerr.Code(err) != pgerr.DeadlockDetected {
		t.Fatalf("three-way deadlock not detected: %v", err)
	}
	lm2.ReleaseAll(3)
	lm2.ReleaseAll(2)
	lm2.ReleaseAll(1)
}

func TestLockWaitCanceled(t *testing.T) {
	lm := NewLockManager()
	tag := Tag{Kind: TagRelation, ID: 9}
	lm.Acquire(context.Background(), 1, tag, AccessExclusive)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := lm.Acquire(ctx, 2, tag, AccessShare); pgerr.Code(err) != pgerr.QueryCanceled {
		t.Fatalf("want 57014, got %v", err)
	}
	if len(lm.Snapshot()) != 1 {
		t.Fatalf("the canceled request must leave the queue: %+v", lm.Snapshot())
	}
}

func newManager(t *testing.T) *Manager {
	t.Helper()
	st, err := storage.Open(storage.Options{FS: vfs.NewFaultFS(), Dir: "/db", PoolPages: 64, CheckpointInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m, err := NewManager(st)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestVisibilityRules exercises the snapshot rules directly.
func TestVisibilityRules(t *testing.T) {
	m := newManager(t)
	committed := m.Begin()
	cx, _ := committed.AssignXid()
	committed.Commit()
	aborted := m.Begin()
	ax, _ := aborted.AssignXid()
	aborted.Abort()
	running := m.Begin()
	rx, _ := running.AssignXid()

	me := m.Begin()
	me.Snapshot()
	later := m.Begin()
	lx, _ := later.AssignXid()
	later.Commit() // committed after me's snapshot
	myx, _ := me.AssignXid()

	H := func(xmin, xmax uint64) storage.TupleHeader { return storage.TupleHeader{Xmin: xmin, Xmax: xmax} }
	cases := []struct {
		name string
		h    storage.TupleHeader
		want bool
	}{
		{"inserted by committed", H(cx, 0), true},
		{"inserted by aborted", H(ax, 0), false},
		{"inserted by running", H(rx, 0), false},
		{"inserted after snapshot", H(lx, 0), false},
		{"deleted by committed", H(cx, cx), false},
		{"deleted by aborted", H(cx, ax), true},
		{"deleted by running", H(cx, rx), true},
		{"deleted after snapshot", H(cx, lx), true},
		{"inserted by me in the current command", storage.TupleHeader{Xmin: myx, Cmin: 0}, false},
	}
	for _, c := range cases {
		if got := me.Visible(c.h); got != c.want {
			t.Errorf("%s: visible=%v, want %v", c.name, got, c.want)
		}
	}
	cases = cases[:0]
	me.CommandCounterIncrement()
	cases = append(cases,
		struct {
			name string
			h    storage.TupleHeader
			want bool
		}{"inserted by me, earlier command (after CCI)", storage.TupleHeader{Xmin: myx, Cmin: 0}, true},
		struct {
			name string
			h    storage.TupleHeader
			want bool
		}{"deleted by me in this command", storage.TupleHeader{Xmin: cx, Xmax: myx, Cmax: 1}, true},
		struct {
			name string
			h    storage.TupleHeader
			want bool
		}{"deleted by me earlier", storage.TupleHeader{Xmin: cx, Xmax: myx, Cmax: 0}, false},
		struct {
			name string
			h    storage.TupleHeader
			want bool
		}{"killed", storage.TupleHeader{Xmin: cx, Flags: storage.FlagKilled}, false},
	)
	for _, c := range cases {
		if got := me.Visible(c.h); got != c.want {
			t.Errorf("%s: visible=%v, want %v", c.name, got, c.want)
		}
	}
	running.Abort()
	me.Abort()
}

func TestOldestXminTracksSnapshots(t *testing.T) {
	m := newManager(t)
	old := m.Begin()
	ox, _ := old.AssignXid()
	reader := m.Begin()
	reader.Snapshot()
	old.Commit()
	if got := m.OldestXmin(); got > ox {
		t.Fatalf("a snapshot taken while %d ran must hold the horizon at or below it, got %d", ox, got)
	}
	reader.Commit()
	if got := m.OldestXmin(); got <= ox {
		t.Fatalf("with no snapshots the horizon advances past %d, got %d", ox, got)
	}
}
