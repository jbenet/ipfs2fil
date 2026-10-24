package repo

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/syndtr/goleveldb/leveldb"
	lerrors "github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

type levelDB struct {
	db   *leveldb.DB
	temp string // snapshot directory to remove on Close, if any
}

// openLevelDB opens a leveldb datastore read-only. A running daemon holds the
// leveldb lock, so when the lock is taken the directory is copied to a temp
// dir and the copy is opened instead; the repo itself is never touched.
func openLevelDB(dir string) (*levelDB, error) {
	if _, err := os.Stat(filepath.Join(dir, "CURRENT")); err != nil {
		return nil, fmt.Errorf("leveldb at %s: %w", dir, err)
	}
	o := &opt.Options{ReadOnly: true, ErrorIfMissing: true}
	db, err := leveldb.OpenFile(dir, o)
	if err == nil {
		return &levelDB{db: db}, nil
	}
	if lerrors.IsCorrupted(err) {
		return nil, fmt.Errorf("leveldb at %s is corrupt (run `ipfs repo verify` with the daemon stopped): %w", dir, err)
	}
	tmp, cerr := snapshotDir(dir)
	if cerr != nil {
		return nil, fmt.Errorf("leveldb at %s is locked (is the daemon running?) and copying it failed: %v: %w", dir, cerr, err)
	}
	// The copy may hold a log the daemon has not compacted yet; open it
	// writable so leveldb can replay it.
	db, err2 := leveldb.OpenFile(tmp, &opt.Options{ErrorIfMissing: true})
	if err2 != nil {
		os.RemoveAll(tmp)
		return nil, fmt.Errorf("leveldb at %s: %w (copy: %v)", dir, err, err2)
	}
	return &levelDB{db: db, temp: tmp}, nil
}

// snapshotDir copies the files of a leveldb, badger or pebble directory,
// except the lock file, into a new temp dir.
func snapshotDir(dir string) (string, error) {
	tmp, err := os.MkdirTemp("", "ipfs2fil-snapshot-")
	if err != nil {
		return "", err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	for _, e := range ents {
		if e.IsDir() || e.Name() == "LOCK" {
			continue
		}
		if err := copyFile(filepath.Join(dir, e.Name()), filepath.Join(tmp, e.Name())); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // compacted away while copying
			}
			os.RemoveAll(tmp)
			return "", err
		}
	}
	return tmp, nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (l *levelDB) Get(key string) ([]byte, error) {
	b, err := l.db.Get([]byte(key), nil)
	if errors.Is(err, leveldb.ErrNotFound) {
		return nil, ErrNotFound
	}
	return b, err
}

func (l *levelDB) Query(prefix string, fn func(string, int) error) error {
	it := l.db.NewIterator(util.BytesPrefix([]byte(prefix)), nil)
	defer it.Release()
	for it.Next() {
		if err := fn(string(it.Key()), len(it.Value())); err != nil {
			return err
		}
	}
	return it.Error()
}

func (l *levelDB) Close() error {
	err := l.db.Close()
	if l.temp != "" {
		os.RemoveAll(l.temp)
	}
	return err
}
