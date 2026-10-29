package explorer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// Serve serves the archive directory root (the one holding explorer/ and
// index/) on addr until ctx ends, so the pages see the same layout a gateway
// gives them. It also serves file contents from the CARs in the pieces/ of
// root and of the dirs in more, which may hold blocks root shares with other
// archives. It calls ready with the explorer's URL once it is listening.
func Serve(ctx context.Context, root string, more []string, addr string, ready func(url string), wraps ...Wrap) error {
	if _, err := os.Stat(filepath.Join(root, "explorer", "index.html")); err != nil {
		return fmt.Errorf("no explorer/ in %s; run ipfs2fil pack first, or pass --archive", root)
	}
	blocks := newCARBlocks(append([]string{root}, more...))
	defer blocks.close()
	blocks.start()
	mux := http.NewServeMux()
	mux.Handle("/", archiveFiles(root))
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/explorer/", http.StatusFound)
	})
	blocks.handle(mux, "")
	return listen(ctx, addr, wrap(mux, wraps), "/explorer/", ready)
}

// Wrap adds to what Serve and ServeMany serve: routes on mux, and a handler
// around everything (h, or one that calls it).
type Wrap func(mux *http.ServeMux, h http.Handler) http.Handler

func wrap(mux *http.ServeMux, wraps []Wrap) http.Handler {
	var h http.Handler = mux
	for _, w := range wraps {
		h = w(mux, h)
	}
	return h
}

// archiveFiles serves an archive directory, with the explorer's pages from
// this build rather than the copy packed with the archive, so archives get a
// newer explorer without a new pack. An index older than the pages read is
// rebuilt in memory, the first time a page asks for it.
func archiveFiles(dir string) http.Handler {
	h := archivePages(dir)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The manifest inside the archive was written before push, so it
		// has no copies; the pages read the one pack and push keep up to date.
		if r.URL.Path == "/index/manifest.json" {
			if full := filepath.Join(dir, "manifest.json"); fileExists(full) {
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Content-Type", "application/json")
				http.ServeFile(w, r, full)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func archivePages(dir string) http.Handler {
	disk := http.FileServer(http.Dir(dir))
	f := indexFormat(filepath.Join(dir, "explorer"))
	if f != IndexFormat && f != 1 {
		return disk
	}
	pages, err := fs.Sub(assets, "assets")
	if err != nil {
		return disk
	}
	var (
		once     sync.Once
		upgraded map[string][]byte
		upErr    error
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := strings.CutPrefix(r.URL.Path, "/explorer/")
		if !ok {
			disk.ServeHTTP(w, r)
			return
		}
		// Pages and index change with ipfs2fil, not with the archive.
		w.Header().Set("Cache-Control", "no-cache")
		if idx, ok := strings.CutPrefix(name, "idx/"); ok && f != IndexFormat {
			once.Do(func() { upgraded, upErr = upgradeIndex(filepath.Join(dir, "explorer")) })
			if upErr != nil {
				http.Error(w, "reading this archive's older index: "+upErr.Error(), http.StatusInternalServerError)
				return
			}
			b, ok := upgraded[idx]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Write(b)
			return
		}
		if name == "" {
			name = "index.html"
		}
		if !strings.Contains(name, "/") {
			if _, err := fs.Stat(pages, name); err == nil {
				http.ServeFileFS(w, r, pages, name)
				return
			}
		}
		disk.ServeHTTP(w, r)
	})
}

// indexFormat reads the format of the index in the explorer at dir, or 0.
func indexFormat(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "idx", "meta.js"))
	if err != nil {
		return 0
	}
	b = bytes.TrimSpace(b)
	b = bytes.TrimPrefix(b, []byte(`I2F.put("meta",`))
	b = bytes.TrimSuffix(b, []byte(");"))
	var mt struct {
		Format int `json:"format"`
	}
	if json.Unmarshal(b, &mt) != nil {
		return 0
	}
	return mt.Format
}

// Archive is one archive ServeMany serves: Name is its URL path and title,
// Repo the IPFS repo it was packed from, if known.
type Archive struct {
	Name string
	Dir  string
	Repo string
}

// ServeMany serves several archive directories, each under /<name>/ the way
// Serve serves one, and one explorer over all of them under /all/ (or another
// name no archive has), which / leads to. The combined index is built the
// first time a page asks for it.
func ServeMany(ctx context.Context, as []Archive, addr string, ready func(url string), wraps ...Wrap) error {
	mux := http.NewServeMux()
	// One block index for all: an archive can share blocks with the others.
	var dirs []string
	names := map[string]bool{}
	for _, a := range as {
		dirs = append(dirs, a.Dir)
		names[a.Name] = true
	}
	all := "all"
	for names[all] {
		all = "_" + all
	}
	allPrefix := "/" + url.PathEscape(all)
	blocks := newCARBlocks(dirs)
	blocks.all = allPrefix + "/explorer/"
	defer blocks.close()
	blocks.start()
	for _, a := range as {
		if _, err := os.Stat(filepath.Join(a.Dir, "explorer", "index.html")); err != nil {
			return fmt.Errorf("no explorer/ in %s; run ipfs2fil pack first", a.Dir)
		}
		prefix := "/" + url.PathEscape(a.Name) + "/"
		mux.Handle(prefix, http.StripPrefix(strings.TrimSuffix(prefix, "/"), archiveFiles(a.Dir)))
		blocks.handle(mux, strings.TrimSuffix(prefix, "/"))
		mux.Handle(prefix+"{$}", http.RedirectHandler(prefix+"explorer/", http.StatusFound))
	}
	mux.Handle(allPrefix+"/", http.StripPrefix(allPrefix, combinedFiles(as, func(a Archive) string {
		return "../../" + url.PathEscape(a.Name) + "/explorer/"
	})))
	blocks.handle(mux, allPrefix)
	mux.Handle(allPrefix+"/{$}", http.RedirectHandler(allPrefix+"/explorer/", http.StatusFound))
	mux.Handle("/{$}", http.RedirectHandler(allPrefix+"/explorer/", http.StatusFound))
	return listen(ctx, addr, wrap(mux, wraps), "/", ready)
}

// combinedFiles serves the explorer over all of as, laid out like one
// archive: explorer/ with this build's pages and the combined index, and
// index/manifest.json.
func combinedFiles(as []Archive, href func(Archive) string) http.Handler {
	pages, _ := fs.Sub(assets, "assets")
	var (
		once  sync.Once
		files map[string][]byte
		man   []byte
		err   error
	)
	build := func() error {
		once.Do(func() {
			var m *manifest.Manifest
			if files, m, err = combine(as, href); err == nil {
				man, err = json.MarshalIndent(m, "", "  ")
			}
		})
		return err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		if r.URL.Path == "/index/manifest.json" {
			if err := build(); err != nil {
				http.Error(w, "combining the archives: "+err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(man)
			return
		}
		name, ok := strings.CutPrefix(r.URL.Path, "/explorer/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		if idx, ok := strings.CutPrefix(name, "idx/"); ok {
			if err := build(); err != nil {
				http.Error(w, "combining the archives: "+err.Error(), http.StatusInternalServerError)
				return
			}
			b, ok := files[idx]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Write(b)
			return
		}
		if name == "" {
			name = "index.html"
		}
		if strings.Contains(name, "/") {
			http.NotFound(w, r)
			return
		}
		http.ServeFileFS(w, r, pages, name)
	})
}

func listen(ctx context.Context, addr string, h http.Handler, path string, ready func(url string)) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if ready != nil {
		ready("http://" + ln.Addr().String() + path)
	}
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// readManifest reads dir's full manifest, else the one inside the archive.
func readManifest(dir string) (*manifest.Manifest, error) {
	m, err := manifest.Read(filepath.Join(dir, "manifest.json"))
	if errors.Is(err, os.ErrNotExist) {
		m, err = manifest.Read(filepath.Join(dir, "index", "manifest.json"))
	}
	return m, err
}
