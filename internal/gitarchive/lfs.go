package gitarchive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ipfs/go-cid"
)

// LFSObject is one Git LFS object: its sha256 oid, size, and its bytes as
// a UnixFS file.
type LFSObject struct {
	OID  string `cbor:"oid"`
	Size int64  `cbor:"size"`
	File Link   `cbor:"file"`
}

// LFSChunk is one block of a snapshot's LFS list.
type LFSChunk struct {
	Objects []LFSObject `cbor:"objects"`
}

// lfsChunkSize is how many objects one LFSChunk lists.
const lfsChunkSize = 2000

// localLFS lists the LFS objects in the repo's lfs/objects/aa/bb/<oid>,
// as git-lfs stores them. Anything else there (temp files, odd names) is
// left alone.
func localLFS(gitDir string) (map[string]string, error) {
	root := filepath.Join(gitDir, "lfs", "objects")
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && p == root {
				return filepath.SkipDir
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		oid := d.Name()
		rel, _ := filepath.Rel(root, p)
		if isOID(oid) && filepath.ToSlash(rel) == oid[:2]+"/"+oid[2:4]+"/"+oid {
			out[oid] = p
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	return out, err
}

func isOID(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// storedLFS is every LFS oid the snapshots up to prevRoot hold. Without a
// way to read them, it is empty, and every local object is listed again
// (their blocks are still not stored twice when Have knows them).
func storedLFS(get BlockGetter, prevRoot cid.Cid) map[string]bool {
	known := map[string]bool{}
	if get == nil || !prevRoot.Defined() {
		return known
	}
	chain, err := Chain(get, prevRoot)
	if err != nil {
		return known
	}
	for _, s := range chain {
		objs, _ := readLFS(get, s.Record)
		for _, o := range objs {
			known[o.OID] = true
		}
	}
	return known
}

// readLFS reads a snapshot's LFS list.
func readLFS(get BlockGetter, r *Record) ([]LFSObject, error) {
	var out []LFSObject
	for _, l := range r.LFS {
		b, err := get(l.Cid)
		if err != nil {
			return out, err
		}
		var ch LFSChunk
		if err := decMode.Unmarshal(b, &ch); err != nil {
			return out, fmt.Errorf("LFS list %s: %w", l.Cid, err)
		}
		for _, o := range ch.Objects {
			if !isOID(o.OID) || o.Size < 0 || !o.File.Defined() {
				return out, fmt.Errorf("LFS list %s has a malformed entry", l.Cid)
			}
		}
		out = append(out, ch.Objects...)
	}
	return out, nil
}

// newLFS is the local LFS objects no earlier snapshot holds, sorted.
func newLFS(local map[string]string, known map[string]bool) []string {
	var oids []string
	for oid := range local {
		if !known[oid] {
			oids = append(oids, oid)
		}
	}
	sort.Strings(oids)
	return oids
}

// checkLFS keeps the objects whose bytes hash to their oid; the rest get
// a note, and no blocks.
func checkLFS(ctx context.Context, local map[string]string, oids []string) ([]string, []string, error) {
	var good, notes []string
	for _, oid := range oids {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		got, err := hashFile(local[oid])
		if err != nil {
			return nil, nil, err
		}
		if got != oid {
			notes = append(notes, fmt.Sprintf("LFS object %s is corrupt (its bytes hash to %s); skipped", oid, got))
			continue
		}
		good = append(good, oid)
	}
	return good, notes, nil
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeLFS stores each object, checking again that its bytes hash to its
// oid as they are read, and the list in chunks.
func writeLFS(ctx context.Context, local map[string]string, oids []string, put BlockSink) ([]Link, int, error) {
	var objs []LFSObject
	for _, oid := range oids {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		f, err := os.Open(local[oid])
		if err != nil {
			return nil, 0, err
		}
		cw := &countWriter{h: sha256.New()}
		c, err := putFile(io.TeeReader(f, cw), put)
		f.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("LFS object %s: %w", oid, err)
		}
		if got := hex.EncodeToString(cw.h.Sum(nil)); got != oid {
			return nil, 0, fmt.Errorf("LFS object %s changed while it was stored", oid)
		}
		objs = append(objs, LFSObject{OID: oid, Size: cw.n, File: Link{c}})
	}
	var links []Link
	for i := 0; i < len(objs); i += lfsChunkSize {
		c, b, err := encodeBlock(LFSChunk{Objects: objs[i:min(i+lfsChunkSize, len(objs))]})
		if err != nil {
			return nil, 0, err
		}
		if err := put(c, b); err != nil {
			return nil, 0, err
		}
		links = append(links, Link{c})
	}
	return links, len(objs), nil
}

type countWriter struct {
	h hash.Hash
	n int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return w.h.Write(p)
}

// fetchLFS runs git lfs fetch --all, so objects only the LFS server has are
// stored too. It is the one step that uses the network and the user's
// credentials, so only an explicit option or a trusted config turns it on;
// hooks stay off. A failure is a note: the local objects are still stored.
func (g *Git) fetchLFS(ctx context.Context) string {
	c := exec.CommandContext(ctx, g.bin(), "-c", "core.hooksPath="+os.DevNull, "-c", "core.fsmonitor=false",
		"--git-dir="+g.Dir, "lfs", "fetch", "--all")
	c.Env = append(gitEnv(), "GIT_TERMINAL_PROMPT=0")
	out, err := c.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return fmt.Sprintf("git lfs fetch --all failed; storing local LFS objects only (%v: %s)", err, msg)
	}
	return ""
}

// restoreLFS writes every snapshot's LFS objects into dest's lfs/objects,
// checking each against its oid.
func restoreLFS(get BlockGetter, chain []Snapshot, gitDir string) (int, error) {
	n := 0
	for _, s := range chain {
		objs, err := readLFS(get, s.Record)
		if err != nil {
			return n, err
		}
		for _, o := range objs {
			dir := filepath.Join(gitDir, "lfs", "objects", o.OID[:2], o.OID[2:4])
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return n, err
			}
			p := filepath.Join(dir, o.OID)
			if err := writeChecked(get, o, p); err != nil {
				return n, fmt.Errorf("LFS object %s: %w", o.OID, err)
			}
			n++
		}
	}
	return n, nil
}

func writeChecked(get BlockGetter, o LFSObject, p string) error {
	f, err := os.CreateTemp(filepath.Dir(p), "tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	h := sha256.New()
	n, err := readFile(get, o.File.Cid, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != o.Size || hex.EncodeToString(h.Sum(nil)) != o.OID {
		return errors.New("stored bytes do not match the oid")
	}
	return os.Rename(f.Name(), p)
}
