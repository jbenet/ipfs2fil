package repo

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// flatfs reads a go-ds-flatfs directory: one file per key, <shard>/<name>.data.
// It is read here directly rather than through go-ds-flatfs, which writes
// diskUsage.cache on close.
type flatfs struct {
	dir   string
	shard func(name string) string
	// hexNames: repos 2 and 3 name files by the hex of the key bytes. Later
	// repos use the key itself (base32 of a CID or multihash).
	hexNames bool
}

func openFlatfs(dir string, v Version) (*flatfs, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("blocks directory: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("blocks directory %s is not a directory", dir)
	}
	f := &flatfs{dir: dir}
	switch {
	case v <= 3:
		// fs-repo-migrations 1-to-2: hex names, 4 bytes (8 hex digits) of splay.
		f.hexNames, f.shard = true, shardPrefix(8)
	case v == 4:
		f.shard = shardPrefix(5) // no SHARDING file yet
	default:
		b, err := os.ReadFile(filepath.Join(dir, "SHARDING"))
		if err != nil {
			return nil, fmt.Errorf("flatfs at %s has no SHARDING file: %w", dir, err)
		}
		if f.shard, err = parseShardFunc(strings.TrimSpace(string(b))); err != nil {
			return nil, fmt.Errorf("flatfs at %s: %w", dir, err)
		}
	}
	return f, nil
}

// parseShardFunc reads a shard id such as /repo/flatfs/shard/v1/next-to-last/2.
func parseShardFunc(id string) (func(string) string, error) {
	parts := strings.Split(strings.TrimPrefix(id, "/"), "/")
	if len(parts) != 6 || parts[0] != "repo" || parts[1] != "flatfs" || parts[2] != "shard" || parts[3] != "v1" {
		return nil, fmt.Errorf("unknown shard function %q", id)
	}
	n, err := strconv.Atoi(parts[5])
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("bad shard length in %q", id)
	}
	switch parts[4] {
	case "prefix":
		return shardPrefix(n), nil
	case "suffix":
		return shardSuffix(n), nil
	case "next-to-last":
		return shardNextToLast(n), nil
	}
	return nil, fmt.Errorf("unknown shard function %q", id)
}

// The shard functions match go-ds-flatfs, padding short names with "_".
func shardPrefix(n int) func(string) string {
	pad := strings.Repeat("_", n)
	return func(s string) string { return (s + pad)[:n] }
}

func shardSuffix(n int) func(string) string {
	pad := strings.Repeat("_", n)
	return func(s string) string { s = pad + s; return s[len(s)-n:] }
}

func shardNextToLast(n int) func(string) string {
	pad := strings.Repeat("_", n+1)
	return func(s string) string {
		s = pad + s
		off := len(s) - n - 1
		return s[off : off+n]
	}
}

func (f *flatfs) name(key string) string {
	k := strings.TrimPrefix(key, "/")
	if f.hexNames {
		return hex.EncodeToString([]byte(k))
	}
	return k
}

func (f *flatfs) key(name string) (string, bool) {
	if f.hexNames {
		b, err := hex.DecodeString(name)
		if err != nil {
			return "", false
		}
		return "/" + string(b), true
	}
	return "/" + name, true
}

func (f *flatfs) Get(key string) ([]byte, error) {
	n := f.name(key)
	if n == "" || strings.ContainsAny(n, `/\`) {
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(f.dir, f.shard(n), n+".data"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

func (f *flatfs) Query(prefix string, fn func(string, int) error) error {
	shards, err := os.ReadDir(f.dir)
	if err != nil {
		return err
	}
	for _, sd := range shards {
		if !sd.IsDir() {
			continue // SHARDING, _README, diskUsage.cache
		}
		ents, err := os.ReadDir(filepath.Join(f.dir, sd.Name()))
		if err != nil {
			return err
		}
		for _, e := range ents {
			n, ok := strings.CutSuffix(e.Name(), ".data")
			if !ok || e.IsDir() {
				continue // temp files from an interrupted put
			}
			k, ok := f.key(n)
			if !ok || !strings.HasPrefix(k, prefix) {
				continue
			}
			info, err := e.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue // deleted by a running daemon's GC
			}
			if err != nil {
				return err
			}
			if err := fn(k, int(info.Size())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *flatfs) Close() error { return nil }
