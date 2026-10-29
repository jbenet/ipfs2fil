package explorer

import (
	"crypto/sha256"
	"fmt"
	"path"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// Demo returns a made-up archive for previews and tests: a few named and
// unnamed pins, an MFS tree, orphans, pieces and three copies. Larger scale
// gives more entries (scale 1 is about 300). The same scale always gives the
// same archive.
func Demo(scale int) (*manifest.Manifest, []Entry) {
	scale = max(scale, 1)
	const MiB = 1 << 20
	pieceSize := int64(64 * MiB * scale)

	wiki := dir("wikipedia-en-2021-snapshot",
		dir("A", files(40*scale, "Article_%d.html", "page", 90_000)...),
		dir("I", files(30*scale, "image_%d.jpg", "image", 400_000)...),
		dir("M", files(10*scale, "meta_%d.json", "code", 4_000)...),
		file("index.html", "page", 38_000),
	)
	photos := files(150*scale, "IMG_%d.HEIC", "image", 3*MiB)
	photos = append(photos, files(8*scale, "VID_%d.mp4", "video", 90*MiB)...)
	photos = append(photos, files(6*scale, "memo-%d.m4a", "audio", 2*MiB)...)
	photos = append(photos, files(6*scale, "receipt-%d.pdf", "doc", 400_000)...)
	photos = append(photos, files(4*scale, "notes-%d.txt", "text", 3_000)...)
	photos = append(photos, files(2*scale, "album-%d.zip", "archive", 60*MiB)...)
	photos = append(photos, dir("screenshots", files(12*scale, "Screenshot %d.png", "image", 600_000)...))
	mfs := dir("mfs",
		dir("photos", dir("2019", photos...), dir("beach-2018", files(20*scale, "DSC%05d.JPG", "image", 5*MiB)...)),
		dir("videos", files(4*scale, "talk-%d.webm", "video", 300*MiB)...),
		dir("code", files(10*scale, "script_%d.py", "code", 6_000)...),
		file("README.md", "text", 2_000),
	)

	type root struct {
		kind, name string
		n          *node
		v0         bool
	}
	roots := []root{
		{manifest.KindPinRecursive, "wikipedia-en-2021-snapshot", wiki, false},
		{manifest.KindPinRecursive, "ubuntu-24.04-desktop-amd64.iso", file("ubuntu-24.04-desktop-amd64.iso", "disk", int64(scale)*700*MiB), false},
		{manifest.KindPinRecursive, "", dir("site", file("index.html", "page", 700_000), file("style.css", "code", 4_000)), true},
	}
	// Three versions of a blog share a logo and a stylesheet, so a search
	// finds the same CIDs in several places.
	for v := 1; v <= 3; v++ {
		blog := dir("blog", file(fmt.Sprintf("index-v%d.html", v), "page", int64(30_000*v)),
			same(file("logo.png", "image", 35_000), "blog/logo.png"), same(file("style.css", "code", 6_000), "blog/style.css"))
		roots = append(roots, root{manifest.KindPinRecursive, fmt.Sprintf("blog-v%d", v), blog, false})
	}
	roots = append(roots, []root{
		{manifest.KindPinDirect, "", file("block", "other", 12_000), false},
		{manifest.KindMFS, "", mfs, false},
	}...)
	for i := 0; i < 3*scale; i++ {
		roots = append(roots, root{manifest.KindOrphan, "", file("orphan", "other", int64(1+i)*200_000), false})
	}

	m := &manifest.Manifest{Format: manifest.Format, Tool: "ipfs2fil demo"}
	var entries []Entry
	var off, blocks int64
	for _, r := range roots {
		c := fakeCID(r.kind+r.name+fmt.Sprint(len(m.Roots)), r.n.kind == "folder", r.v0)
		p := "mfs"
		switch r.kind {
		case manifest.KindPinRecursive:
			p = "pins/recursive/" + c
		case manifest.KindPinDirect:
			p = "pins/direct/" + c
		case manifest.KindOrphan:
			p = "orphans/" + c
		}
		first := int(off / pieceSize)
		name := r.name
		if name == "" && r.kind != manifest.KindMFS {
			name = c
		} else if r.kind == manifest.KindMFS {
			name = "mfs"
		}
		n := r.n.flatten(p, name, c, &off, pieceSize, &entries)
		m.Roots = append(m.Roots, manifest.Root{
			CID: c, Kind: r.kind, Name: r.name, Path: p, Bytes: r.n.bytes,
			Blocks: n, FirstPiece: first, LastPiece: int((off - 1) / pieceSize),
		})
		blocks += n
	}
	m.Source = manifest.Source{RepoVersion: 15, Blocks: blocks, Bytes: off}
	m.DataRoot = fakeCID("data", true, false)
	// Like pack, the manifest inside the archive leaves the archive root and
	// the current snapshot's root empty; the pages read the root from the URL.

	pieces := int((off-1)/pieceSize) + 2 // the last one holds index/ and explorer/
	t0 := time.Date(2026, 6, 12, 9, 30, 0, 0, time.UTC)
	t1 := time.Date(2026, 10, 21, 14, 5, 0, 0, time.UTC)
	m.Snapshots = []manifest.Snapshot{{Time: t0, Root: fakeCID("archive-0", true, false)}, {Time: t1}}
	copies := []manifest.Copy{
		{Provider: "f04", DataSetID: "1745", PieceIDs: map[int]string{}, Service: "foc-fwss"},
		{Provider: "f09", DataSetID: "1746", PieceIDs: map[int]string{}, Service: "foc-fwss"},
		{Provider: "f032", DataSetID: "1748", PieceIDs: map[int]string{}, Service: "foc-fwss"},
	}
	for i := 0; i < pieces; i++ {
		size := pieceSize
		if i >= pieces-2 {
			size = pieceSize/2 + int64(i)*MiB
		}
		snap := 0
		if i%7 == 3 || i == pieces-1 {
			snap = 1
			m.Snapshots[1].NewPieces = append(m.Snapshots[1].NewPieces, i)
		} else {
			m.Snapshots[0].NewPieces = append(m.Snapshots[0].NewPieces, i)
		}
		m.Pieces = append(m.Pieces, manifest.Piece{
			Index: i, PieceCID: fakePieceCID(i), CARBytes: size, Blocks: size / (256 << 10),
			BlockList: fmt.Sprintf("pieces/%d.txt", i), Snapshot: snap,
		})
		for j := range copies {
			copies[j].PieceIDs[i] = fmt.Sprint(i + 1)
		}
	}
	m.Copies = copies
	return m, entries
}

type node struct {
	name, kind string
	bytes      int64
	kids       []*node
	seed       string // when set, the CID comes from it instead of the path
}

// same gives n a CID that does not depend on where it sits.
func same(n *node, seed string) *node { n.seed = seed; return n }

func file(name, kind string, bytes int64) *node { return &node{name: name, kind: kind, bytes: bytes} }

func dir(name string, kids ...*node) *node {
	n := &node{name: name, kind: "folder", kids: kids}
	for _, k := range kids {
		n.bytes += k.bytes
	}
	return n
}

func files(n int, pattern, kind string, bytes int64) []*node {
	out := make([]*node, n)
	for i := range out {
		// Sizes vary a little so sorting by size means something.
		out[i] = file(fmt.Sprintf(pattern, 1000+i), kind, bytes/2+int64(i*7919)%bytes)
	}
	return out
}

// flatten appends n and everything under it to entries, laying files out
// depth first from *off, and returns the number of blocks it made up.
func (n *node) flatten(p, name, c string, off *int64, pieceSize int64, entries *[]Entry) int64 {
	e := Entry{Name: name, Path: p, CID: c, Kind: n.kind, Bytes: n.bytes, Piece: int(*off / pieceSize)}
	*entries = append(*entries, e)
	if n.kind != "folder" {
		*off += n.bytes
		return 1 + n.bytes/(256<<10)
	}
	var blocks int64 = 1
	for _, k := range n.kids {
		kp, seed := path.Join(p, k.name), k.seed
		if seed == "" {
			seed = kp
		}
		blocks += k.flatten(kp, k.name, fakeCID(seed, k.kind == "folder", false), off, pieceSize, entries)
	}
	return blocks
}

func fakeCID(seed string, folder, v0 bool) string {
	h, _ := multihash.Sum([]byte(seed), multihash.SHA2_256, -1)
	switch {
	case v0:
		return cid.NewCidV0(h).String()
	case folder:
		return cid.NewCidV1(cid.DagProtobuf, h).String()
	}
	return cid.NewCidV1(cid.Raw, h).String()
}

func fakePieceCID(i int) string {
	d := sha256.Sum256(fmt.Appendf(nil, "piece %d", i))
	h, _ := multihash.Encode(append([]byte{0x80, 30}, d[:]...), 0x1011) // fr32-sha256-trunc254-padbintree
	return cid.NewCidV1(cid.Raw, h).String()
}
