package repo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dgraph-io/badger"
)

// badgerDB reads a badgerds (badger v1) datastore. Keys are datastore key
// bytes, as go-ds-badger writes them.
type badgerDB struct {
	db   *badger.DB
	temp string
}

// openBadger opens read-only. A running daemon holds badger's lock, an
// unclean shutdown needs a replay, and Windows has no read-only mode; in each
// case the directory is copied and the copy opened instead.
func openBadger(dir string) (*badgerDB, error) {
	if _, err := os.Stat(filepath.Join(dir, "MANIFEST")); err != nil {
		return nil, fmt.Errorf("badger datastore at %s: %w", dir, err)
	}
	db, err := badger.Open(badgerOptions(dir).WithReadOnly(true))
	if err == nil {
		return &badgerDB{db: db}, nil
	}
	tmp, cerr := snapshotDir(dir)
	if cerr != nil {
		return nil, fmt.Errorf("badger datastore at %s could not be opened read-only (is the daemon running?) and copying it failed: %v: %w", dir, cerr, err)
	}
	db, err2 := badger.Open(badgerOptions(tmp))
	if err2 != nil {
		os.RemoveAll(tmp)
		return nil, fmt.Errorf("badger datastore at %s: %w (copy: %v)", dir, err, err2)
	}
	return &badgerDB{db: db, temp: tmp}, nil
}

func badgerOptions(dir string) badger.Options {
	return badger.DefaultOptions(dir).WithLogger(nil).WithTruncate(true)
}

func (b *badgerDB) Get(key string) ([]byte, error) {
	var v []byte
	err := b.db.View(func(txn *badger.Txn) error {
		it, err := txn.Get([]byte(key))
		if err != nil {
			return err
		}
		v, err = it.ValueCopy(nil)
		return err
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	return v, err
}

func (b *badgerDB) Query(prefix string, fn func(string, int) error) error {
	return b.db.View(func(txn *badger.Txn) error {
		o := badger.DefaultIteratorOptions
		o.PrefetchValues = false
		o.Prefix = []byte(prefix)
		it := txn.NewIterator(o)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			if err := fn(string(item.Key()), int(item.ValueSize())); err != nil {
				return err
			}
		}
		return nil
	})
}

func (b *badgerDB) Close() error {
	err := b.db.Close()
	if b.temp != "" {
		os.RemoveAll(b.temp)
	}
	return err
}
