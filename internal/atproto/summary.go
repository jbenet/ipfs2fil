package atproto

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/ipfs/go-cid"
)

// Summary is what the explorer shows of one account: its profile, posts,
// and a count of every record collection, with the records themselves for
// the collections that are not likes, follows and the like. It also says
// what the archive holds of the account, in the account's own terms.
type Summary struct {
	DID     string         `json:"did"`
	Name    string         `json:"name"`             // @handle from the pin name, else the DID
	Handle  string         `json:"handle,omitempty"` // from the DID document, when the archive has it
	PDS     string         `json:"pds,omitempty"`    // the account's server, from the DID document
	Rev     string         `json:"rev"`
	Time    string         `json:"time,omitempty"` // when the commit was made, from its revision
	Commit  string         `json:"commit"`
	Profile map[string]any `json:"profile,omitempty"`
	Counts  map[string]int `json:"counts"`
	Posts   []Post         `json:"posts"`
	// Records holds, per collection not shown elsewhere, up to MaxRecords
	// records as [rkey, CID, record].
	Records map[string][][3]any `json:"records"`
	// Missing counts records the archive does not hold.
	Missing int `json:"missing,omitempty"`
	// Blobs counts the blobs the records link and how many the archive holds.
	Blobs BlobStats `json:"blobs"`
	// DIDDoc and PLCLog are the CIDs of the identity documents pinned beside
	// the commit, when there are any.
	DIDDoc string `json:"didDoc,omitempty"`
	PLCLog string `json:"plcLog,omitempty"`
	// Files names every record and blob as a file, for the Files page:
	// <collection>/<rkey>.json and blobs/<name>.<ext>.
	Files []File `json:"-"`
}

// BlobStats counts the blobs (images, video, other files) records link.
type BlobStats struct {
	Linked int   `json:"linked"`
	Held   int   `json:"held"`
	Bytes  int64 `json:"bytes"` // of the held ones
	Images int   `json:"images"`
	Videos int   `json:"videos"`
}

// File is one record or blob, named as the Files page shows it.
type File struct {
	Path string
	CID  cid.Cid
}

// Post is one app.bsky.feed.post, flattened for the page.
type Post struct {
	RKey      string  `json:"k"`
	CID       string  `json:"c"`
	CreatedAt string  `json:"t"`
	Text      string  `json:"x,omitempty"`
	Reply     string  `json:"r,omitempty"`  // at:// URI of the post replied to
	Root      string  `json:"rt,omitempty"` // at:// URI of the thread's first post, when not Reply
	Quote     string  `json:"q,omitempty"`  // at:// URI of the post quoted
	Images    [][]any `json:"i,omitempty"`  // [blob CID, alt, mime, width, height]; no size when unknown
	Video     []any   `json:"v,omitempty"`  // [blob CID, alt, mime, width, height]
	Link      []any   `json:"l,omitempty"`  // [uri, title, description, thumb blob CID]
	Langs     []any   `json:"lg,omitempty"`
	// Facets mark links, mentions and tags in Text as [start, end, type,
	// value], in UTF-16 code units as JavaScript counts them; type is
	// "link", "mention" (value a DID) or "tag".
	Facets [][4]any `json:"f,omitempty"`
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

// pointers are collections whose records only point at other accounts'
// records and never link blobs, so they are listed but not read.
var pointers = map[string]bool{
	"app.bsky.feed.like": true, "app.bsky.feed.repost": true, "app.bsky.graph.follow": true,
	"app.bsky.graph.block": true, "app.bsky.graph.listitem": true, "app.bsky.graph.verification": true,
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
	if t, ok := TIDTime(cm.Rev); ok {
		s.Time = t.UTC().Format(time.RFC3339)
	}
	if s.Name == "" {
		s.Name = cm.DID
	}
	blobs := &blobNames{seen: map[string]bool{}, get: get, s: s}
	err = Walk(get, cm.Data, func(key string, rc cid.Cid) error {
		coll, rkey, ok := strings.Cut(key, "/")
		if !ok {
			return nil
		}
		s.Counts[coll]++
		if pointers[coll] {
			// listed, not read: they never link blobs
			if _, err := get(rc); err != nil {
				s.Missing++
			} else {
				s.Files = append(s.Files, File{Path: coll + "/" + safe(rkey) + ".json", CID: rc})
			}
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
		s.Files = append(s.Files, File{Path: coll + "/" + safe(rkey) + ".json", CID: rc})
		switch coll {
		case "app.bsky.feed.post":
			p := post(rkey, rc, rec)
			s.Posts = append(s.Posts, p)
			stem := rkey
			if len(p.CreatedAt) >= 10 {
				stem = p.CreatedAt[:10] + " " + rkey
			}
			blobs.add(rec, stem)
		case "app.bsky.actor.profile":
			if rkey == "self" {
				s.Profile = profile(rec)
			}
			blobs.add(rec, "profile")
		default:
			if !shown[coll] && len(s.Records[coll]) < MaxRecords {
				s.Records[coll] = append(s.Records[coll], [3]any{rkey, rc.String(), rec})
			}
			blobs.add(rec, path.Base(strings.ReplaceAll(coll, ".", "/"))+" "+rkey)
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

// blobNames names the blobs records link, each once, after the first
// record that links it: "2024-05-01 3kabc 1.jpg" for a post's first image,
// "profile avatar.jpg" for the avatar.
type blobNames struct {
	seen map[string]bool
	get  func(cid.Cid) ([]byte, error)
	s    *Summary
}

func (bn *blobNames) add(rec map[string]any, stem string) {
	n := 0
	var visit func(k string, v any)
	visit = func(k string, v any) {
		switch x := v.(type) {
		case map[string]any:
			if str(x, "$type") == "blob" || (x["cid"] != nil && x["mimeType"] != nil) {
				c, err := cid.Decode(blobCID(x))
				if err != nil || bn.seen[c.KeyString()] {
					return
				}
				bn.seen[c.KeyString()] = true
				st := &bn.s.Blobs
				st.Linked++
				b, err := bn.get(c)
				if err != nil {
					return
				}
				st.Held++
				st.Bytes += int64(len(b))
				mt := str(x, "mimeType")
				switch {
				case strings.HasPrefix(mt, "image/"):
					st.Images++
				case strings.HasPrefix(mt, "video/"):
					st.Videos++
				}
				n++
				label := fmt.Sprint(n)
				if stem == "profile" {
					label = k
				}
				bn.s.Files = append(bn.s.Files, File{Path: "blobs/" + safe(stem+" "+label) + extOf(mt), CID: c})
				return
			}
			keys := make([]string, 0, len(x))
			for kk := range x {
				keys = append(keys, kk)
			}
			sort.Strings(keys) // the same names every time
			// a post's images come in order under embed
			for _, kk := range keys {
				visit(kk, x[kk])
			}
		case []any:
			for _, e := range x {
				visit(k, e)
			}
		}
	}
	visit("", rec)
}

// extOf is the usual file extension for a blob's MIME type.
func extOf(mt string) string {
	switch mt {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/heic":
		return ".heic"
	case "image/avif":
		return ".avif"
	case "image/svg+xml":
		return ".svg"
	case "video/mp4":
		return ".mp4"
	case "video/quicktime":
		return ".mov"
	case "video/webm":
		return ".webm"
	case "text/plain":
		return ".txt"
	case "application/pdf":
		return ".pdf"
	}
	return ""
}

// safe keeps a name to one path segment.
func safe(s string) string {
	return strings.NewReplacer("/", "_", "\x00", "").Replace(s)
}

const tidChars = "234567abcdefghijklmnopqrstuvwxyz"

// TIDTime reads the time out of a TID (a revision or record key): 13
// base32-sortable characters holding microseconds since 1970 and a clock id.
func TIDTime(tid string) (time.Time, bool) {
	if len(tid) != 13 {
		return time.Time{}, false
	}
	var v uint64
	for i := 0; i < len(tid); i++ {
		d := strings.IndexByte(tidChars, tid[i])
		if d < 0 {
			return time.Time{}, false
		}
		v = v<<5 | uint64(d)
	}
	us := int64(v >> 10)
	return time.UnixMicro(us), true
}

// DIDDoc reads a DID document: its DID, the handle it claims (at://handle
// in alsoKnownAs) and the account's PDS.
func DIDDoc(b []byte) (did, handle, pds string, ok bool) {
	var d struct {
		ID          string   `json:"id"`
		AlsoKnownAs []string `json:"alsoKnownAs"`
		Service     []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Endpoint any    `json:"serviceEndpoint"`
		} `json:"service"`
	}
	if json.Unmarshal(b, &d) != nil || !strings.HasPrefix(d.ID, "did:") {
		return "", "", "", false
	}
	for _, a := range d.AlsoKnownAs {
		if h, ok := strings.CutPrefix(a, "at://"); ok {
			handle = h
			break
		}
	}
	for _, s := range d.Service {
		if e, ok := s.Endpoint.(string); ok && (s.Type == "AtprotoPersonalDataServer" || strings.HasSuffix(s.ID, "#atproto_pds")) {
			pds = e
		}
	}
	return d.ID, handle, pds, true
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
		if rt := str(obj(r, "root"), "uri"); rt != p.Reply {
			p.Root = rt
		}
	}
	if l, ok := rec["langs"].([]any); ok {
		p.Langs = l
	}
	p.Facets = facets(p.Text, rec["facets"])
	embed(&p, obj(rec, "embed"))
	return p
}

// facets reads rich text facets, turning their UTF-8 byte ranges into
// UTF-16 ones. Facets that do not fit the text are left out.
func facets(text string, v any) [][4]any {
	fs, _ := v.([]any)
	var out [][4]any
	for _, f := range fs {
		m, _ := f.(map[string]any)
		ix := obj(m, "index")
		bs, ok1 := num(ix["byteStart"])
		be, ok2 := num(ix["byteEnd"])
		if !ok1 || !ok2 || bs < 0 || be <= bs || be > len(text) || !utf8.ValidString(text[bs:be]) {
			continue
		}
		feats, _ := m["features"].([]any)
		for _, ft := range feats {
			fm, _ := ft.(map[string]any)
			var typ, val string
			switch str(fm, "$type") {
			case "app.bsky.richtext.facet#link":
				typ, val = "link", str(fm, "uri")
			case "app.bsky.richtext.facet#mention":
				typ, val = "mention", str(fm, "did")
			case "app.bsky.richtext.facet#tag":
				typ, val = "tag", str(fm, "tag")
			default:
				continue
			}
			out = append(out, [4]any{u16(text[:bs]), u16(text[:be]), typ, val})
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0].(int) < out[j][0].(int) })
	return out
}

// u16 is the length of s in UTF-16 code units.
func u16(s string) int {
	return len(utf16.Encode([]rune(s)))
}

func num(v any) (int, bool) {
	switch x := v.(type) {
	case uint64:
		return int(x), true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	case int:
		return x, true
	}
	return 0, false
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
				p.Images = append(p.Images, aspect([]any{c, str(m, "alt"), str(obj(m, "image"), "mimeType")}, m))
			}
		}
	case "app.bsky.embed.video":
		if c := blobCID(obj(e, "video")); c != "" {
			p.Video = aspect([]any{c, str(e, "alt"), str(obj(e, "video"), "mimeType")}, e)
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

// aspect adds the width and height an embed gives for its media, if any.
func aspect(v []any, m map[string]any) []any {
	ar := obj(m, "aspectRatio")
	w, ok1 := num(ar["width"])
	h, ok2 := num(ar["height"])
	if ok1 && ok2 && w > 0 && h > 0 {
		v = append(v, w, h)
	}
	return v
}
