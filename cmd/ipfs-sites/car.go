package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/ipfs/go-cid"

	"github.com/jbenet/ipfs2fil/internal/car"
	"github.com/jbenet/ipfs2fil/internal/dagfetch"
	"github.com/jbenet/ipfs2fil/internal/sitelist"
)

// writeCAR writes every site the lock names into one CAR at dest: its
// roots are the sites' roots, in list order, and each block appears once.
// It returns how many blocks it wrote.
func writeCAR(dest string, l *sitelist.Lock, f *dagfetch.Fetcher) (int, error) {
	roots := rootsOf(l)
	if len(roots) == 0 {
		return 0, errors.New("no site was fetched")
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".car-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	w := bufio.NewWriterSize(tmp, 1<<20)
	section := func(parts ...[]byte) error {
		n := 0
		for _, p := range parts {
			n += len(p)
		}
		var vb [binary.MaxVarintLen64]byte
		if _, err := w.Write(vb[:binary.PutUvarint(vb[:], uint64(n))]); err != nil {
			return err
		}
		for _, p := range parts {
			if _, err := w.Write(p); err != nil {
				return err
			}
		}
		return nil
	}
	if err := section(car.Header(roots...)); err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	n := 0
	for _, root := range roots {
		if err := copyBlocks(f.File(root), seen, func(c cid.Cid, b []byte) error {
			n++
			return section(c.Bytes(), b)
		}); err != nil {
			return 0, err
		}
	}
	if err := w.Flush(); err != nil {
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	os.Chmod(tmp.Name(), 0o644)
	return n, os.Rename(tmp.Name(), dest)
}

// copyBlocks hands put each block of the CAR at path not yet seen. The
// CARs were checked block by block when fetched.
func copyBlocks(path string, seen map[string]bool, put func(cid.Cid, []byte) error) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	r, err := car.NewReader(fh)
	if err != nil {
		return err
	}
	for {
		c, b, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if k := string(c.Hash()); !seen[k] {
			seen[k] = true
			if err := put(c, b); err != nil {
				return err
			}
		}
	}
}
