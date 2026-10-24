package explorer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Serve serves the archive directory root (the one holding explorer/ and
// index/) on addr until ctx ends, so the pages see the same layout a gateway
// gives them. It calls ready with the explorer's URL once it is listening.
func Serve(ctx context.Context, root, addr string, ready func(url string)) error {
	if _, err := os.Stat(filepath.Join(root, "explorer", "index.html")); err != nil {
		return fmt.Errorf("no explorer/ in %s; run ipfs2fil pack first, or pass --archive", root)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(root)))
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/explorer/", http.StatusFound)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if ready != nil {
		ready("http://" + ln.Addr().String() + "/explorer/")
	}
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
