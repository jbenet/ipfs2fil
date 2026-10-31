package explorer

// What `ipfs2fil explore` serves besides the archive's own files: the bytes
// of any file in the archive, read from the CARs in pieces/, and the kinds of
// files an older pack could not tell from their names. From a gateway the
// pages fetch the same things from the gateway instead.
//
//	GET  _i2f/info             {"content": true, "all": "<path>"}; all is the explorer over
//	                           every archive, when explore serves several
//	GET  _i2f/content/<cid>    the file, with Range support; ?name= sets the type, ?dl=1 downloads;
//	                           a dag-cbor block (a Bluesky record) or dag-json block comes as JSON;
//	                           ?w=<px> gets a JPEG thumbnail (see thumbnails.go), or 404
//	GET  _i2f/path/<cid>/<path> the file at a path under a folder, so pages' relative links work
//	POST _i2f/thumb/<cid>?w=   keeps a thumbnail the browser made
//	POST _i2f/parts            CIDs, one per line -> {"<cid>": [have, total]} for files only partly here
//	POST _i2f/kinds            CIDs, one per line -> {"<cid>": "<kind>"} for the files among them

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	mdag "github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/boxo/ipld/unixfs"
	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/atproto"
)

// carBlocks finds blocks in the CARs under each dir's pieces/. It indexes
// them in the background on first use; a lookup waits for the index.
type carBlocks struct {
	dirs   []string
	once   sync.Once
	ready  chan struct{}
	at     map[string]blockAt // multihash -> where
	files  []*os.File
	err    error
	thumb  *thumbs
	thumbs thumbCache // on disk
	whole  sync.Map   // CID -> [2]int{have, total}, from parts
	all    string     // the combined explorer's path, if any
}

type blockAt struct {
	file int32
	n    int32
	off  int64
}

var errNoBlock = errors.New("block not in this archive's pieces")

func newCARBlocks(dirs []string) *carBlocks {
	return &carBlocks{dirs: dirs, ready: make(chan struct{}), at: map[string]blockAt{}, thumb: newThumbs(), thumbs: newThumbCache()}
}

func (s *carBlocks) start() {
	s.once.Do(func() {
		go func() {
			defer close(s.ready)
			s.err = s.index()
		}()
	})
}

func (s *carBlocks) index() error {
	var paths []string
	for _, d := range s.dirs {
		ps, err := filepath.Glob(filepath.Join(d, "pieces", "*.car"))
		if err != nil {
			return err
		}
		paths = append(paths, ps...)
	}
	sort.Strings(paths)
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue // a piece removed meanwhile; its blocks show as missing
		}
		if err := s.scan(f, int32(len(s.files))); err != nil {
			f.Close()
			continue
		}
		s.files = append(s.files, f)
	}
	return nil
}

// scan records where each block in a CARv1 sits, reading only section
// headers.
func (s *carBlocks) scan(f *os.File, file int32) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	buf := make([]byte, 160)
	read := func(off int64) []byte {
		n, _ := f.ReadAt(buf, off)
		return buf[:n]
	}
	b := read(0)
	h, vl := binary.Uvarint(b)
	if vl <= 0 || h == 0 {
		return fmt.Errorf("%s: not a CAR", f.Name())
	}
	off := int64(vl) + int64(h)
	for off < size {
		b := read(off)
		n, vl := binary.Uvarint(b)
		if vl <= 0 || n == 0 || off+int64(vl)+int64(n) > size {
			return fmt.Errorf("%s: bad section at %d", f.Name(), off)
		}
		cl, c, err := cid.CidFromBytes(b[vl:])
		if err != nil || uint64(cl) > n {
			return fmt.Errorf("%s: bad CID at %d", f.Name(), off)
		}
		if _, ok := s.at[string(c.Hash())]; !ok {
			s.at[string(c.Hash())] = blockAt{file: file, n: int32(n) - int32(cl), off: off + int64(vl) + int64(cl)}
		}
		off += int64(vl) + int64(n)
	}
	return nil
}

func (s *carBlocks) wait(ctx context.Context) error {
	s.start()
	select {
	case <-s.ready:
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// block returns c's bytes, checked against its hash.
func (s *carBlocks) block(ctx context.Context, c cid.Cid) ([]byte, error) {
	if c.Prefix().MhType == multihash.IDENTITY {
		d, err := multihash.Decode(c.Hash())
		if err != nil {
			return nil, err
		}
		return d.Digest, nil
	}
	if err := s.wait(ctx); err != nil {
		return nil, err
	}
	a, ok := s.at[string(c.Hash())]
	if !ok {
		return nil, errNoBlock
	}
	data := make([]byte, a.n)
	if _, err := s.files[a.file].ReadAt(data, a.off); err != nil {
		return nil, err
	}
	sum, err := c.Prefix().Sum(data)
	if err != nil {
		return nil, err
	}
	if string(sum.Hash()) != string(c.Hash()) {
		return nil, fmt.Errorf("block %s does not match its hash; piece %s is damaged", c, s.files[a.file].Name())
	}
	return data, nil
}

// Get and GetMany make carBlocks a format.NodeGetter for UnixFS readers.
func (s *carBlocks) Get(ctx context.Context, c cid.Cid) (format.Node, error) {
	data, err := s.block(ctx, c)
	if err != nil {
		return nil, err
	}
	switch c.Type() {
	case cid.Raw:
		return mdag.NewRawNodeWPrefix(data, c.Prefix())
	case cid.DagProtobuf:
		n, err := mdag.DecodeProtobuf(data)
		if err != nil {
			return nil, err
		}
		if err := n.SetCidBuilder(c.Prefix()); err != nil {
			return nil, err
		}
		return n, nil
	}
	return nil, fmt.Errorf("block %s is not UnixFS", c)
}

func (s *carBlocks) GetMany(ctx context.Context, cs []cid.Cid) <-chan *format.NodeOption {
	out := make(chan *format.NodeOption, len(cs))
	go func() {
		defer close(out)
		for _, c := range cs {
			n, err := s.Get(ctx, c)
			select {
			case out <- &format.NodeOption{Node: n, Err: err}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// readOnly is carBlocks as the DAG service UnixFS folders want.
type readOnly struct{ *carBlocks }

var errReadOnly = errors.New("the archive is read only")

func (readOnly) Add(context.Context, format.Node) error       { return errReadOnly }
func (readOnly) AddMany(context.Context, []format.Node) error { return errReadOnly }
func (readOnly) Remove(context.Context, cid.Cid) error        { return errReadOnly }
func (readOnly) RemoveMany(context.Context, []cid.Cid) error  { return errReadOnly }

// kind sniffs the file at c from its first block of data, or returns "".
func (s *carBlocks) kind(ctx context.Context, c cid.Cid) string {
	for range 64 {
		data, err := s.block(ctx, c)
		if err != nil {
			return ""
		}
		switch c.Type() {
		case cid.Raw:
			return Sniff(data)
		case cid.DagProtobuf:
			n, err := mdag.DecodeProtobuf(data)
			if err != nil {
				return ""
			}
			fs, err := unixfs.FSNodeFromBytes(n.Data())
			if err != nil || (fs.Type() != unixfs.TFile && fs.Type() != unixfs.TRaw) {
				return ""
			}
			if len(fs.Data()) > 0 {
				return Sniff(fs.Data())
			}
			if len(n.Links()) == 0 {
				return ""
			}
			c = n.Links()[0].Cid
		default:
			return ""
		}
	}
	return ""
}

// parts counts the blocks of the file at c, n its root, and how many of them
// the pieces hold, so a file a repo only partly had (an orphan whose root
// was fetched but not the rest, say) is refused up front instead of failing
// after its headers are sent. Only nodes that must be inner ones (those over
// 1 MiB, more than any chunk) are read; the rest are looked up, not read.
func (s *carBlocks) parts(ctx context.Context, c cid.Cid, n format.Node) (have, total int) {
	if v, ok := s.whole.Load(c.KeyString()); ok {
		p := v.([2]int)
		return p[0], p[1]
	}
	have, total = 1, 1
	var walk func(n format.Node)
	walk = func(n format.Node) {
		for _, l := range n.Links() {
			if total >= 1<<20 {
				return
			}
			total++
			if _, ok := s.at[string(l.Cid.Hash())]; !ok {
				continue
			}
			have++
			if l.Size > 1<<20 && l.Cid.Type() == cid.DagProtobuf {
				if k, err := s.Get(ctx, l.Cid); err == nil {
					walk(k)
				}
			}
		}
	}
	walk(n)
	if ctx.Err() == nil {
		s.whole.Store(c.KeyString(), [2]int{have, total})
	}
	return have, total
}

// close closes the CARs once indexing is done.
func (s *carBlocks) close() {
	go func() {
		<-s.ready
		for _, f := range s.files {
			f.Close()
		}
	}()
}

// handle adds the _i2f/ endpoints under prefix ("" or "/<name>").
func (s *carBlocks) handle(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("GET "+prefix+"/_i2f/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(struct {
			Content bool   `json:"content"`
			All     string `json:"all,omitempty"`
		}{true, s.all})
	})
	mux.HandleFunc("GET "+prefix+"/_i2f/content/{cid}", s.serveContent)
	mux.HandleFunc("GET "+prefix+"/_i2f/path/{cid}/{rest...}", s.servePath)
	mux.HandleFunc("POST "+prefix+"/_i2f/thumb/{cid}", s.keepThumb)
	mux.HandleFunc("POST "+prefix+"/_i2f/parts", func(w http.ResponseWriter, r *http.Request) {
		out := map[string][2]int{}
		sc := bufio.NewScanner(io.LimitReader(r.Body, 8<<20))
		for n := 0; sc.Scan() && n < 20000; n++ {
			c, err := cid.Decode(strings.TrimSpace(sc.Text()))
			if err != nil {
				continue
			}
			nd, err := s.Get(r.Context(), c)
			if errors.Is(err, errNoBlock) {
				out[c.String()] = [2]int{0, 1}
			} else if err == nil {
				if have, total := s.parts(r.Context(), c, nd); have < total {
					out[c.String()] = [2]int{have, total}
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST "+prefix+"/_i2f/kinds", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]string{}
		sc := bufio.NewScanner(io.LimitReader(r.Body, 8<<20))
		for n := 0; sc.Scan() && n < 20000; n++ {
			c, err := cid.Decode(strings.TrimSpace(sc.Text()))
			if err != nil {
				continue
			}
			if k := s.kind(r.Context(), c); k != "" {
				out[c.String()] = k
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
}

func (s *carBlocks) serveContent(w http.ResponseWriter, r *http.Request) {
	c, err := cid.Decode(r.PathValue("cid"))
	if err != nil {
		http.Error(w, "not a CID", http.StatusBadRequest)
		return
	}
	if c.Type() == cid.DagCBOR || c.Type() == cid.DagJSON {
		s.serveRecord(w, r, c)
		return
	}
	n, err := s.Get(r.Context(), c)
	if s.failed(w, err) {
		return
	}
	s.serveFile(w, r, c, n, path.Base("/"+r.URL.Query().Get("name")), false)
}

// serveRecord serves a dag-cbor block, such as a Bluesky record, as JSON
// (links as {"$link": cid}, bytes as {"$bytes": base64}), and a dag-json
// block as it is.
func (s *carBlocks) serveRecord(w http.ResponseWriter, r *http.Request, c cid.Cid) {
	data, err := s.block(r.Context(), c)
	if s.failed(w, err) {
		return
	}
	js, err := data, error(nil)
	if c.Type() == cid.DagCBOR {
		js, err = atproto.JSON(data)
	}
	if err != nil {
		http.Error(w, "not dag-cbor: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	name := path.Base("/" + r.URL.Query().Get("name"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Security-Policy", "sandbox")
	if r.URL.Query().Get("dl") != "" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Etag", `"`+c.String()+`"`)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(js))
}

// servePath serves the file at a path under a folder, as a gateway does, so
// a web page's relative links to its pictures, styles and scripts work. A
// folder serves its index.html. Pages served here may run scripts: they run
// sandboxed, as an origin of their own, so they cannot reach the explorer.
func (s *carBlocks) servePath(w http.ResponseWriter, r *http.Request) {
	c, err := cid.Decode(r.PathValue("cid"))
	if err != nil {
		http.Error(w, "not a CID", http.StatusBadRequest)
		return
	}
	rest := r.PathValue("rest")
	n, err := s.Get(r.Context(), c)
	name := ""
	for _, seg := range strings.Split(rest, "/") {
		if s.failed(w, err) {
			return
		}
		if seg == "" {
			continue
		}
		d, derr := uio.NewDirectoryFromNode(readOnly{s}, n)
		if derr != nil {
			http.Error(w, "not a folder", http.StatusNotFound)
			return
		}
		n, err = d.Find(r.Context(), seg)
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "no "+seg+" here", http.StatusNotFound)
			return
		}
		name = seg
	}
	if s.failed(w, err) {
		return
	}
	if d, derr := uio.NewDirectoryFromNode(readOnly{s}, n); derr == nil {
		if rest != "" && !strings.HasSuffix(rest, "/") {
			http.Redirect(w, r, path.Base(rest)+"/", http.StatusMovedPermanently)
			return
		}
		if n, err = d.Find(r.Context(), "index.html"); err != nil {
			http.Error(w, "this folder has no index.html", http.StatusNotFound)
			return
		}
		name = "index.html"
	}
	s.serveFile(w, r, n.Cid(), n, name, true)
}

// pageHost says whether r.Host is the name pages are served under. The
// explorer is at 127.0.0.1 and shows web pages from localhost, which to a
// browser is another origin: there a page keeps its own storage, fonts and
// requests to its own files, as on a gateway, and cannot reach the
// explorer. Asked under any other name, pages run sandboxed with no origin.
func pageHost(hostport string) bool {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport
	}
	return h == "localhost"
}

// failed answers a failed block lookup.
func (s *carBlocks) failed(w http.ResponseWriter, err error) bool {
	if errors.Is(err, errNoBlock) {
		http.Error(w, "this file's blocks are not in the pieces on this machine", http.StatusNotFound)
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	return err != nil
}

func (s *carBlocks) serveFile(w http.ResponseWriter, r *http.Request, c cid.Cid, n format.Node, name string, scripts bool) {
	if have, total := s.parts(r.Context(), c, n); have < total {
		http.Error(w, fmt.Sprintf("Only %d of the %d parts of this file are in the pieces on this machine: the IPFS repo it was packed from did not have the rest.", have, total), http.StatusNotFound)
		return
	}
	rd, err := uio.NewDagReader(r.Context(), n, s)
	if err != nil {
		http.Error(w, "not a file: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		head := make([]byte, 512)
		k, _ := io.ReadFull(rd, head)
		ctype = http.DetectContentType(head[:k])
		if _, err := rd.Seek(0, io.SeekStart); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if tw, _ := strconv.Atoi(r.URL.Query().Get("w")); tw >= 16 && tw <= 2048 {
		s.serveThumb(w, r, c, rd, int64(rd.Size()), name, ctype, tw)
		return
	}
	w.Header().Set("Content-Type", ctype)
	if r.URL.Query().Get("dl") != "" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	// Files are other people's content: a page or an SVG in one must not
	// script the explorer, which shares its origin. (Browsers will not show
	// a PDF under a sandbox, and media cannot run scripts.) Under _i2f/path
	// pages run their scripts, still sandboxed as an origin of their own.
	if !strings.HasPrefix(ctype, "image/") && !strings.HasPrefix(ctype, "video/") && !strings.HasPrefix(ctype, "audio/") &&
		!strings.HasPrefix(ctype, "application/pdf") && !strings.HasPrefix(ctype, "text/plain") || strings.HasPrefix(ctype, "image/svg") {
		if scripts && pageHost(r.Host) {
			// as an origin of its own, not sandboxed: see pageHost
		} else if scripts {
			w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-popups allow-modals allow-pointer-lock")
		} else {
			w.Header().Set("Content-Security-Policy", "sandbox")
		}
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Etag", `"`+c.String()+`"`)
	http.ServeContent(w, r, name, time.Time{}, rd)
}
