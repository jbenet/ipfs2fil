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
	// More names the other archives in a bundle piece, in header order.
	More []pieceInfo `json:"more,omitempty"`
}

// archives lists the archives a piece names: one, or a bundle's several.
func (pi pieceInfo) archives() []pieceInfo {
	if pi.Root == "" {
		return nil
	}
	first := pi
	first.More = nil
	return append([]pieceInfo{first}, pi.More...)
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
	cachePath := filepath.Join(c.Catalog, "pieces2.json")
	info := map[string]pieceInfo{}
	if b, err := os.ReadFile(cachePath); err == nil {
		json.Unmarshal(b, &info) // a bad cache is read again
	} else if b, err := os.ReadFile(filepath.Join(c.Catalog, "pieces.json")); err == nil {
		// Before bundles, a piece naming more than one archive was taken
		// for a data piece: keep only what named an archive.
		old := map[string]pieceInfo{}
		json.Unmarshal(b, &old)
		for p, pi := range old {
			if pi.Root != "" {
				info[p] = pi
			}
		}
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
		for _, pi := range info[p].archives() {
			if err := addPiece(snaps, len(sets), p, pi, secret); err != nil {
				errs = append(errs, err)
			}
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
	mine := c.ownArchives()
	for _, ss := range repos {
		slices.SortFunc(ss, func(a, b *snapshot) int { return cmp.Or(cmp.Compare(a.set, b.set), -cmpID(a.last, b.last)) })
		r := ss[0].record
		if r.Host == c.Host {
			continue // this machine's own list says what it has
		}
		dir := filepath.Join(c.Catalog, r.Host, r.Name)
		if mine.packed(r.Name, ss) {
			os.RemoveAll(dir) // this machine's, from before its host name changed
			continue
		}
		var err error
		for _, sn := range ss {
			if err = c.cacheSnapshot(ctx, s, sets, held, where, sn, dir); err == nil {
				break
			}
		}
		if err == nil && mine.sameData(r.Name, dir) {
			os.RemoveAll(dir)
			continue
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

// ownArchives is what this machine's archives were, by repo name, to tell
// its own records under an earlier host name from another machine's.
type ownArchives map[string]ownArchive

type ownArchive struct {
	roots    map[string]bool // the archive root and every snapshot's
	dataRoot string
}

func (c *Collection) ownArchives() ownArchives {
	out := ownArchives{}
	for _, r := range c.Repos {
		m, err := manifest.Read(filepath.Join(c.OutDir(r), archive.ManifestFile))
		if err != nil {
			continue
		}
		a := ownArchive{roots: map[string]bool{m.Root: true}, dataRoot: m.DataRoot}
		for _, sn := range m.Snapshots {
			a.roots[sn.Root] = true
		}
		out[r.Name] = a
	}
	return out
}

// packed says whether one of these snapshots is an archive this machine
// packed of the repo name.
func (o ownArchives) packed(name string, ss []*snapshot) bool {
	a, ok := o[name]
	if !ok {
		return false
	}
	for _, sn := range ss {
		if a.roots[sn.root] {
			return true
		}
	}
	return false
}

// sameData says whether the archive cached in dir holds the same data as
// this machine's archive of the repo name.
func (o ownArchives) sameData(name, dir string) bool {
	a, ok := o[name]
	if !ok || a.dataRoot == "" {
		return false
	}
	m, err := manifest.Read(filepath.Join(dir, archive.ManifestFile))
	return err == nil && m.DataRoot == a.dataRoot
}

// addPiece adds piece p, which names the archive pi, to the archive's
// snapshot.
func addPiece(snaps map[string]*snapshot, sets int, p string, pi pieceInfo, secret []byte) error {
	sn := snaps[pi.Root]
	if sn == nil {
		sn = &snapshot{root: pi.Root, set: sets}
		snaps[pi.Root] = sn
	}
	sn.pieces = append(sn.pieces, p)
	if pi.Record != nil {
		r, err := OpenRecord(secret, pi.Record)
		if err != nil {
			return fmt.Errorf("archive %s: %w", pi.Root, err)
		}
		sn.record = r
	}
	return nil
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
	// An archive's own piece names its root and record; a bundle names
	// several in turn, and its records come first.
	n := len(r.Roots)
	if n < 2 || n%2 != 0 {
		return pi, nil // a data piece, or packed without a record
	}
	for i := 1; i < n; i += 2 {
		if r.Roots[i].Prefix().Codec != cid.Raw {
			return pi, nil
		}
	}
	as := make([]pieceInfo, n/2)
	reading := true
	for k := range as {
		as[k].Root = r.Roots[2*k].String()
		if !reading {
			continue
		}
		c, data, err := r.Next()
		if err != nil {
			return pi, err
		}
		if !c.Equals(r.Roots[2*k+1]) {
			reading = false // a later own piece of an archive: no record
			continue
		}
		if got, err := archive.RecordCID(data); err != nil || !got.Equals(c) {
			return pi, fmt.Errorf("the record block does not match its hash")
		}
		as[k].Record = data
	}
	pi = as[0]
	pi.More = as[1:]
	if len(pi.More) == 0 {
		pi.More = nil
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
		// The block lists are in the snapshot's own pieces, or (archives
		// with parts) in its index pieces, which the manifest names.
		fetch := func(pieceCID string) error {
			i, ok := where[pieceCID]
			if !ok {
				return fmt.Errorf("index piece %s is not stored", pieceCID)
			}
			if _, _, err := readPiece(ctx, s, sets[i], pieceCID, dag); err != nil {
				return fmt.Errorf("piece %s: %w", pieceCID, err)
			}
			return nil
		}
		var err error
		if m, err = readIndex(ctx, dag, sn.root, dir, fetch); err != nil {
			return err
		}
		head := ""
		for _, p := range m.Pieces {
			if p.Part != "" {
				head = manifest.PartHead
			}
		}
		snap := len(m.Snapshots) - 1
		if snap < 0 {
			return errors.New("its manifest has no snapshots")
		}
		m.Root, m.Collection = sn.root, c.Key
		m.Snapshots[snap].Root = sn.root
		// A bundle holding a data piece is in the manifest inside the
		// archive, without its PieceCID, which it cannot know: it is the
		// snapshot's own piece.
		for i := range m.Pieces {
			if p := &m.Pieces[i]; p.PieceCID == "" {
				if len(own) != 1 {
					return fmt.Errorf("its manifest names a bundle, and it has %d pieces of its own, not 1", len(own))
				}
				p.PieceCID, p.CARBytes, p.Snapshot = own[0].PieceCID, own[0].CARBytes, snap
				own = nil
			}
		}
		for _, p := range own {
			p.Index, p.Snapshot, p.Part = len(m.Pieces), snap, head
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
// dag, and writes the lists into dir/index. fetch adds a piece's blocks to
// dag; it is called for the manifest's index pieces.
func readIndex(ctx context.Context, dag mapDAG, root, dir string, fetch func(pieceCID string) error) (*manifest.Manifest, error) {
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
	for _, p := range m.Pieces {
		if p.Part == manifest.PartIndex {
			if err := fetch(p.PieceCID); err != nil {
				return nil, err
			}
		}
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
