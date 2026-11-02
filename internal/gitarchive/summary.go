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
	Refs         [][3]string   `json:"refs"`              // name, id, peeled id of an annotated tag
	Remotes      [][2]string   `json:"remotes,omitempty"` // name, URL
	Tips         [][3]string   `json:"tips,omitempty"`    // ref, commit, folder CID
	Snapshots    []SnapSummary `json:"snapshots"`
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
	Packs   [][2]string `json:"packs"` // pack CID, idx CID
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
	for _, t := range last.Tips {
		s.Tips = append(s.Tips, [3]string{t.Ref, t.Commit, t.Root.String()})
	}
	var commits []Commit
	for i := len(chain) - 1; i >= 0; i-- {
		snap := chain[i]
		ss := SnapSummary{Root: snap.Root.String(), Commits: snap.Record.Commits, Refs: len(snap.Record.Refs)}
		for _, p := range snap.Record.Packs {
			ss.Objects += p.Objects
			ss.Bytes += p.Size
			ss.Packs = append(ss.Packs, [2]string{p.Pack.String(), p.Idx.String()})
		}
		s.Snapshots = append(s.Snapshots, ss)
		for _, l := range snap.Record.Log {
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
		}
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
// record names as Prev. Roots that are not records are ignored.
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
