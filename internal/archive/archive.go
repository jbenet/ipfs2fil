// Package archive turns a repo into an archive: one UnixFS root linking every
// pin, the MFS tree and orphans under data/, plus index/ and explorer/, packed
// into pieces.
//
// Determinism: data/ and the piece cuts depend only on the repo's blocks and
// roots. Blocks are laid out depth first with roots sorted by kind (recursive
// pins, MFS, direct pins, orphans) and then by CID. A piece is cut at the
// start of a root, or at a block whose multihash ends in CutBits zero bits,
// once the piece holds at least MinPiece bytes; it is always cut before
// MaxPiece. So unchanged subtrees give the same pieces, and a re-run skips
// pieces whose PieceCID is already stored. Anything that varies between runs
// (times, copies) lives in index/.
//
// Data pieces hold repo blocks only, and each CAR header names the piece's
// first block, so a data piece's bytes depend on nothing outside it. The
// blocks the archive makes (data/ directories, index/, explorer/ and the
// archive root) go in the last pieces, whose headers name the archive root.
// index/manifest.json cannot name the archive root or those last pieces,
// since it is inside them; outDir/manifest.json, written next to the CARs,
// has everything.
package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ipfs/boxo/ipld/merkledag"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/car"
	"github.com/jbenet/ipfs2fil/internal/explorer"
	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/repo"
)

type Options struct {
	MinPiece int64 // bytes of CAR data before a content-defined cut is allowed
	MaxPiece int64 // hard cap on CAR bytes; must fit the store's piece limit
	CutBits  int   // a block whose multihash ends in this many zero bits may start a piece

	Orphans bool // include blocks no pin or MFS reaches, under data/orphans
	Tool    string
	Now     func() time.Time

	// Shared, if set, says which other archive's data piece already holds a
	// block. Such blocks are linked from data/ as usual but left out of this
	// archive's pieces, so blocks shared between repos are stored once.
	// Blocks in pieces this archive already stored are never shared, so a
	// re-run gives the same pieces even after other archives grow.
	Shared func(h multihash.Multihash) (SharedBlock, bool)

	// Record, if set, is a block of catalog data (collection.SealRecord)
	// that names the archive to the machines sharing its data sets. It goes
	// first in the archive's own pieces, whose CAR headers then name it as a
	// second root after the archive root, so a reader finds it from a piece's
	// first bytes. Nothing links to it.
	Record []byte

	// Progress, if set, is called as Build goes: stage "walk" counts blocks
	// laid out against the repo's block count, "write" counts CAR bytes
	// written against the bytes planned. total is 0 while unknown.
	Progress func(stage string, done, total int64)
}

// DefaultOptions fit synapse-go's 1 GiB*127/128 piece limit.
func DefaultOptions() Options {
	return Options{MinPiece: 512 << 20, MaxPiece: (1 << 30) * 127 / 128, CutBits: 10,
		Orphans: true, Tool: "ipfs2fil", Now: time.Now}
}

// SharedBlock names the other archive's piece that holds a block.
type SharedBlock struct {
	Archive  string // that archive's key
	PieceCID string
}

// Piece is a planned piece: a CAR file on disk and its PieceCID.
type Piece struct {
	manifest.Piece
	Path string // the CAR file
}

type Result struct {
	Manifest *manifest.Manifest
	Pieces   []Piece
	// Missing lists blocks that roots link to but the repo does not hold. The
	// archive has everything else; these were never in the repo to save.
	Missing []string
}

// ManifestFile is the full manifest Build writes in outDir. Build reads it
// back on the next run to carry snapshots and copies forward.
const ManifestFile = "manifest.json"

// Build reads r, links its roots, cuts pieces into outDir and returns the
// manifest with Roots and Pieces filled in. Copies are left to the store.
func Build(ctx context.Context, r repo.Repo, outDir string, opts Options) (*Result, error) {
	if opts.MaxPiece <= opts.MinPiece || opts.MinPiece < 0 || opts.CutBits < 0 || opts.CutBits > 64 {
		return nil, fmt.Errorf("bad piece options %+v", opts)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	prev, err := manifest.Read(filepath.Join(outDir, ManifestFile))
	if errors.Is(err, os.ErrNotExist) {
		prev, err = nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the last run's manifest: %w (move it away to start over)", err)
	}

	l := newLayout(r)
	if opts.Progress != nil {
		l.progress = opts.Progress
	}
	roots, err := l.walkRoots(ctx, opts.Orphans)
	if err != nil {
		return nil, err
	}
	shared, err := l.markShared(outDir, prev, opts.Shared)
	if err != nil {
		return nil, err
	}
	dataPieces, err := l.cut(opts)
	if err != nil {
		return nil, err
	}

	dag := memDAG{}
	dataRoot, err := l.dataDir(ctx, dag, roots)
	if err != nil {
		return nil, err
	}

	m := &manifest.Manifest{Format: manifest.Format, Tool: opts.Tool, DataRoot: dataRoot.Cid().String(),
		Source: manifest.Source{RepoVersion: int(r.Version()), Blocks: l.repoBlocks, Bytes: l.repoBytes}, Shared: shared}
	for _, rt := range roots {
		m.Roots = append(m.Roots, l.rootEntry(rt))
	}
	for i, p := range dataPieces {
		m.Pieces = append(m.Pieces, manifest.Piece{Index: i, Blocks: l.own(p),
			BlockList: fmt.Sprintf("pieces/%04d", i)})
	}

	// Write data pieces first: index/ needs their PieceCIDs.
	pieceDir := filepath.Join(outDir, "pieces")
	if err := os.MkdirAll(pieceDir, 0o755); err != nil {
		return nil, err
	}
	var planned, written, finished int64
	for _, p := range dataPieces {
		planned += car.HeaderSize(l.tab[p.start].c)
		for j := p.start; j < p.end; j++ {
			if l.tab[j].shared == 0 {
				planned += car.EntrySize(l.tab[j].c, l.tab[j].size)
			}
		}
	}
	l.progress("write", 0, planned)
	var out []Piece
	for i, p := range dataPieces {
		path, sum, err := writePiece(pieceDir, []cid.Cid{l.tab[p.start].c}, func(w *car.Writer) error {
			for j := p.start; j < p.end; j++ {
				b := &l.tab[j]
				if b.shared != 0 {
					continue
				}
				data, err := l.get(ctx, b.c)
				if err != nil {
					return err
				}
				if err := w.Put(b.c, data); err != nil {
					return err
				}
				if written += car.EntrySize(b.c, len(data)); j%256 == 0 {
					l.progress("write", written, planned)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		m.Pieces[i].PieceCID, m.Pieces[i].CARBytes = sum.PieceCID.String(), sum.Bytes
		finished += sum.Bytes
		written = finished
		out = append(out, Piece{Piece: m.Pieces[i], Path: path})
	}

	// History: a piece keeps the snapshot that first stored it. A run that
	// changes nothing adds no snapshot, so it rebuilds the same archive.
	snap, known := 0, map[string]int{}
	if prev != nil {
		m.Snapshots, m.Copies, m.Collection = prev.Snapshots, prev.Copies, prev.Collection
		for _, p := range prev.Pieces {
			known[p.PieceCID] = p.Snapshot
		}
		snap = len(m.Snapshots)
		if prev.DataRoot == m.DataRoot && snap > 0 {
			snap--
		}
	}
	var fresh []int
	for i := range m.Pieces {
		if s, ok := known[m.Pieces[i].PieceCID]; ok {
			m.Pieces[i].Snapshot = s
		} else {
			m.Pieces[i].Snapshot = snap
			fresh = append(fresh, i)
		}
		out[i].Piece = m.Pieces[i]
	}
	newSnap := snap == len(m.Snapshots)
	if newSnap {
		m.Snapshots = append(m.Snapshots, manifest.Snapshot{Time: opts.Now().UTC().Truncate(time.Second), NewPieces: fresh})
	} else {
		// the same data again: keep the snapshot, minus the archive's own pieces
		var keep []int
		for _, i := range m.Snapshots[snap].NewPieces {
			if i < len(dataPieces) {
				keep = append(keep, i)
			}
		}
		m.Snapshots[snap].NewPieces = keep
	}

	// index/ and explorer/
	// The archive cannot name its own root or the pieces it sits in, so
	// index/ and explorer/ see the manifest without them.
	// Copies stay out too: a push adds piece ids, and the next pack must not
	// change because of them (explorer.WriteManifest shows them locally).
	inner := *m
	inner.Copies, inner.Collection = nil, ""
	inner.Snapshots = slices.Clone(m.Snapshots)
	inner.Snapshots[snap].Root = ""
	idx, err := l.indexDir(ctx, dag, outDir, &inner, dataPieces)
	if err != nil {
		return nil, err
	}
	exp, err := explorerDir(ctx, dag, outDir, &inner, l, roots)
	if err != nil {
		return nil, err
	}
	root, err := mkdir(ctx, dag, []child{{"data", dataRoot}, {"index", idx}, {"explorer", exp}})
	if err != nil {
		return nil, err
	}
	m.Root = root.Cid().String()

	// The blocks the archive made, depth first from the root, in the last pieces.
	var made []format.Node
	seen := map[string]bool{}
	var walk func(c cid.Cid) error
	walk = func(c cid.Cid) error {
		k := string(c.Hash())
		if _, inRepo := l.idx[k]; inRepo || seen[k] {
			return nil
		}
		seen[k] = true
		n, err := dag.Get(ctx, c)
		if err != nil {
			return fmt.Errorf("archive block %s: %w", c, err)
		}
		made = append(made, n)
		for _, ln := range n.Links() {
			if err := walk(ln.Cid); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root.Cid()); err != nil {
		return nil, err
	}
	heads := []cid.Cid{root.Cid()}
	if opts.Record != nil {
		rec, err := RecordCID(opts.Record)
		if err != nil {
			return nil, err
		}
		heads = append(heads, rec)
		b, _ := blocks.NewBlockWithCid(opts.Record, rec)
		made = append([]format.Node{&merkledag.RawNode{Block: b}}, made...)
	}
	hdr := car.HeaderSize(heads...)
	for start := 0; start < len(made); {
		end, size := start, hdr
		for end < len(made) {
			e := car.EntrySize(made[end].Cid(), len(made[end].RawData()))
			if end > start && size+e > opts.MaxPiece {
				break
			}
			size += e
			end++
		}
		if size > opts.MaxPiece {
			return nil, fmt.Errorf("block %s does not fit in a piece", made[start].Cid())
		}
		path, sum, err := writePiece(pieceDir, heads, func(w *car.Writer) error {
			for _, n := range made[start:end] {
				if err := w.Put(n.Cid(), n.RawData()); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		i := len(m.Pieces)
		p := manifest.Piece{Index: i, PieceCID: sum.PieceCID.String(), CARBytes: sum.Bytes, Blocks: sum.Blocks, Snapshot: snap}
		if s, ok := known[p.PieceCID]; ok {
			p.Snapshot = s
		} else {
			m.Snapshots[snap].NewPieces = append(m.Snapshots[snap].NewPieces, i)
		}
		m.Pieces = append(m.Pieces, p)
		out = append(out, Piece{Piece: p, Path: path})
		start = end
	}
	l.progress("write", planned, planned)
	// A snapshot's root is set once: Snapshots[0].Root is the key the store
	// finds the archive's data sets by.
	if newSnap {
		m.Snapshots[snap].Root = m.Root
	}
	if err := m.Write(filepath.Join(outDir, ManifestFile)); err != nil {
		return nil, err
	}
	return &Result{Manifest: m, Pieces: out, Missing: l.missing}, nil
}

// RecordCID is the CID of an Options.Record block: raw, sha2-256.
func RecordCID(data []byte) (cid.Cid, error) {
	return cid.V1Builder{Codec: cid.Raw, MhType: multihash.SHA2_256}.Sum(data)
}

// writePiece writes one CAR into dir, named by its PieceCID.
func writePiece(dir string, roots []cid.Cid, fill func(*car.Writer) error) (string, car.Summary, error) {
	f, err := os.CreateTemp(dir, ".piece-*")
	if err != nil {
		return "", car.Summary{}, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	w, err := car.NewWriter(f, roots...)
	if err == nil {
		err = fill(w)
	}
	var sum car.Summary
	if err == nil {
		sum, err = w.Finish()
	}
	if err == nil {
		err = f.Close()
	}
	if err != nil {
		return "", sum, err
	}
	path := filepath.Join(dir, sum.PieceCID.String()+".car")
	return path, sum, os.Rename(f.Name(), path)
}

func (l *layout) rootEntry(rt root) manifest.Root {
	e := manifest.Root{CID: rt.c.String(), Kind: rt.kind, Name: rt.name, Path: rt.path(), FirstPiece: -1, LastPiece: -1}
	l.gen++
	var walk func(i int32)
	walk = func(i int32) {
		b := &l.tab[i]
		if b.mark == l.gen {
			return
		}
		b.mark = l.gen
		e.Blocks++
		e.Bytes += int64(b.size)
		// a shared block (piece -1) is in another archive's piece
		if b.piece >= 0 && (e.FirstPiece < 0 || int(b.piece) < e.FirstPiece) {
			e.FirstPiece = int(b.piece)
		}
		if int(b.piece) > e.LastPiece {
			e.LastPiece = int(b.piece)
		}
		for _, k := range b.kids {
			walk(k)
		}
	}
	if i, ok := l.idx[string(rt.c.Hash())]; ok {
		walk(i)
	}
	return e
}

func (rt root) path() string {
	switch rt.kind {
	case manifest.KindPinRecursive:
		return "pins/recursive/" + rt.c.String()
	case manifest.KindPinDirect:
		return "pins/direct/" + rt.c.String()
	case manifest.KindMFS:
		return "mfs"
	}
	return "orphans/" + rt.c.String()
}

// dataDir makes data/: pins/recursive, pins/direct, mfs and orphans.
func (l *layout) dataDir(ctx context.Context, dag memDAG, roots []root) (format.Node, error) {
	dirs := map[string][]child{}
	var top []child
	for _, rt := range roots {
		data, err := l.get(ctx, rt.c)
		if err != nil {
			return nil, err
		}
		n, err := linkNode(rt.c, data)
		if err != nil {
			return nil, fmt.Errorf("root %s: %w", rt.c, err)
		}
		if rt.kind == manifest.KindMFS {
			top = append(top, child{"mfs", n})
			continue
		}
		p := filepath.Dir(rt.path())
		dirs[p] = append(dirs[p], child{rt.c.String(), n})
	}
	var pins []child
	for _, name := range []string{"recursive", "direct"} {
		n, err := mkdir(ctx, dag, dirs["pins/"+name])
		if err != nil {
			return nil, err
		}
		pins = append(pins, child{name, n})
	}
	n, err := mkdir(ctx, dag, pins)
	if err != nil {
		return nil, err
	}
	top = append(top, child{"pins", n})
	if kids := dirs["orphans"]; len(kids) > 0 {
		n, err := mkdir(ctx, dag, kids)
		if err != nil {
			return nil, err
		}
		top = append(top, child{"orphans", n})
	}
	return mkdir(ctx, dag, top)
}

// indexDir writes index/ into outDir and adds it: the manifest as known from
// inside the archive, and one block list per data piece.
func (l *layout) indexDir(ctx context.Context, dag memDAG, outDir string, inner *manifest.Manifest, pieces []span) (format.Node, error) {
	dir := filepath.Join(outDir, "index")
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "pieces"), 0o755); err != nil {
		return nil, err
	}
	for i, p := range pieces {
		var sb strings.Builder
		for j := p.start; j < p.end; j++ {
			if l.tab[j].shared != 0 {
				continue
			}
			sb.WriteString(l.tab[j].c.Hash().B58String())
			sb.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(dir, "pieces", fmt.Sprintf("%04d", i)), []byte(sb.String()), 0o644); err != nil {
			return nil, err
		}
	}
	b, err := json.MarshalIndent(inner, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	return importDir(ctx, dag, dir)
}

// explorerDir has the explorer write its pages into outDir and adds them.
func explorerDir(ctx context.Context, dag memDAG, outDir string, m *manifest.Manifest, l *layout, roots []root) (format.Node, error) {
	dir := filepath.Join(outDir, "explorer")
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := explorer.Write(dir, m, l.entries(roots, m)); err != nil {
		return nil, fmt.Errorf("explorer: %w", err)
	}
	return importDir(ctx, dag, dir)
}

// hashOK reports whether data hashes to c.
func hashOK(c cid.Cid, data []byte) (bool, error) {
	d, err := multihash.Decode(c.Hash())
	if err != nil {
		return false, err
	}
	got, err := multihash.Sum(data, d.Code, d.Length)
	if err != nil {
		return false, fmt.Errorf("block %s uses a hash function ipfs2fil cannot check (%s)", c, multihash.Codes[d.Code])
	}
	return string(got) == string(c.Hash()), nil
}
