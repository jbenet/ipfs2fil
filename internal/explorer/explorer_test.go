package explorer

import (
	"bytes"
	"encoding/json"
	"flag"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

var update = flag.Bool("update", false, "rewrite testdata/golden")

// sample is a small archive: a named pin with a folder, an unnamed pin, MFS
// and an orphan, over three pieces plus one for index/ and explorer/.
func sample() (*manifest.Manifest, []Entry) {
	m := &manifest.Manifest{
		Format: 1, Tool: "ipfs2fil test", DataRoot: "bafydata",
		Source: manifest.Source{RepoVersion: 18, Blocks: 40, Bytes: 9000},
		Roots: []manifest.Root{
			{CID: "bafysite", Kind: manifest.KindPinRecursive, Name: "my site", Path: "pins/recursive/bafysite", Bytes: 5000, Blocks: 20, FirstPiece: 0, LastPiece: 1},
			{CID: "QmBlob", Kind: manifest.KindPinDirect, Path: "pins/direct/QmBlob", Bytes: 100, Blocks: 1, FirstPiece: 1, LastPiece: 1},
			{CID: "bafymfs", Kind: manifest.KindMFS, Path: "mfs", Bytes: 3800, Blocks: 18, FirstPiece: 1, LastPiece: 2},
			{CID: "bafkorphan", Kind: manifest.KindOrphan, Path: "orphans/bafkorphan", Bytes: 100, Blocks: 1, FirstPiece: 2, LastPiece: 2},
		},
		Pieces: []manifest.Piece{
			{Index: 0, PieceCID: "bafkzcib0", CARBytes: 4000, Blocks: 15},
			{Index: 1, PieceCID: "bafkzcib1", CARBytes: 4000, Blocks: 15},
			{Index: 2, PieceCID: "bafkzcib2", CARBytes: 1200, Blocks: 10},
			{Index: 3, PieceCID: "bafkzcib3", CARBytes: 300, Blocks: 4, Snapshot: 1},
		},
	}
	entries := []Entry{
		{Name: "my site", Path: "pins/recursive/bafysite", CID: "bafysite", Kind: "folder", Bytes: 5000, Piece: 0},
		{Name: "index.html", Path: "pins/recursive/bafysite/index.html", CID: "bafkindex", Kind: "page", Bytes: 1000, Piece: 0},
		{Name: "img", Path: "pins/recursive/bafysite/img", CID: "bafyimg", Kind: "folder", Bytes: 4000, Piece: 0},
		{Name: "a.png", Path: "pins/recursive/bafysite/img/a.png", CID: "bafka", Kind: "image", Bytes: 3000, Piece: 0},
		{Name: "B.png", Path: "pins/recursive/bafysite/img/B.png", CID: "bafkb", Kind: "image", Bytes: 1000, Piece: 1},
		{Name: "QmBlob", Path: "pins/direct/QmBlob", CID: "QmBlob", Kind: "other", Bytes: 100, Piece: 1},
		{Name: "mfs", Path: "mfs", CID: "bafymfs", Kind: "folder", Bytes: 3800, Piece: 1},
		{Name: "talk.mp4", Path: "mfs/talk.mp4", CID: "bafktalk", Kind: "video", Bytes: 3500, Piece: 1},
		{Name: "notes.txt", Path: "mfs/notes.txt", CID: "bafknotes", Kind: "text", Bytes: 300, Piece: 2},
		{Name: "empty", Path: "mfs/empty", CID: "bafyempty", Kind: "folder", Bytes: 0, Piece: 2},
	}
	return m, entries
}

func small(t *testing.T) {
	r, b := shardRows, dirsPerBucket
	shardRows, dirsPerBucket = 4, 3
	t.Cleanup(func() { shardRows, dirsPerBucket = r, b })
}

func TestWriteGolden(t *testing.T) {
	small(t)
	m, entries := sample()
	dir := t.TempDir()
	if err := Write(dir, m, entries); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "golden")
	if *update {
		os.RemoveAll(golden)
		if err := os.CopyFS(golden, os.DirFS(filepath.Join(dir, "idx"))); err != nil {
			t.Fatal(err)
		}
	}
	got := readTree(t, filepath.Join(dir, "idx"))
	want := readTree(t, golden)
	for name, w := range want {
		if g, ok := got[name]; !ok {
			t.Errorf("idx/%s missing", name)
		} else if !bytes.Equal(g, w) {
			t.Errorf("idx/%s differs from golden (go test -update to accept):\n got %s\nwant %s", name, g, w)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("idx/%s not in golden (go test -update to accept)", name)
		}
	}
	for _, page := range []string{"index.html", "browse.html", "pin.html", "pieces.html", "search.html", "explorer.js", "explorer.css"} {
		if _, err := os.Stat(filepath.Join(dir, page)); err != nil {
			t.Errorf("page %s not written: %v", page, err)
		}
	}
}

// The same archive gives the same explorer, whatever order entries come in.
func TestDeterministic(t *testing.T) {
	m, entries := Demo(2)
	a, err := buildIndex(m, entries)
	if err != nil {
		t.Fatal(err)
	}
	rand.New(rand.NewSource(1)).Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
	b, err := buildIndex(m, entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("%d files, then %d", len(a), len(b))
	}
	for k := range a {
		if !bytes.Equal(a[k], b[k]) {
			t.Errorf("%s changed with entry order", k)
		}
	}
}

// Every entry can be found by browsing to its folder: bucket, then chunks.
func TestEveryEntryReachable(t *testing.T) {
	r, b := shardRows, dirsPerBucket
	shardRows, dirsPerBucket = 37, 5
	defer func() { shardRows, dirsPerBucket = r, b }()
	m, entries := Demo(3)
	files, err := buildIndex(m, entries)
	if err != nil {
		t.Fatal(err)
	}
	var mt meta
	decode(t, files["meta.js"], &mt)
	shards := map[string]shard{}
	for _, name := range mt.Shards {
		var s shard
		decode(t, files["s/"+name+".js"], &s)
		shards[name] = s
	}
	listed := map[string]bool{}
	for i := 0; i < mt.Buckets; i++ {
		var bucket map[string]dirInfo
		decode(t, files["d/"+itoa(i)+".js"], &bucket)
		for d, info := range bucket {
			if bucketOf(d, mt.Buckets) != i {
				t.Errorf("dir %q in bucket %d, hashes to %d", d, i, bucketOf(d, mt.Buckets))
			}
			n := 0
			for _, c := range info.Chunks {
				s := shards[c[0].(string)]
				start, count := int(c[1].(float64)), int(c[2].(float64))
				for _, row := range s.Rows[start : start+count] {
					if got := s.Dirs[int(row[0].(float64))]; got != d {
						t.Errorf("row %v of %q says dir %q", row, d, got)
					}
					seg := row[1].(string)
					if len(row) > 6 {
						seg = row[6].(string)
					}
					listed[strings.TrimPrefix(d+"/"+seg, "/")] = true
					n++
				}
			}
			if n != info.N {
				t.Errorf("dir %q: %d rows in chunks, n=%d", d, n, info.N)
			}
		}
	}
	for _, e := range entries {
		if !listed[e.Path] {
			t.Errorf("%s not reachable", e.Path)
		}
	}
	if len(listed) != mt.Entries {
		t.Errorf("%d listed, meta says %d", len(listed), mt.Entries)
	}
}

// One more name changes the shards around it and its bucket, not the rest,
// so a new snapshot of the archive reuses the others' blocks.
func TestSmallChangeKeepsMostFiles(t *testing.T) {
	r := shardRows
	shardRows = 40
	defer func() { shardRows = r }()
	m, entries := Demo(3)
	a, err := buildIndex(m, entries)
	if err != nil {
		t.Fatal(err)
	}
	more := append(entries[:len(entries):len(entries)], Entry{Name: "new.txt", Path: entries[len(entries)/2].Path + "-new.txt", CID: "bafknew", Kind: "text", Bytes: 10, Piece: 0})
	b, err := buildIndex(m, more)
	if err != nil {
		t.Fatal(err)
	}
	same, shards := 0, 0
	for k := range a {
		if strings.HasPrefix(k, "s/") {
			shards++
			if bytes.Equal(a[k], b[k]) {
				same++
			}
		}
	}
	if shards < 10 || shards-same > 2 {
		t.Errorf("%d of %d shards changed", shards-same, shards)
	}
}

func TestBadEntries(t *testing.T) {
	m, entries := sample()
	for _, p := range []string{"", "/abs", "../up", "a//b", "a/"} {
		if _, err := buildIndex(m, append(entries[:len(entries):len(entries)], Entry{Name: "x", Path: p})); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
	if _, err := buildIndex(m, append(entries[:len(entries):len(entries)], entries[0])); err == nil {
		t.Error("duplicate path accepted")
	}
}

// The pages must work from any gateway path and from file://: no absolute
// URLs to load, nothing from a CDN.
func TestAssetsSelfContained(t *testing.T) {
	ref := regexp.MustCompile(`(?:src|href)\s*=\s*["']([^"']+)|url\(([^)]+)\)|@import`)
	for _, name := range []string{"index.html", "browse.html", "pin.html", "pieces.html", "search.html", "explorer.css"} {
		b, err := assets.ReadFile("assets/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ref.FindAllStringSubmatch(string(b), -1) {
			u := m[1] + m[2]
			if m[0] == "@import" || strings.Contains(u, "//") || strings.HasPrefix(u, "/") {
				t.Errorf("%s loads %q; use relative URLs only", name, m[0])
			}
		}
	}
	js, _ := assets.ReadFile("assets/explorer.js")
	if regexp.MustCompile(`import\s|importScripts|\.src\s*=\s*['"]https?:`).Match(js) {
		t.Error("explorer.js loads code from outside the archive")
	}
}

// bucketOf in explorer.js must agree with bucketOf here.
func TestBucketOfMatchesJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	js, _ := assets.ReadFile("assets/explorer.js")
	fn := regexp.MustCompile(`(?s)function bucketOf\(p, n\) \{.*?\n  \}`).Find(js)
	if fn == nil {
		t.Fatal("bucketOf not found in explorer.js")
	}
	paths := []string{"", "mfs", "pins/recursive/bafysite", "mfs/photos/2019", "mfs/фото/日本", "a b/c\"d"}
	in, _ := json.Marshal(paths)
	out, err := exec.Command(node, "-e", string(fn)+"\nconsole.log(JSON.stringify("+string(in)+".map((p) => bucketOf(p, 97))))").Output()
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for i, p := range paths {
		if want := bucketOf(p, 97); got[i] != want {
			t.Errorf("bucketOf(%q): js %d, go %d", p, got[i], want)
		}
	}
}

func TestWriteManifest(t *testing.T) {
	m, entries := sample()
	dir := t.TempDir()
	if err := WriteManifest(dir, m); err == nil {
		t.Error("WriteManifest before Write succeeded")
	}
	if err := Write(dir, m, entries); err != nil {
		t.Fatal(err)
	}
	m.Copies = []manifest.Copy{{Provider: "f04", DataSetID: "7", PieceIDs: map[int]string{0: "1"}}}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "idx", "manifest.js"))
	if !bytes.Contains(b, []byte(`"provider":"f04"`)) {
		t.Errorf("manifest.js not refreshed: %s", b)
	}
}

func readTree(t *testing.T, root string) map[string][]byte {
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var jsWrap = regexp.MustCompile(`(?s)^I2F\.put\("[^"]*",(.*)\);\n$`)

func decode(t *testing.T, b []byte, v any) {
	t.Helper()
	m := jsWrap.FindSubmatch(b)
	if m == nil {
		t.Fatalf("not an index file: %.80s", b)
	}
	if err := json.Unmarshal(m[1], v); err != nil {
		t.Fatal(err)
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestKindOf(t *testing.T) {
	for _, c := range []struct {
		name string
		dir  bool
		want string
	}{
		{"photos", true, "folder"}, {"IMG_1.HEIC", false, "image"}, {"talk.mp4", false, "video"},
		{"memo.m4a", false, "audio"}, {"scan.PDF", false, "doc"}, {"index.html", false, "page"},
		{"notes.txt", false, "text"}, {"main.go", false, "code"}, {"backup.tar.gz", false, "archive"},
		{"ubuntu.iso", false, "disk"}, {"README", false, "other"}, {"x.unknownext", false, "other"},
	} {
		if got := KindOf(c.name, c.dir); got != c.want {
			t.Errorf("KindOf(%q, %v) = %q, want %q", c.name, c.dir, got, c.want)
		}
		if !strings.Contains(strings.Join(Kinds, " "), KindOf(c.name, c.dir)) {
			t.Errorf("KindOf(%q) not in Kinds", c.name)
		}
	}
}
