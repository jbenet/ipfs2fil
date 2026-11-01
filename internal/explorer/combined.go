package explorer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// repoInfo is one archive in a combined index, listed in meta.repos.
type repoInfo struct {
	Name   string `json:"name"`             // its folder at the top of the combined data/
	Repo   string `json:"repo,omitempty"`   // the IPFS repo it was packed from
	Root   string `json:"root,omitempty"`   // the archive root
	Bytes  int64  `json:"bytes"`            // the repo's size
	Shared int64  `json:"shared,omitempty"` // of Bytes, what earlier repos hold already
	Pieces int    `json:"pieces"`
	Href   string `json:"href"` // the archive's own explorer, from the combined one
}

// combine builds one explorer index over several archives, so the pages can
// browse and search all of them at once. Each archive's data/ becomes the
// folder <name>/ of the combined data/, its roots and pieces are listed in one
// manifest, and the totals count a file that several repos hold once. It
// returns the index files keyed by path under idx/, and the manifest.
func combine(as []Archive, href func(Archive) string) (map[string][]byte, *manifest.Manifest, error) {
	cm := &manifest.Manifest{Format: manifest.Format}
	var (
		entries  []Entry
		repos    []repoInfo
		accounts []accountInfo
		atFiles  = map[int]json.RawMessage{} // at/<n>.js, n in accounts
		types    = map[string]tally{}
		parts    = map[string]tally{}
		seen     = map[string]bool{} // CIDs of files in earlier repos
		pieceOf  = map[string]int{}  // CID -> first piece, from earlier repos
		pieceAt  = map[string]int{}  // PieceCID -> index in cm
		copyAt   = map[[2]string]int{}
		snaps    []manifest.Snapshot
	)
	for _, a := range as {
		m, err := readManifest(a.Dir)
		if errors.Is(err, os.ErrNotExist) {
			m = &manifest.Manifest{}
			err = readJS(filepath.Join(a.Dir, "explorer", "idx", "manifest.js"), "manifest", m)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", a.Name, err)
		}
		es, mt, err := readEntries(filepath.Join(a.Dir, "explorer"), m)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", a.Name, err)
		}
		if mt.Types == nil {
			mt.Types = map[string]tally{}
		}
		fixRecordKinds(es, mt.Types)
		if cm.Tool == "" {
			cm.Tool, cm.Collection = m.Tool, m.Collection
		}

		// Pieces, once each, then everything that points at them.
		remap := make([]int, len(m.Pieces))
		for i, p := range m.Pieces {
			if k, ok := pieceAt[p.PieceCID]; ok && p.PieceCID != "" {
				remap[i] = k
				continue
			}
			remap[i] = len(cm.Pieces)
			pieceAt[p.PieceCID] = remap[i]
			p.Index = remap[i]
			p.Snapshot += len(snaps)
			cm.Pieces = append(cm.Pieces, p)
		}
		at := func(i int) int {
			if i < 0 || i >= len(remap) {
				return -1
			}
			return remap[i]
		}
		for _, s := range m.Snapshots {
			ns := manifest.Snapshot{Time: s.Time, Root: s.Root}
			for _, i := range s.NewPieces {
				if k := at(i); k >= 0 {
					ns.NewPieces = append(ns.NewPieces, k)
				}
			}
			snaps = append(snaps, ns)
		}
		for _, c := range m.Copies {
			key := [2]string{c.Provider, c.DataSetID}
			k, ok := copyAt[key]
			if !ok {
				k = len(cm.Copies)
				copyAt[key] = k
				cm.Copies = append(cm.Copies, manifest.Copy{Provider: c.Provider, DataSetID: c.DataSetID, Service: c.Service, PieceIDs: map[int]string{}})
			}
			for i, id := range c.PieceIDs {
				if j := at(i); j >= 0 {
					cm.Copies[k].PieceIDs[j] = id
				}
			}
		}
		top := folderName(a.Name)
		for _, r := range m.Roots {
			r.Path = top + "/" + r.Path
			r.FirstPiece, r.LastPiece = at(r.FirstPiece), at(r.LastPiece)
			cm.Roots = append(cm.Roots, r)
		}
		var shared int64
		if m.Shared != nil {
			shared = m.Shared.Bytes
			cm.Source.Blocks -= m.Shared.Blocks
		}
		cm.Source.Blocks += m.Source.Blocks
		cm.Source.Bytes += m.Source.Bytes - shared
		cm.Source.RepoVersion = max(cm.Source.RepoVersion, m.Source.RepoVersion)

		// The repo's folder, then its entries under it.
		folder := Entry{Name: a.Name, Path: top, CID: m.DataRoot, Kind: "folder", Piece: -1}
		for _, e := range es {
			if !strings.Contains(e.Path, "/") {
				folder.Bytes += e.Bytes
			}
		}
		for k, t := range mt.Types {
			types[k] = tally{types[k][0] + t[0], types[k][1] + t[1]}
		}
		for k, t := range mt.Parts {
			parts[k] = tally{parts[k][0] + t[0], parts[k][1] + t[1]}
		}
		for i := range es {
			e := &es[i]
			e.Path = top + "/" + e.Path
			e.Piece = at(e.Piece)
			// blocks an earlier repo stored are in its pieces
			if k, ok := pieceOf[e.CID]; ok && e.Piece < 0 {
				e.Piece = k
			}
			if e.Piece >= 0 && (folder.Piece < 0 || e.Piece < folder.Piece) {
				folder.Piece = e.Piece
			}
		}
		// A file an earlier repo holds counts there, not again here. The
		// repo's own totals count it where pack met it first: pins, then
		// MFS, then orphans.
		byPart := make([]int, len(es))
		for i := range byPart {
			byPart[i] = i
		}
		rank := map[string]int{"pins": 0, "mfs": 1, "orphans": 2, "": 3}
		sort.SliceStable(byPart, func(i, j int) bool { return rank[where(es[byPart[i]].Path)] < rank[where(es[byPart[j]].Path)] })
		mine := map[string]bool{}
		for _, i := range byPart {
			e := &es[i]
			if e.Kind == "folder" || e.CID == "" || mine[e.CID] {
				continue
			}
			mine[e.CID] = true
			if seen[e.CID] {
				types[e.Kind] = tally{types[e.Kind][0], max(0, types[e.Kind][1]-e.Bytes)}
				if w := where(e.Path); w != "" {
					parts[w] = tally{parts[w][0], max(0, parts[w][1]-e.Bytes)}
				}
			}
		}
		for c := range mine {
			seen[c] = true
		}
		for _, e := range es {
			if _, ok := pieceOf[e.CID]; !ok && e.CID != "" && e.Piece >= 0 {
				pieceOf[e.CID] = e.Piece
			}
		}
		entries = append(entries, folder)
		entries = append(entries, es...)
		// Each Bluesky account's posts come along, so the combined explorer
		// shows them with the files of every repo; an archive without them
		// links to the Bluesky App viewer in its own explorer.
		for i, ac := range mt.Accounts {
			if ac.Path != "" {
				ac.Path = top + "/" + ac.Path
			}
			b, err := os.ReadFile(filepath.Join(a.Dir, "explorer", "idx", "at", fmt.Sprintf("%d.js", i)))
			if err != nil {
				ac.Href = href(a) + fmt.Sprintf("browse.html?app=bsky&a=%d", i)
			} else {
				atFiles[len(accounts)] = jsValue(b, fmt.Sprintf("at/%d", i))
				ac.Href = fmt.Sprintf("browse.html?app=bsky&a=%d", len(accounts))
			}
			accounts = append(accounts, ac)
		}
		repos = append(repos, repoInfo{Name: a.Name, Repo: a.Repo, Root: m.Root, Bytes: m.Source.Bytes, Shared: shared, Pieces: len(m.Pieces), Href: href(a)})
	}

	// Snapshots in time order, with the pieces' numbers to match.
	order := make([]int, len(snaps))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return snaps[order[i]].Time.Before(snaps[order[j]].Time) })
	newAt := make([]int, len(snaps))
	for n, i := range order {
		newAt[i] = n
		cm.Snapshots = append(cm.Snapshots, snaps[i])
	}
	for i := range cm.Pieces {
		if s := cm.Pieces[i].Snapshot; s >= 0 && s < len(newAt) {
			cm.Pieces[i].Snapshot = newAt[s]
		}
	}

	files, err := buildIndex(cm, entries)
	if err != nil {
		return nil, nil, err
	}
	var mt meta
	if err := json.Unmarshal(jsValue(files["meta.js"], "meta"), &mt); err != nil {
		return nil, nil, err
	}
	mt.Types, mt.Parts, mt.Repos, mt.Accounts = types, parts, repos, accounts
	for i, v := range atFiles {
		k := fmt.Sprintf("at/%d", i)
		if files[k+".js"], err = jsFile(k, v); err != nil {
			return nil, nil, err
		}
	}
	if files["meta.js"], err = jsFile("meta", mt); err != nil {
		return nil, nil, err
	}
	return files, cm, nil
}

// folderName is an archive's name as one path segment.
func folderName(name string) string {
	name = strings.NewReplacer("/", "_", "\\", "_").Replace(name)
	if name == "" || name == "." || name == ".." {
		return "_" + name
	}
	return name
}

// readEntries reads back the entries and totals of the explorer at dir, with
// pieces numbered as in m. An index older than the current format is read
// through upgradeIndex. Entries keep their size, not the part of it other
// entries share (Dup), which the index does not record; the totals have it.
func readEntries(dir string, m *manifest.Manifest) ([]Entry, meta, error) {
	var mt meta
	get := func(name string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, "idx", filepath.FromSlash(name)))
	}
	switch f := indexFormat(dir); f {
	case IndexFormat:
	case 1:
		files, err := upgradeIndex(dir)
		if err != nil {
			return nil, mt, err
		}
		get = func(name string) ([]byte, error) {
			b, ok := files[name]
			if !ok {
				return nil, fmt.Errorf("no idx/%s in the upgraded index", name)
			}
			return b, nil
		}
	default:
		return nil, mt, fmt.Errorf("explorer index in %s is format %d; this ipfs2fil reads %d", dir, f, IndexFormat)
	}
	b, err := get("meta.js")
	if err != nil {
		return nil, mt, err
	}
	if err := json.Unmarshal(jsValue(b, "meta"), &mt); err != nil {
		return nil, mt, fmt.Errorf("reading %s meta: %w", dir, err)
	}
	pieceAt := map[string]int{}
	bundle := -1
	for i, p := range m.Pieces {
		pieceAt[p.PieceCID] = i
		if p.Part == manifest.PartBundle && p.BlockList != "" && (bundle < 0 || p.Snapshot >= m.Pieces[bundle].Snapshot) {
			bundle = i
		}
	}
	// Shards name this snapshot's bundle "": its PieceCID was not known yet.
	if bundle >= 0 {
		pieceAt[""] = bundle
	}
	var out []Entry
	for _, name := range mt.Shards {
		key := "s/" + name
		b, err := get(key + ".js")
		if err != nil {
			return nil, mt, err
		}
		var s struct {
			Dirs   []string            `json:"dirs"`
			Pieces []string            `json:"pieces"`
			Rows   [][]json.RawMessage `json:"rows"`
		}
		if err := json.Unmarshal(jsValue(b, key), &s); err != nil {
			return nil, mt, fmt.Errorf("reading %s %s: %w", dir, key, err)
		}
		for _, r := range s.Rows {
			if len(r) < 6 {
				return nil, mt, fmt.Errorf("%s: short row", key)
			}
			var d, piece int
			var e Entry
			seg := ""
			for i, v := range []any{&d, &e.Name, &e.Kind, &e.Bytes, &e.CID, &piece} {
				if err := json.Unmarshal(r[i], v); err != nil {
					return nil, mt, fmt.Errorf("%s: %w", key, err)
				}
			}
			if len(r) > 6 {
				if err := json.Unmarshal(r[6], &seg); err != nil {
					return nil, mt, fmt.Errorf("%s: %w", key, err)
				}
			}
			if d < 0 || d >= len(s.Dirs) {
				return nil, mt, fmt.Errorf("%s: bad dir %d", key, d)
			}
			e.Piece = -1
			if piece >= 0 && piece < len(s.Pieces) {
				if i, ok := pieceAt[s.Pieces[piece]]; ok {
					e.Piece = i
				}
			}
			if seg == "" {
				seg = e.Name
			}
			e.Path = seg
			if s.Dirs[d] != "" {
				e.Path = path.Join(s.Dirs[d], seg)
			}
			out = append(out, e)
		}
	}
	return out, mt, nil
}
