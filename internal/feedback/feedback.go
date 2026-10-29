// Package feedback is the developer feedback mode of 'ipfs2fil explore
// --feedback': the client of github.com/jbenet/feedback-dev-kit, unchanged,
// added to the explorer's pages as they are served, and a small Go port of
// the kit's server that files reports into a folder on this machine, in the
// kit's own journal and issue formats, where a coding agent can read them.
//
// Nothing here is ever written into an archive: pages get the kit only on
// their way out of the local explore server.
package feedback

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed assets/feedback.js.gz assets/feedback.css
var assets embed.FS

// Base is where the kit's script, its pages and its API are served.
const Base = "/_i2f/feedback"

// Server files feedback into Dir: reports land in Dir/inbox and become
// issues in Dir/issues/NNNN-slug.md. One Server per folder: it numbers
// issues by reading the folder.
type Server struct {
	Dir      string
	reporter string
	j        journal
	s        *store
	kick     chan struct{}
	now      func() time.Time
	logf     func(format string, args ...any)
}

// New makes a Server for dir, files whatever an earlier run left in its
// inbox, and keeps filing until stop is closed. reporter names who is filing
// (the local user); the browser never says.
func New(dir, reporter string, stop <-chan struct{}) (*Server, error) {
	s := &Server{
		Dir: dir, reporter: reporter,
		j:    journal{filepath.Join(dir, "inbox")},
		s:    &store{dir: filepath.Join(dir, "issues")},
		kick: make(chan struct{}, 1),
		now:  time.Now,
		logf: log.Printf,
	}
	if err := os.MkdirAll(s.j.dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.s.dir, 0o700); err != nil {
		return nil, err
	}
	s.ingest()
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-s.kick:
			case <-t.C:
			}
			s.ingest()
		}
	}()
	return s, nil
}

// ingest files every journaled report, oldest first. A report that fails
// stays in the journal for the next pass.
func (s *Server) ingest() {
	for _, e := range s.j.pending() {
		pics, err := s.j.pictures(e)
		if err != nil {
			s.j.refuse(e, "its pictures are missing: "+err.Error())
			continue
		}
		r := e.Request
		title := strings.Join(strings.Fields(r.Title), " ")
		if title == "" {
			title = titleFrom(r.Body)
		}
		if title == "" {
			title = lineTitle(r.Body)
		}
		if title == "" {
			title = "Untitled"
		}
		reporter := "unknown"
		if e.Reporter != nil {
			reporter = *e.Reporter
		}
		ctx := map[string]any{}
		for k, v := range r.Context {
			ctx[k] = v
		}
		ctx["journaledAt"] = e.ReceivedAt
		ctx["reporter"] = reporter
		ctx["reporterVerification"] = "local"
		is, err := s.s.create(draft{
			Title: title, Body: r.Body, Kind: r.Kind, Priority: r.Priority, Reporter: reporter,
			Page: r.Page, ClientID: e.ClientID, Created: e.ReceivedAt, Context: ctx,
			Pictures: pics, TokenOffset: r.ImageOffset,
		})
		if err != nil {
			s.logf("feedback: filing %s: %v (will retry)", e.ClientID, err)
			continue
		}
		if err := s.j.filed(e, filedRecord{
			ClientID: e.ClientID, IssueID: is.ID, Location: is.Location, Title: is.Title,
			ReceivedAt: e.ReceivedAt, FiledAt: isoTime(s.now()),
		}); err != nil {
			s.logf("feedback: %s filed as %s, but the journal says otherwise: %v", e.ClientID, is.ID, err)
		}
	}
}

// Handle adds the kit's script, its issues pages and its API to mux.
func (s *Server) Handle(mux *http.ServeMux) {
	mux.HandleFunc("GET "+Base+"/feedback.js", serveScript)
	mux.HandleFunc("GET "+Base+"/feedback.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, assets, "assets/feedback.css")
	})
	mux.HandleFunc("GET "+Base+"/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, Base+"/issues", http.StatusFound)
	})
	mux.HandleFunc("GET "+Base+"/issues", func(w http.ResponseWriter, r *http.Request) { issuesPage(w, "issues", "") })
	mux.HandleFunc("GET "+Base+"/issues/{id}", func(w http.ResponseWriter, r *http.Request) { issuesPage(w, "issue", r.PathValue("id")) })
	mux.HandleFunc(Base+"/api/feedback", s.feedback)
	mux.HandleFunc("GET "+Base+"/api/issues", s.issues)
	mux.HandleFunc("GET "+Base+"/api/issues/attachments/{name}", s.attachment)
	mux.HandleFunc(Base+"/api/issues/{id}", s.issue)
}

func serveScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	gz, err := assets.ReadFile("assets/feedback.js.gz")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(gz)
		return
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	io.Copy(w, zr)
}

// Tag is what Inject adds to each page.
const Tag = `<link rel="stylesheet" href="` + Base + `/feedback.css"><style>` + pageCSS + `</style><script src="` + Base + `/feedback.js" defer></script>`

// pageCSS places the kit on pages with no rail of their own: the floating
// button (whose position:fixed the kit's own [data-tip]{position:relative}
// overrides, so it is restated here) above the explorer's shortcut bar, and
// the status chip above the button. The issues pages get a column.
const pageCSS = `.fbk .floatfeedback[data-tip]{position:fixed;z-index:39}` +
	`body:has(footer.rail-wrap) .fbk .floatfeedback[data-tip]{bottom:calc(56px + env(safe-area-inset-bottom))}` +
	`.i2f-fbk-status{position:fixed;z-index:39;right:calc(18px + env(safe-area-inset-right));bottom:calc(66px + env(safe-area-inset-bottom))}` +
	`body:has(footer.rail-wrap) .i2f-fbk-status{bottom:calc(104px + env(safe-area-inset-bottom))}` +
	`.i2f-page{max-width:1180px;margin:0 auto;padding:24px 16px 80px}`

var pageTmpl = template.Must(template.New("p").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Explorer feedback · ipfs2fil</title>
<script>window.I2F_FEEDBACK = {page: {{.Page}}, id: {{.ID}}};</script>
` + Tag + `
<style>body{margin:0;background:var(--fbk-ground,#F1F4F0)}</style>
</head><body></body></html>
`))

func issuesPage(w http.ResponseWriter, page, id string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	pageTmpl.Execute(w, struct{ Page, ID string }{page, id})
}

// Inject adds the kit to every HTML page h serves.
func Inject(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || strings.HasPrefix(r.URL.Path, Base+"/") ||
			!(strings.HasSuffix(r.URL.Path, "/") || strings.HasSuffix(r.URL.Path, ".html")) {
			h.ServeHTTP(w, r)
			return
		}
		// A 304 or a range would keep or cut a page without the kit.
		for _, k := range []string{"If-Modified-Since", "If-None-Match", "Range", "If-Range"} {
			r.Header.Del(k)
		}
		rec := &recorder{header: http.Header{}, code: http.StatusOK}
		h.ServeHTTP(rec, r)
		body := rec.body.Bytes()
		if rec.code == http.StatusOK && strings.HasPrefix(rec.header.Get("Content-Type"), "text/html") {
			if i := bytes.LastIndex(body, []byte("</body>")); i >= 0 {
				body = append(append(append([]byte{}, body[:i]...), Tag...), body[i:]...)
			} else {
				body = append(body, Tag...)
			}
			rec.header.Del("Content-Length")
			rec.header.Del("ETag")
			rec.header.Del("Last-Modified")
			rec.header.Set("Cache-Control", "no-cache")
		}
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.code)
		w.Write(body)
	})
}

type recorder struct {
	header http.Header
	code   int
	wrote  bool
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.code, r.wrote = code, true
	}
}
func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.body.Write(b)
}

// ---- The API (feedback-dev-kit docs/SERVER.md §2)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	io.WriteString(w, jsonText(v, ""))
}

type errBody struct {
	Error    string `json:"error"`
	ClientID string `json:"clientId,omitempty"`
	State    string `json:"state,omitempty"`
}

// sameOrigin refuses a write from another site: any page open in the
// browser could otherwise post to a server on localhost.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return r.Header.Get("Sec-Fetch-Site") == "" || r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

var clientID = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

func isClientID(s string) bool { return clientID.MatchString(s) }

func (s *Server) feedback(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		id := r.URL.Query().Get("clientId")
		if id == "" {
			id = r.URL.Query().Get("client_id")
		}
		if !isClientID(id) {
			writeJSON(w, 400, errBody{Error: "Give a clientId."})
			return
		}
		switch st, f, reason := s.j.state(id); st {
		case "filed":
			writeJSON(w, 200, map[string]any{"state": "filed", "id": f.IssueID, "location": f.Location})
		case "refused":
			writeJSON(w, 200, map[string]any{"state": "refused", "error": reason})
		case "journaled":
			writeJSON(w, 200, map[string]any{"state": "journaled"})
		default:
			writeJSON(w, 404, map[string]any{"state": "unknown"})
		}
	case http.MethodPost:
		s.submit(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, 405, errBody{Error: "Method not allowed."})
	}
}

var submitMu sync.Mutex

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeJSON(w, 403, errBody{Error: "Feedback is taken only from the explorer's own pages."})
		return
	}
	text, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeJSON(w, 413, errBody{Error: "The report is too large. Remove a picture and send again."})
		return
	}
	var raw map[string]any
	if err != nil || json.Unmarshal(text, &raw) != nil || raw == nil {
		writeJSON(w, 400, errBody{Error: "The report did not arrive whole. It is still in your browser; it will be sent again."})
		return
	}
	given, _ := raw["clientId"].(string)
	if given == "" {
		given, _ = raw["client_id"].(string)
	}
	if given != "" && !isClientID(given) {
		writeJSON(w, 400, errBody{Error: "The report's client id is malformed."})
		return
	}
	id := given
	if id == "" {
		id = newClientID()
	}
	rep, pics, code, msg := checkReport(raw)
	if code != 0 {
		writeJSON(w, code, errBody{Error: msg, ClientID: id})
		return
	}
	submitMu.Lock()
	defer submitMu.Unlock()
	switch st, f, reason := s.j.state(id); st {
	case "refused":
		writeJSON(w, 422, errBody{Error: reason, ClientID: id, State: "refused"})
		return
	case "journaled":
		writeJSON(w, 202, map[string]any{"journaled": true, "clientId": id, "repeat": true})
		return
	case "filed":
		writeJSON(w, 202, map[string]any{"journaled": true, "clientId": id, "repeat": true, "id": f.IssueID})
		return
	}
	var who *string
	if s.reporter != "" {
		who = &s.reporter
	}
	e := journalEntry{V: 1, ClientID: id, ReceivedAt: isoTime(s.now()), Reporter: who, Request: rep}
	if err := s.j.write(e, pics); err != nil {
		writeJSON(w, 500, errBody{Error: fmt.Sprintf("The server could not save it: %v. It is still in your browser and will be sent again.", err)})
		return
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
	writeJSON(w, 202, map[string]any{"journaled": true, "clientId": id, "repeat": false})
}

func (s *Server) issues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	in := func(key, v string) bool {
		f := q.Get(key)
		return f == "" || oneOf(v, strings.Split(f, ","))
	}
	needle := strings.ToLower(strings.TrimSpace(q.Get("q")))
	out := []Issue{}
	for _, is := range s.s.list() {
		if !in("status", is.Status) || !in("kind", is.Kind) || !in("priority", is.Priority) {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(is.ID+"\n"+is.Title+"\n"+is.Body+"\n"+is.Reporter+"\n"+is.Page), needle) {
			continue
		}
		out = append(out, is)
	}
	writeJSON(w, 200, map[string]any{"issues": out, "destination": s.s.dir + "/NNNN-slug.md", "store": "files"})
}

var issueID = regexp.MustCompile(`^[\w.-]{1,64}$`)

func (s *Server) issue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !issueID.MatchString(id) {
		writeJSON(w, 404, errBody{Error: "Not found."})
		return
	}
	switch r.Method {
	case http.MethodGet:
		if hit := s.s.find(id); hit != nil {
			writeJSON(w, 200, map[string]any{"issue": hit.issue})
			return
		}
		writeJSON(w, 404, errBody{Error: "Not found."})
	case http.MethodPatch:
		if !sameOrigin(r) {
			writeJSON(w, 403, errBody{Error: "Issues are changed only from the explorer's own pages."})
			return
		}
		var p patch
		if json.NewDecoder(io.LimitReader(r.Body, 64_000)).Decode(&p) != nil {
			writeJSON(w, 400, errBody{Error: "Send JSON."})
			return
		}
		switch {
		case p.Status != nil && !oneOf(*p.Status, statuses):
			writeJSON(w, 400, errBody{Error: "Unknown status."})
			return
		case p.Priority != nil && !oneOf(*p.Priority, priorities):
			writeJSON(w, 400, errBody{Error: "Unknown priority."})
			return
		case p.Kind != nil && !oneOf(*p.Kind, kinds):
			writeJSON(w, 400, errBody{Error: "Unknown kind."})
			return
		}
		if p.Labels != nil {
			for _, l := range *p.Labels {
				if !labelOK.MatchString(l) {
					writeJSON(w, 400, errBody{Error: "Labels are short words."})
					return
				}
			}
		}
		is, err := s.s.update(id, p, s.now())
		if err != nil {
			writeJSON(w, 500, errBody{Error: err.Error()})
			return
		}
		if is == nil {
			writeJSON(w, 404, errBody{Error: "Not found."})
			return
		}
		writeJSON(w, 200, map[string]any{"issue": is})
	default:
		w.Header().Set("Allow", "GET, PATCH")
		writeJSON(w, 405, errBody{Error: "Method not allowed."})
	}
}

var labelOK = regexp.MustCompile(`^[\w:./-]{1,50}$`)

var pictureName = regexp.MustCompile(`^[\w.-]+\.(png|jpg|jpeg|gif|webp)$`)

func (s *Server) attachment(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	m := pictureName.FindStringSubmatch(name)
	if m == nil || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	b, err := os.ReadFile(filepath.Join(s.s.dir, "attachments", name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	types := map[string]string{"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}
	w.Header().Set("Content-Type", types[m[1]])
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.Write(b)
}
