package archive

import (
	"bufio"
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
	own := map[string]bool{}
	if prev != nil {
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
