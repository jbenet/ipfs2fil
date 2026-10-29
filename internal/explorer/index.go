package explorer

// The explorer index lives in explorer/idx. Every file is one line of
// JavaScript, `I2F.put("<key>", <json>);`, so the pages can load it with a
// <script> tag, which works from file:// where fetch does not.
//
//	manifest.js  key "manifest": index/manifest.json as written by pack
//	meta.js      key "meta": counts, totals by kind and part, one summary per piece
//	s/<n>.js     key "s/<n>": shard n, up to shardRows entries grouped by directory:
//	             {"dirs": [path, ...], "rows": [[dir, name, kind, bytes, cid, piece, seg?], ...]}
//	             dir indexes "dirs"; seg is the last path element when it differs from name
//	d/<n>.js     key "d/<n>": bucket n, the directories whose path hashes to n
//	             (FNV-1a 32 of the UTF-8 path, mod meta.buckets), each with its
//	             totals and the shard ranges holding its entries:
//	             {"<path>": {"cid", "n", "files", "folders", "bytes", "lo", "hi", "types", "chunks": [[shard, start, count], ...]}}
//
// Browse loads one bucket and the shards its directory spans. Search streams
// every shard and shows matches as they arrive. The data/ directory is path "".

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"path"
	"sort"
	"strings"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// IndexFormat is bumped when the index changes in a way old pages cannot read.
const IndexFormat = 1

var (
	shardRows     = 4000 // entries per shard
	dirsPerBucket = 128
)

type dirInfo struct {
	CID     string           `json:"cid"`
	N       int              `json:"n"`
	Files   int              `json:"files"`
	Folders int              `json:"folders"`
	Bytes   int64            `json:"bytes"`
	Lo      int              `json:"lo"` // lowest and highest piece among its entries
	Hi      int              `json:"hi"`
	Types   map[string]tally `json:"types"`
	Chunks  [][3]int         `json:"chunks"`
}

// tally is [count, bytes].
type tally [2]int64

type pieceInfo struct {
	Where string `json:"where"` // pins, mfs, orphans or index
	Kind  string `json:"kind"`  // the kind holding most of its bytes
	Label string `json:"label"`
	Path  string `json:"path"`          // directory to browse for it
	CID   string `json:"cid,omitempty"` // the root, when one root holds it
}

type meta struct {
	Format  int              `json:"format"`
	Entries int              `json:"entries"`
	Dirs    int              `json:"dirs"`
	Shards  int              `json:"shards"`
	Buckets int              `json:"buckets"`
	Types   map[string]tally `json:"types"`
	Parts   map[string]tally `json:"parts"`
	Pieces  []pieceInfo      `json:"pieces"`
}

type shard struct {
	Dirs []string `json:"dirs"`
	Rows [][]any  `json:"rows"`
}

// buildIndex returns the index files, keyed by path under idx/.
func buildIndex(m *manifest.Manifest, entries []Entry) (map[string][]byte, error) {
	byPath := map[string]*Entry{}
	for i := range entries {
		e := entries[i]
		if err := checkPath(e.Path); err != nil {
			return nil, err
		}
		if _, dup := byPath[e.Path]; dup {
			return nil, fmt.Errorf("explorer: two entries for data/%s; the archive builder should list each path once", e.Path)
		}
		byPath[e.Path] = &e
	}
	// Every root gets an entry, even when the builder listed nothing for it.
	for _, r := range m.Roots {
		if err := checkPath(r.Path); err != nil {
			return nil, err
		}
		if _, ok := byPath[r.Path]; !ok {
			byPath[r.Path] = &Entry{Name: rootName(r), Path: r.Path, CID: r.CID, Kind: "other", Bytes: r.Bytes, Piece: r.FirstPiece}
		}
	}
	// Every ancestor gets a folder entry, so each path can be browsed to.
	var made []string
	for _, p := range sortedKeys(byPath) {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if _, ok := byPath[d]; ok {
				break
			}
			byPath[d] = &Entry{Name: path.Base(d), Path: d, Kind: "folder", Piece: -1}
			made = append(made, d)
		}
	}
	children := map[string][]*Entry{"": nil}
	for _, e := range byPath {
		children[parent(e.Path)] = append(children[parent(e.Path)], e)
		if e.Kind == "folder" && children[e.Path] == nil {
			children[e.Path] = []*Entry{}
		}
	}
	// Made-up folders take their size and first piece from their contents,
	// deepest first so nested ones add up.
	sort.Slice(made, func(i, j int) bool { return strings.Count(made[i], "/") > strings.Count(made[j], "/") })
	for _, d := range made {
		for _, c := range children[d] {
			byPath[d].Bytes += c.Bytes
			if c.Piece >= 0 && (byPath[d].Piece < 0 || c.Piece < byPath[d].Piece) {
				byPath[d].Piece = c.Piece
			}
		}
	}
	for p, e := range byPath {
		if _, ok := children[p]; ok && e.Kind != "folder" {
			e.Kind = "folder" // anything with contents lists as a folder
		}
	}

	mt := meta{Format: IndexFormat, Entries: len(byPath), Types: map[string]tally{}, Parts: map[string]tally{}}
	for _, e := range byPath {
		if e.Kind == "folder" {
			continue
		}
		// totals count each block once, however many names reach it
		own := max(0, e.Bytes-e.Dup)
		mt.Types[e.Kind] = add(mt.Types[e.Kind], own)
		if w := where(e.Path); w != "" {
			mt.Parts[w] = add(mt.Parts[w], own)
		}
	}

	dirs := sortedKeys(children)
	mt.Dirs = len(dirs)
	mt.Buckets = (len(dirs) + dirsPerBucket - 1) / dirsPerBucket
	buckets := make([]map[string]*dirInfo, mt.Buckets)
	var shards []*shard
	cur := &shard{}
	for _, d := range dirs {
		kids := children[d]
		sort.Slice(kids, func(i, j int) bool { return less(kids[i], kids[j]) })
		info := &dirInfo{N: len(kids), Lo: -1, Hi: -1, Types: map[string]tally{}, Chunks: [][3]int{}}
		if d == "" {
			info.CID = m.DataRoot
		} else {
			info.CID = byPath[d].CID
		}
		for i := 0; i < len(kids); {
			if len(cur.Rows) == shardRows {
				shards, cur = append(shards, cur), &shard{}
			}
			n := min(shardRows-len(cur.Rows), len(kids)-i)
			info.Chunks = append(info.Chunks, [3]int{len(shards), len(cur.Rows), n})
			di := len(cur.Dirs)
			cur.Dirs = append(cur.Dirs, d)
			for _, e := range kids[i : i+n] {
				cur.Rows = append(cur.Rows, row(di, e))
			}
			i += n
		}
		for _, e := range kids {
			info.Bytes += e.Bytes
			if e.Kind == "folder" {
				info.Folders++
			} else {
				info.Files++
			}
			info.Types[e.Kind] = add(info.Types[e.Kind], e.Bytes)
			if e.Piece >= 0 {
				if info.Lo < 0 || e.Piece < info.Lo {
					info.Lo = e.Piece
				}
				info.Hi = max(info.Hi, e.Piece)
			}
		}
		b := bucketOf(d, mt.Buckets)
		if buckets[b] == nil {
			buckets[b] = map[string]*dirInfo{}
		}
		buckets[b][d] = info
	}
	if len(cur.Rows) > 0 || len(shards) == 0 {
		shards = append(shards, cur)
	}
	mt.Shards = len(shards)
	mt.Pieces = summarizePieces(m, byPath)

	files := map[string][]byte{}
	put := func(name, key string, v any) error {
		b, err := jsFile(key, v)
		files[name] = b
		return err
	}
	if err := put("manifest.js", "manifest", m); err != nil {
		return nil, err
	}
	if err := put("meta.js", "meta", mt); err != nil {
		return nil, err
	}
	for i, s := range shards {
		if err := put(fmt.Sprintf("s/%d.js", i), fmt.Sprintf("s/%d", i), s); err != nil {
			return nil, err
		}
	}
	for i, b := range buckets {
		if b == nil {
			b = map[string]*dirInfo{}
		}
		if err := put(fmt.Sprintf("d/%d.js", i), fmt.Sprintf("d/%d", i), b); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// summarizePieces says what each piece mostly holds, for the Pieces page and
// the piece map on the overview.
func summarizePieces(m *manifest.Manifest, byPath map[string]*Entry) []pieceInfo {
	kindBytes := make([]map[string]int64, len(m.Pieces))
	dirOf := make([]string, len(m.Pieces))
	seen := make([]bool, len(m.Pieces))
	for _, p := range sortedKeys(byPath) {
		e := byPath[p]
		if e.Kind == "folder" || e.Piece < 0 || e.Piece >= len(m.Pieces) {
			continue
		}
		if kindBytes[e.Piece] == nil {
			kindBytes[e.Piece] = map[string]int64{}
		}
		kindBytes[e.Piece][e.Kind] += e.Bytes
		if !seen[e.Piece] {
			dirOf[e.Piece], seen[e.Piece] = parent(e.Path), true
		} else {
			dirOf[e.Piece] = commonDir(dirOf[e.Piece], parent(e.Path))
		}
	}
	// A piece with no entry starting in it continues a file from the one before.
	for i := 1; i < len(m.Pieces); i++ {
		if !seen[i] && seen[i-1] {
			kindBytes[i], dirOf[i], seen[i] = kindBytes[i-1], dirOf[i-1], true
		}
	}
	out := make([]pieceInfo, len(m.Pieces))
	for i := range m.Pieces {
		var in []manifest.Root
		for _, r := range m.Roots {
			if r.FirstPiece <= i && i <= r.LastPiece {
				in = append(in, r)
			}
		}
		pi := pieceInfo{Kind: "other"}
		var top int64 = -1
		for _, k := range Kinds {
			if b, ok := kindBytes[i][k]; ok && b > top {
				pi.Kind, top = k, b
			}
		}
		switch len(in) {
		case 0:
			pi.Where, pi.Label, pi.Kind = "index", "index/ and explorer/", "code"
		case 1:
			r := in[0]
			pi.Where, pi.Path, pi.CID = where(r.Path), r.Path, r.CID
			pi.Label = rootName(r)
			if d := dirOf[i]; strings.HasPrefix(d, r.Path+"/") {
				pi.Path = d
				pi.Label += strings.TrimPrefix(d, r.Path) + "/"
				pi.CID = ""
			} else if byPath[r.Path] != nil && byPath[r.Path].Kind == "folder" {
				pi.Label += "/"
			}
			if r.LastPiece > r.FirstPiece {
				pi.Label += fmt.Sprintf(" · part %d of %d", i-r.FirstPiece+1, r.LastPiece-r.FirstPiece+1)
			}
		default:
			count := map[string]int{}
			pi.Path = in[0].Path
			for _, r := range in {
				count[where(r.Path)]++
				pi.Path = commonDir(pi.Path, r.Path)
			}
			for _, w := range []string{"pins", "mfs", "orphans"} {
				if count[w] > count[pi.Where] {
					pi.Where = w
				}
			}
			pi.Label = fmt.Sprintf("%s (%d roots)", or(pi.Path, "data"), len(in))
		}
		out[i] = pi
	}
	return out
}

func row(dir int, e *Entry) []any {
	r := []any{dir, e.Name, e.Kind, e.Bytes, e.CID, e.Piece}
	if seg := path.Base(e.Path); seg != e.Name {
		r = append(r, seg)
	}
	return r
}

// less orders a listing: folders first, then by name ignoring case.
func less(a, b *Entry) bool {
	if fa, fb := a.Kind == "folder", b.Kind == "folder"; fa != fb {
		return fa
	}
	if la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name); la != lb {
		return la < lb
	}
	return a.Path < b.Path
}

func jsFile(key string, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "I2F.put(%q,%s);\n", key, b), nil
}

// bucketOf must match bucketOf in assets/explorer.js.
func bucketOf(dir string, n int) int {
	h := fnv.New32a()
	h.Write([]byte(dir))
	return int(h.Sum32() % uint32(n))
}

func checkPath(p string) error {
	if p == "" || p != path.Clean(p) || strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("explorer: bad entry path %q; paths are relative to data/, like mfs/photos/a.jpg", p)
	}
	return nil
}

func parent(p string) string {
	if d := path.Dir(p); d != "." {
		return d
	}
	return ""
}

// where names the part of the repo a data/ path is in.
func where(p string) string {
	switch top, _, _ := strings.Cut(p, "/"); top {
	case "pins", "mfs", "orphans":
		return top
	}
	return ""
}

func commonDir(a, b string) string {
	for a != b {
		if len(a) > len(b) {
			a = parent(a)
		} else {
			b = parent(b)
		}
	}
	return a
}

func rootName(r manifest.Root) string { return or(r.Name, r.CID) }

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func add(t tally, bytes int64) tally { return tally{t[0] + 1, t[1] + bytes} }

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
