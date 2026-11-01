package archive

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ipfs/go-cid"

	"github.com/jbenet/ipfs2fil/internal/repo"
)

// CARDir is where index/ keeps the layout of each CAR file the repo was
// made of: NNNN.json describes a file, NNNN.cids holds its sections' CIDs
// back to back (CIDs say their own length), and NNNN.suffix, if there is
// one, the bytes after its last section. With the blocks, that writes the
// file again byte for byte.
const CARDir = "cars"

type carIndex struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Prefix   []byte `json:"prefix"` // base64 in JSON
	Sections int    `json:"sections"`
	CIDs     string `json:"cids"`             // path under index/
	Suffix   string `json:"suffix,omitempty"` // path under index/
}

func writeCARLayouts(indexDir string, ls []repo.CARLayout) error {
	if len(ls) == 0 {
		return nil
	}
	dir := filepath.Join(indexDir, CARDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for i, l := range ls {
		base := fmt.Sprintf("%04d", i)
		ci := carIndex{Name: l.Name, Size: l.Size, SHA256: l.SHA256, Prefix: l.Prefix, Sections: len(l.Sections),
			CIDs: CARDir + "/" + base + ".cids"}
		var b []byte
		for _, c := range l.Sections {
			b = append(b, c.Bytes()...)
		}
		if err := os.WriteFile(filepath.Join(dir, base+".cids"), b, 0o644); err != nil {
			return err
		}
		if len(l.Suffix) > 0 {
			ci.Suffix = CARDir + "/" + base + ".suffix"
			if err := os.WriteFile(filepath.Join(dir, base+".suffix"), l.Suffix, 0o644); err != nil {
				return err
			}
		}
		j, err := json.MarshalIndent(ci, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, base+".json"), append(j, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// readCARLayouts reads the CAR layouts in an archive's index/, if it has
// any.
func readCARLayouts(indexDir string) ([]repo.CARLayout, error) {
	names, err := filepath.Glob(filepath.Join(indexDir, CARDir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []repo.CARLayout
	for _, name := range names { // Glob sorts, so NNNN order
		b, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		var ci carIndex
		if err := json.Unmarshal(b, &ci); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if ci.Name == "" || strings.Contains(ci.Name, "..") || filepath.IsAbs(ci.Name) {
			return nil, fmt.Errorf("%s: bad file name %q", name, ci.Name)
		}
		l := repo.CARLayout{Name: ci.Name, Size: ci.Size, SHA256: ci.SHA256, Prefix: ci.Prefix}
		f, err := os.Open(filepath.Join(indexDir, filepath.FromSlash(ci.CIDs)))
		if err != nil {
			return nil, err
		}
		br := bufio.NewReader(f)
		for {
			if _, err := br.Peek(1); errors.Is(err, io.EOF) {
				break
			}
			_, c, err := cid.CidFromReader(br)
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("%s: %w", ci.CIDs, err)
			}
			l.Sections = append(l.Sections, c)
		}
		f.Close()
		if len(l.Sections) != ci.Sections {
			return nil, fmt.Errorf("%s: %d sections, the index says %d", ci.CIDs, len(l.Sections), ci.Sections)
		}
		if ci.Suffix != "" {
			if l.Suffix, err = os.ReadFile(filepath.Join(indexDir, filepath.FromSlash(ci.Suffix))); err != nil {
				return nil, err
			}
		}
		out = append(out, l)
	}
	return out, nil
}
