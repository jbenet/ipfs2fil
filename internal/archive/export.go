package archive

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	mdag "github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/boxo/ipld/unixfs"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/atproto"
	"github.com/jbenet/ipfs2fil/internal/car"
	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/repo"
)

// ExportOptions say where Export finds the pieces an archive's blocks are in.
type ExportOptions struct {
	// Dirs are other archives' directories: blocks shared between repos sit
	// in the pieces of the archive that stored them first.
	Dirs []string
	// Fetch, if set, reads a piece that no directory has on disk, such as
	// from a provider. The pieces it reads are deleted after the export.
	Fetch func(ctx context.Context, pieceCID string) (io.ReadCloser, error)
	// Progress, if set, hears the export go: stage "fetch" counts pieces
	// read back, "write" counts blocks written against the blocks to write.
	Progress func(stage string, done, total int64)
}

// ExportFile is one file Export wrote.
type ExportFile struct {
	Path   string `json:"path"` // relative to the export directory
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Same says whether the file is byte for byte the one the repo had, for
	// files whose original the archive recorded (CAR files); nil otherwise.
	Same *bool `json:"same,omitempty"`
}

// ExportPin is a pin as the export lists it in pins.json.
type ExportPin struct {
	CID  string `json:"cid"`
	Mode string `json:"mode"` // "recursive" or "direct"
	Name string `json:"name,omitempty"`
}

// ExportResult says what Export wrote; it is also written as export.json.
type ExportResult struct {
	Archive string `json:"archive"` // the archive root
	Kind    string `json:"kind"`    // the repo kind, as repo.Kind names it
	Blocks  int64  `json:"blocks"`
	Bytes   int64  `json:"bytes"`
	// Missing lists blocks the archive's roots link to that no piece holds:
	// blocks the repo never had when it was packed.
	Missing []string     `json:"missing,omitempty"`
	Pins    []ExportPin  `json:"pins,omitempty"`
	MFS     string       `json:"mfs,omitempty"`
	Files   []ExportFile `json:"files"`
}

// ExportFile names in the export directory.
const (
	ExportBlocks   = "blocks.car"
	ExportPins     = "pins.json"
	ExportManifest = "export.json"
	ExportImport   = "import-to-kubo.sh"
)

// Export writes the repo an archive came from into dir, from the archive's
// pieces alone: the archive in archiveDir must have its index/ and
// manifest.json, as pack writes them.
//
// A repo of CAR files (a CAR repo, a Bluesky account's repo.car) comes back
// as the very files, byte for byte, when the archive recorded their layout
// (index/cars, from this version on). A Bluesky account comes back as the
// directory ipfs2fil fetched: repo.car, blobs/ and the identity documents.
// Every other repo comes back as blocks.car with every block, pins.json with
// its pins and export.json with its MFS root, plus a script that loads them
// into a kubo repo.
func Export(ctx context.Context, archiveDir, dir string, opts ExportOptions) (*ExportResult, error) {
	if opts.Progress == nil {
		opts.Progress = func(string, int64, int64) {}
	}
	m, err := manifest.Read(filepath.Join(archiveDir, ManifestFile))
	if err != nil {
		return nil, err
	}
	layouts, err := readCARLayouts(filepath.Join(archiveDir, "index"))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	src, err := openPieces(ctx, m, append([]string{archiveDir}, opts.Dirs...), dir, opts)
	if err != nil {
		return nil, err
	}
	defer src.Close()

	kind := m.Source.Kind
	if kind == "" {
		kind = repo.KindFSRepo
	}
	res := &ExportResult{Archive: m.Root, Kind: kind}
	order, err := src.walk(ctx, m.Roots, res)
	if err != nil {
		return nil, err
	}
	w := &exporter{ctx: ctx, src: src, dir: dir, res: res, progress: opts.Progress, total: int64(len(order))}

	covered := map[string]bool{}
	if len(layouts) > 0 && (kind == repo.KindCAR || kind == repo.KindATProto) {
		for _, l := range layouts {
			if err := w.writeCAR(l); err != nil {
				return nil, err
			}
			for _, c := range l.Sections {
				covered[string(c.Hash())] = true
			}
		}
	}
	switch {
	case kind == repo.KindATProto && len(layouts) > 0:
		err = w.writeAccount(m, order, covered)
	case kind == repo.KindCAR && len(layouts) > 0:
		// Every block is in the files; nothing else to write.
	default:
		err = w.writeBlocks(m, order)
	}
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, ExportManifest), append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	return res, nil
}

// pieceSource reads blocks from piece CARs.
type pieceSource struct {
	cars  []repo.Repo
	tmp   string // fetched pieces, deleted on Close
	known map[string]int
}

func (s *pieceSource) Close() error {
	for _, r := range s.cars {
		r.Close()
	}
	if s.tmp != "" {
		return os.RemoveAll(s.tmp)
	}
	return nil
}

// openPieces opens every piece the archive's repo blocks are in: its data
// pieces and the pieces of other archives it shares blocks with.
func openPieces(ctx context.Context, m *manifest.Manifest, dirs []string, out string, opts ExportOptions) (*pieceSource, error) {
	var need []string
	for _, p := range m.Pieces {
		if p.BlockList != "" {
			need = append(need, p.PieceCID)
		}
	}
	if m.Shared != nil {
		for _, p := range m.Shared.Pieces {
			need = append(need, p.PieceCID)
		}
	}
	s := &pieceSource{known: map[string]int{}}
	var lost []string
	fetched := int64(0)
	for _, pc := range need {
		path := ""
		for _, d := range dirs {
			p := filepath.Join(d, "pieces", pc+".car")
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
		if path == "" && opts.Fetch != nil {
			if s.tmp == "" {
				t, err := os.MkdirTemp(out, ".pieces-")
				if err != nil {
					s.Close()
					return nil, err
				}
				s.tmp = t
			}
			p, err := fetchPiece(ctx, opts.Fetch, pc, s.tmp)
			if err != nil {
				s.Close()
				return nil, fmt.Errorf("reading piece %s back: %w", pc, err)
			}
			path = p
			fetched++
			opts.Progress("fetch", fetched, 0)
		}
		if path == "" {
			lost = append(lost, pc)
			continue
		}
		r, err := repo.Open(path)
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("piece %s: %w", pc, err)
		}
		s.cars = append(s.cars, r)
	}
	if len(lost) > 0 {
		s.Close()
		return nil, fmt.Errorf("%d pieces are not on disk: %s (export from the pieces stored on Filecoin instead)", len(lost), strings.Join(lost, ", "))
	}
	return s, nil
}

func fetchPiece(ctx context.Context, fetch func(context.Context, string) (io.ReadCloser, error), pieceCID, dir string) (string, error) {
	rc, err := fetch(ctx, pieceCID)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	path := filepath.Join(dir, pieceCID+".car")
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		return "", err
	}
	return path, f.Close()
}

// get reads a block by multihash from whichever piece holds it.
func (s *pieceSource) get(ctx context.Context, h multihash.Multihash) ([]byte, error) {
	if i, ok := s.known[string(h)]; ok {
		return s.cars[i].Get(ctx, h)
	}
	for i, r := range s.cars {
		data, err := r.Get(ctx, h)
		if errors.Is(err, repo.ErrNotFound) {
			continue
		}
		if err == nil {
			s.known[string(h)] = i
		}
		return data, err
	}
	return nil, repo.ErrNotFound
}

// getChecked reads a block and checks it hashes to c.
func (s *pieceSource) getChecked(ctx context.Context, c cid.Cid) ([]byte, error) {
	data, err := s.get(ctx, c.Hash())
	if err != nil {
		return nil, err
	}
	ok, err := hashOK(c, data)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("block %s does not match its hash: a piece is damaged", c)
	}
	return data, nil
}

// walk lists every block the roots reach, depth first in root order, once
// each, under the CID that first links it.
func (s *pieceSource) walk(ctx context.Context, roots []manifest.Root, res *ExportResult) ([]cid.Cid, error) {
	seen := map[string]bool{}
	var order []cid.Cid
	for _, rt := range roots {
		c, err := cid.Decode(rt.CID)
		if err != nil {
			return nil, fmt.Errorf("root %q: %w", rt.CID, err)
		}
		stack := []cid.Cid{c}
		for len(stack) > 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			c := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if isIdentity(c) || seen[string(c.Hash())] {
				continue
			}
			seen[string(c.Hash())] = true
			data, err := s.getChecked(ctx, c)
			if errors.Is(err, repo.ErrNotFound) {
				res.Missing = append(res.Missing, c.String())
				continue
			}
			if err != nil {
				return nil, err
			}
			order = append(order, c)
			res.Blocks++
			res.Bytes += int64(len(data))
			kids, err := links(c, data)
			if err != nil {
				continue // a block that does not parse links nothing, as in pack
			}
			for i := len(kids) - 1; i >= 0; i-- {
				stack = append(stack, kids[i])
			}
		}
	}
	return order, nil
}

type exporter struct {
	ctx      context.Context
	src      *pieceSource
	dir      string
	res      *ExportResult
	progress func(string, int64, int64)
	done     int64
	total    int64
}

// create makes a file under the export directory, refusing paths that
// would leave it.
func (w *exporter) create(name string) (*os.File, error) {
	p := filepath.Join(w.dir, filepath.FromSlash(name))
	if rel, err := filepath.Rel(w.dir, p); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("refusing to write %q outside %s", name, w.dir)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	return os.Create(p)
}

// hashedFile counts and hashes what is written to a file.
type hashedFile struct {
	f *os.File
	h hash.Hash
	n int64
}

func (w *exporter) open(name string) (*hashedFile, error) {
	f, err := w.create(name)
	if err != nil {
		return nil, err
	}
	return &hashedFile{f: f, h: sha256.New()}, nil
}

func (h *hashedFile) Write(p []byte) (int, error) {
	n, err := h.f.Write(p)
	h.h.Write(p[:n])
	h.n += int64(n)
	return n, err
}

func (w *exporter) close(name string, h *hashedFile, want string) error {
	if err := h.f.Close(); err != nil {
		return err
	}
	f := ExportFile{Path: name, Size: h.n, SHA256: hex.EncodeToString(h.h.Sum(nil))}
	if want != "" {
		same := f.SHA256 == want
		f.Same = &same
	}
	w.res.Files = append(w.res.Files, f)
	return nil
}

func (w *exporter) tick() {
	if w.done++; w.done%256 == 0 || w.done == w.total {
		w.progress("write", w.done, w.total)
	}
}

// writeCAR writes one CAR file as the repo had it.
func (w *exporter) writeCAR(l repo.CARLayout) error {
	f, err := w.open(l.Name)
	if err != nil {
		return err
	}
	if _, err := f.Write(l.Prefix); err != nil {
		f.f.Close()
		return err
	}
	var vb [binary.MaxVarintLen64]byte
	for _, c := range l.Sections {
		data, err := w.src.getChecked(w.ctx, c)
		if err != nil {
			f.f.Close()
			return fmt.Errorf("%s: block %s: %w", l.Name, c, err)
		}
		cb := c.Bytes()
		k := binary.PutUvarint(vb[:], uint64(len(cb)+len(data)))
		for _, part := range [][]byte{vb[:k], cb, data} {
			if _, err := f.Write(part); err != nil {
				f.f.Close()
				return err
			}
		}
		w.tick()
	}
	if _, err := f.Write(l.Suffix); err != nil {
		f.f.Close()
		return err
	}
	return w.close(l.Name, f, l.SHA256)
}

// writeAccount writes what a fetched Bluesky account has beside repo.car:
// its blobs and identity documents.
func (w *exporter) writeAccount(m *manifest.Manifest, order []cid.Cid, covered map[string]bool) error {
	handle := ""
	docs := map[string]string{} // multihash -> file name
	for _, rt := range m.Roots {
		switch rt.Kind {
		case manifest.KindPinRecursive:
			if h, ok := strings.CutPrefix(rt.Name, "@"); ok && handle == "" {
				handle = h
			}
		case manifest.KindPinDirect:
			c, err := cid.Decode(rt.CID)
			if err != nil {
				return err
			}
			for _, name := range []string{atproto.DIDDocFile, atproto.AuditLogFile} {
				if strings.HasSuffix(rt.Name, " "+name) {
					docs[string(c.Hash())] = name
				}
			}
		}
	}
	var didDoc []byte
	for _, c := range order {
		if covered[string(c.Hash())] {
			continue
		}
		data, err := w.src.getChecked(w.ctx, c)
		if err != nil {
			return err
		}
		name, isDoc := docs[string(c.Hash())]
		if !isDoc {
			name = atproto.BlobDir + "/" + cid.NewCidV1(cid.Raw, c.Hash()).String()
		}
		if name == atproto.DIDDocFile {
			didDoc = data
		}
		if err := w.writeFile(name, data); err != nil {
			return err
		}
		w.tick()
	}
	if didDoc == nil {
		return nil
	}
	id, err := atproto.IdentityFromDoc(didDoc, handle)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	return w.writeFile(atproto.IdentityFile, b)
}

func (w *exporter) writeFile(name string, data []byte) error {
	f, err := w.open(name)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.f.Close()
		return err
	}
	return w.close(name, f, "")
}

// writeBlocks writes blocks.car, pins.json and the kubo import script.
func (w *exporter) writeBlocks(m *manifest.Manifest, order []cid.Cid) error {
	var heads []cid.Cid
	for i, rt := range append(append([]manifest.Root(nil), m.Roots...), m.Unstored...) {
		c, err := cid.Decode(rt.CID)
		if err != nil {
			return err
		}
		if i >= len(m.Roots) {
			// No block in the archive: pinned again, not a CAR root.
			c = cid.Undef
		}
		switch rt.Kind {
		case manifest.KindPinRecursive, manifest.KindPinDirect:
			mode := "recursive"
			if rt.Kind == manifest.KindPinDirect {
				mode = "direct"
			}
			w.res.Pins = append(w.res.Pins, ExportPin{CID: rt.CID, Mode: mode, Name: rt.Name})
		case manifest.KindMFS:
			w.res.MFS = rt.CID
		default:
			continue
		}
		if c.Defined() {
			heads = append(heads, c)
		}
	}
	f, err := w.open(ExportBlocks)
	if err != nil {
		return err
	}
	cw, err := car.NewWriter(f, heads...)
	if err != nil {
		f.f.Close()
		return err
	}
	for _, c := range order {
		data, err := w.src.getChecked(w.ctx, c)
		if err != nil {
			f.f.Close()
			return err
		}
		if err := cw.Put(c, data); err != nil {
			f.f.Close()
			return err
		}
		w.tick()
	}
	if _, err := cw.Finish(); err != nil {
		f.f.Close()
		return err
	}
	if err := w.close(ExportBlocks, f, ""); err != nil {
		return err
	}
	pins := w.res.Pins
	if pins == nil {
		pins = []ExportPin{}
	}
	b, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return err
	}
	if err := w.writeFile(ExportPins, append(b, '\n')); err != nil {
		return err
	}
	return w.writeFile(ExportImport, []byte(w.importScript()))
}

// importScript loads the export into a kubo repo: every block, the pins
// with their names, and the MFS tree's top entries.
func (w *exporter) importScript() string {
	var sb strings.Builder
	sb.WriteString("#!/bin/sh\n# Loads this export into the kubo repo at $IPFS_PATH (default ~/.ipfs).\n# Written by ipfs2fil export.\nset -e\ncd \"$(dirname \"$0\")\"\n")
	sb.WriteString("ipfs dag import --pin-roots=false " + ExportBlocks + "\n")
	for _, p := range w.res.Pins {
		flags := ""
		if p.Mode == "direct" {
			flags = " --recursive=false"
		}
		if p.Name != "" {
			flags += " --name " + shellQuote(p.Name)
		}
		sb.WriteString("ipfs pin add" + flags + " " + p.CID + " >/dev/null\n")
	}
	if w.res.MFS != "" {
		names, err := w.mfsNames()
		if err != nil {
			sb.WriteString("# The MFS root is a sharded directory; it is copied whole to /restored-mfs.\n")
			sb.WriteString("ipfs files cp /ipfs/" + w.res.MFS + " /restored-mfs\n")
		} else {
			for _, n := range names {
				sb.WriteString("ipfs files cp " + shellQuote("/ipfs/"+w.res.MFS+"/"+n) + " " + shellQuote("/"+n) + "\n")
			}
		}
	}
	return sb.String()
}

// mfsNames lists the MFS root's entries, if it is a plain directory.
func (w *exporter) mfsNames() ([]string, error) {
	c, err := cid.Decode(w.res.MFS)
	if err != nil {
		return nil, err
	}
	data, err := w.src.getChecked(w.ctx, c)
	if err != nil {
		return nil, err
	}
	n, err := mdag.DecodeProtobuf(data)
	if err != nil {
		return nil, err
	}
	fs, err := unixfs.FSNodeFromBytes(n.Data())
	if err != nil || fs.Type() != unixfs.TDirectory {
		return nil, errors.New("not a plain directory")
	}
	var names []string
	for _, l := range n.Links() {
		names = append(names, l.Name)
	}
	sort.Strings(names)
	return names, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
