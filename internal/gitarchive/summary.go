package gitarchive

import (
	"sort"

	"github.com/ipfs/go-cid"
)

// MaxSummaryCommits caps the commits a Summary lists; the log in the
// archive keeps every one.
const MaxSummaryCommits = 200_000

// Summary is what an explorer shows of a git archive: refs, the commit log
// across every snapshot, and the snapshots and packs.
type Summary struct {
	Name         string        `json:"name"`
	Root         string        `json:"root"`
	ObjectFormat string        `json:"objectFormat"`
	Head         string        `json:"head,omitempty"`
	Refs         [][3]string   `json:"refs"`                 // name, id, peeled id of an annotated tag
	Remotes      [][2]string   `json:"remotes,omitempty"`    // name, URL
	Tips         [][3]string   `json:"tips,omitempty"`       // ref, commit, folder CID
	Submodules   [][4]string   `json:"submodules,omitempty"` // path, URL, commit, record CID when archived
	Snapshots    []SnapSummary `json:"snapshots"`
	// BasePacks are the packs of the repos this one is packed against
	// (pack CID, idx CID), newest first: a fork's shared objects are there.
	BasePacks [][2]string `json:"basePacks,omitempty"`
	// Commits are [id, parents, tree, author, email, author time,
	// committer time, subject, signed], newest first.
	Commits   [][9]any `json:"commits"`
	Truncated int      `json:"truncated,omitempty"` // commits left out past MaxSummaryCommits
	Missing   int      `json:"missing,omitempty"`   // log chunks not in the archive
}

// SnapSummary is one snapshot: its record and what it added.
type SnapSummary struct {
	Root    string      `json:"root"`
	Commits int         `json:"commits"`
	Objects int         `json:"objects"`
	Bytes   int64       `json:"bytes"`
	Refs    int         `json:"refs"`
	Packs   [][2]string `json:"packs"`         // pack CID, idx CID
	LFS     int         `json:"lfs,omitempty"` // LFS objects added
	// Base marks a compacted snapshot, which stands alone; Compacted, a
	// snapshot one replaced, kept only as refs in its history (it
	// restores from the compacted one).
	Base      bool `json:"base,omitempty"`
	Compacted bool `json:"compacted,omitempty"`
}

// Summarize reads the snapshot at root and every one before it.
func Summarize(get BlockGetter, root cid.Cid, name string) (*Summary, error) {
	chain, err := Chain(get, root)
	if err != nil {
		return nil, err
	}
	last := chain[len(chain)-1].Record
	s := &Summary{Name: name, Root: root.String(), ObjectFormat: last.ObjectFormat, Head: last.Head}
	for _, r := range last.Refs {
		s.Refs = append(s.Refs, [3]string{r.Name, r.ID, r.Peeled})
	}
	for _, r := range last.Remotes {
		s.Remotes = append(s.Remotes, [2]string{r.Name, r.URL})
	}
	for _, m := range last.Submodules {
		rc := ""
		if m.Record != nil {
			rc = m.Record.String()
		}
		s.Submodules = append(s.Submodules, [4]string{m.Path, m.URL, m.Commit, rc})
	}
	for _, t := range last.Tips {
		s.Tips = append(s.Tips, [3]string{t.Ref, t.Commit, t.Root.String()})
	}
	var commits []Commit
	read := map[string]bool{} // a chunk is read once, however often it is named
	full := false             // MaxSummaryCommits reached: list no more
	for i := len(chain) - 1; i >= 0; i-- {
		snap := chain[i]
		ss := SnapSummary{Root: snap.Root.String(), Commits: snap.Record.Commits, Refs: len(snap.Record.Refs), LFS: snap.Record.LFSObjects, Base: snap.Record.Base}
		for _, p := range snap.Record.Packs {
			ss.Objects += p.Objects
			ss.Bytes += p.Size
			ss.Packs = append(ss.Packs, [2]string{p.Pack.String(), p.Idx.String()})
		}
		s.Snapshots = append(s.Snapshots, ss)
		listed := 0
		for _, l := range snap.Record.Log {
			if full {
				break
			}
			if read[l.KeyString()] {
				continue
			}
			read[l.KeyString()] = true
			b, err := get(l.Cid)
			if err != nil {
				s.Missing++
				continue
			}
			var ch LogChunk
			if err := decMode.Unmarshal(b, &ch); err != nil {
				s.Missing++
				continue
			}
			commits = append(commits, ch.Commits...)
			listed += len(ch.Commits)
			full = len(commits) >= MaxSummaryCommits
		}
		if full {
			s.Truncated += max(0, snap.Record.Commits-listed)
		}
	}
	if packs, based, err := packsToIndex(get, chain); err == nil && based {
		own := map[string]bool{}
		for _, snap := range chain {
			own[snap.Root.KeyString()] = true
		}
		for i := len(packs) - 1; i >= 0; i-- {
			if !own[packs[i].root.KeyString()] {
				s.BasePacks = append(s.BasePacks, [2]string{packs[i].Pack.Pack.String(), packs[i].Idx.String()})
			}
		}
	}
	if !full {
		commits = append(commits, baseCommits(get, chain, last, commits, MaxSummaryCommits-len(commits))...)
	}
	// A compacted snapshot's log has every commit; the snapshots it
	// replaced are listed after it, newest first.
	hist, err := History(get, chain[0].Record)
	if err != nil {
		s.Missing++
	}
	for i := len(hist) - 1; i >= 0; i-- {
		e := hist[i]
		s.Snapshots = append(s.Snapshots, SnapSummary{Root: e.Root, Commits: e.Commits, Refs: len(e.Refs), Packs: [][2]string{}, Compacted: true})
	}
	sort.SliceStable(commits, func(i, j int) bool { return commits[i].Committer.Time > commits[j].Committer.Time })
	if len(commits) > MaxSummaryCommits {
		s.Truncated += len(commits) - MaxSummaryCommits
		commits = commits[:MaxSummaryCommits]
	}
	for _, c := range commits {
		ps := c.Parents
		if ps == nil {
			ps = []string{}
		}
		s.Commits = append(s.Commits, [9]any{c.ID, ps, c.Tree, c.Author.Name, c.Author.Email, c.Author.Time, c.Committer.Time, c.Subject, c.Signed})
	}
	return s, nil
}

// IsRecord says whether the block at c decodes as a git archive record.
func IsRecord(get BlockGetter, c cid.Cid) bool {
	_, err := LoadRecord(get, c)
	return err == nil
}

// LatestOf picks the newest snapshot among roots: the record no other
// record names as Prev, or lists in its history (a compacted snapshot
// supersedes the ones it replaces). Roots that are not records are ignored.
func LatestOf(get BlockGetter, roots []cid.Cid) ([]cid.Cid, error) {
	var recs []cid.Cid
	prev := map[string]bool{}
	for _, c := range roots {
		r, err := LoadRecord(get, c)
		if err != nil {
			continue
		}
		recs = append(recs, c)
		if r.Prev != nil {
			prev[r.Prev.KeyString()] = true
		}
		hist, _ := History(get, r)
		for _, e := range hist {
			if hc, err := cid.Decode(e.Root); err == nil {
				prev[hc.KeyString()] = true
			}
		}
	}
	var heads []cid.Cid
	seen := map[string]bool{}
	for _, c := range recs {
		if !prev[c.KeyString()] && !seen[c.KeyString()] {
			heads = append(heads, c)
			seen[c.KeyString()] = true
		}
	}
	return heads, nil
}

// baseCommits lists the commits a repo's refs reach that are stored in its
// bases' logs rather than its own, as a fork's shared history is, up to
// limit. It reads each base's log once.
func baseCommits(get BlockGetter, chain []Snapshot, last *Record, own []Commit, limit int) []Commit {
	based := false
	for _, s := range chain {
		based = based || len(s.Record.Bases) > 0
	}
	if !based || limit <= 0 {
		return nil
	}
	// Every log chunk of every base chain, through the same walk restore does.
	all := map[string]Commit{}
	read := map[string]bool{}
	var walk func(chain []Snapshot, depth int)
	seen := map[string]bool{}
	walk = func(chain []Snapshot, depth int) {
		for _, s := range chain {
			for _, b := range s.Record.Bases {
				if seen[b.KeyString()] || depth >= maxSubmoduleDepth*2 || len(seen) > maxBaseRecords {
					continue
				}
				seen[b.KeyString()] = true
				bc, err := Chain(get, b.Cid)
				if err != nil {
					continue
				}
				for _, x := range bc {
					for _, l := range x.Record.Log {
						if read[l.KeyString()] || len(all) >= MaxSummaryCommits {
							continue
						}
						read[l.KeyString()] = true
						blk, err := get(l.Cid)
						if err != nil {
							continue
						}
						var ch LogChunk
						if decMode.Unmarshal(blk, &ch) != nil {
							continue
						}
						for _, c := range ch.Commits {
							all[c.ID] = c
						}
					}
				}
				walk(bc, depth+1)
			}
		}
	}
	walk(chain, 0)
	have := map[string]bool{}
	for _, c := range own {
		have[c.ID] = true
	}
	var queue []string
	for _, r := range last.Refs {
		queue = append(queue, r.ID)
		if r.Peeled != "" {
			queue = append(queue, r.Peeled)
		}
	}
	// Walk parents from the refs: through own commits, into the bases'.
	byID := map[string]Commit{}
	for _, c := range own {
		byID[c.ID] = c
	}
	var out []Commit
	visited := map[string]bool{}
	for len(queue) > 0 && len(out) < limit {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if visited[id] {
			continue
		}
		visited[id] = true
		c, ok := byID[id]
		if !ok {
			if c, ok = all[id]; !ok {
				continue
			}
			if !have[id] {
				out = append(out, c)
			}
		}
		queue = append(queue, c.Parents...)
	}
	return out
}
