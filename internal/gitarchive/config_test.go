package gitarchive

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/boxo/ipld/unixfs"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
)

func run(t *testing.T, path string, o Options, bs *Blocks) *Result {
	t.Helper()
	o.TempDir = t.TempDir()
	res, err := Archive(context.Background(), path, o, bs.Put)
	if err != nil {
		t.Fatal(err)
	}
	bs.roots = append(bs.roots, res.Root)
	return res
}

// listFolder walks a UnixFS folder: path -> file contents, or "-> target"
// for a symlink.
func listFolder(t *testing.T, bs *Blocks, c cid.Cid, prefix string, out map[string]string) {
	t.Helper()
	data, err := bs.Get(c)
	if err != nil {
		t.Fatal(err)
	}
	if c.Type() == cid.Raw {
		out[prefix] = string(data)
		return
	}
	b, _ := blocks.NewBlockWithCid(data, c)
	n, err := merkledag.DecodeProtobufBlock(b)
	if err != nil {
		t.Fatal(err)
	}
	fsn, err := unixfs.FSNodeFromBytes(n.(*merkledag.ProtoNode).Data())
	if err != nil {
		t.Fatal(err)
	}
	switch fsn.Type() {
	case unixfs.TDirectory:
		for _, l := range n.Links() {
			listFolder(t, bs, l.Cid, strings.TrimPrefix(prefix+"/"+l.Name, "/"), out)
		}
	case unixfs.TSymlink:
		out[prefix] = "-> " + string(fsn.Data())
	default:
		body, err := readAll(bs.Get, c)
		if err != nil {
			t.Fatal(err)
		}
		out[prefix] = string(body)
	}
}

func TestTipFolders(t *testing.T) {
	needGit(t)
	dir := newRepo(t, "")
	os.Symlink("big.txt", filepath.Join(dir, "link"))
	write(t, filepath.Join(dir, "run.sh"), "#!/bin/sh\n")
	os.Chmod(filepath.Join(dir, "run.sh"), 0o755)
	write(t, filepath.Join(dir, "empty"), "")
	git(t, dir, "add", "-A")
	// a submodule's commit, which the folder leaves out
	git(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+git(t, dir, "rev-parse", "HEAD")+",vendor/mod")
	git(t, dir, "commit", "-q", "-m", "more")

	bs := NewBlocks()
	none := run(t, dir, Options{}, bs)
	if len(none.Record.Tips) != 0 || none.Record.Settings != nil {
		t.Fatalf("tips by default: %+v", none.Record.Tips)
	}

	bs = NewBlocks()
	res := run(t, dir, Options{Tips: "default"}, bs)
	if len(res.Record.Tips) != 1 {
		t.Fatalf("tips %+v", res.Record.Tips)
	}
	tip := res.Record.Tips[0]
	if tip.Ref != "refs/heads/main" || tip.Commit != git(t, dir, "rev-parse", "main") || tip.Tree != git(t, dir, "rev-parse", "main^{tree}") || tip.Skipped != 1 {
		t.Fatalf("tip %+v", tip)
	}
	if res.Record.Settings["archive.tips"] != "default" {
		t.Fatalf("settings %v", res.Record.Settings)
	}
	got := map[string]string{}
	listFolder(t, bs, tip.Root.Cid, "", got)
	want := map[string]string{}
	for _, p := range strings.Split(git(t, dir, "ls-files"), "\n") {
		if p == "vendor/mod" {
			continue
		}
		if p == "link" {
			want[p] = "-> big.txt"
			continue
		}
		b, _ := os.ReadFile(filepath.Join(dir, p))
		want[p] = string(b)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("folder:\n%v\nwant\n%v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}

	// all: both branches, the shared files stored once
	n := len(bs.m)
	all := run(t, dir, Options{Tips: "all", Prev: res.Record, PrevRoot: res.Root, Have: bs.Has}, bs)
	if len(all.Record.Tips) != 2 || all.Record.Tips[1].Ref != tip.Ref || all.Record.Tips[1].Root != tip.Root {
		t.Fatalf("all: %+v", all.Record.Tips)
	}
	// feature adds one file: its blob and the changed root folder, plus the
	// record; no pack, as no objects are new
	if added := len(bs.m) - n; added > 3 {
		t.Fatalf("all added %d blocks", added)
	}
	// unchanged refs and settings: nothing new
	if again := run(t, dir, Options{Tips: "all", Prev: all.Record, PrevRoot: all.Root}, bs); !again.Unchanged {
		t.Fatal("same settings made a new snapshot")
	}
	// it still restores
	restore(t, bs, all.Root)

	// a ref by short name, and one that does not exist
	one := run(t, dir, Options{Tips: "feature, nope", NoConfig: true}, NewBlocks())
	if len(one.Record.Tips) != 1 || one.Record.Tips[0].Ref != "refs/heads/feature" || len(one.Notes) != 1 || !strings.Contains(one.Notes[0], "nope") {
		t.Fatalf("named: %+v %v", one.Record.Tips, one.Notes)
	}
}

func TestConfigFile(t *testing.T) {
	needGit(t)
	dir := newRepo(t, "")
	write(t, filepath.Join(dir, ConfigFile), "[archive]\n\ttips = default\n[ipfs2fil]\n\tcopies = 3\n[archive]\n\treflogs = true\n[include]\n\tpath = /etc/passwd\n")
	git(t, dir, "add", ConfigFile)
	git(t, dir, "commit", "-q", "-m", "config")
	// the working tree's edit does not count; HEAD's file does
	write(t, filepath.Join(dir, ConfigFile), "[archive]\n\ttips = all\n")

	bs := NewBlocks()
	res := run(t, dir, Options{}, bs)
	if len(res.Record.Tips) != 1 || res.Record.Tips[0].Ref != "refs/heads/main" {
		t.Fatalf("tips %+v", res.Record.Tips)
	}
	if !maps.Equal(res.Record.Instructions, map[string]string{"ipfs2fil.copies": "3"}) {
		t.Fatalf("instructions %v", res.Record.Instructions)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "archive.reflogs") {
		t.Fatalf("notes %v", res.Notes)
	}
	// options win over the file
	o := run(t, dir, Options{Tips: "none", Instructions: map[string]string{"ipfs2fil.copies": "2", "x.y": "z"}}, NewBlocks())
	if len(o.Record.Tips) != 0 || !maps.Equal(o.Record.Instructions, map[string]string{"ipfs2fil.copies": "2", "x.y": "z"}) {
		t.Fatalf("options: %+v %v", o.Record.Tips, o.Record.Instructions)
	}
	// NoConfig ignores it
	if nc := run(t, dir, Options{NoConfig: true}, NewBlocks()); len(nc.Record.Tips) != 0 || nc.Record.Instructions != nil {
		t.Fatalf("no config: %+v", nc.Record)
	}

	// an untrusted file cannot ask for every branch
	write(t, filepath.Join(dir, ConfigFile), "[archive]\n\ttips = all\n")
	git(t, dir, "commit", "-q", "-am", "all")
	u := run(t, dir, Options{UntrustedConfig: true}, NewBlocks())
	if len(u.Record.Tips) != 0 || len(u.Notes) != 1 || !strings.Contains(u.Notes[0], "not trusted") {
		t.Fatalf("untrusted: %+v %v", u.Record.Tips, u.Notes)
	}
	if tr := run(t, dir, Options{}, NewBlocks()); len(tr.Record.Tips) != 2 {
		t.Fatalf("trusted: %+v", tr.Record.Tips)
	}

	// a file that does not parse is ignored with a note
	write(t, filepath.Join(dir, ConfigFile), "[archive\n")
	git(t, dir, "commit", "-q", "-am", "broken")
	if b := run(t, dir, Options{}, NewBlocks()); len(b.Record.Tips) != 0 || len(b.Notes) != 1 {
		t.Fatalf("broken: %+v %v", b.Record.Tips, b.Notes)
	}
}
