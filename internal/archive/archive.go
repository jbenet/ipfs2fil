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
	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/atproto"
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

	// NewPages, if set, writes index/ and explorer/ anew even when the data
	// is the same as the last pack's, so a new ipfs2fil's explorer goes into
	// the archive. Only those pieces and the root change.
	NewPages bool

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
	// Removed and RemovedBytes count the CARs Build deleted from outDir:
	// pieces of earlier packs the archive no longer has, and partial files a
	// killed run left. Pieces already stored stay stored with the snapshot
	// that added them; nothing reads the local CARs again.
	Removed      int
	RemovedBytes int64
	// Reused is set when nothing Build depends on changed since the last
	// pack in outDir, so it returned that pack without reading any block.
	Reused bool
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
	if err := l.list(ctx); err != nil {
		return nil, err
	}
	cache := readCache(outDir)
	inputs, err := l.inputs(ctx, outDir, prev, opts)
	if err != nil {
		return nil, err
	}
	if !opts.NewPages && prev != nil && cache.Inputs == inputs && cache.Root == prev.Root && reusable(outDir, prev) {
		return reuse(outDir, prev, cache)
	}
	// Until this pack completes, the cache only vouches for pieces.
	cache.Inputs, cache.Root, cache.Missing = "", "", nil
	if err := cache.write(outDir); err != nil {
		return nil, fmt.Errorf("writing %s: %w", CacheFile, err)
	}
	roots, err := l.walkRoots(ctx, opts.Orphans)
	if err != nil {
		return nil, err
	}
	if lr, ok := r.(repo.CARLayouter); ok {
		// index/cars lets an export write the CAR files back byte for
		// byte. A file it cannot lay out still exports, as blocks.
		if l.cars, err = lr.CARLayouts(); err != nil {
			l.cars = nil
		}
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
	if r.Kind() != repo.KindFSRepo {
		m.Source.Kind = r.Kind()
	}
	owned := make([]bool, len(l.tab))
	for _, rt := range roots {
		m.Roots = append(m.Roots, l.rootEntry(rt, owned))
	}
	for _, rt := range l.unstored {
		m.Unstored = append(m.Unstored, manifest.Root{CID: rt.c.String(), Kind: rt.kind, Name: rt.name})
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
	keys := make([]string, len(dataPieces))
	lists := l.lastLists(outDir, prev)
	for i, p := range dataPieces {
		keys[i] = l.pieceKey(p)
		if _, _, ok := cache.piece(pieceDir, keys[i]); !ok {
			// No cache entry (a pack from before the cache, or a lost
			// cache): a CAR the last pack wrote for the same block list
			// is the same bytes.
			if c, ok := lists.match(l, p, pieceDir); ok {
				cache.Pieces[keys[i]] = c
			}
		}
		if c, path, ok := cache.piece(pieceDir, keys[i]); ok {
			// The same blocks as a CAR already on disk: keep it.
			m.Pieces[i].PieceCID, m.Pieces[i].CARBytes = c.PieceCID, c.CARBytes
			finished += c.CARBytes
			written = finished
			l.progress("write", written, planned)
			out = append(out, Piece{Piece: m.Pieces[i], Path: path})
			continue
		}
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
		// Saved per piece, so a pack stopped part way keeps what it wrote.
		cache.Pieces[keys[i]] = cachedPiece{PieceCID: sum.PieceCID.String(), CARBytes: sum.Bytes}
		if err := cache.write(outDir); err != nil {
			return nil, fmt.Errorf("writing %s: %w", CacheFile, err)
		}
		m.Pieces[i].PieceCID, m.Pieces[i].CARBytes = sum.PieceCID.String(), sum.Bytes
		finished += sum.Bytes
		written = finished
		out = append(out, Piece{Piece: m.Pieces[i], Path: path})
	}

	// The same data as the last pack: keep its index/, explorer/ and root,
	// so a new ipfs2fil version, or a new layout for them, does not make
	// pieces to upload until the repo changes.
	if !opts.NewPages && sameData(prev, m, len(dataPieces), opts.Record, pieceDir) && reusable(outDir, prev) {
		res, err := reuse(outDir, prev, cache)
		if err != nil {
			return nil, err
		}
		res.Missing = l.missing
		done := &packCache{Format: cacheFormat, Inputs: inputs, Root: prev.Root, Missing: l.missing, Pieces: map[string]cachedPiece{}}
		for i, k := range keys {
			done.Pieces[k] = cachedPiece{PieceCID: m.Pieces[i].PieceCID, CARBytes: m.Pieces[i].CARBytes}
		}
		if err := done.write(outDir); err != nil {
			return nil, fmt.Errorf("writing %s: %w", CacheFile, err)
		}
		return res, nil
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
	//
	// Most of index/ and explorer/, and the data/ folders, depend on the repo
	// alone. Those blocks go in pieces cut by content like data pieces, so a
	// new snapshot keeps the pieces of the parts that did not change. The
	// rest (the root, the record, and what reads the manifest) goes in the
	// head pieces, small, which name the root. So index/ and explorer/ are
	// made twice: once to find the stable blocks and cut their pieces, then
	// with a manifest that lists those pieces.
	innerOf := func() *manifest.Manifest {
		inner := *m
		inner.Copies, inner.Collection = nil, ""
		inner.Snapshots = slices.Clone(m.Snapshots)
		inner.Snapshots[snap].Root = ""
		return &inner
	}
	l.accts = l.accounts(ctx, m.Roots)
	entries := l.entries(roots, m)
	idx, exp, err := l.ownDirs(ctx, dag, outDir, innerOf(), dataPieces, entries)
	if err != nil {
		return nil, err
	}
	shards, err := explorer.Shards(filepath.Join(outDir, "explorer"))
	if err != nil {
		return nil, err
	}
	stable, inIndex, err := l.stableBlocks(ctx, dag, dataRoot, idx, exp, shards)
	if err != nil {
		return nil, err
	}
	inMeta := map[string]bool{}
	for _, sp := range cutMeta(stable, opts) {
		first := stable[sp.start]
		part := manifest.PartMeta
		for _, n := range stable[sp.start:sp.end] {
			inMeta[string(n.Cid().Hash())] = true
			if inIndex[string(n.Cid().Hash())] {
				part = manifest.PartIndex
			}
		}
		path, sum, err := writePiece(pieceDir, []cid.Cid{first.Cid()}, func(w *car.Writer) error {
			for _, n := range stable[sp.start:sp.end] {
				if err := w.Put(n.Cid(), n.RawData()); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		out = append(out, addOwn(m, known, snap, sum, part, path))
	}
	if idx, exp, err = l.ownDirs(ctx, dag, outDir, innerOf(), dataPieces, entries); err != nil {
		return nil, err
	}
	root, err := mkdir(ctx, dag, []child{{"data", dataRoot}, {"index", idx}, {"explorer", exp}})
	if err != nil {
		return nil, err
	}
	m.Root = root.Cid().String()

	// The rest of the blocks the archive made, depth first from the root, in
	// the head pieces.
	var made []format.Node
	seen := map[string]bool{}
	var walk func(c cid.Cid) error
	walk = func(c cid.Cid) error {
		k := string(c.Hash())
		if _, inRepo := l.idx[k]; inRepo || seen[k] || inMeta[k] {
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
		out = append(out, addOwn(m, known, snap, sum, manifest.PartHead, path))
		start = end
	}
	l.progress("write", planned, planned)
	// A snapshot's root is set once: Snapshots[0].Root is the key the store
	// finds the archive's data sets by.
	if newSnap {
		m.Snapshots[snap].Root = m.Root
	}
	if prev != nil {
		m.Copies = remap(prev, m)
	}
	if err := m.Write(filepath.Join(outDir, ManifestFile)); err != nil {
		return nil, err
	}
	res := &Result{Manifest: m, Pieces: out, Missing: l.missing}
	if res.Removed, res.RemovedBytes, err = prune(pieceDir, m); err != nil {
		return nil, fmt.Errorf("removing old pieces: %w", err)
	}
	done := &packCache{Format: cacheFormat, Inputs: inputs, Root: m.Root, Missing: l.missing, Pieces: map[string]cachedPiece{}}
	for i, k := range keys {
		done.Pieces[k] = cachedPiece{PieceCID: m.Pieces[i].PieceCID, CARBytes: m.Pieces[i].CARBytes}
	}
	if err := done.write(outDir); err != nil {
		return nil, fmt.Errorf("writing %s: %w", CacheFile, err)
	}
	return res, nil
}

// sameData reports whether prev holds the same data as m's first n pieces:
// the same data root and data pieces, and its own pieces start with the
// same catalog record.
func sameData(prev, m *manifest.Manifest, n int, record []byte, pieceDir string) bool {
	if prev == nil || prev.DataRoot != m.DataRoot {
		return false
	}
	var data []manifest.Piece
	head := ""
	for _, p := range prev.Pieces {
		if p.BlockList != "" {
			data = append(data, p)
		} else if head == "" {
			head = p.PieceCID
		}
	}
	if len(data) != n || head == "" {
		return false
	}
	for i, p := range data {
		if p.PieceCID != m.Pieces[i].PieceCID {
			return false
		}
	}
	// The record is the second root in the CAR header of the piece that
	// names the archive root: the head piece, or an old layout's own piece.
	var want cid.Cid
	if record != nil {
		c, err := RecordCID(record)
		if err != nil {
			return false
		}
		want = c
	}
	for _, p := range prev.Pieces {
		if p.BlockList != "" || p.Part == manifest.PartIndex || p.Part == manifest.PartMeta {
			continue
		}
		f, err := os.Open(filepath.Join(pieceDir, p.PieceCID+".car"))
		if err != nil {
			return false
		}
		r, err := car.NewReader(f)
		f.Close()
		if err != nil {
			return false
		}
		if record == nil {
			return len(r.Roots) == 1
		}
		return len(r.Roots) == 2 && r.Roots[1].Equals(want)
	}
	return false
}

// reuse returns the last pack in outDir as this one's result, its inputs
// being the same.
func reuse(outDir string, prev *manifest.Manifest, cache *packCache) (*Result, error) {
	res := &Result{Manifest: prev, Missing: cache.Missing, Reused: true}
	pieceDir := filepath.Join(outDir, "pieces")
	for _, p := range prev.Pieces {
		res.Pieces = append(res.Pieces, Piece{Piece: p, Path: filepath.Join(pieceDir, p.PieceCID+".car")})
	}
	var err error
	if res.Removed, res.RemovedBytes, err = prune(pieceDir, prev); err != nil {
		return nil, fmt.Errorf("removing old pieces: %w", err)
	}
	return res, nil
}

// prune deletes the files in pieceDir that are not one of m's pieces: CARs
// an earlier pack wrote for pieces the archive no longer has, and the
// .piece-* files a killed run left behind. Other files are left alone.
func prune(pieceDir string, m *manifest.Manifest) (n int, bytes int64, err error) {
	keep := map[string]bool{}
	for _, p := range m.Pieces {
		keep[p.PieceCID+".car"] = true
	}
	ents, err := os.ReadDir(pieceDir)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range ents {
		name := e.Name()
		if keep[name] || !e.Type().IsRegular() || !strings.HasSuffix(name, ".car") && !strings.HasPrefix(name, ".piece-") {
			continue
		}
		var size int64
		if fi, err := e.Info(); err == nil {
			size = fi.Size()
		}
		if err := os.Remove(filepath.Join(pieceDir, name)); err != nil && !os.IsNotExist(err) {
			return n, bytes, err
		}
		n, bytes = n+1, bytes+size
	}
	return n, bytes, nil
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

// rootEntry describes rt. owned marks the blocks earlier roots hold; rt's
// other blocks count as its Own and are marked.
// addOwn adds one of the archive's own pieces to m, in the snapshot that
// first stored it.
func addOwn(m *manifest.Manifest, known map[string]int, snap int, sum car.Summary, part, path string) Piece {
	i := len(m.Pieces)
	p := manifest.Piece{Index: i, PieceCID: sum.PieceCID.String(), CARBytes: sum.Bytes, Blocks: sum.Blocks, Snapshot: snap, Part: part}
	if s, ok := known[p.PieceCID]; ok {
		p.Snapshot = s
	}
	// an unchanged re-run keeps its own pieces in its snapshot
	if p.Snapshot == snap {
		m.Snapshots[snap].NewPieces = append(m.Snapshots[snap].NewPieces, i)
	}
	m.Pieces = append(m.Pieces, p)
	return Piece{Piece: p, Path: path}
}

// ownDirs makes index/ and explorer/ for inner.
func (l *layout) ownDirs(ctx context.Context, dag memDAG, outDir string, inner *manifest.Manifest, pieces []span,
	entries []explorer.Entry) (idx, exp format.Node, err error) {
	if idx, err = l.indexDir(ctx, dag, outDir, inner, pieces); err != nil {
		return nil, nil, err
	}
	exp, err = explorerDir(ctx, dag, outDir, inner, entries, l.accts)
	return idx, exp, err
}

// accounts summarizes the roots that are Bluesky (atproto) repo commits,
// whatever repo they came from, for the explorer's Posts and Files pages,
// with what the identity documents pinned beside them say. A root that is
// not one, or whose records are not all here, is shown as far as it goes.
func (l *layout) accounts(ctx context.Context, roots []manifest.Root) []*atproto.Summary {
	get := func(c cid.Cid) ([]byte, error) { return l.get(ctx, c) }
	var out []*atproto.Summary
	for _, rt := range roots {
		c, err := cid.Decode(rt.CID)
		if err != nil || c.Type() != cid.DagCBOR || rt.Kind == manifest.KindOrphan {
			continue
		}
		if s, err := atproto.Summarize(get, c, rt.Name); err == nil {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	// DID documents and PLC logs are direct pins of JSON (raw blocks).
	for _, rt := range roots {
		c, err := cid.Decode(rt.CID)
		if err != nil || c.Type() != cid.Raw || rt.Kind != manifest.KindPinDirect {
			continue
		}
		b, err := get(c)
		if err != nil || len(b) > 8<<20 {
			continue
		}
		for _, s := range out {
			if did, handle, pds, ok := atproto.DIDDoc(b); ok && did == s.DID {
				s.DIDDoc, s.Handle, s.PDS = rt.CID, handle, pds
			} else if plcFor(b) == s.DID {
				s.PLCLog = rt.CID
			}
		}
	}
	return out
}

// plcFor returns the DID a PLC audit log is for, or "".
func plcFor(b []byte) string {
	var ops []struct {
		DID string `json:"did"`
	}
	if json.Unmarshal(b, &ops) != nil || len(ops) == 0 {
		return ""
	}
	return ops[0].DID
}

// stableBlocks lists, depth first, the blocks the archive made that do not
// depend on the manifest and are not the repo's: index/pieces (inIndex),
// the data/ folders, and explorer/ but for explorer/idx/manifest.js and
// meta.js and the folders holding them. The explorer's shards go last, in
// shards order (by path, not by file name), so a change to nearby names
// falls in one piece.
func (l *layout) stableBlocks(ctx context.Context, dag memDAG, dataRoot, idx, exp format.Node, shards []string) ([]format.Node, map[string]bool, error) {
	var out []format.Node
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
		out = append(out, n)
		for _, ln := range n.Links() {
			if err := walk(ln.Cid); err != nil {
				return err
			}
		}
		return nil
	}
	// links of a small folder, but for skip
	kids := func(n format.Node, skip ...string) []cid.Cid {
		var cs []cid.Cid
		for _, ln := range n.Links() {
			if !slices.Contains(skip, ln.Name) {
				cs = append(cs, ln.Cid)
			}
		}
		return cs
	}
	find := func(n format.Node, name string) (format.Node, error) {
		for _, ln := range n.Links() {
			if ln.Name == name {
				return dag.Get(ctx, ln.Cid)
			}
		}
		return nil, fmt.Errorf("archive: no %s in folder %s", name, n.Cid())
	}
	lists, err := find(idx, "pieces")
	if err != nil {
		return nil, nil, err
	}
	if err := walk(lists.Cid()); err != nil {
		return nil, nil, err
	}
	inIndex := map[string]bool{}
	for _, n := range out {
		inIndex[string(n.Cid().Hash())] = true
	}
	if err := walk(dataRoot.Cid()); err != nil {
		return nil, nil, err
	}
	ix, err := find(exp, "idx")
	if err != nil {
		return nil, nil, err
	}
	for _, c := range append(kids(exp, "idx"), kids(ix, "manifest.js", "meta.js", "s")...) {
		if err := walk(c); err != nil {
			return nil, nil, err
		}
	}
	sdir, err := find(ix, "s")
	if err != nil {
		return nil, nil, err
	}
	d, err := uio.NewDirectoryFromNode(dag, sdir)
	if err != nil {
		return nil, nil, err
	}
	links, err := d.Links(ctx)
	if err != nil {
		return nil, nil, err
	}
	files := map[string]cid.Cid{}
	for _, ln := range links {
		files[ln.Name] = ln.Cid
		seen[string(ln.Cid.Hash())] = true // walked below, in order
	}
	if err := walk(sdir.Cid()); err != nil {
		return nil, nil, err
	}
	for _, name := range shards {
		c, ok := files[name]
		if !ok {
			return nil, nil, fmt.Errorf("archive: no shard %s in explorer/idx/s", name)
		}
		delete(seen, string(c.Hash()))
		if err := walk(c); err != nil {
			return nil, nil, err
		}
	}
	return out, inIndex, nil
}

// Own pieces other than head pieces are cut like data pieces, but smaller:
// a change rewrites the pieces it falls in, 128 to about 140 MiB each. Each
// piece added costs about the same whatever its size (store.Estimate), while
// storing more bytes costs little, so this favours few pieces over small
// uploads.
const (
	metaMinPiece = 128 << 20
	metaCutBits  = 6
)

// cutMeta cuts ns into pieces at content-defined points.
func cutMeta(ns []format.Node, opts Options) []span {
	minSize, bits := min(int64(metaMinPiece), opts.MinPiece), min(metaCutBits, opts.CutBits)
	var out []span
	start, size := -1, int64(0)
	for i, n := range ns {
		e := car.EntrySize(n.Cid(), len(n.RawData()))
		if start < 0 || cutHere(n.Cid().Hash(), bits) && size >= minSize || size+e > opts.MaxPiece {
			if start >= 0 {
				out = append(out, span{start, i})
			}
			start, size = i, car.HeaderSize(n.Cid())
		}
		size += e
	}
	if start >= 0 {
		out = append(out, span{start, len(ns)})
	}
	return out
}

// remap carries prev's copies over to m. Piece ids are kept by piece index,
// and a new pack can put a stored piece at another index or a new piece at
// a stored one's, so each id follows its PieceCID.
func remap(prev, m *manifest.Manifest) []manifest.Copy {
	at := map[string]int{}
	for _, p := range prev.Pieces {
		at[p.PieceCID] = p.Index
	}
	var out []manifest.Copy
	for _, c := range prev.Copies {
		ids := map[int]string{}
		for _, p := range m.Pieces {
			if j, ok := at[p.PieceCID]; ok && c.PieceIDs[j] != "" {
				ids[p.Index] = c.PieceIDs[j]
			}
		}
		c.PieceIDs = ids
		out = append(out, c)
	}
	return out
}

func (l *layout) rootEntry(rt root, owned []bool) manifest.Root {
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
		if !owned[i] {
			owned[i] = true
			e.Own += int64(b.size)
		}
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
	if err := writeCARLayouts(dir, l.cars); err != nil {
		return nil, err
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
func explorerDir(ctx context.Context, dag memDAG, outDir string, m *manifest.Manifest, entries []explorer.Entry, accounts []*atproto.Summary) (format.Node, error) {
	dir := filepath.Join(outDir, "explorer")
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := explorer.Write(dir, m, entries, accounts...); err != nil {
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
