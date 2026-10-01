package vfs

import (
	"bytes"
	"math/rand"
	"path/filepath"
	"testing"
)

func readAll(t *testing.T, fs FS, name string) []byte {
	t.Helper()
	f, err := fs.OpenFile(name, false)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := f.Size()
	b := make([]byte, n)
	f.ReadAt(b, 0)
	return b
}

func TestFaultFSKeepsSyncedData(t *testing.T) {
	fs := NewFaultFS()
	f, _ := fs.OpenFile("/d/a", true)
	f.WriteAt([]byte("durable!"), 0)
	f.Sync()
	f.WriteAt([]byte("volatile"), 8)
	// Losing every unsynced write keeps exactly the synced prefix.
	lost := fs.Crash(rand.New(rand.NewSource(1)), CrashOptions{KeepProbability: 0})
	if got := readAll(t, lost, "/d/a"); string(got) != "durable!" {
		t.Fatalf("after crash: %q", got)
	}
	// Keeping every write (and tearing none) keeps everything.
	kept := fs.Crash(rand.New(rand.NewSource(1)), CrashOptions{KeepProbability: 1})
	if got := readAll(t, kept, "/d/a"); string(got) != "durable!volatile" {
		t.Fatalf("after crash keeping writes: %q", got)
	}
	// The running file system is unaffected by Crash.
	if got := readAll(t, fs, "/d/a"); string(got) != "durable!volatile" {
		t.Fatalf("live view changed: %q", got)
	}
}

func TestFaultFSTornWrite(t *testing.T) {
	fs := NewFaultFS()
	f, _ := fs.OpenFile("/d/p", true)
	old := bytes.Repeat([]byte{1}, 4096)
	f.WriteAt(old, 0)
	f.Sync()
	f.WriteAt(bytes.Repeat([]byte{2}, 4096), 0)
	torn := 0
	for seed := int64(0); seed < 50; seed++ {
		img := readAll(t, fs.Crash(rand.New(rand.NewSource(seed)), CrashOptions{KeepProbability: 1, TornProbability: 1}), "/d/p")
		n1, n2 := bytes.Count(img, []byte{1}), bytes.Count(img, []byte{2})
		if n1+n2 != 4096 {
			t.Fatalf("seed %d: unexpected bytes", seed)
		}
		if n1 > 0 && n2 > 0 {
			torn++
			if n2%512 != 0 {
				t.Fatalf("seed %d: tear not on a sector boundary (%d new bytes)", seed, n2)
			}
		}
	}
	if torn == 0 {
		t.Fatal("TornProbability 1 never produced a torn page")
	}
}

func TestFaultFSInjectedErrors(t *testing.T) {
	fs := NewFaultFS()
	fs.ErrAfterWrites = 2
	f, _ := fs.OpenFile("/d/e", true)
	for i := 0; i < 2; i++ {
		if _, err := f.WriteAt([]byte("x"), int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.WriteAt([]byte("x"), 2); err != ErrInjected {
		t.Fatalf("want injected error, got %v", err)
	}
}

func TestOSFileSyncAndList(t *testing.T) {
	dir := t.TempDir()
	var fs FS = OS{}
	f, err := fs.OpenFile(filepath.Join(dir, "x"), true)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte("hi"), 0)
	if err := f.Sync(); err != nil {
		t.Fatalf("full sync: %v", err)
	}
	b := make([]byte, 4)
	f.ReadAt(b, 0) // reading past the end returns zeros
	if string(b) != "hi\x00\x00" {
		t.Fatalf("read %q", b)
	}
	f.Close()
	if err := fs.SyncDir(dir); err != nil {
		t.Fatal(err)
	}
	names, _ := fs.List(dir)
	if len(names) != 1 || names[0] != "x" {
		t.Fatalf("list %v", names)
	}
	ns := NoSync{FS: fs}
	g, _ := ns.OpenFile(filepath.Join(dir, "y"), true)
	if err := g.Sync(); err != nil {
		t.Fatal(err)
	}
	g.Close()
}
