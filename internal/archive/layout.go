package archive

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/car"
	"github.com/jbenet/ipfs2fil/internal/explorer"
	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/repo"
)

// blk is one repo block in layout order.
type blk struct {
	c     cid.Cid
	size  int
	kids  []int32
	start bool // a root starts here
	piece int32
	mark  uint32
	ufs   ufsInfo
}

type root struct {
	c    cid.Cid
	kind string
	name string
}

// layout places every block reachable from the roots, depth first, once.
type layout struct {
	r          repo.Repo
	tab        []blk
	idx        map[string]int32 // multihash -> position in tab
	missing    []string
	gen        uint32
	repoBlocks int64
	repoBytes  int64
}

func newLayout(r repo.Repo) *layout {
	return &layout{r: r, idx: map[string]int32{}}
}

// get reads a block and checks its hash.
func (l *layout) get(ctx context.Context, c cid.Cid) ([]byte, error) {
	data, err := l.r.Get(ctx, c.Hash())
	if err != nil {
		return nil, err
	}
	ok, err := hashOK(c, data)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("block %s does not match its hash: the repo is damaged; `ipfs repo verify` lists the bad blocks", c)
	}
	return data, nil
}

func isIdentity(c cid.Cid) bool {
	return c.Prefix().MhType == multihash.IDENTITY
}

// place lays out c and everything below it that is not placed yet. It returns
// c's position, or -1 if the block has no place of its own (an identity CID,
// or a block the repo lacks).
func (l *layout) place(ctx context.Context, c cid.Cid) (int32, error) {
	if isIdentity(c) {
		return -1, nil
	}
	if i, ok := l.idx[string(c.Hash())]; ok {
		return i, nil
	}
	data, err := l.r.Get(ctx, c.Hash())
	if errors.Is(err, repo.ErrNotFound) {
		l.missing = append(l.missing, c.String())
		return -1, nil
	}
	if err != nil {
		return -1, err
	}
	i := int32(len(l.tab))
	l.idx[string(c.Hash())] = i
	l.tab = append(l.tab, blk{c: c, size: len(data)})
	ls, err := links(c, data)
	if err != nil {
		return -1, fmt.Errorf("block %s: %w", c, err)
	}
	ufs := unixfsInfo(c, data)
	if ufs.names != nil && len(ufs.names) != len(ls) {
		ufs = ufsInfo{}
	}
	var kids []int32
	var names []string
	for j, lc := range ls {
		k, err := l.place(ctx, lc)
		if err != nil {
			return -1, err
		}
		if k >= 0 {
			kids = append(kids, k)
			if ufs.names != nil {
				names = append(names, ufs.names[j])
			}
		}
	}
	if ufs.names != nil {
		ufs.names = names
	}
	l.tab[i].kids, l.tab[i].ufs = kids, ufs
	return i, nil
}

// walkRoots lays out pins, the MFS root and (if asked) orphans, and returns
// the roots that are in the repo, in layout order.
func (l *layout) walkRoots(ctx context.Context, orphans bool) ([]root, error) {
	pins, err := l.r.Pins(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading pins: %w", err)
	}
	var rec, dir []root
	for _, p := range pins {
		rt := root{c: p.Root, kind: manifest.KindPinRecursive, name: p.Name}
		if p.Mode == repo.Direct {
			rt.kind = manifest.KindPinDirect
			dir = append(dir, rt)
		} else {
			rec = append(rec, rt)
		}
	}
	byCID := func(rs []root) {
		sort.Slice(rs, func(i, j int) bool { return rs[i].c.String() < rs[j].c.String() })
	}
	byCID(rec)
	byCID(dir)
	var out []root
	add := func(rt root, recurse bool) error {
		if isIdentity(rt.c) {
			return nil
		}
		k := string(rt.c.Hash())
		if _, placed := l.idx[k]; !placed {
			if !recurse {
				data, err := l.r.Get(ctx, rt.c.Hash())
				if errors.Is(err, repo.ErrNotFound) {
					l.missing = append(l.missing, rt.c.String())
					return nil
				} else if err != nil {
					return err
				}
				l.idx[k] = int32(len(l.tab))
				l.tab = append(l.tab, blk{c: rt.c, size: len(data)})
			} else if _, err := l.place(ctx, rt.c); err != nil {
				return err
			}
			if i, ok := l.idx[k]; ok {
				l.tab[i].start = true
			}
		}
		if _, ok := l.idx[k]; ok {
			out = append(out, rt)
		}
		return nil
	}
	// Direct pins go after everything walked in full, so a block both
	// direct-pinned and reached from another root is laid out with its children.
	for _, rt := range rec {
		if err := add(rt, true); err != nil {
			return nil, err
		}
	}
	if c, ok, err := l.r.MFSRoot(ctx); err != nil {
		return nil, fmt.Errorf("reading the MFS root: %w", err)
	} else if ok {
		if err := add(root{c: c, kind: manifest.KindMFS}, true); err != nil {
			return nil, err
		}
	}
	for _, rt := range dir {
		if err := add(rt, false); err != nil {
			return nil, err
		}
	}

	// Orphans: blocks nothing above reaches. One orphan linked by another is
	// not a root of its own.
	var loose []multihash.Multihash
	// Repos 5 to 11 can list one multihash twice, stored under two CIDs.
	listed := map[string]bool{}
	err = l.r.ForEachBlock(ctx, func(h multihash.Multihash, size int) error {
		if listed[string(h)] {
			return nil
		}
		listed[string(h)] = true
		l.repoBlocks++
		l.repoBytes += int64(size)
		if _, ok := l.idx[string(h)]; !ok && orphans {
			loose = append(loose, h)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing blocks: %w", err)
	}
	guessed := make([]cid.Cid, len(loose))
	linked := map[string]bool{}
	for i, h := range loose {
		data, err := l.r.Get(ctx, h)
		if err != nil {
			return nil, err
		}
		guessed[i] = guess(h, data)
		ls, _ := links(guessed[i], data)
		for _, c := range ls {
			linked[string(c.Hash())] = true
		}
	}
	var orph []root
	for _, c := range guessed {
		if !linked[string(c.Hash())] {
			orph = append(orph, root{c: c, kind: manifest.KindOrphan})
		}
	}
	byCID(orph)
	for _, rt := range orph {
		if err := add(rt, true); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type span struct{ start, end int }

// cutHere reports whether h's digest ends in bits zero bits.
func cutHere(h multihash.Multihash, bits int) bool {
	d, err := multihash.Decode(h)
	if err != nil || len(d.Digest)*8 < bits || bits == 0 {
		return bits == 0
	}
	for i := len(d.Digest) - 1; bits > 0; i-- {
		b := d.Digest[i]
		if bits < 8 {
			b &= 1<<bits - 1
		}
		if b != 0 {
			return false
		}
		bits -= 8
	}
	return true
}

// cut splits the layout into data pieces and marks each block's piece.
func (l *layout) cut(opts Options) ([]span, error) {
	var out []span
	start, size := 0, int64(0)
	for i := range l.tab {
		b := &l.tab[i]
		e := car.EntrySize(b.c, b.size)
		if i > start {
			if (b.start || cutHere(b.c.Hash(), opts.CutBits)) && size >= opts.MinPiece || size+e > opts.MaxPiece {
				out = append(out, span{start, i})
				start = i
			}
		}
		if i == start {
			size = car.HeaderSize(b.c)
		}
		size += e
		if size > opts.MaxPiece {
			return nil, fmt.Errorf("block %s (%d bytes) does not fit in a piece of %d bytes", b.c, b.size, opts.MaxPiece)
		}
		b.piece = int32(len(out))
	}
	if start < len(l.tab) {
		out = append(out, span{start, len(l.tab)})
	}
	return out, nil
}

// entries lists every UnixFS name under each root for the explorer: the root
// itself, then every file and directory below it, depth first.
func (l *layout) entries(roots []root, m *manifest.Manifest) []explorer.Entry {
	var out []explorer.Entry
	var walk func(i int32, p, name string) int64
	walk = func(i int32, p, name string) int64 {
		b := &l.tab[i]
		at := len(out)
		isDir := b.ufs.kind == ufsDir || b.ufs.kind == ufsShard
		out = append(out, explorer.Entry{Name: name, Path: p, CID: b.c.String(), Kind: explorer.KindOf(name, isDir),
			Bytes: b.ufs.size, Piece: int(b.piece)})
		if !isDir {
			if b.ufs.kind == ufsNone {
				out[at].Bytes = int64(b.size)
			}
			return out[at].Bytes
		}
		var total int64
		var kids func(i int32)
		kids = func(i int32) {
			d := &l.tab[i]
			for j, k := range d.kids {
				if j >= len(d.ufs.names) {
					break
				}
				if n := d.ufs.names[j]; n == "" && d.ufs.kind == ufsShard {
					kids(k) // a sub-shard: its links are this directory's too
				} else {
					total += walk(k, p+"/"+n, n)
				}
			}
		}
		kids(i)
		out[at].Bytes = total
		return total
	}
	for n, rt := range roots {
		i, ok := l.idx[string(rt.c.Hash())]
		if !ok {
			continue
		}
		p := m.Roots[n].Path
		name := rt.name
		if name == "" {
			name = path.Base(p)
		}
		if rt.kind == manifest.KindPinDirect {
			// a direct pin holds only its own block
			b := &l.tab[i]
			out = append(out, explorer.Entry{Name: name, Path: p, CID: rt.c.String(), Kind: explorer.KindOf(name, false),
				Bytes: int64(b.size), Piece: int(b.piece)})
			continue
		}
		walk(i, p, name)
	}
	return out
}
