package feedback

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tinyPNG(t *testing.T) string {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b.Bytes())
}

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	fb, err := New(t.TempDir(), "juan", stop)
	if err != nil {
		t.Fatal(err)
	}
	fb.now = func() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) }
	mux := http.NewServeMux()
	mux.HandleFunc("/explorer/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".js") {
			w.Header().Set("Content-Type", "text/javascript")
			io.WriteString(w, "</body>")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", "40")
		io.WriteString(w, "<html><body><p>hi</p></body></html>")
	})
	fb.Handle(mux)
	ts := httptest.NewServer(Inject(mux))
	t.Cleanup(ts.Close)
	return fb, ts
}

func do(t *testing.T, ts *httptest.Server, method, path, origin string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestFileAReport(t *testing.T) {
	fb, ts := newTestServer(t)
	report := map[string]any{
		"clientId": "3f0a2c9e-6b1d-4b8e-9d2e-0c1f5a7b8e21",
		"body":     "The pieces table shows the wrong size. See ![chart.png](attachment:1)",
		"kind":     "bug", "priority": "P1", "page": "/explorer/pieces.html",
		"context": map[string]any{
			"url": "http://127.0.0.1:8484/explorer/pieces.html?token=abc", "reporter": "forged",
		},
		"screenshots": []string{tinyPNG(t)},
		"images":      []map[string]any{{"name": "chart.png", "dataUrl": tinyPNG(t)}},
		"imageOffset": 1,
	}
	if code, out := do(t, ts, "POST", Base+"/api/feedback", "http://evil.example", report); code != 403 {
		t.Fatalf("cross-site POST: %d %v", code, out)
	}
	code, out := do(t, ts, "POST", Base+"/api/feedback", ts.URL, report)
	if code != 202 || out["repeat"] != false {
		t.Fatalf("POST: %d %v", code, out)
	}
	fb.ingest()
	code, out = do(t, ts, "GET", Base+"/api/feedback?clientId=3f0a2c9e-6b1d-4b8e-9d2e-0c1f5a7b8e21", "", nil)
	if code != 200 || out["state"] != "filed" || out["id"] != "0001" {
		t.Fatalf("status: %d %v", code, out)
	}
	// A resend files nothing new.
	if code, out = do(t, ts, "POST", Base+"/api/feedback", ts.URL, report); code != 202 || out["repeat"] != true || out["id"] != "0001" {
		t.Fatalf("resend: %d %v", code, out)
	}

	b, err := os.ReadFile(filepath.Join(fb.Dir, "issues", "0001-the-pieces-table-shows-the-wrong-size.md"))
	if err != nil {
		t.Fatal(err)
	}
	file := string(b)
	for _, want := range []string{
		`id: "0001"`,
		"title: The pieces table shows the wrong size\n",
		"status: open          # open",
		"priority: P1          # P0",
		"reporter: juan\n",
		"created: 2026-10-29T12:00:00.000Z\n",
		"screenshots: [attachments/0001-screenshot.png]\n",
		"attachments: [attachments/0001-screenshot.png, attachments/0001-image-1.png]\n",
		"client_id: 3f0a2c9e-6b1d-4b8e-9d2e-0c1f5a7b8e21\n",
		"See ![chart.png](attachments/0001-image-1.png)",
		"![Screenshot](attachments/0001-screenshot.png)",
		"```json context",
		`"reporter": "juan"`,
		"token=%5Bremoved%5D",
	} {
		if !strings.Contains(file, want) {
			t.Errorf("issue file lacks %q:\n%s", want, file)
		}
	}
	if strings.Contains(file, "forged") {
		t.Errorf("the page set its own reporter:\n%s", file)
	}
	if left, _ := filepath.Glob(filepath.Join(fb.Dir, "inbox", "*.json")); len(left) != 0 {
		t.Errorf("journal still holds %v", left)
	}

	code, out = do(t, ts, "GET", Base+"/api/issues", "", nil)
	issues, _ := out["issues"].([]any)
	if code != 200 || len(issues) != 1 {
		t.Fatalf("issues: %d %v", code, out)
	}
	is := issues[0].(map[string]any)
	if is["body"] != "The pieces table shows the wrong size. See ![chart.png](attachments/0001-image-1.png)" {
		t.Errorf("body read back as %q", is["body"])
	}

	if code, out = do(t, ts, "PATCH", Base+"/api/issues/0001", ts.URL, map[string]any{"status": "done"}); code != 200 {
		t.Fatalf("PATCH: %d %v", code, out)
	}
	_, out = do(t, ts, "GET", Base+"/api/issues/0001", "", nil)
	got := out["issue"].(map[string]any)
	if got["status"] != "done" || got["closedAt"] == nil || got["clientId"] != "3f0a2c9e-6b1d-4b8e-9d2e-0c1f5a7b8e21" {
		t.Errorf("after PATCH: %v", got)
	}

	res, err := http.Get(ts.URL + Base + "/api/issues/attachments/0001-screenshot.png")
	if err != nil || res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/png" {
		t.Errorf("attachment: %v %v", err, res)
	}
	for _, bad := range []string{"..%2Finbox%2Fx.png", ".hidden.png", "0001-screenshot.svg"} {
		if res, _ := http.Get(ts.URL + Base + "/api/issues/attachments/" + bad); res.StatusCode != 404 {
			t.Errorf("attachment %s: %d", bad, res.StatusCode)
		}
	}
}

func TestRefusals(t *testing.T) {
	_, ts := newTestServer(t)
	for _, c := range []struct {
		body map[string]any
		code int
	}{
		{map[string]any{"body": "  \\\n"}, 400},
		{map[string]any{"body": "hi there", "screenshots": []string{"data:image/svg+xml;base64,PHN2Zz4="}}, 400},
		{map[string]any{"clientId": "../../etc", "body": "hi there"}, 400},
	} {
		if code, out := do(t, ts, "POST", Base+"/api/feedback", ts.URL, c.body); code != c.code {
			t.Errorf("%v: %d %v", c.body, code, out)
		}
	}
}

func TestInject(t *testing.T) {
	_, ts := newTestServer(t)
	get := func(p string) string {
		req, _ := http.NewRequest("GET", ts.URL+p, nil)
		req.Header.Set("If-Modified-Since", time.Now().Format(http.TimeFormat))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	if page := get("/explorer/index.html"); !strings.Contains(page, Tag+"</body>") {
		t.Errorf("page without the kit: %s", page)
	}
	if js := get("/explorer/explorer.js"); strings.Contains(js, Tag) {
		t.Errorf("script got the kit: %s", js)
	}
	if page := get(Base + "/issues/0007"); !strings.Contains(page, `{page: "issue", id: "0007"}`) || !strings.Contains(page, Base+"/feedback.js") {
		t.Errorf("issue page: %s", page)
	}
	res, err := http.Get(ts.URL + Base + "/feedback.js")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("feedback.js: %v %v", err, res)
	}
	b, _ := io.ReadAll(res.Body)
	if len(b) < 100_000 {
		t.Errorf("feedback.js is %d bytes", len(b))
	}
}

func TestIssueFileRoundTrip(t *testing.T) {
	file := "---\n" +
		"id: \"0024\"\n" +
		"title: \"Fix: the \\\"rail\\\"\"\n" +
		"status: review        # open | triaged | agent-ready | in-progress | done\n" +
		"kind: request          # bug | request | question | chore\n" +
		"priority: P3          # P0 blocking | P1 serious | P2 normal | P3 someday\n" +
		"reporter: robin\n" +
		"page: /\n" +
		"created: 2026-09-29T20:02:29.311Z\n" +
		"labels: [ui, \"two words\"]\n" +
		"assignee: claude\n" +
		"---\n\nIt broke.\n\n**Done (N30).** Fixed.\n"
	is := parseIssue(file, "x")
	if is.Title != `Fix: the "rail"` || is.Status != "done" || is.Kind != "request" || len(is.Labels) != 2 || is.FixedIn == nil || *is.FixedIn != "N30" {
		t.Fatalf("parsed %+v", is)
	}
	once := serializeIssue(is)
	if !strings.Contains(once, "assignee: claude\n") {
		t.Errorf("lost an unmanaged line:\n%s", once)
	}
	if twice := serializeIssue(parseIssue(once, "x")); twice != once {
		t.Errorf("second rewrite changed the file:\n%s\n---\n%s", once, twice)
	}
}

func TestTitleFrom(t *testing.T) {
	for in, want := range map[string]string{
		"the late tile counts an order that is still proofing. Also other stuff": "The late tile counts an order that is still proofing",
		"- ![x](attachment:1)\n* [the link](http://x) is broken":                 "The link is broken",
		"```\ncode\n```":                                                         "Code",
		"\\":                                                                     "",
	} {
		if got := titleFrom(in); got != want {
			t.Errorf("titleFrom(%q) = %q, want %q", in, got, want)
		}
	}
}
