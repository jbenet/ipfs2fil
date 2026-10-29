package explorer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
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
// Serve serves one, with a page at / linking to their explorers.
func ServeMany(ctx context.Context, as []Archive, addr string, ready func(url string), wraps ...Wrap) error {
	type row struct {
		Archive
		Root   string
		Pieces int
		Bytes  string
	}
	var rows []row
	mux := http.NewServeMux()
	// One block index for all: an archive can share blocks with the others.
	var dirs []string
	for _, a := range as {
		dirs = append(dirs, a.Dir)
	}
	blocks := newCARBlocks(dirs)
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
		r := row{Archive: a}
		if m, err := readManifest(a.Dir); err == nil {
			var n int64
			for _, p := range m.Pieces {
				n += p.CARBytes
			}
			r.Root, r.Pieces, r.Bytes = m.Root, len(m.Pieces), humanBytes(n)
		}
		rows = append(rows, r)
	}
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		manyPage.Execute(w, rows)
	})
	return listen(ctx, addr, wrap(mux, wraps), "/", ready)
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

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	d, e := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		d *= unit
		e++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(d), "KMGTPE"[e])
}

var manyPage = template.Must(template.New("many").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>ipfs2fil archives</title>
{{with index . 0}}<link rel="stylesheet" href="/{{.Name}}/explorer/explorer.css">{{end}}
<style>main{max-width:960px;margin:0 auto;padding:24px 16px}table{width:100%;border-collapse:collapse}
td,th{text-align:left;padding:8px;border-bottom:1px solid var(--line,#ddd);vertical-align:top}
.mono{font-family:var(--mono,monospace);font-size:13px;word-break:break-all}.muted{color:var(--muted,#555)}</style>
</head><body><main>
<h1>Archives</h1>
<p class="muted">{{len .}} repos packed by ipfs2fil on this machine.</p>
<table><thead><tr><th>Repo</th><th>Pieces</th><th>Archive root</th></tr></thead><tbody>
{{range .}}<tr><td><a href="/{{.Name}}/explorer/">{{.Name}}</a>{{if .Repo}}<div class="muted mono">{{.Repo}}</div>{{end}}</td>
<td>{{if .Pieces}}{{.Pieces}} ({{.Bytes}}){{end}}</td><td class="mono">{{.Root}}</td></tr>
{{end}}</tbody></table>
</main></body></html>
`))
