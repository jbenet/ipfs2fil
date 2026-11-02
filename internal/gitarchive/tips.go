package gitarchive

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/boxo/ipld/unixfs"
	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	format "github.com/ipfs/go-ipld-format"
)

// Tip is a UnixFS folder of the files at a ref, so a gateway shows the
// code without git. It is derived from the packs: restore does not need it.
type Tip struct {
	Ref    string `cbor:"ref"`    // full ref name, or HEAD when detached
	Commit string `cbor:"commit"` // the commit the folder is the tree of
	Tree   string `cbor:"tree"`
	Root   Link   `cbor:"root"`
	// Skipped counts entries the folder leaves out: submodules, and names
	// a folder cannot hold (not UTF-8, ".", "..").
	Skipped int `cbor:"skipped,omitempty"`
}

// maxTreeDepth bounds how deep a tip folder goes.
const maxTreeDepth = 1024

// tipRefs picks the refs a tips setting names: none, default (what HEAD
// names), all (every branch), or refs by name, comma-separated.
func tipRefs(spec string, rec *Record) ([]Ref, []string) {
	var out []Ref
	var notes []string
	byName := map[string]Ref{}
	for _, r := range rec.Refs {
		byName[r.Name] = r
	}
	add := func(r Ref) {
		for _, o := range out {
			if o.Name == r.Name {
				return
			}
		}
		out = append(out, r)
	}
	for _, s := range strings.Split(spec, ",") {
		s = strings.TrimSpace(s)
		switch s {
		case "", "none":
		case "default":
			if r, ok := byName[rec.Head]; ok {
				add(r)
			} else if _, err := hex.DecodeString(rec.Head); err == nil && rec.Head != "" {
				add(Ref{Name: "HEAD", ID: rec.Head}) // detached
			}
		case "all":
			for _, r := range rec.Refs {
				if strings.HasPrefix(r.Name, "refs/heads/") {
					add(r)
				}
			}
		default:
			found := false
			for _, name := range []string{s, "refs/heads/" + s, "refs/tags/" + s} {
				if r, ok := byName[name]; ok {
					add(r)
					found = true
					break
				}
			}
			if !found {
				notes = append(notes, fmt.Sprintf("tips: no ref %q; skipped", s))
			}
		}
	}
	return out, notes
}

// writeTips stores a folder for each ref the setting names. A ref whose
// tree the previous snapshot already has a folder for reuses it.
func writeTips(ctx context.Context, g *Git, spec string, rec *Record, prev *Record, put BlockSink) ([]Tip, []string, error) {
	refs, notes := tipRefs(spec, rec)
	if len(refs) == 0 {
		return nil, notes, nil
	}
	// One rev-parse for every ref: its commit and its tree. A tag of a
	// tree or a blob has no commit, and gets no folder.
	args := []string{"rev-parse", "--verify", "-q", "--end-of-options"}
	for _, r := range refs {
		args = append(args, r.ID+"^{commit}", r.ID+"^{tree}")
	}
	out, err := g.run(ctx, nil, args...)
	lines := strings.Fields(string(out))
	if err != nil || len(lines) != 2*len(refs) {
		// Some ref is not a commit: ask one at a time.
		lines = lines[:0]
		for _, r := range refs {
			c, cerr := g.run(ctx, nil, "rev-parse", "--verify", "-q", "--end-of-options", r.ID+"^{commit}")
			t, terr := g.run(ctx, nil, "rev-parse", "--verify", "-q", "--end-of-options", r.ID+"^{tree}")
			if cerr != nil || terr != nil {
				lines = append(lines, "", "")
				continue
			}
			lines = append(lines, strings.TrimSpace(string(c)), strings.TrimSpace(string(t)))
		}
	}
	reuse := map[string]Tip{}
	if prev != nil {
		for _, t := range prev.Tips {
			reuse[t.Tree] = t
		}
	}
	cf, err := g.catFile(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer cf.close()
	b := &treeBuilder{ctx: ctx, cf: cf, f: g.Format, put: put, trees: map[string]built{}, blobs: map[string]format.Node{}}
	var tips []Tip
	for i, r := range refs {
		commit, tree := lines[2*i], lines[2*i+1]
		if commit == "" {
			notes = append(notes, fmt.Sprintf("tips: %s is not a commit; skipped", r.Name))
			continue
		}
		if t, ok := reuse[tree]; ok {
			tips = append(tips, Tip{Ref: r.Name, Commit: commit, Tree: tree, Root: t.Root, Skipped: t.Skipped})
			continue
		}
		id, err := hex.DecodeString(tree)
		if err != nil {
			return nil, nil, err
		}
		n, err := b.tree(id, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("tip folder of %s: %w", r.Name, err)
		}
		tips = append(tips, Tip{Ref: r.Name, Commit: commit, Tree: tree, Root: Link{n.node.Cid()}, Skipped: n.skipped})
	}
	return tips, notes, nil
}

type built struct {
	node    format.Node
	skipped int // entries left out, this tree and below
}

// treeBuilder turns git trees into UnixFS folders, each tree and blob once.
type treeBuilder struct {
	ctx   context.Context
	cf    *catFile
	f     Format
	put   BlockSink
	trees map[string]built
	blobs map[string]format.Node
}

func (b *treeBuilder) tree(id []byte, depth int) (built, error) {
	if n, ok := b.trees[string(id)]; ok {
		return n, nil
	}
	if depth > maxTreeDepth {
		return built{}, errors.New("trees nest too deep")
	}
	if err := b.ctx.Err(); err != nil {
		return built{}, err
	}
	typ, data, err := b.cf.get(id)
	if err != nil {
		return built{}, err
	}
	if typ != objTree {
		return built{}, fmt.Errorf("object %x is a %s, not a tree", id, TypeName(typ))
	}
	dag := sinkDAG{b.put}
	dir, err := uio.NewDirectory(dag)
	if err != nil {
		return built{}, err
	}
	dir.SetCidBuilder(fileBuilder)
	var res built
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		nul := bytes.IndexByte(data, 0)
		if sp < 0 || nul < sp || len(data) < nul+1+b.f.Size {
			return built{}, fmt.Errorf("tree %x is malformed", id)
		}
		mode, name := string(data[:sp]), string(data[sp+1:nul])
		child := data[nul+1 : nul+1+b.f.Size]
		data = data[nul+1+b.f.Size:]
		if !utf8.ValidString(name) || name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
			res.skipped++
			continue
		}
		var n format.Node
		switch mode {
		case "40000":
			sub, err := b.tree(child, depth+1)
			if err != nil {
				return built{}, err
			}
			n, res.skipped = sub.node, res.skipped+sub.skipped
		case "100644", "100755", "100664":
			if n, err = b.blob(child); err != nil {
				return built{}, err
			}
		case "120000":
			if n, err = b.symlink(child); err != nil {
				return built{}, err
			}
		default: // 160000, a submodule's commit: not in this repo
			res.skipped++
			continue
		}
		if err := dir.AddChild(b.ctx, name, n); err != nil {
			return built{}, err
		}
	}
	nd, err := dir.GetNode()
	if err != nil {
		return built{}, err
	}
	if err := b.put(nd.Cid(), nd.RawData()); err != nil {
		return built{}, err
	}
	res.node = nd
	b.trees[string(id)] = res
	return res, nil
}

func (b *treeBuilder) blob(id []byte) (format.Node, error) {
	if n, ok := b.blobs[string(id)]; ok {
		return n, nil
	}
	typ, data, err := b.cf.get(id)
	if err != nil {
		return nil, err
	}
	if typ != objBlob {
		return nil, fmt.Errorf("object %x is a %s, not a blob", id, TypeName(typ))
	}
	n, err := putFileNode(bytes.NewReader(data), b.put)
	if err != nil {
		return nil, err
	}
	b.blobs[string(id)] = n
	return n, nil
}

func (b *treeBuilder) symlink(id []byte) (format.Node, error) {
	_, data, err := b.cf.get(id)
	if err != nil {
		return nil, err
	}
	d, err := unixfs.SymlinkData(string(data))
	if err != nil {
		return nil, err
	}
	n := merkledag.NodeWithData(d)
	if err := n.SetCidBuilder(fileBuilder); err != nil {
		return nil, err
	}
	if err := b.put(n.Cid(), n.RawData()); err != nil {
		return nil, err
	}
	return n, nil
}
