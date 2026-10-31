package explorer

// Thumbnails for the explorer's grid, lists and folder mosaics. explore
// makes them with the thumbnailers below, one per kind of file, and keeps
// them in a cache on disk so each is made once. To thumbnail another kind of
// file, add a thumbnailer to the list. What explore cannot make (a video's
// frame when there is no ffmpeg, say) the pages make in the browser and send
// back to be kept; see thumbs in viewers.js.

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"image"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
)

// thumbSource is a file to thumbnail.
type thumbSource struct {
	name  string // its name, for tools that go by the extension
	ext   string // lower case, without the dot; from the content type when the name has none
	kind  string // as KindOf
	ctype string
	size  int64
	r     io.ReadSeeker
	url   string // where explore serves it, for tools that read over HTTP
}

// A thumbnailer makes a picture of a file: JPEG, PNG or GIF bytes of any
// size, which explore then scales. It returns errNoTool when a program it
// needs is not installed, so the file is tried again once it is.
type thumbnailer struct {
	name string
	ok   func(f *thumbSource) bool
	make func(ctx context.Context, f *thumbSource, w int) ([]byte, error)
}

var errNoTool = errors.New("no program to make this thumbnail")

var thumbnailers = []thumbnailer{
	{"image", func(f *thumbSource) bool {
		return f.ctype == "image/jpeg" || f.ctype == "image/png" || f.ctype == "image/gif"
	}, readAll(maxThumbSource)},
	{"epub cover", exts("epub"), func(_ context.Context, f *thumbSource, _ int) ([]byte, error) { return epubCover(f.r), nil }},
	// Office and iWork files keep a preview picture inside their zip.
	{"office preview", exts("docx xlsx pptx odt ods odp odg pages key numbers"), zipPreview},
	{"pdf", exts("pdf"), pdfPage},
	{"video frame", func(f *thumbSource) bool { return f.kind == "video" }, videoFrame},
	{"audio cover", func(f *thumbSource) bool { return f.kind == "audio" }, audioCover},
	// On a Mac, Quick Look draws most other things: HEIC and raw photos,
	// fonts, Office files without a preview inside, videos without ffmpeg.
	{"quick look", func(f *thumbSource) bool {
		return runtime.GOOS == "darwin" && f.kind != "text" && f.kind != "code" && f.kind != "page" && f.kind != "archive" && f.kind != "disk" && f.size <= 1<<30
	}, quickLook},
}

func exts(list string) func(f *thumbSource) bool {
	set := map[string]bool{}
	for _, e := range strings.Fields(list) {
		set[e] = true
	}
	return func(f *thumbSource) bool { return set[f.ext] }
}

func readAll(limit int64) func(context.Context, *thumbSource, int) ([]byte, error) {
	return func(_ context.Context, f *thumbSource, _ int) ([]byte, error) {
		if f.size > limit {
			return nil, nil
		}
		return io.ReadAll(io.LimitReader(f.r, limit))
	}
}

func zipPreview(_ context.Context, f *thumbSource, _ int) ([]byte, error) {
	if f.size > maxThumbSource {
		return nil, nil
	}
	data, err := io.ReadAll(f.r)
	if err != nil {
		return nil, err
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, nil
	}
	for _, name := range []string{"docProps/thumbnail.jpeg", "docProps/thumbnail.png", "Thumbnails/thumbnail.png", "QuickLook/Thumbnail.jpg", "QuickLook/Preview.jpg", "preview.jpg", "preview-web.jpg"} {
		if r, err := z.Open(name); err == nil {
			b, _ := io.ReadAll(io.LimitReader(r, maxThumbSource))
			r.Close()
			if len(b) > 0 {
				return b, nil
			}
		}
	}
	return nil, nil
}

// pdfPage draws a PDF's first page with whichever of poppler's pdftoppm or
// MuPDF's mutool is installed.
func pdfPage(ctx context.Context, f *thumbSource, w int) ([]byte, error) {
	var run func(file, dir string) ([]byte, error)
	switch {
	case tool("pdftoppm") != "":
		run = func(file, dir string) ([]byte, error) {
			out := filepath.Join(dir, "page")
			if err := command(ctx, tool("pdftoppm"), "-png", "-f", "1", "-l", "1", "-singlefile", "-scale-to", strconv.Itoa(w*2), file, out).Run(); err != nil {
				return nil, err
			}
			return os.ReadFile(out + ".png")
		}
	case tool("mutool") != "":
		run = func(file, dir string) ([]byte, error) {
			out := filepath.Join(dir, "page.png")
			if err := command(ctx, tool("mutool"), "draw", "-q", "-w", strconv.Itoa(w*2), "-o", out, file, "1").Run(); err != nil {
				return nil, err
			}
			return os.ReadFile(out)
		}
	default:
		return nil, errNoTool
	}
	if f.size > 512<<20 {
		return nil, nil
	}
	return withTempFile(f, run)
}

// videoFrame picks a telling frame from the first seconds of a video with
// ffmpeg, which reads the file from explore over HTTP, so a long video is
// not copied out first.
func videoFrame(ctx context.Context, f *thumbSource, _ int) ([]byte, error) {
	if tool("ffmpeg") == "" {
		return nil, errNoTool
	}
	for _, at := range []string{"3", "0"} { // a video under 3 seconds has nothing at 3
		var out bytes.Buffer
		c := command(ctx, tool("ffmpeg"), "-v", "error", "-ss", at, "-i", f.url, "-an", "-sn", "-vf", "thumbnail=24", "-frames:v", "1", "-f", "image2pipe", "-c:v", "mjpeg", "-q:v", "3", "pipe:1")
		c.Stdout = &out
		if c.Run() == nil && out.Len() > 0 {
			return out.Bytes(), nil
		}
	}
	return nil, nil
}

// audioCover is the album art a music file carries, if any.
func audioCover(ctx context.Context, f *thumbSource, _ int) ([]byte, error) {
	if tool("ffmpeg") == "" {
		return nil, errNoTool
	}
	var out bytes.Buffer
	c := command(ctx, tool("ffmpeg"), "-v", "error", "-i", f.url, "-map", "0:v:0", "-frames:v", "1", "-f", "image2pipe", "-c:v", "mjpeg", "pipe:1")
	c.Stdout = &out
	if c.Run() != nil {
		return nil, nil
	}
	return out.Bytes(), nil
}

func quickLook(ctx context.Context, f *thumbSource, w int) ([]byte, error) {
	if tool("qlmanage") == "" {
		return nil, errNoTool
	}
	return withTempFile(f, func(file, dir string) ([]byte, error) {
		out := filepath.Join(dir, "out")
		os.Mkdir(out, 0o700)
		if err := command(ctx, tool("qlmanage"), "-t", "-s", strconv.Itoa(w*2), "-o", out, file).Run(); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(filepath.Join(out, filepath.Base(file)+".png"))
		if os.IsNotExist(err) {
			return nil, nil // Quick Look has nothing for this file
		}
		return b, err
	})
}

// withTempFile copies f out to a file named like it, for tools that only
// read files, and removes it after.
func withTempFile(f *thumbSource, run func(file, dir string) ([]byte, error)) ([]byte, error) {
	dir, err := os.MkdirTemp("", "ipfs2fil-thumb-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	name := "file"
	if f.ext != "" {
		name += "." + f.ext
	}
	file := filepath.Join(dir, name)
	out, err := os.Create(file)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(out, f.r); err != nil {
		out.Close()
		return nil, err
	}
	if err := out.Close(); err != nil {
		return nil, err
	}
	return run(file, dir)
}

var tools sync.Map // name -> path, "" when missing

func tool(name string) string {
	if p, ok := tools.Load(name); ok {
		return p.(string)
	}
	p, _ := exec.LookPath(name)
	tools.Store(name, p)
	return p
}

func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, name, args...)
	c.WaitDelay = time.Second
	return c
}

// thumbCache keeps made thumbnails on disk, under the user's cache folder,
// as <cid>-<width>.jpg, and <cid>-<width>.none for files that have none.
type thumbCache struct{ dir string }

func newThumbCache() thumbCache {
	d, err := os.UserCacheDir()
	if err != nil {
		return thumbCache{}
	}
	return thumbCache{filepath.Join(d, "ipfs2fil", "thumbs")}
}

func (t thumbCache) path(key, ext string) string {
	return filepath.Join(t.dir, strings.ReplaceAll(key, "/", "-")+ext)
}

// get returns the thumbnail, or none when the file is known to have none.
func (t thumbCache) get(key string) (b []byte, none bool) {
	if t.dir == "" {
		return nil, false
	}
	if b, err := os.ReadFile(t.path(key, ".jpg")); err == nil {
		return b, false
	}
	_, err := os.Stat(t.path(key, ".none"))
	return nil, err == nil
}

func (t thumbCache) put(key string, b []byte) {
	if t.dir == "" || os.MkdirAll(t.dir, 0o755) != nil {
		return
	}
	ext := ".jpg"
	if b == nil {
		ext = ".none"
	}
	tmp := t.path(key, ext) + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, t.path(key, ext))
	}
}

// makeThumb returns the thumbnail of f, at most w pixels a side, from the
// caches or the first thumbnailer that makes one. none says there is none
// to make; when neither, a program it needs is missing.
func (s *carBlocks) makeThumb(ctx context.Context, key string, f *thumbSource, w int) (b []byte, none bool) {
	if b, ok := s.thumb.get(key); ok {
		return b, b == nil
	}
	if b, none := s.thumbs.get(key); b != nil || none {
		s.thumb.put(key, b)
		return b, none
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	missing := false
	for _, t := range thumbnailers {
		if !t.ok(f) {
			continue
		}
		if _, err := f.r.Seek(0, io.SeekStart); err != nil {
			return nil, false
		}
		s.thumb.slots <- struct{}{}
		pic, err := t.make(ctx, f, w)
		<-s.thumb.slots
		if errors.Is(err, errNoTool) {
			missing = true
			continue
		}
		if ctx.Err() != nil {
			return nil, false // too slow this time; try again later
		}
		if len(pic) > 0 {
			if b = s.thumb.thumbnail(bytes.NewReader(pic), w); b != nil {
				break
			}
		}
	}
	if b == nil && missing {
		return nil, false
	}
	s.thumb.put(key, b)
	s.thumbs.put(key, b)
	return b, b == nil
}

// serveThumb answers ?w= for the file at c, read by rd: its thumbnail, a
// picture the browser can scale itself sent as it is, or 404.
func (s *carBlocks) serveThumb(w http.ResponseWriter, r *http.Request, c cid.Cid, rd io.ReadSeeker, size int64, name, ctype string, tw int) {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	if ext == "" {
		if es, _ := mime.ExtensionsByType(ctype); len(es) > 0 {
			ext = strings.TrimPrefix(es[0], ".")
		}
	}
	kind := KindOf("x."+ext, false)
	if kind == "other" {
		kind = strings.SplitN(ctype, "/", 2)[0]
	}
	u := "http://" + r.Host + r.URL.Path + "?name=" + url.QueryEscape(name)
	f := &thumbSource{name: name, ext: ext, kind: kind, ctype: ctype, size: size, r: rd, url: u}
	b, none := s.makeThumb(r.Context(), c.String()+"/"+strconv.Itoa(tw), f, tw)
	if b != nil {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Write(b)
		return
	}
	// SVG, WebP and the like the browser draws at any size.
	if strings.HasPrefix(ctype, "image/") && ctype != "image/heic" && ctype != "image/heif" && ctype != "image/tiff" {
		if _, err := rd.Seek(0, io.SeekStart); err == nil {
			w.Header().Set("Content-Type", ctype)
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if strings.HasPrefix(ctype, "image/svg") {
				w.Header().Set("Content-Security-Policy", "sandbox")
			}
			http.ServeContent(w, r, name, time.Time{}, rd)
			return
		}
	}
	if !none {
		w.Header().Set("Cache-Control", "no-store") // a program to make it may be installed later
	}
	http.Error(w, "no thumbnail for this file", http.StatusNotFound)
}

// keepThumb stores a thumbnail the browser made (a video's frame, say) for
// explore to serve from then on. The custom header keeps other sites from
// posting here: they cannot send it without a CORS preflight, which explore
// does not answer.
func (s *carBlocks) keepThumb(w http.ResponseWriter, r *http.Request) {
	c, err := cid.Decode(r.PathValue("cid"))
	tw, _ := strconv.Atoi(r.URL.Query().Get("w"))
	// Only the explorer's pages send these, not web pages from the archive
	// (served under localhost, the same origin as the explorer there).
	ref, _ := url.Parse(r.Referer())
	if err != nil || tw < 16 || tw > 2048 || r.Header.Get("X-I2F-Thumb") != "1" || ref == nil || !strings.Contains(ref.Path, "/explorer/") || ref.Host != r.Host {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	pic, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return
	}
	if _, _, err := image.DecodeConfig(bytes.NewReader(pic)); err != nil {
		http.Error(w, "not a picture", http.StatusBadRequest)
		return
	}
	b := s.thumb.thumbnail(bytes.NewReader(pic), tw)
	if b == nil {
		http.Error(w, "not a picture", http.StatusBadRequest)
		return
	}
	key := c.String() + "/" + strconv.Itoa(tw)
	s.thumb.put(key, b)
	s.thumbs.put(key, b)
	w.WriteHeader(http.StatusNoContent)
}
