package explorer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// TestServeMany: each archive's explorer is under /<name>/, and one over all
// of them under /all/, where / leads.
func TestServeMany(t *testing.T) {
	var as []Archive
	for _, name := range []string{"photos", "backup"} {
		dir := t.TempDir()
		m, entries := sample()
		if err := Write(filepath.Join(dir, "explorer"), m, entries); err != nil {
			t.Fatal(err)
		}
		as = append(as, Archive{Name: name, Dir: dir, Repo: "/home/me/" + name})
	}
	ctx, cancel := context.WithCancel(context.Background())
	urls := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- ServeMany(ctx, as, "127.0.0.1:0", func(u string) { urls <- u }) }()
	var base string
	select {
	case base = <-urls:
	case err := <-done:
		t.Fatal(err)
	}
	get := func(path string) string {
		t.Helper()
		r, err := http.Get(strings.TrimSuffix(base, "/") + path)
		if err == nil && r.Request.URL.Path != path && !strings.HasSuffix(path, "/") {
			t.Errorf("GET %s went to %s", path, r.Request.URL.Path)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %s", path, r.Status)
		}
		return string(b)
	}
	if page := get("/"); !strings.Contains(page, `data-page="overview"`) {
		t.Errorf("/ is not the combined overview:\n%s", page)
	}
	for _, p := range []string{"/photos/", "/backup/explorer/", "/backup/explorer/explorer.css", "/all/explorer/browse.html", "/all/index/manifest.json"} {
		get(p)
	}
	if meta := get("/all/explorer/idx/meta.js"); !strings.Contains(meta, `"repos":[{"name":"photos","repo":"/home/me/photos"`) || !strings.Contains(meta, `"href":"../../backup/explorer/"`) {
		t.Errorf("combined meta lacks the repos:\n%s", meta)
	}
	if info := get("/photos/_i2f/info"); !strings.Contains(info, `"all":"/all/explorer/"`) {
		t.Errorf("info does not point at the combined explorer: %s", info)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestServeFullManifest: explore hands the pages the manifest push keeps,
// with the copies, not the one packed into the archive before any upload.
func TestServeFullManifest(t *testing.T) {
	dir := t.TempDir()
	m, entries := sample()
	if err := Write(filepath.Join(dir, "explorer"), m, entries); err != nil {
		t.Fatal(err)
	}
	packed := *m
	packed.Copies = nil
	if err := os.MkdirAll(filepath.Join(dir, "index"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := packed.Write(filepath.Join(dir, "index", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	get := func() *manifest.Manifest {
		t.Helper()
		w := httptest.NewRecorder()
		archiveFiles(dir).ServeHTTP(w, httptest.NewRequest("GET", "/index/manifest.json", nil))
		var got manifest.Manifest
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%v: %s", err, w.Body)
		}
		return &got
	}
	if got := get(); len(got.Copies) != 0 {
		t.Fatalf("with no full manifest, got %d copies, want the packed one's 0", len(got.Copies))
	}
	m.Copies = []manifest.Copy{{Provider: "f04", DataSetID: "1", PieceIDs: map[int]string{0: "1"}}}
	if err := m.Write(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if got := get(); len(got.Copies) != len(m.Copies) {
		t.Errorf("got %d copies, want %d from the full manifest", len(got.Copies), len(m.Copies))
	}
}
