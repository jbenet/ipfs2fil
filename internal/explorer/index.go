package explorer

// The explorer index lives in explorer/idx. Every file is one line of
// JavaScript, `I2F.put("<key>", <json>);`, so the pages can load it with a
// <script> tag, which works from file:// where fetch does not.
//
//	manifest.js  key "manifest": index/manifest.json as written by pack
//	meta.js      key "meta": counts, totals by kind and part, one summary per
//	             piece, the names of the shards, and in the combined index
//	             explore serves over several archives, the repos (see combine)
//	s/<name>.js  key "s/<name>": a shard of entries grouped by directory:
//	             {"dirs": [path, ...], "pieces": [PieceCID, ...], "rows": [[dir, name, kind, bytes, cid, piece, seg?], ...]}
//	             dir indexes "dirs" and piece "pieces" (-1 for none); seg is the
//	             last path element when it differs from name
//	d/<n>.js     key "d/<n>": bucket n, the directories whose path hashes to n
//	             (FNV-1a 32 of the UTF-8 path, mod meta.buckets), each with its
//	             totals and the shard ranges holding its entries:
//	             {"<path>": {"cid", "n", "files", "folders", "bytes", "lo", "hi", "types", "chunks": [[shard, start, count], ...]}}
//	             lo and hi are the PieceCIDs of the lowest and highest piece among its entries
//
// Browse loads one bucket and the shards its directory spans. Search streams
// every shard and shows matches as they arrive. The data/ directory is path "".
//
// Only manifest.js and meta.js depend on the manifest. Shards and buckets
// depend on the entries and the data pieces alone, and are cut so that a
// small change to the repo changes few of them: shards end where an entry's
// path hashes to a cut point and are named by their contents, entries name
// pieces by PieceCID, and the bucket count only grows in powers of two. The
// archive then stores the unchanged ones in the same pieces as before.

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
// explorer.js asks for idx files with ?f=<IndexFormat>, so browsers do not
// reuse ones cached from an older index; bump it there too.
const IndexFormat = 2

var (
	shardRows     = 4000 // entries per shard, on average
	dirsPerBucket = 128  // at most, until the bucket count doubles
)

type dirInfo struct {
	CID     string           `json:"cid"`
	N       int              `json:"n"`
	Files   int              `json:"files"`
	Folders int              `json:"folders"`
	Bytes   int64            `json:"bytes"`
	Lo      string           `json:"lo"` // lowest and highest piece among its entries
	Hi      string           `json:"hi"`
	Types   map[string]tally `json:"types"`
	Chunks  [][3]any         `json:"chunks"` // [shard name, start, count]
	chunks  [][3]int         // [shard index, start, count]
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
	Shards  []string         `json:"shards"`
	Buckets int              `json:"buckets"`
	Types   map[string]tally `json:"types"`
	Parts   map[string]tally `json:"parts"`
	Pieces  []pieceInfo      `json:"pieces"`
	// Repos lists the archives of a combined index (see combine), whose
	// data/ holds one folder per repo; nil for one archive.
	Repos []repoInfo `json:"repos,omitempty"`
}

type shard struct {
	Dirs   []string    `json:"dirs"`
	Pieces []string    `json:"pieces"`
	Rows   [][]any     `json:"rows"`
	at     map[int]int // piece index -> position in Pieces
}

// piece returns the shard's number for data piece i, or -1.
func (s *shard) piece(m *manifest.Manifest, i int) int {
	if i < 0 || i >= len(m.Pieces) {
		return -1
	}
	if s.at == nil {
		s.at = map[int]int{}
	}
	if k, ok := s.at[i]; ok {
		return k
	}
	s.at[i] = len(s.Pieces)
	s.Pieces = append(s.Pieces, m.Pieces[i].PieceCID)
	return s.at[i]
}

// endsShard says whether a shard holding n rows ends after the entry at p:
// at a content-defined point once it has a quarter of shardRows, and always
// at twice shardRows.
func endsShard(p string, n int) bool {
	if n >= 2*shardRows {
		return true
	}
	if n < max(1, shardRows/4) {
		return false
	}
	h := fnv.New32a()
	h.Write([]byte(p))
	return h.Sum32()%uint32(max(1, shardRows*3/4)) == 0
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
	mt.Buckets = 1
	for mt.Buckets*dirsPerBucket < len(dirs) {
		mt.Buckets *= 2
	}
	buckets := make([]map[string]*dirInfo, mt.Buckets)
	var shards []*shard
	cur := &shard{}
	var infos []*dirInfo
	for _, d := range dirs {
		kids := children[d]
		sort.Slice(kids, func(i, j int) bool { return less(kids[i], kids[j]) })
		info := &dirInfo{N: len(kids), Types: map[string]tally{}, Chunks: [][3]any{}}
		infos = append(infos, info)
		if d == "" {
			info.CID = m.DataRoot
		} else {
			info.CID = byPath[d].CID
		}
		start := len(cur.Rows)
		di := -1
		for _, e := range kids {
			if di < 0 {
				di = len(cur.Dirs)
				cur.Dirs = append(cur.Dirs, d)
			}
			cur.Rows = append(cur.Rows, row(di, cur.piece(m, e.Piece), e))
			if endsShard(e.Path, len(cur.Rows)) {
				info.chunks = append(info.chunks, [3]int{len(shards), start, len(cur.Rows) - start})
				shards, cur, start, di = append(shards, cur), &shard{}, 0, -1
			}
		}
		if n := len(cur.Rows) - start; n > 0 {
			info.chunks = append(info.chunks, [3]int{len(shards), start, n})
		}
		lo, hi := -1, -1
		for _, e := range kids {
			info.Bytes += e.Bytes
			if e.Kind == "folder" {
				info.Folders++
			} else {
				info.Files++
			}
			info.Types[e.Kind] = add(info.Types[e.Kind], e.Bytes)
			if e.Piece >= 0 && e.Piece < len(m.Pieces) {
				if lo < 0 || e.Piece < lo {
					lo = e.Piece
				}
				hi = max(hi, e.Piece)
			}
		}
		if lo >= 0 {
			info.Lo, info.Hi = m.Pieces[lo].PieceCID, m.Pieces[hi].PieceCID
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
	// Shards are named by their contents, so an unchanged one keeps its file.
	names := make([]string, len(shards))
	shardFiles := map[string][]byte{}
	for i, sh := range shards {
		if sh.Pieces == nil {
			sh.Pieces = []string{}
		}
		b, err := json.Marshal(sh)
		if err != nil {
			return nil, err
		}
		h := fnv.New64a()
		h.Write(b)
		names[i] = fmt.Sprintf("%016x", h.Sum64())
		key := "s/" + names[i]
		shardFiles[key+".js"] = []byte(fmt.Sprintf("I2F.put(%q,%s);\n", key, b))
	}
	for _, info := range infos {
		for _, c := range info.chunks {
			info.Chunks = append(info.Chunks, [3]any{names[c[0]], c[1], c[2]})
		}
	}
	mt.Shards = names
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
	for name, b := range shardFiles {
		files[name] = b
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
			pi.Where, pi.Label, pi.Kind = "index", partLabel[m.Pieces[i].Part], "code"
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

var partLabel = map[string]string{
	"":                 "index/ and explorer/",
	manifest.PartIndex: "index/: block lists",
	manifest.PartMeta:  "explorer/ and data/ folders",
	manifest.PartHead:  "the archive root and manifest",
}

func row(dir, piece int, e *Entry) []any {
	r := []any{dir, e.Name, e.Kind, e.Bytes, e.CID, piece}
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

// where names the part of the repo a data/ path is in: its first element,
// or in a combined index, where the repo's folder comes first, its second.
func where(p string) string {
	top, rest, _ := strings.Cut(p, "/")
	for _, t := range []string{top, strings.SplitN(rest, "/", 2)[0]} {
		switch t {
		case "pins", "mfs", "orphans":
			return t
		}
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
