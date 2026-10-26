package collection

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/ipfs/boxo/ipld/merkledag"
	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/car"
	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/store"
)

// The default catalog is the collection's data sets themselves. Every
// archive's own pieces (index/, explorer/, the root) start with a sealed
// Record and name it in their CAR header, so the catalog is read from chain
// and the providers alone:
//
//  1. list the data sets holding the collection key, and their pieces;
//  2. read the first bytes of each piece not seen before: an archive's own
//     piece names the archive root and its record;
//  3. per repo, take the snapshot added last, download its own pieces and
//     read index/manifest.json and the block lists out of them.
//
// What it finds is cached under ChainDir in the collection's directory,
// laid out like a catalog folder, so the rest of the package reads it the
// same way. Nothing is written anywhere but the pieces push stores anyway: a
// repo shows up for other machines once its pieces are stored.

// Chain is the --catalog value for the catalog read from chain, the default.
const Chain = "filecoin"

// ChainDir caches the catalog read from chain, in the collection's directory.
const ChainDir = "chain"

// OnChain reports whether the catalog is read from chain.
func (c *Collection) OnChain() bool { return c.chain }

// pieceInfo is what a piece's first bytes say. Pieces never change, so it is
// cached for good, by PieceCID.
type pieceInfo struct {
	Root   string `json:"root,omitempty"`   // the archive root, for an archive's own piece
	Record []byte `json:"record,omitempty"` // the sealed record, in an archive's first own piece
}

// snapshot is one archive root found on chain.
type snapshot struct {
	root   string
	record Record
	pieces []string // its own pieces, in piece id order
	// set and last order snapshots of a repo: the one in the earliest copy,
	// then the one added last there.
	set  int
	last string
}

// SyncChain reads the catalog from the collection's data sets into the cache,
// for copies copies. secret opens the records (Secret). Problems with single
// pieces or repos do not stop the others; they come back joined, and the
// cache keeps what was read before.
func (c *Collection) SyncChain(ctx context.Context, s store.Store, secret []byte, copies int) error {
	if !c.chain {
		return nil
	}
	ls, ok := s.(store.Lister)
	if !ok {
		return errors.New("this store cannot list its pieces, so the catalog cannot be read from chain")
	}
	sets, held, err := ls.Pieces(ctx, c.Key, copies)
	if err != nil {
		return fmt.Errorf("reading the catalog from chain: %w", err)
	}
	cachePath := filepath.Join(c.Catalog, "pieces.json")
	info := map[string]pieceInfo{}
	if b, err := os.ReadFile(cachePath); err == nil {
		json.Unmarshal(b, &info) // a bad cache is read again
	}

	// Where each piece is: the first copy holding it.
	where := map[string]int{}
	var all []string
	for i, h := range held {
		for p := range h {
			if _, ok := where[p]; !ok {
				where[p] = i
				all = append(all, p)
			}
		}
	}
	slices.Sort(all)
	var unseen []string
	for _, p := range all {
		if _, ok := info[p]; !ok {
			unseen = append(unseen, p)
		}
	}
	var errs []error
	var mu sync.Mutex
	var wg sync.WaitGroup
	todo := make(chan string)
	for range 8 {
		wg.Go(func() {
			for p := range todo {
				pi, err := peek(ctx, s, sets[where[p]], p)
				mu.Lock()
				if err != nil {
					errs = append(errs, fmt.Errorf("reading piece %s from provider %s: %w", p, sets[where[p]].Provider, err))
				} else {
					info[p] = pi
				}
				mu.Unlock()
			}
		})
	}
	for _, p := range unseen {
		todo <- p
	}
	close(todo)
	wg.Wait()
	if b, err := json.Marshal(info); err == nil {
		errs = append(errs, writeFile(cachePath, b))
	}

	// Snapshots, by root, and the repo each belongs to.
	snaps := map[string]*snapshot{}
	for _, p := range all {
		pi, ok := info[p]
		if !ok || pi.Root == "" {
			continue
		}
		sn := snaps[pi.Root]
		if sn == nil {
			sn = &snapshot{root: pi.Root, set: len(sets)}
			snaps[pi.Root] = sn
		}
		sn.pieces = append(sn.pieces, p)
		if pi.Record != nil {
			r, err := OpenRecord(secret, pi.Record)
			if err != nil {
				errs = append(errs, fmt.Errorf("archive %s: %w", pi.Root, err))
				continue
			}
			sn.record = r
		}
	}
	repos := map[string][]*snapshot{} // host/name -> snapshots, best first
	for _, sn := range snaps {
		if sn.record.Name == "" {
			continue // no record: packed without one, or its first own piece is not stored yet
		}
		for i, h := range held {
			if _, ok := h[sn.pieces[0]]; ok {
				sn.set = i
				for _, p := range sn.pieces {
					if h[p] != "" && cmpID(h[p], sn.last) > 0 {
						sn.last = h[p]
					}
				}
				break
			}
		}
		slices.SortFunc(sn.pieces, func(a, b string) int { return cmpID(held[sn.set][a], held[sn.set][b]) })
		id := sn.record.Host + "/" + sn.record.Name
		repos[id] = append(repos[id], sn)
	}

	// Each host's list, and each repo's manifest and block lists.
	lists := map[string][]Repo{}
	if old, err := os.ReadDir(c.Catalog); err == nil {
		for _, h := range old {
			if h.IsDir() && hostOK.MatchString(h.Name()) {
				lists[h.Name()] = nil // a host whose repos are gone gets an empty list
			}
		}
	}
	for _, ss := range repos {
		slices.SortFunc(ss, func(a, b *snapshot) int { return cmp.Or(cmp.Compare(a.set, b.set), -cmpID(a.last, b.last)) })
		r := ss[0].record
		if r.Host == c.Host {
			continue // this machine's own list says what it has
		}
		dir := filepath.Join(c.Catalog, r.Host, r.Name)
		var err error
		for _, sn := range ss {
			if err = c.cacheSnapshot(ctx, s, sets, held, where, sn, dir); err == nil {
				break
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("repo %s/%s: %w", r.Host, r.Name, err))
			if _, serr := os.Stat(filepath.Join(dir, archive.ManifestFile)); serr != nil {
				continue // nothing cached to fall back on
			}
		}
		lists[r.Host] = append(lists[r.Host], Repo{Name: r.Name, Added: r.Added})
	}
	for host, rs := range lists {
		if host == c.Host {
			continue
		}
		slices.SortFunc(rs, func(a, b Repo) int { return a.Added.Compare(b.Added) })
		o := &Collection{Dir: filepath.Join(c.Catalog, host), Format: 1, Key: c.Key, Repos: rs}
		if o.Repos == nil {
			o.Repos = []Repo{}
		}
		errs = append(errs, o.Save())
	}
	return errors.Join(errs...)
}

// peek reads a piece's CAR header and, for an archive's own piece, its first
// block, then stops: data pieces are up to a GiB.
func peek(ctx context.Context, s store.Store, ds store.DataSet, pieceCID string) (pieceInfo, error) {
	var pi pieceInfo
	rc, err := s.Fetch(ctx, ds, pieceCID)
	if err != nil {
		return pi, err
	}
	defer rc.Close()
	r, err := car.NewReader(io.LimitReader(rc, 4<<20))
	if err != nil {
		return pi, err
	}
	if len(r.Roots) != 2 || r.Roots[1].Prefix().Codec != cid.Raw {
		return pi, nil // a data piece, or packed without a record
	}
	pi.Root = r.Root.String()
	c, data, err := r.Next()
	if err != nil {
		return pi, err
	}
	if c.Equals(r.Roots[1]) {
		if got, err := archive.RecordCID(data); err != nil || !got.Equals(c) {
			return pi, fmt.Errorf("the record block does not match its hash")
		}
		pi.Record = data
	}
	return pi, nil
}

// cacheSnapshot downloads a snapshot's own pieces and writes its manifest and
// block lists into dir, unless dir already holds that snapshot. The manifest's
// Copies are what the data sets hold now.
func (c *Collection) cacheSnapshot(ctx context.Context, s store.Store, sets []store.DataSet, held []map[string]string,
	where map[string]int, sn *snapshot, dir string) error {
	mpath := filepath.Join(dir, archive.ManifestFile)
	var m *manifest.Manifest
	if b, err := os.ReadFile(filepath.Join(dir, "root")); err == nil && strings.TrimSpace(string(b)) == sn.root {
		m, _ = manifest.Read(mpath)
	}
	if m == nil {
		dag := mapDAG{}
		var own []manifest.Piece
		for _, p := range sn.pieces {
			n, b, err := readPiece(ctx, s, sets[where[p]], p, dag)
			if err != nil {
				return fmt.Errorf("piece %s: %w", p, err)
			}
			own = append(own, manifest.Piece{PieceCID: p, CARBytes: b, Blocks: n})
		}
		var err error
		if m, err = readIndex(ctx, dag, sn.root, dir); err != nil {
			return err
		}
		snap := len(m.Snapshots) - 1
		if snap < 0 {
			return errors.New("its manifest has no snapshots")
		}
		m.Root, m.Collection = sn.root, c.Key
		m.Snapshots[snap].Root = sn.root
		for _, p := range own {
			p.Index, p.Snapshot = len(m.Pieces), snap
			m.Pieces = append(m.Pieces, p)
		}
		if err := writeFile(filepath.Join(dir, "root"), []byte(sn.root+"\n")); err != nil {
			return err
		}
	}
	m.Copies = nil
	for i, ds := range sets {
		cp := manifest.Copy{Provider: ds.Provider, DataSetID: ds.ID, PieceIDs: map[int]string{}, Service: "chain"}
		for _, p := range m.Pieces {
			if id, ok := held[i][p.PieceCID]; ok {
				cp.PieceIDs[p.Index] = id
			}
		}
		m.Copies = append(m.Copies, cp)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(mpath, append(b, '\n'))
}

// readPiece downloads a whole piece (the store checks its PieceCID at EOF)
// and adds its blocks to dag.
func readPiece(ctx context.Context, s store.Store, ds store.DataSet, pieceCID string, dag mapDAG) (n, bytes int64, err error) {
	rc, err := s.Fetch(ctx, ds, pieceCID)
	if err != nil {
		return 0, 0, err
	}
	defer rc.Close()
	cr := &countReader{r: rc}
	r, err := car.NewReader(cr)
	if err != nil {
		return 0, 0, err
	}
	for {
		c, data, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, err
		}
		b, err := blocks.NewBlockWithCid(data, c)
		if err != nil {
			return 0, 0, err
		}
		if got, err := c.Prefix().Sum(data); err != nil || !got.Equals(c) {
			return 0, 0, fmt.Errorf("block %s does not match its hash", c)
		}
		n++
		switch c.Prefix().Codec {
		case cid.DagProtobuf:
			nd, err := merkledag.DecodeProtobufBlock(b)
			if err != nil {
				return 0, 0, err
			}
			dag[string(c.Hash())] = nd
		case cid.Raw:
			dag[string(c.Hash())] = &merkledag.RawNode{Block: b}
		}
	}
	// Drain to EOF so the store's PieceCID check runs.
	if _, err := io.Copy(io.Discard, cr); err != nil {
		return 0, 0, fmt.Errorf("PieceCID check: %w", err)
	}
	return n, cr.n, nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	return n, err
}

// readIndex reads root/index/manifest.json and the block lists it names out of
// dag, and writes the lists into dir/index.
func readIndex(ctx context.Context, dag mapDAG, root, dir string) (*manifest.Manifest, error) {
	rc, err := cid.Decode(root)
	if err != nil {
		return nil, err
	}
	file := func(path ...string) ([]byte, error) {
		n, err := dag.Get(ctx, rc)
		if err != nil {
			return nil, err
		}
		for _, name := range path {
			d, err := uio.NewDirectoryFromNode(dag, n)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if n, err = d.Find(ctx, name); err != nil {
				return nil, fmt.Errorf("%s: %w", strings.Join(path, "/"), err)
			}
		}
		r, err := uio.NewDagReader(ctx, n, dag)
		if err != nil {
			return nil, err
		}
		return io.ReadAll(r)
	}
	b, err := file("index", "manifest.json")
	if err != nil {
		return nil, fmt.Errorf("reading the archive's index: %w", err)
	}
	var m manifest.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("index/manifest.json: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "index")); err != nil {
		return nil, err
	}
	for _, p := range m.Pieces {
		if p.BlockList == "" {
			continue
		}
		parts := strings.Split(p.BlockList, "/")
		if slices.Contains(parts, "..") || filepath.IsAbs(p.BlockList) {
			return nil, fmt.Errorf("block list %q is not a path under index/", p.BlockList)
		}
		b, err := file(append([]string{"index"}, parts...)...)
		if err != nil {
			return nil, fmt.Errorf("reading block list %s: %w", p.BlockList, err)
		}
		if err := writeFile(filepath.Join(dir, "index", filepath.FromSlash(p.BlockList)), b); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

// mapDAG is a read-only DAG of the blocks read from pieces, by multihash.
type mapDAG map[string]format.Node

func (m mapDAG) Get(_ context.Context, c cid.Cid) (format.Node, error) {
	if n, ok := m[string(c.Hash())]; ok {
		return n, nil
	}
	return nil, format.ErrNotFound{Cid: c}
}

func (m mapDAG) GetMany(ctx context.Context, cs []cid.Cid) <-chan *format.NodeOption {
	ch := make(chan *format.NodeOption, len(cs))
	for _, c := range cs {
		n, err := m.Get(ctx, c)
		ch <- &format.NodeOption{Node: n, Err: err}
	}
	close(ch)
	return ch
}

func (m mapDAG) Add(context.Context, format.Node) error       { return errors.New("read only") }
func (m mapDAG) AddMany(context.Context, []format.Node) error { return errors.New("read only") }
func (m mapDAG) Remove(context.Context, cid.Cid) error        { return errors.New("read only") }
func (m mapDAG) RemoveMany(context.Context, []cid.Cid) error  { return errors.New("read only") }

// cmpID orders decimal ids numerically; "" sorts first.
func cmpID(a, b string) int {
	return cmp.Or(cmp.Compare(len(a), len(b)), cmp.Compare(a, b))
}
