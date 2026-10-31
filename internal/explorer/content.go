package explorer

// What `ipfs2fil explore` serves besides the archive's own files: the bytes
// of any file in the archive, read from the CARs in pieces/, and the kinds of
// files an older pack could not tell from their names. From a gateway the
// pages fetch the same things from the gateway instead.
//
//	GET  _i2f/info             {"content": true, "all": "<path>"}; all is the explorer over
//	                           every archive, when explore serves several
//	GET  _i2f/content/<cid>    the file, with Range support; ?name= sets the type, ?dl=1 downloads,
//	                           ?w=<px> gets a JPEG thumbnail of a photo
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
)

// carBlocks finds blocks in the CARs under each dir's pieces/. It indexes
// them in the background on first use; a lookup waits for the index.
type carBlocks struct {
	dirs  []string
	once  sync.Once
	ready chan struct{}
	at    map[string]blockAt // multihash -> where
	files []*os.File
	err   error
	thumb *thumbs
	all   string // the combined explorer's path, if any
}

type blockAt struct {
	file int32
	n    int32
	off  int64
}

var errNoBlock = errors.New("block not in this archive's pieces")

func newCARBlocks(dirs []string) *carBlocks {
	return &carBlocks{dirs: dirs, ready: make(chan struct{}), at: map[string]blockAt{}, thumb: newThumbs()}
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
	n, err := s.Get(r.Context(), c)
	if errors.Is(err, errNoBlock) {
		http.Error(w, "this file's blocks are not in the pieces on this machine", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rd, err := uio.NewDagReader(r.Context(), n, s)
	if err != nil {
		http.Error(w, "not a file: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	name := path.Base("/" + r.URL.Query().Get("name"))
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
	epub := strings.EqualFold(path.Ext(name), ".epub")
	if tw, _ := strconv.Atoi(r.URL.Query().Get("w")); tw >= 16 && tw <= 2048 && (ctype == "image/jpeg" || ctype == "image/png" || ctype == "image/gif" || epub) {
		key := c.String() + "/" + strconv.Itoa(tw)
		b, ok := s.thumb.get(key)
		if !ok {
			src := io.Reader(rd)
			if epub { // a book's thumbnail is its cover
				src = bytes.NewReader(epubCover(rd))
			}
			if b = s.thumb.thumbnail(src, tw); b != nil {
				s.thumb.put(key, b)
			}
		}
		if b == nil && epub {
			http.Error(w, "this book has no cover", http.StatusNotFound)
			return
		}
		if b != nil {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			w.Write(b)
			return
		}
		if _, err := rd.Seek(0, io.SeekStart); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", ctype)
	if r.URL.Query().Get("dl") != "" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	// Files are other people's content: a page or an SVG in one must not
	// script the explorer, which shares its origin. (Browsers will not show
	// a PDF under a sandbox, and media cannot run scripts.)
	if !strings.HasPrefix(ctype, "image/") && !strings.HasPrefix(ctype, "video/") && !strings.HasPrefix(ctype, "audio/") &&
		!strings.HasPrefix(ctype, "application/pdf") && !strings.HasPrefix(ctype, "text/plain") || strings.HasPrefix(ctype, "image/svg") {
		w.Header().Set("Content-Security-Policy", "sandbox")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Etag", `"`+c.String()+`"`)
	http.ServeContent(w, r, name, time.Time{}, rd)
}
