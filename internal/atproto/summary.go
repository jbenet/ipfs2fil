package atproto

import (
	"sort"
	"strings"

	"github.com/ipfs/go-cid"
)

// Summary is what the explorer shows of one account: its profile, posts,
// and a count of every record collection, with the records themselves for
// the collections that are not likes, follows and the like.
type Summary struct {
	DID     string         `json:"did"`
	Name    string         `json:"name"` // @handle from the pin name, else the DID
	Rev     string         `json:"rev"`
	Commit  string         `json:"commit"`
	Profile map[string]any `json:"profile,omitempty"`
	Counts  map[string]int `json:"counts"`
	Posts   []Post         `json:"posts"`
	// Records holds, per collection not shown elsewhere, up to MaxRecords
	// records as [rkey, CID, record].
	Records map[string][][3]any `json:"records"`
	// Missing counts records the archive does not hold.
	Missing int `json:"missing,omitempty"`
}

// Post is one app.bsky.feed.post, flattened for the page.
type Post struct {
	RKey      string   `json:"k"`
	CID       string   `json:"c"`
	CreatedAt string   `json:"t"`
	Text      string   `json:"x,omitempty"`
	Reply     string   `json:"r,omitempty"` // at:// URI of the post replied to
	Quote     string   `json:"q,omitempty"` // at:// URI of the post quoted
	Images    [][3]any `json:"i,omitempty"` // [blob CID, alt, mime]
	Video     []any    `json:"v,omitempty"` // [blob CID, alt, mime]
	Link      []any    `json:"l,omitempty"` // [uri, title, description, thumb blob CID]
	Langs     []any    `json:"lg,omitempty"`
}

// MaxRecords bounds how many records of one collection a summary keeps.
const MaxRecords = 500

// shown are collections the summary already shows (posts, the profile) or
// that are long lists of pointers to other accounts' records, which it only
// counts.
var shown = map[string]bool{
	"app.bsky.feed.post": true, "app.bsky.actor.profile": true, "app.bsky.feed.like": true,
	"app.bsky.feed.repost": true, "app.bsky.graph.follow": true, "app.bsky.graph.block": true,
	"app.bsky.graph.listitem": true, "app.bsky.graph.verification": true,
}

// Summarize reads the commit at root and every record under it. name is how
// the account is called; get returns block bytes.
func Summarize(get func(cid.Cid) ([]byte, error), root cid.Cid, name string) (*Summary, error) {
	data, err := get(root)
	if err != nil {
		return nil, err
	}
	cm, err := DecodeCommit(data)
	if err != nil {
		return nil, err
	}
	s := &Summary{DID: cm.DID, Name: name, Rev: cm.Rev, Commit: root.String(), Counts: map[string]int{}, Records: map[string][][3]any{}}
	if s.Name == "" {
		s.Name = cm.DID
	}
	err = Walk(get, cm.Data, func(key string, rc cid.Cid) error {
		coll, rkey, ok := strings.Cut(key, "/")
		if !ok {
			return nil
		}
		s.Counts[coll]++
		if (shown[coll] && coll != "app.bsky.feed.post" && coll != "app.bsky.actor.profile") || (!shown[coll] && len(s.Records[coll]) >= MaxRecords) {
			return nil
		}
		b, err := get(rc)
		if err != nil {
			s.Missing++
			return nil
		}
		rec, err := Record(b)
		if err != nil {
			s.Missing++
			return nil
		}
		switch coll {
		case "app.bsky.feed.post":
			s.Posts = append(s.Posts, post(rkey, rc, rec))
		case "app.bsky.actor.profile":
			if rkey == "self" {
				s.Profile = profile(rec)
			}
		default:
			s.Records[coll] = append(s.Records[coll], [3]any{rkey, rc.String(), rec})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Newest first; rkeys are timestamps, and break ties.
	sort.SliceStable(s.Posts, func(i, j int) bool {
		a, b := s.Posts[i], s.Posts[j]
		if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt > b.CreatedAt
		}
		return a.RKey > b.RKey
	})
	return s, nil
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func obj(m map[string]any, k string) map[string]any {
	o, _ := m[k].(map[string]any)
	return o
}

// blobCID reads a blob reference: {"$type": "blob", "ref": {"$link": cid}},
// or the legacy {"cid": cid}.
func blobCID(b map[string]any) string {
	if b == nil {
		return ""
	}
	if ref := obj(b, "ref"); ref != nil {
		return str(ref, "$link")
	}
	return str(b, "cid")
}

func profile(rec map[string]any) map[string]any {
	p := map[string]any{}
	for _, k := range []string{"displayName", "description", "createdAt"} {
		if v := str(rec, k); v != "" {
			p[k] = v
		}
	}
	for _, k := range []string{"avatar", "banner"} {
		if c := blobCID(obj(rec, k)); c != "" {
			p[k] = c
		}
	}
	return p
}

func post(rkey string, rc cid.Cid, rec map[string]any) Post {
	p := Post{RKey: rkey, CID: rc.String(), CreatedAt: str(rec, "createdAt"), Text: str(rec, "text")}
	if r := obj(rec, "reply"); r != nil {
		p.Reply = str(obj(r, "parent"), "uri")
	}
	if l, ok := rec["langs"].([]any); ok {
		p.Langs = l
	}
	embed(&p, obj(rec, "embed"))
	return p
}

// embed reads images, video, a link card and a quoted post, including the
// media-plus-quote form.
func embed(p *Post, e map[string]any) {
	if e == nil {
		return
	}
	switch str(e, "$type") {
	case "app.bsky.embed.images":
		imgs, _ := e["images"].([]any)
		for _, im := range imgs {
			m, _ := im.(map[string]any)
			if c := blobCID(obj(m, "image")); c != "" {
				p.Images = append(p.Images, [3]any{c, str(m, "alt"), str(obj(m, "image"), "mimeType")})
			}
		}
	case "app.bsky.embed.video":
		if c := blobCID(obj(e, "video")); c != "" {
			p.Video = []any{c, str(e, "alt"), str(obj(e, "video"), "mimeType")}
		}
	case "app.bsky.embed.external":
		x := obj(e, "external")
		p.Link = []any{str(x, "uri"), str(x, "title"), str(x, "description"), blobCID(obj(x, "thumb"))}
	case "app.bsky.embed.record":
		p.Quote = str(obj(e, "record"), "uri")
	case "app.bsky.embed.recordWithMedia":
		p.Quote = str(obj(obj(e, "record"), "record"), "uri")
		embed(p, obj(e, "media"))
	}
}
