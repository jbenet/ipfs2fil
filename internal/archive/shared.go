package archive

import (
	"bufio"
	"errors"
	"github.com/jbenet/ipfs2fil/internal/car"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// markShared marks the blocks other archives already hold, per lookup, and
// returns what this archive leaves to them. Blocks in prev's data pieces that
// are stored (every copy holds the piece) stay this archive's own, so re-runs
// keep the pieces already paid for; pieces not stored yet are free to shrink.
func (l *layout) markShared(outDir string, prev *manifest.Manifest, lookup func(multihash.Multihash) (SharedBlock, bool)) (*manifest.Shared, error) {
	if lookup == nil {
		return nil, nil
	}
	own, err := ownBlocks(outDir, prev)
	if err != nil {
		return nil, err
	}
	out := &manifest.Shared{}
	at := map[SharedBlock]int32{}
	counts := map[SharedBlock]int64{}
	for i := range l.tab {
		b := &l.tab[i]
		h := b.c.Hash()
		if own[string(h)] {
			continue
		}
		sb, ok := lookup(h)
		if !ok {
			continue
		}
		k, seen := at[sb]
		if !seen {
			l.shared = append(l.shared, sb)
			k = int32(len(l.shared))
			at[sb] = k
		}
		b.shared = k
		counts[sb]++
		out.Blocks++
		out.Bytes += int64(b.size)
	}
	if out.Blocks == 0 {
		return nil, nil
	}
	for _, sb := range l.shared {
		out.Pieces = append(out.Pieces, manifest.SharedPiece{PieceCID: sb.PieceCID, Archive: sb.Archive, Blocks: counts[sb]})
	}
	slices.SortFunc(out.Pieces, func(a, b manifest.SharedPiece) int { return strings.Compare(a.PieceCID, b.PieceCID) })
	return out, nil
}

// ownBlocks lists the blocks in prev's data pieces that every copy holds.
// They stay this archive's own, so re-runs keep the pieces already paid for.
func ownBlocks(outDir string, prev *manifest.Manifest) (map[string]bool, error) {
	own := map[string]bool{}
	if prev == nil {
		return own, nil
	}
	for _, p := range prev.Pieces {
		if p.BlockList == "" || !storedIn(prev, p.Index) {
			continue
		}
		err := ReadBlockList(filepath.Join(outDir, "index", p.BlockList), func(h multihash.Multihash) error {
			own[string(h)] = true
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return own, nil
}

// ReadBlockList calls fn with each multihash in an index/pieces block list.
func ReadBlockList(path string, fn func(multihash.Multihash) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		h, err := multihash.FromB58String(line)
		if err != nil {
			return err
		}
		if err := fn(h); err != nil {
			return err
		}
	}
	return sc.Err()
}

// storedIn reports whether every copy m records holds piece i.
func storedIn(m *manifest.Manifest, i int) bool {
	if len(m.Copies) == 0 {
		return false
	}
	for _, c := range m.Copies {
		if c.PieceIDs[i] == "" {
			return false
		}
	}
	return true
}

// KeepLive is the share of a stored piece's bytes that must still be in the
// repo for a new snapshot to keep using the piece. A piece mostly of blocks
// the repo no longer has is not kept: its live blocks go in new pieces.
const KeepLive = 0.25

// keptPiece is a data piece an earlier snapshot stored that this one keeps
// using for the blocks it still holds, so they are not stored again.
type keptPiece struct {
	manifest.Piece
	list []byte // its index/pieces block list, as written then
	live int64  // bytes of its blocks still in the repo
}

// markKept finds the data pieces of prev that every copy holds and marks
// the repo's blocks in them as kept there. So a new snapshot uploads only
// the blocks no stored piece has: an edit to a small repo adds a small
// piece instead of writing its one big piece again. Blocks another
// archive's piece holds (shared) were marked first and stay shared.
func (l *layout) markKept(outDir string, prev *manifest.Manifest) error {
	l.kept = nil
	if prev == nil {
		return nil
	}
	for _, p := range prev.Pieces {
		if p.BlockList == "" || !storedIn(prev, p.Index) {
			continue
		}
		list, err := os.ReadFile(filepath.Join(outDir, "index", p.BlockList))
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		kp := keptPiece{Piece: p, list: list}
		var mine []int32
		err = ReadBlockList(filepath.Join(outDir, "index", p.BlockList), func(h multihash.Multihash) error {
			i, ok := l.idx[string(h)]
			if !ok || l.tab[i].shared != 0 || l.tab[i].kept != 0 {
				return nil
			}
			kp.live += car.EntrySize(l.tab[i].c, l.tab[i].size)
			mine = append(mine, i)
			return nil
		})
		if err != nil {
			return err
		}
		// A bundle holds other blocks too: measure against this archive's.
		size := p.CARBytes
		if p.DataBytes > 0 {
			size = p.DataBytes
		}
		if len(mine) == 0 || float64(kp.live) < KeepLive*float64(size) {
			continue
		}
		l.kept = append(l.kept, kp)
		for _, i := range mine {
			l.tab[i].kept = int32(len(l.kept))
		}
	}
	return nil
}
