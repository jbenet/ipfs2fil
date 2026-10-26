package archive

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/repo"
)

// CacheFile, in outDir, remembers what the last pack read and wrote, so a
// pack after a failed or stopped run skips work whose inputs did not change:
//
//   - If the repo lists the same blocks with the same sizes, has the same pins
//     and MFS root, and every other input is the same (options, tool, record,
//     shared blocks), the archive would come out the same, so Build returns
//     the last one without reading a block.
//   - Otherwise each data piece whose CAR header and blocks match a piece
//     already on disk keeps that CAR, without reading, hashing or writing
//     its blocks again.
//
// Blocks are addressed by their hashes, so the same CIDs mean the same bytes.
// Deleting the file only makes the next pack do everything again.
const CacheFile = "pack-cache.json"

// cacheFormat is bumped whenever Build would lay out the same inputs
// differently, so a cache an older ipfs2fil wrote is not trusted.
const cacheFormat = 1

type packCache struct {
	Format int `json:"format"`
	// Inputs hashes everything the last complete pack depended on, and Root
	// is its archive root; they are empty while a pack is under way.
	Inputs  string   `json:"inputs,omitempty"`
	Root    string   `json:"root,omitempty"`
	Missing []string `json:"missing,omitempty"`
	// Pieces maps pieceKey of each data piece written to its CAR.
	Pieces map[string]cachedPiece `json:"pieces"`
}

type cachedPiece struct {
	PieceCID string `json:"pieceCid"`
	CARBytes int64  `json:"carBytes"`
}

func readCache(outDir string) *packCache {
	c := &packCache{Format: cacheFormat, Pieces: map[string]cachedPiece{}}
	b, err := os.ReadFile(filepath.Join(outDir, CacheFile))
	if err != nil {
		return c
	}
	var got packCache
	if json.Unmarshal(b, &got) != nil || got.Format != cacheFormat {
		return c
	}
	if got.Pieces == nil {
		got.Pieces = map[string]cachedPiece{}
	}
	return &got
}

func (c *packCache) write(outDir string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(outDir, "."+CacheFile+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(outDir, CacheFile))
}

// piece returns the CAR on disk for key, if it is still there whole.
func (c *packCache) piece(pieceDir, key string) (cachedPiece, string, bool) {
	p, ok := c.Pieces[key]
	if !ok {
		return p, "", false
	}
	path := filepath.Join(pieceDir, p.PieceCID+".car")
	fi, err := os.Stat(path)
	return p, path, err == nil && fi.Mode().IsRegular() && fi.Size() == p.CARBytes
}

// reusable reports whether the last pack in outDir is complete on disk:
// its manifest, every piece's CAR, index/ and explorer/.
func reusable(outDir string, prev *manifest.Manifest) bool {
	for _, p := range prev.Pieces {
		fi, err := os.Stat(filepath.Join(outDir, "pieces", p.PieceCID+".car"))
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != p.CARBytes {
			return false
		}
	}
	for _, p := range []string{"index/manifest.json", "explorer"} {
		if _, err := os.Stat(filepath.Join(outDir, p)); err != nil {
			return false
		}
	}
	return true
}

// hasher hashes length-prefixed fields, so no two inputs run together.
type hasher struct{ hash.Hash }

func newHasher() hasher { return hasher{sha256.New()} }

func (h hasher) bytes(b []byte) {
	h.int(int64(len(b)))
	h.Write(b)
}

func (h hasher) str(s string) { h.bytes([]byte(s)) }

func (h hasher) int(v int64) {
	var n [binary.MaxVarintLen64]byte
	h.Write(n[:binary.PutVarint(n[:], v)])
}

func (h hasher) sum() string { return hex.EncodeToString(h.Sum(nil)) }

// listed is one block of the repo's listing.
type listed struct {
	h    multihash.Multihash
	size int
}

// list reads the repo's block listing once, deduplicated: repos 5 to 11 can
// list one multihash twice, stored under two CIDs.
func (l *layout) list(ctx context.Context) error {
	seen := map[string]bool{}
	err := l.r.ForEachBlock(ctx, func(h multihash.Multihash, size int) error {
		if seen[string(h)] {
			return nil
		}
		seen[string(h)] = true
		l.repoBlocks++
		l.repoBytes += int64(size)
		l.all = append(l.all, listed{h, size})
		return nil
	})
	if err != nil {
		return fmt.Errorf("listing blocks: %w", err)
	}
	return nil
}

// inputs hashes everything Build's output depends on, short of the blocks'
// bytes, which their hashes stand for: the listing, the pins, the MFS root,
// the options, and which blocks other archives would hold for this one.
func (l *layout) inputs(ctx context.Context, outDir string, prev *manifest.Manifest, opts Options) (string, error) {
	h := newHasher()
	h.int(cacheFormat)
	h.str(opts.Tool)
	h.int(opts.MinPiece)
	h.int(opts.MaxPiece)
	h.int(int64(opts.CutBits))
	h.str(fmt.Sprint(opts.Orphans, opts.Shared != nil))
	h.bytes(opts.Record)

	pins, err := l.r.Pins(ctx)
	if err != nil {
		return "", fmt.Errorf("reading pins: %w", err)
	}
	pins = slices.Clone(pins)
	slices.SortFunc(pins, func(a, b repo.Pin) int {
		return cmp.Or(cmp.Compare(a.Mode, b.Mode), strings.Compare(a.Root.KeyString(), b.Root.KeyString()), strings.Compare(a.Name, b.Name))
	})
	h.int(int64(len(pins)))
	for _, p := range pins {
		h.int(int64(p.Mode))
		h.bytes(p.Root.Bytes())
		h.str(p.Name)
	}
	mfs, ok, err := l.r.MFSRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("reading the MFS root: %w", err)
	}
	if ok {
		h.bytes(mfs.Bytes())
	} else {
		h.bytes(nil)
	}

	var own map[string]bool
	if opts.Shared != nil {
		if own, err = ownBlocks(outDir, prev); err != nil {
			return "", err
		}
	}
	sorted := slices.Clone(l.all)
	slices.SortFunc(sorted, func(a, b listed) int { return strings.Compare(string(a.h), string(b.h)) })
	h.int(int64(len(sorted)))
	for _, b := range sorted {
		h.bytes(b.h)
		h.int(int64(b.size))
		if opts.Shared == nil || own[string(b.h)] {
			h.bytes(nil)
			continue
		}
		if sb, ok := opts.Shared(b.h); ok {
			h.str(sb.Archive + " " + sb.PieceCID)
		} else {
			h.bytes(nil)
		}
	}
	return h.sum(), nil
}

// pieceKey hashes what a data piece's CAR is made of: the header's root
// and the CID and size of each block it holds.
func (l *layout) pieceKey(p span) string {
	h := newHasher()
	h.int(cacheFormat)
	h.bytes(l.tab[p.start].c.Bytes())
	for j := p.start; j < p.end; j++ {
		if b := &l.tab[j]; b.shared == 0 {
			h.bytes(b.c.Bytes())
			h.int(int64(b.size))
		}
	}
	return h.sum()
}
