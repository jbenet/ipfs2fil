package explorer

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestServeMany: each archive's explorer is under /<name>/, with a page at /
// linking to them.
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
	if page := get("/"); !strings.Contains(page, `href="/photos/explorer/"`) || !strings.Contains(page, `href="/backup/explorer/"`) || !strings.Contains(page, "/home/me/photos") {
		t.Errorf("index page lacks the archives:\n%s", page)
	}
	for _, p := range []string{"/photos/", "/backup/explorer/", "/backup/explorer/explorer.css"} {
		get(p)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
