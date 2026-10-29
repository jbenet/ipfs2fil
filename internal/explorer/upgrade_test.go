package explorer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An archive packed before index format 2 keeps its old explorer/; explore
// shows it with the current pages from an index rebuilt in memory.
func TestUpgradeIndex(t *testing.T) {
	dir := t.TempDir()
	idx := filepath.Join(dir, "explorer", "idx")
	files := map[string]string{
		"index.html":       "old page",
		"explorer.js":      "old script",
		"idx/manifest.js":  `I2F.put("manifest",{"root":"bafyroot","dataRoot":"bafydata","pieces":[{"pieceCid":"baga6ea4seaqaaa"},{"pieceCid":"baga6ea4seaqbbb"}],"roots":[{"cid":"bafypin","path":"pins/recursive/photos"}]});`,
		"idx/meta.js":      `I2F.put("meta",{"format":1,"entries":5,"dirs":4,"shards":1,"buckets":1,"types":{"image":[2,30]},"parts":{"pins":[2,30]},"pieces":[]});`,
		"idx/s/0.js":       `I2F.put("s/0",{"dirs":["","pins","pins/recursive","pins/recursive/photos"],"rows":[[0,"pins","folder",40,"",-1],[1,"recursive","folder",40,"",-1],[2,"photos","folder",40,"bafypin",0],[3,"a.jpg","image",20,"bafya",0],[3,"b.jpg","image",20,"bafyb",1,"b:jpg"]]});`,
		"idx/d/0.js":       `I2F.put("d/0",{});`,
		"../index/x.json":  "{}",
		"../pieces/.empty": "",
	}
	for name, body := range files {
		p := filepath.Join(dir, "explorer", filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(archiveFiles(dir))
	defer srv.Close()
	get := func(p string) string {
		t.Helper()
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s %v", p, resp.StatusCode, b, err)
		}
		return string(b)
	}
	if got := get("/explorer/explorer.js"); got == "old script" {
		t.Fatal("served the packed script, not this build's")
	}
	metaJS := get("/explorer/idx/meta.js")
	var mt meta
	if err := json.Unmarshal(jsValue([]byte(metaJS), "meta"), &mt); err != nil {
		t.Fatal(err)
	}
	if mt.Format != IndexFormat || mt.Entries != 5 || len(mt.Shards) != 1 || mt.Types["image"] != (tally{2, 30}) {
		t.Fatalf("meta = %s", metaJS)
	}
	shard := get("/explorer/idx/s/" + mt.Shards[0] + ".js")
	for _, want := range []string{`"b.jpg","image",20,"bafyb"`, `"b:jpg"`, `"baga6ea4seaqbbb"`, `"pins/recursive/photos"`} {
		if !strings.Contains(shard, want) {
			t.Errorf("shard lacks %s: %s", want, shard)
		}
	}
	get("/explorer/idx/d/0.js")
	if got := get("/explorer/idx/manifest.js"); !strings.Contains(got, "bafydata") {
		t.Errorf("manifest.js = %s", got)
	}
	// Nothing on disk changed.
	if b, _ := os.ReadFile(filepath.Join(idx, "meta.js")); !strings.Contains(string(b), `"format":1`) {
		t.Error("meta.js on disk was rewritten")
	}
}
