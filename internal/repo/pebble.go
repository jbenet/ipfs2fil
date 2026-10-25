package repo

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	pebble1 "github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/v2"
)

// pebbleDB reads a pebbleds datastore. Keys are datastore key bytes, as
// go-ds-pebble writes them. kubo 0.37 and later write with pebble v2; earlier
// kubo wrote formats pebble v2 no longer opens, which pebble v1 reads.
type pebbleDB struct {
	get   func(key []byte) ([]byte, io.Closer, error)
	scan  func(lo, hi []byte, fn func(k, v []byte) error) error
	close func() error
	temp  string
}

// openPebble opens read-only; when a running daemon holds the lock, the
// directory is copied and the copy opened instead.
func openPebble(dir string) (*pebbleDB, error) {
	if m, _ := filepath.Glob(filepath.Join(dir, "MANIFEST-*")); len(m) == 0 {
		return nil, fmt.Errorf("pebble datastore at %s: no MANIFEST file", dir)
	}
	p, err := openPebbleAny(dir, true)
	if err == nil {
		return p, nil
	}
	tmp, cerr := snapshotDir(dir)
	if cerr != nil {
		return nil, fmt.Errorf("pebble datastore at %s is locked (is the daemon running?) and copying it failed: %v: %w", dir, cerr, err)
	}
	p, err2 := openPebbleAny(tmp, false)
	if err2 != nil {
		os.RemoveAll(tmp)
		return nil, fmt.Errorf("pebble datastore at %s: %w (copy: %v)", dir, err, err2)
	}
	p.temp = tmp
	return p, nil
}

func openPebbleAny(dir string, readOnly bool) (*pebbleDB, error) {
	db, err := pebble.Open(dir, &pebble.Options{ReadOnly: readOnly, ErrorIfNotExists: true, Logger: quietLogger{}})
	if err == nil {
		return &pebbleDB{
			get:   db.Get,
			close: db.Close,
			scan: func(lo, hi []byte, fn func(k, v []byte) error) error {
				it, err := db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
				if err != nil {
					return err
				}
				return iterate(it, it.First, it.Valid, it.Next, it.Key, it.ValueAndErr, it.Error, fn)
			},
		}, nil
	}
	if !strings.Contains(err.Error(), "format major version") {
		return nil, err
	}
	db1, err1 := pebble1.Open(dir, &pebble1.Options{ReadOnly: readOnly, ErrorIfNotExists: true, Logger: quietLogger{}})
	if err1 != nil {
		return nil, fmt.Errorf("%w (pebble v1: %v)", err, err1)
	}
	return &pebbleDB{
		get:   db1.Get,
		close: db1.Close,
		scan: func(lo, hi []byte, fn func(k, v []byte) error) error {
			it, err := db1.NewIter(&pebble1.IterOptions{LowerBound: lo, UpperBound: hi})
			if err != nil {
				return err
			}
			return iterate(it, it.First, it.Valid, it.Next, it.Key, it.ValueAndErr, it.Error, fn)
		},
	}, nil
}

func iterate(it io.Closer, first, valid, next func() bool, key func() []byte,
	value func() ([]byte, error), ierr func() error, fn func(k, v []byte) error) error {
	for first(); valid(); next() {
		v, err := value()
		if err == nil {
			err = fn(key(), v)
		}
		if err != nil {
			it.Close()
			return err
		}
	}
	return errors.Join(ierr(), it.Close())
}

type quietLogger struct{}

func (quietLogger) Infof(string, ...any)  {}
func (quietLogger) Errorf(string, ...any) {}
func (quietLogger) Fatalf(format string, args ...any) {
	panic(fmt.Sprintf(format, args...))
}

func (p *pebbleDB) Get(key string) ([]byte, error) {
	v, closer, err := p.get([]byte(key))
	if errors.Is(err, pebble.ErrNotFound) || errors.Is(err, pebble1.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	return append([]byte(nil), v...), nil
}

func (p *pebbleDB) Query(prefix string, fn func(string, int) error) error {
	lo := []byte(prefix)
	return p.scan(lo, prefixEnd(lo), func(k, v []byte) error {
		return fn(string(k), len(v))
	})
}

// prefixEnd is the first key after every key with prefix p.
func prefixEnd(p []byte) []byte {
	end := append([]byte(nil), p...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

func (p *pebbleDB) Close() error {
	err := p.close()
	if p.temp != "" {
		os.RemoveAll(p.temp)
	}
	return err
}
