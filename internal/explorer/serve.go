package explorer

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// Serve serves the archive directory root (the one holding explorer/ and
// index/) on addr until ctx ends, so the pages see the same layout a gateway
// gives them. It calls ready with the explorer's URL once it is listening.
func Serve(ctx context.Context, root, addr string, ready func(url string)) error {
	if _, err := os.Stat(filepath.Join(root, "explorer", "index.html")); err != nil {
		return fmt.Errorf("no explorer/ in %s; run ipfs2fil pack first, or pass --archive", root)
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(root)))
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/explorer/", http.StatusFound)
	})
	return listen(ctx, addr, mux, "/explorer/", ready)
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
func ServeMany(ctx context.Context, as []Archive, addr string, ready func(url string)) error {
	type row struct {
		Archive
		Root   string
		Pieces int
		Bytes  string
	}
	var rows []row
	mux := http.NewServeMux()
	for _, a := range as {
		if _, err := os.Stat(filepath.Join(a.Dir, "explorer", "index.html")); err != nil {
			return fmt.Errorf("no explorer/ in %s; run ipfs2fil pack first", a.Dir)
		}
		prefix := "/" + url.PathEscape(a.Name) + "/"
		mux.Handle(prefix, http.StripPrefix(strings.TrimSuffix(prefix, "/"), http.FileServer(http.Dir(a.Dir))))
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
	return listen(ctx, addr, mux, "/", ready)
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
