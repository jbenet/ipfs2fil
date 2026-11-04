package explorer

import (
	"path"
	"sort"
	"strings"

	"github.com/ipfs/go-cid"
)

// recordKind is the kind a record listed as a .json file gets now, for an
// index packed before the "post" and "record" kinds, which listed a Bluesky
// account's records and a CAR's record roots as code. It is "" when e keeps
// its kind.
func recordKind(e Entry) string {
	if (e.Kind != "code" && e.Kind != "text" && e.Kind != "other") || !strings.HasSuffix(e.Name, ".json") {
		return ""
	}
	c, err := cid.Decode(e.CID)
	if err != nil {
		return ""
	}
	if t := c.Type(); t != cid.DagCBOR && t != cid.DagJSON {
		return ""
	}
	if path.Base(path.Dir(e.Path)) == "app.bsky.feed.post" {
		return "post"
	}
	return "record"
}

// fixRecordKinds gives the entries an older index listed as code the kind
// recordKind says, moving them in the totals too, and says how many changed.
func fixRecordKinds(es []Entry, types map[string]tally) int {
	n := 0
	for i := range es {
		k := recordKind(es[i])
		if k == "" {
			continue
		}
		e := &es[i]
		if t, ok := types[e.Kind]; ok {
			types[e.Kind] = tally{max(0, t[0]-1), max(0, t[1]-e.Bytes)}
			if types[e.Kind][0] == 0 {
				delete(types, e.Kind)
			}
		}
		types[k] = add(types[k], e.Bytes)
		e.Kind = k
		n++
	}
	return n
}

// subRepos lists the paths of repos held inside the content: git working
// copies and bare repos, IPFS repos (kubo, js-ipfs, Helia) and CAR files,
// for Library > Repos. Bluesky accounts are listed in meta.Accounts.
func subRepos(byPath map[string]*Entry, children map[string][]*Entry) []string {
	var out []string
	for p, e := range byPath {
		if e.Kind != "folder" {
			if strings.EqualFold(path.Ext(e.Name), ".car") {
				out = append(out, p)
			}
			continue
		}
		if e.Name == ".git" {
			continue // its working copy is listed
		}
		has := map[string]string{}
		for _, c := range children[p] {
			has[c.Name] = c.Kind
		}
		dir := func(n string) bool { return has[n] == "folder" }
		file := func(n string) bool { k, ok := has[n]; return ok && k != "folder" }
		switch {
		case dir(".git"), // a working copy
			file("HEAD") && dir("objects") && dir("refs"),               // a bare git repo
			file("datastore_spec") || (dir("blocks") && file("config")), // kubo
			dir("blocks") && dir("datastore") && file("version"),        // js-ipfs
			dir("blockstore") && dir("datastore"):                       // Helia
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// maxWebRoots bounds the folders webRoots lists.
const maxWebRoots = 2000

// webRoots lists the folders in the content that look like a website's
// root: a folder holding an index.html (or index.htm), not inside another
// such folder. Library > Web sites lists them under Other archives, where
// the explorer shows them as sites (the folder as the site's root). Site
// lists' sites are listed on their own; withoutSites drops them.
func webRoots(byPath map[string]*Entry, children map[string][]*Entry) []string {
	var found []string
	for p, e := range byPath {
		if e.Kind != "folder" || p == "" {
			continue
		}
		for _, c := range children[p] {
			if c.Kind != "folder" && (strings.EqualFold(c.Name, "index.html") || strings.EqualFold(c.Name, "index.htm")) {
				found = append(found, p)
				break
			}
		}
	}
	sort.Strings(found)
	var out []string
	for _, p := range found {
		// sorted, so a folder comes before the folders inside it
		if n := len(out); n > 0 && strings.HasPrefix(p, out[n-1]+"/") {
			continue
		}
		out = append(out, p)
		if len(out) == maxWebRoots {
			break
		}
	}
	return out
}

// withoutSites drops from roots the site lists' sites and the folders in
// them.
func withoutSites(roots []string, sites []siteInfo) []string {
	var out []string
	for _, p := range roots {
		keep := true
		for _, s := range sites {
			if s.Path != "" && (p == s.Path || strings.HasPrefix(p, s.Path+"/")) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, p)
		}
	}
	return out
}
