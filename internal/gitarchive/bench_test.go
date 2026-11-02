package gitarchive

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// BenchmarkIndexPackFile indexes the pack named by $GITARCHIVE_BENCH_PACK
// (a full pack, no external bases), for profiling on real repos.
func BenchmarkIndexPackFile(b *testing.B) {
	p := os.Getenv("GITARCHIVE_BENCH_PACK")
	if p == "" {
		b.Skip("set GITARCHIVE_BENCH_PACK to a .pack file")
	}
	f, err := os.Open(p)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	b.SetBytes(st.Size())
	for b.Loop() {
		if _, err := IndexPack(f, st.Size(), SHA1, nil, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// Indexing a pack of many small deltas allocates about what the objects
// need, not a zlib window per delta (that was 32 KiB+ each, 80% of all
// allocation on kubo's pack).
func TestIndexPackAllocations(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	body := strings.Repeat("a line that stays the same in every version of the file\n", 200)
	for i := range 150 {
		write(t, filepath.Join(dir, "f.txt"), body+fmt.Sprintf("version %d\n", i))
		git(t, dir, "add", "f.txt")
		git(t, dir, "commit", "-q", "-m", fmt.Sprint(i))
	}
	git(t, dir, "repack", "-adq")
	packs, _ := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "*.pack"))
	f, err := os.Open(packs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	ix, err := IndexPack(f, st.Size(), SHA1, nil, nil)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("%d objects, %d byte pack: %d bytes allocated", len(ix.Objects), st.Size(), alloc)
	if per := alloc / uint64(len(ix.Objects)); per > 16<<10 {
		t.Fatalf("%d bytes allocated per object (%d objects)", per, len(ix.Objects))
	}
}

// Loading CARs keeps their blocks on disk: memory grows with the number of
// blocks, not their bytes, so restoring a big repo does not need its size
// in RAM.
func TestLoadKeepsBlocksOnDisk(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	big := make([]byte, 24<<20)
	rand.Read(big) // incompressible, so the pack is big
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "big")
	out := filepath.Join(t.TempDir(), "a.car")
	if _, err := WriteCAR(out, func(put BlockSink) (*Result, error) {
		return Archive(t.Context(), dir, Options{TempDir: t.TempDir()}, put)
	}); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(out)
	big = nil
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	bs := NewBlocks()
	if err := bs.Load(out); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	held := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("%d byte CAR: %d bytes held after Load", st.Size(), held)
	if held > st.Size()/8 {
		t.Fatalf("Load holds %d bytes of a %d byte CAR", held, st.Size())
	}
	root, err := bs.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(t.Context(), bs.Get, root, filepath.Join(t.TempDir(), "r.git"), RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	runtime.KeepAlive(bs)
}
