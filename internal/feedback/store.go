package feedback

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Issue is an issue as the kit's client reads it (feedback-dev-kit
// docs/REBUILD.md §12.1).
type Issue struct {
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	Status      string         `json:"status"`
	Kind        string         `json:"kind"`
	Priority    string         `json:"priority"`
	Reporter    string         `json:"reporter"`
	Page        string         `json:"page"`
	Labels      []string       `json:"labels"`
	Body        string         `json:"body"`
	Context     map[string]any `json:"context"`
	Created     string         `json:"created"`
	ClosedAt    *string        `json:"closedAt"`
	Location    string         `json:"location"`
	Screenshots []string       `json:"screenshots"`
	Attachments []string       `json:"attachments"`
	ClientID    *string        `json:"clientId"`
	FixedIn     *string        `json:"fixedIn"`

	// extra holds frontmatter lines this code does not manage, kept verbatim.
	extra []string
}

var (
	statuses   = []string{"open", "triaged", "agent-ready", "in-progress", "done"}
	kinds      = []string{"bug", "request", "question", "chore"}
	priorities = []string{"P0", "P1", "P2", "P3"}
)

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// store keeps issues as the kit's files store does: <dir>/NNNN-slug.md, with
// pictures in <dir>/attachments/ (feedback-dev-kit docs/SERVER.md §8), so the
// same tools and agents read them.
type store struct {
	dir string
	mu  sync.Mutex
}

var issueFile = regexp.MustCompile(`^(\d{4,})-([a-z0-9-]+)\.md$`)

type issueAt struct {
	file  string
	issue Issue
}

func (s *store) read() []issueAt {
	names, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []issueAt
	for _, n := range names {
		m := issueFile.FindStringSubmatch(n.Name())
		if m == nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, n.Name()))
		if err != nil {
			continue
		}
		is := parseIssue(string(b), m[1])
		is.Location = n.Name()
		out = append(out, issueAt{n.Name(), is})
	}
	return out
}

// list returns every issue, newest first.
func (s *store) list() []Issue {
	all := s.read()
	out := make([]Issue, 0, len(all))
	for _, a := range all {
		out = append(out, a.issue)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.Atoi(out[i].ID)
		b, _ := strconv.Atoi(out[j].ID)
		return a > b
	})
	return out
}

func (s *store) find(id string) *issueAt {
	for _, a := range s.read() {
		if a.issue.ID == id {
			return &a
		}
	}
	return nil
}

// draft is a report ready to file.
type draft struct {
	Title, Body, Kind, Priority, Reporter, Page, ClientID, Created string
	Context                                                        map[string]any
	Pictures                                                       []picture
	TokenOffset                                                    int
}

type picture struct {
	Kind        string // screenshot or image
	ContentType string
	Bytes       []byte
}

var extOf = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp"}

// create files d under the next number, or returns the issue an earlier
// create made for the same client id.
func (s *store) create(d draft) (Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.read()
	max := 0
	for _, a := range all {
		if d.ClientID != "" && a.issue.ClientID != nil && *a.issue.ClientID == d.ClientID {
			return a.issue, nil
		}
		if n, err := strconv.Atoi(a.issue.ID); err == nil && n > max {
			max = n
		}
	}
	id := fmt.Sprintf("%04d", max+1)
	var paths, shots []string
	shotNo, imageNo := 0, 0
	for _, p := range d.Pictures {
		var name string
		if p.Kind == "screenshot" {
			shotNo++
			name = id + "-screenshot"
			if shotNo > 1 {
				name += "-" + strconv.Itoa(shotNo)
			}
		} else {
			imageNo++
			name = id + "-image-" + strconv.Itoa(imageNo)
		}
		rel := "attachments/" + name + "." + extOf[p.ContentType]
		if err := writeAtomic(filepath.Join(s.dir, "attachments"), path.Base(rel), p.Bytes); err != nil {
			return Issue{}, err
		}
		paths = append(paths, rel)
		if p.Kind == "screenshot" {
			shots = append(shots, rel)
		}
	}
	body := attachmentToken.ReplaceAllStringFunc(d.Body, func(tok string) string {
		n, _ := strconv.Atoi(attachmentToken.FindStringSubmatch(tok)[1])
		if i := n - 1 + d.TokenOffset; i >= 0 && i < len(paths) {
			return "(" + paths[i] + ")"
		}
		return tok
	})
	is := Issue{
		ID: id, Title: d.Title, Status: "open", Kind: d.Kind, Priority: d.Priority,
		Reporter: d.Reporter, Page: d.Page, Labels: []string{}, Body: body, Context: d.Context,
		Created: d.Created, Screenshots: shots, Attachments: paths,
	}
	if d.ClientID != "" {
		c := d.ClientID
		is.ClientID = &c
		is.extra = []string{"client_id: " + c}
	}
	is.Location = id + "-" + slugify(d.Title) + ".md"
	if err := writeAtomic(s.dir, is.Location, []byte(serializeIssue(is))); err != nil {
		return Issue{}, err
	}
	return is, nil
}

var attachmentToken = regexp.MustCompile(`\(attachment:(\d+)\)`)

// patch is a change to an issue from the issues page.
type patch struct {
	Status   *string   `json:"status"`
	Priority *string   `json:"priority"`
	Kind     *string   `json:"kind"`
	Labels   *[]string `json:"labels"`
}

func (s *store) update(id string, p patch, now time.Time) (*Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hit := s.find(id)
	if hit == nil {
		return nil, nil
	}
	is := hit.issue
	if p.Status != nil && *p.Status != is.Status {
		is.Status = *p.Status
		is.ClosedAt = nil
		if is.Status == "done" {
			t := isoTime(now)
			is.ClosedAt = &t
		}
	}
	if p.Priority != nil {
		is.Priority = *p.Priority
	}
	if p.Kind != nil {
		is.Kind = *p.Kind
	}
	if p.Labels != nil {
		is.Labels = *p.Labels
	}
	if err := writeAtomic(s.dir, hit.file, []byte(serializeIssue(is))); err != nil {
		return nil, err
	}
	return &is, nil
}

func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// ---- The issue file format (feedback-dev-kit packages/feedback-server/src/stores/format.ts)

var (
	frontMatter = regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---\r?\n?(.*)$`)
	frontLine   = regexp.MustCompile(`^([a-zA-Z_][\w-]*):\s*(.*)$`)
	trailNote   = regexp.MustCompile(`\s+#.*$`)
	contextFace = regexp.MustCompile("(?s)```json context\r?\n(.*?)```")
	fixedNote   = regexp.MustCompile(`\*\*[^*]*?\(([A-Z]+\d+)\)`)
	bodyContext = regexp.MustCompile("(?m)^(\\s*)```json context\\b")
	needsQuotes = regexp.MustCompile(`^$|^[\d\s\[{'"]|[:#"\\\r\n,\]]|\s$`)
	itemQuotes  = regexp.MustCompile(`[,\]\s"'\[]`)
	slugJunk    = regexp.MustCompile(`[^a-z0-9]+`)
)

var managed = map[string]bool{
	"id": true, "title": true, "status": true, "kind": true, "priority": true, "reporter": true,
	"page": true, "created": true, "labels": true, "screenshots": true, "attachments": true,
	"screenshot": true, "attachment": true, "closed_at": true,
}

func parseIssue(file, fallbackID string) Issue {
	front, rest := "", file
	if m := frontMatter.FindStringSubmatch(strings.TrimSpace(file)); m != nil {
		front, rest = m[1], m[2]
	}
	scalars := map[string]string{}
	lists := map[string][]string{}
	var extra []string
	for _, line := range strings.Split(front, "\n") {
		line = strings.TrimRight(line, "\r")
		m := frontLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key, raw := m[1], strings.TrimSpace(m[2])
		if !managed[key] {
			extra = append(extra, strings.TrimRight(line, " \t"))
		}
		if key == "status" || key == "kind" || key == "priority" {
			raw = strings.TrimSpace(trailNote.ReplaceAllString(raw, ""))
		}
		if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
			var items []string
			for _, it := range strings.Split(raw[1:len(raw)-1], ",") {
				if it = unquote(strings.TrimSpace(it)); it != "" {
					items = append(items, it)
				}
			}
			lists[key] = items
		} else {
			scalars[key] = unquote(raw)
		}
	}
	str := func(k, d string) string {
		if v, ok := scalars[k]; ok {
			return v
		}
		return d
	}
	shots, ok := lists["screenshots"]
	if !ok {
		if l := str("screenshot", str("attachment", "")); l != "" {
			shots = []string{l}
		}
	}
	atts, ok := lists["attachments"]
	if !ok {
		atts = append([]string(nil), shots...)
	}
	for _, s := range shots {
		rest = strings.Replace(rest, "![Screenshot]("+s+")", "", 1)
	}
	body, ctx := strings.TrimSpace(rest), map[string]any(nil)
	if all := contextFace.FindAllStringSubmatchIndex(rest, -1); len(all) > 0 {
		f := all[len(all)-1]
		if json.Unmarshal([]byte(rest[f[2]:f[3]]), &ctx) != nil {
			ctx = nil
		}
		body = strings.TrimSpace(rest[:f[0]] + rest[f[1]:])
	}
	status := str("status", "open")
	if status == "review" {
		status = "done"
	}
	is := Issue{
		ID: str("id", fallbackID), Title: str("title", "(untitled)"), Status: status,
		Kind: str("kind", "bug"), Priority: str("priority", "P2"), Reporter: str("reporter", "unknown"),
		Page: str("page", ""), Created: str("created", ""), Labels: lists["labels"],
		Screenshots: shots, Attachments: atts, Body: body, Context: ctx, extra: extra,
	}
	if is.Labels == nil {
		is.Labels = []string{}
	}
	if is.Screenshots == nil {
		is.Screenshots = []string{}
	}
	if is.Attachments == nil {
		is.Attachments = []string{}
	}
	if v := str("closed_at", ""); v != "" {
		is.ClosedAt = &v
	}
	if v := str("client_id", ""); v != "" {
		is.ClientID = &v
	}
	if v := str("fixed_in", ""); v != "" {
		is.FixedIn = &v
	} else if m := fixedNote.FindStringSubmatch(body); m != nil {
		is.FixedIn = &m[1]
	}
	return is
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var v string
		if json.Unmarshal([]byte(s), &v) == nil {
			return v
		}
		return s[1 : len(s)-1]
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return s[1 : len(s)-1]
	}
	return s
}

// jsonText is JSON.stringify: no HTML escaping.
func jsonText(v any, indent string) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", indent)
	e.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

func quote(s string) string {
	if needsQuotes.MatchString(s) {
		return jsonText(s, "")
	}
	return s
}

func serializeIssue(is Issue) string {
	pad := func(s string, n int) string { return s + strings.Repeat(" ", max(0, n-len(s))) }
	labels := make([]string, len(is.Labels))
	for i, l := range is.Labels {
		labels[i] = l
		if itemQuotes.MatchString(l) {
			labels[i] = jsonText(l, "")
		}
	}
	lines := []string{
		"---",
		"id: " + jsonText(is.ID, ""),
		"title: " + quote(is.Title),
		"status: " + pad(is.Status, 14) + "# open | triaged | agent-ready | in-progress | done",
		"kind: " + pad(is.Kind, 16) + "# bug | request | question | chore",
		"priority: " + pad(is.Priority, 12) + "# P0 blocking | P1 serious | P2 normal | P3 someday",
		"reporter: " + quote(is.Reporter),
		"page: " + quote(is.Page),
		"created: " + is.Created,
	}
	if is.ClosedAt != nil {
		lines = append(lines, "closed_at: "+*is.ClosedAt)
	}
	lines = append(lines, "labels: ["+strings.Join(labels, ", ")+"]")
	if len(is.Screenshots) > 0 {
		lines = append(lines, "screenshots: ["+strings.Join(is.Screenshots, ", ")+"]")
	}
	if len(is.Attachments) > 0 {
		lines = append(lines, "attachments: ["+strings.Join(is.Attachments, ", ")+"]")
	}
	lines = append(lines, is.extra...)
	lines = append(lines, "---", "", bodyContext.ReplaceAllString(strings.TrimSpace(is.Body), "$1```json"), "")
	for _, s := range is.Screenshots {
		lines = append(lines, "![Screenshot]("+s+")", "")
	}
	if is.Context != nil {
		// A fence inside the context would end the block early.
		lines = append(lines, "```json context", strings.ReplaceAll(jsonText(is.Context, "  "), "```", "`​``"), "```", "")
	}
	return strings.Join(lines, "\n")
}

func slugify(title string) string {
	s := strings.Trim(slugJunk.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > 48 {
		s = strings.TrimSuffix(s[:48], "-")
	}
	if s == "" {
		return "issue"
	}
	return s
}

// writeAtomic writes name in dir so that it is either absent or complete:
// a temporary dot-file, fsynced, then renamed into place.
func writeAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
