package explorer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// upgradeIndex reads the format 1 index in the explorer at dir and returns
// the same entries as a current index, keyed by path under idx/, so explore
// can show an archive packed before format 2 with the current pages. Nothing
// on disk changes: a pack of the same data keeps its old explorer/, and so
// its pieces.
func upgradeIndex(dir string) (map[string][]byte, error) {
	idx := filepath.Join(dir, "idx")
	var old struct {
		Format int              `json:"format"`
		Shards int              `json:"shards"`
		Types  map[string]tally `json:"types"`
		Parts  map[string]tally `json:"parts"`
	}
	if err := readJS(filepath.Join(idx, "meta.js"), "meta", &old); err != nil {
		return nil, err
	}
	if old.Format != 1 {
		return nil, fmt.Errorf("explorer index in %s is format %d; this ipfs2fil reads %d", dir, old.Format, IndexFormat)
	}
	var m manifest.Manifest
	if err := readJS(filepath.Join(idx, "manifest.js"), "manifest", &m); err != nil {
		return nil, err
	}
	var entries []Entry
	for n := 0; n < old.Shards; n++ {
		key := fmt.Sprintf("s/%d", n)
		var s struct {
			Dirs []string            `json:"dirs"`
			Rows [][]json.RawMessage `json:"rows"`
		}
		if err := readJS(filepath.Join(idx, "s", fmt.Sprintf("%d.js", n)), key, &s); err != nil {
			return nil, err
		}
		for _, r := range s.Rows {
			if len(r) < 6 {
				return nil, fmt.Errorf("%s: short row", key)
			}
			var d int
			var e Entry
			seg := ""
			for i, v := range []any{&d, &e.Name, &e.Kind, &e.Bytes, &e.CID, &e.Piece} {
				if err := json.Unmarshal(r[i], v); err != nil {
					return nil, fmt.Errorf("%s: %w", key, err)
				}
			}
			if len(r) > 6 {
				if err := json.Unmarshal(r[6], &seg); err != nil {
					return nil, fmt.Errorf("%s: %w", key, err)
				}
			}
			if d < 0 || d >= len(s.Dirs) {
				return nil, fmt.Errorf("%s: bad dir %d", key, d)
			}
			if seg == "" {
				seg = e.Name
			}
			e.Path = seg
			if s.Dirs[d] != "" {
				e.Path = path.Join(s.Dirs[d], seg)
			}
			entries = append(entries, e)
		}
	}
	files, err := buildIndex(&m, entries)
	if err != nil {
		return nil, err
	}
	// The old totals count each block once; the rows alone cannot.
	var mt meta
	if err := json.Unmarshal(jsValue(files["meta.js"], "meta"), &mt); err != nil {
		return nil, err
	}
	mt.Types, mt.Parts = old.Types, old.Parts
	if files["meta.js"], err = jsFile("meta", mt); err != nil {
		return nil, err
	}
	return files, nil
}

// readJS reads v from an index file holding I2F.put(key, v).
func readJS(p, key string, v any) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(jsValue(b, key), v); err != nil {
		return fmt.Errorf("reading %s: %w", p, err)
	}
	return nil
}

func jsValue(b []byte, key string) []byte {
	b = bytes.TrimSpace(b)
	b = bytes.TrimPrefix(b, fmt.Appendf(nil, "I2F.put(%q,", key))
	return bytes.TrimSuffix(b, []byte(");"))
}
