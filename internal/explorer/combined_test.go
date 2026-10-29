package explorer

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestCombine: two archives become one data/ with a folder per repo, one
// list of pieces and roots, and totals that count a file both hold once.
func TestCombine(t *testing.T) {
	small(t)
	var as []Archive
	for _, name := range []string{"laptop", "server"} {
		dir := t.TempDir()
		m, entries := sample()
		if name == "server" {
			// the server has its own pieces but the same site photo
			for i := range m.Pieces {
				m.Pieces[i].PieceCID += "-server"
			}
			entries = entries[:4]
		}
		if err := Write(filepath.Join(dir, "explorer"), m, entries); err != nil {
			t.Fatal(err)
		}
		as = append(as, Archive{Name: name, Dir: dir})
	}
	files, m, err := combine(as, func(a Archive) string { return "../../" + a.Name + "/explorer/" })
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Pieces) != 8 || m.Pieces[5].Index != 5 || m.Pieces[5].PieceCID != "bafkzcib1-server" {
		t.Errorf("pieces: %+v", m.Pieces)
	}
	if r := m.Roots[4]; r.Path != "server/pins/recursive/bafysite" || r.FirstPiece != 4 || r.LastPiece != 5 {
		t.Errorf("server's first root: %+v", r)
	}
	if m.Source.Bytes != 18000 {
		t.Errorf("source bytes %d, want 18000", m.Source.Bytes)
	}
	var mt meta
	if err := json.Unmarshal(jsValue(files["meta.js"], "meta"), &mt); err != nil {
		t.Fatal(err)
	}
	if len(mt.Repos) != 2 || mt.Repos[1].Name != "server" || mt.Repos[1].Href != "../../server/explorer/" {
		t.Errorf("repos: %+v", mt.Repos)
	}
	// a.png (3000 B) and index.html (1000 B) are in both; they count once
	if got := mt.Types["image"]; got != (tally{3, 4000}) {
		t.Errorf("image totals %v, want 3 places, 4000 B", got)
	}
	if got := mt.Types["page"]; got != (tally{2, 1000}) {
		t.Errorf("page totals %v, want 2 places, 1000 B", got)
	}
	if got := mt.Parts["pins"]; got[1] != 5100 {
		t.Errorf("pins total %v, want 5100 B", got)
	}
	// both repos' folders at the top, and a.png under each
	var b map[string]*dirInfo
	if err := json.Unmarshal(jsValue(files["d/"+itoa(bucketOf("", mt.Buckets))+".js"], "d/"+itoa(bucketOf("", mt.Buckets))), &b); err != nil {
		t.Fatal(err)
	}
	if top := b[""]; top == nil || top.Folders != 2 {
		t.Errorf("top folder: %+v", top)
	}
	var all strings.Builder
	for name, f := range files {
		if strings.HasPrefix(name, "s/") {
			all.Write(f)
		}
	}
	for _, want := range []string{`"laptop/pins/recursive/bafysite/img"`, `"server/pins/recursive/bafysite/img"`, `"bafydata"`} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("shards lack %s", want)
		}
	}
}
