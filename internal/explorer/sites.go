package explorer

import (
	"encoding/json"
	"strings"

	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/sitelist"
)

// SiteList is a site list's sites.json (pinned by a site archive): every
// site, the root it is archived at, and how that root was found.
type SiteList struct {
	Type    string           `json:"$type"` // "ipfs2fil.sites"
	Title   string           `json:"title,omitempty"`
	List    string           `json:"list,omitempty"`
	Checked string           `json:"checked,omitempty"`
	Sites   []sitelist.Entry `json:"sites"`
}

// SitesType is sites.json's $type.
const SitesType = "ipfs2fil.sites"

// ParseSites reads sites.json, saying whether b is one.
func ParseSites(b []byte) (*SiteList, bool) {
	var s SiteList
	if json.Unmarshal(b, &s) != nil || s.Type != SitesType {
		return nil, false
	}
	return &s, true
}

// siteInfo is a website in Library > Web sites: a root that a live
// endpoint serves (or served), unlike a web page, which is any HTML file.
type siteInfo struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	Kind    string `json:"kind"` // how it is found: dnslink, url, ens, ipns, ipfs
	Note    string `json:"note,omitempty"`
	Root    string `json:"root,omitempty"`
	How     string `json:"how,omitempty"`
	Since   string `json:"since,omitempty"`
	Checked string `json:"checked,omitempty"`
	Stale   bool   `json:"stale,omitempty"`
	Error   string `json:"error,omitempty"`
	// Live is where the site is served now, to check it against.
	Live    string             `json:"live,omitempty"`
	Earlier []sitelist.Earlier `json:"earlier,omitempty"`
	List    string             `json:"list,omitempty"` // the list's title
	// Path is the site's root under data/; Bytes its size.
	Path  string `json:"path,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
}

// liveURL is where a site of kind is served on the web.
func liveURL(e sitelist.Entry) string {
	host := e.Source
	switch e.Kind {
	case sitelist.KindDNSLink, sitelist.KindURL, sitelist.KindENS:
		host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
		host = strings.TrimPrefix(strings.TrimPrefix(host, "/ipns/"), "ipns://")
		host = strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(host, "/"), "."))
		if e.Kind == sitelist.KindENS && !strings.HasSuffix(host, ".limo") {
			host += ".limo"
		}
		return "https://" + host + "/"
	case sitelist.KindIPNS:
		key := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(e.Source, "/ipns/"), "ipns://"), "/")
		return "https://dweb.link/ipns/" + key + "/"
	}
	return ""
}

// siteInfos lists the sites of lists, each with where its root sits.
func siteInfos(m *manifest.Manifest, lists []*SiteList) []siteInfo {
	var out []siteInfo
	for _, l := range lists {
		for _, e := range l.Sites {
			si := siteInfo{Name: e.Name, Source: e.Source, Kind: e.Kind, Note: e.Note, Root: e.Root, How: e.How,
				Stale: e.Stale, Error: e.Error, Live: liveURL(e), Earlier: e.Earlier, List: l.Title}
			if e.Since != nil {
				si.Since = e.Since.UTC().Format("2006-01-02T15:04:05Z")
			}
			if e.Checked != nil {
				si.Checked = e.Checked.UTC().Format("2006-01-02T15:04:05Z")
			}
			best := -1
			for i, r := range m.Roots {
				if r.CID != e.Root || r.Kind == manifest.KindOrphan {
					continue
				}
				if best < 0 || r.Name == e.Name {
					best = i
				}
			}
			if best >= 0 {
				si.Path, si.Bytes = m.Roots[best].Path, m.Roots[best].Bytes
			}
			out = append(out, si)
		}
	}
	return out
}
