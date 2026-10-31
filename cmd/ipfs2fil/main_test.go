package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/atproto/atprototest"
	"github.com/jbenet/ipfs2fil/internal/car"
	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/repo"
	"github.com/jbenet/ipfs2fil/internal/repo/repotest"
)

// ipfs2fil runs the CLI in-process and returns stdout and stderr.
func ipfs2fil(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	err := run(context.Background(), args, &out, &errb)
	return out.String(), errb.String(), err
}

func TestScanJSON(t *testing.T) {
	dir := fixture(t, "v18")
	out, _, err := ipfs2fil(t, "scan", "--repo", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var s scanResult
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("%v in %q", err, out)
	}
	if s.RepoVersion != 18 || s.Blocks == 0 || s.Bytes == 0 || s.Pins == 0 || s.MFSRoot == "" {
		t.Errorf("scan = %+v", s)
	}
	if s.Copies != 3 || s.Pieces < 2 || s.Monthly <= 0 || s.OneTime <= 0 || !s.AtMinimum {
		t.Errorf("estimate = %+v", s)
	}
}

func TestScanText(t *testing.T) {
	dir := fixture(t, "v00")
	out, _, err := ipfs2fil(t, "scan", "--repo", dir, "--copies", "2")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fs-repo 0", "blocks", "pins", "USDFC/month for 2 copies", "(the minimum: each copy costs at least 0.12 USDFC/month, which covers up to about 49.2 GiB)"} {
		if !strings.Contains(out, want) {
			t.Errorf("scan output lacks %q:\n%s", want, out)
		}
	}
}

func TestErrorsSayWhatToDo(t *testing.T) {
	_, _, err := ipfs2fil(t, "scan", "--repo", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "point --repo at") {
		t.Errorf("not a repo: %v", err)
	}
	t.Setenv("IPFS_PATH", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	if _, _, err := ipfs2fil(t, "scan"); err == nil || !strings.Contains(err.Error(), "pass --repo") {
		t.Errorf("no repo: %v", err)
	}
	_, stderr, err := ipfs2fil(t, "nope")
	if err != errUsage || !strings.Contains(stderr, `no command "nope"`) || !strings.Contains(stderr, "Commands:") {
		t.Errorf("unknown command: %v %q", err, stderr)
	}
	_, stderr, err = ipfs2fil(t, "scan", "--bogus")
	if err != errUsage || !strings.Contains(stderr, "ipfs2fil help scan") {
		t.Errorf("bad flag: %v %q", err, stderr)
	}
	if _, _, err := ipfs2fil(t, "explore", "--out", t.TempDir()); err == nil || !strings.Contains(err.Error(), "ipfs2fil pack") {
		t.Errorf("explore with no archive: %v", err)
	}
}

func TestHelp(t *testing.T) {
	out, _, err := ipfs2fil(t, "help")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range order {
		if !strings.Contains(out, "\n  "+n+" ") {
			t.Errorf("usage lacks %s:\n%s", n, out)
		}
	}
	for n := range commands {
		out, _, err := ipfs2fil(t, "help", n)
		if err != nil || !strings.HasPrefix(out, "ipfs2fil "+n+": ") {
			t.Errorf("help %s: %v %q", n, err, out)
		}
	}
	for n := range commands {
		out, _, _ := ipfs2fil(t, "help", n)
		if n != "version" && n != "wallet" && !strings.Contains(out, "-repo") {
			t.Errorf("%s does not take --repo", n)
		}
	}
	for _, n := range []string{"status", "verify"} {
		if out, _, _ := ipfs2fil(t, "help", n); !strings.Contains(out, "-address") {
			t.Errorf("%s does not take --address", n)
		}
	}
	out, _, _ = ipfs2fil(t, "scan", "-h")
	if !strings.Contains(out, "-json") {
		t.Errorf("scan -h lacks flags: %q", out)
	}
}

func TestVersion(t *testing.T) {
	out, _, err := ipfs2fil(t, "--version")
	if err != nil || !strings.HasPrefix(out, "ipfs2fil ") {
		t.Errorf("version: %v %q", err, out)
	}
}

func TestProgress(t *testing.T) {
	var b bytes.Buffer
	now := time.Unix(0, 0)
	p := newProgress(&b, "uploading", 1000, true)
	p.now = func() time.Time { return now }
	p.start, p.last = now, now
	now = now.Add(10 * time.Second)
	p.Add(250)
	now = now.Add(10 * time.Second)
	p.Add(250)
	p.Done()
	got := b.String()
	for _, want := range []string{"[======                  ]  25% 250 B/1000 B 25 B/s ETA 30s", " 50% 500 B/1000 B in 20s (25 B/s)"} {
		if !strings.Contains(got, want) {
			t.Errorf("progress lacks %q:\n%s", want, got)
		}
	}
}

func TestProgressSpeed(t *testing.T) {
	// Skipped bytes fill the bar but not the speed, and the speed covers
	// the last speedWindow, so a fast start does not hide a slow end.
	var b bytes.Buffer
	now := time.Unix(0, 0)
	p := newProgress(&b, "uploading", 10000, true)
	p.now = func() time.Time { return now }
	p.start, p.last = now, now
	p.Skip(5000)
	now = now.Add(10 * time.Second)
	p.Add(1000) // 100 B/s
	now = now.Add(30 * time.Second)
	p.Add(300) // 10 B/s since the last sample
	got := b.String()
	for _, want := range []string{"60% 5.9 KiB/9.8 KiB 100 B/s ETA 40s", "63% 6.2 KiB/9.8 KiB 10 B/s ETA 6m0s"} {
		if !strings.Contains(got, want) {
			t.Errorf("progress lacks %q:\n%s", want, got)
		}
	}
}

func TestProgressDrawsAtOnce(t *testing.T) {
	// A push that resumes reports the pieces held already in a burst and may
	// then wait minutes on providers copying pieces between them: the bar
	// and a changed note show at once, not after the next bytes sent.
	var b bytes.Buffer
	now := time.Unix(0, 0)
	p := newProgress(&b, "uploading", 1000, true)
	p.tty = true
	p.now = func() time.Time { return now }
	p.start, p.last = now, now
	p.Skip(500)
	p.Skip(100)
	if got := b.String(); !strings.Contains(got, " 50% 500 B/1000 B") {
		t.Errorf("first update not drawn:\n%q", got)
	}
	p.Suffix("4 pieces copying between providers")
	if got := b.String(); !strings.Contains(got, " 60% 600 B/1000 B · 4 pieces copying between providers") {
		t.Errorf("new note not drawn:\n%q", got)
	}
}

func TestProgressSaysWhenNothingMoves(t *testing.T) {
	// With the bytes stopped, redraws drop the speed and ETA and say how
	// long nothing has moved, instead of showing the last speed for good.
	var b bytes.Buffer
	now := time.Unix(0, 0)
	p := newProgress(&b, "uploading", 1000, true)
	p.idle = "nothing sent from here"
	p.now = func() time.Time { return now }
	p.start, p.last, p.movedAt = now, now, now
	now = now.Add(10 * time.Second)
	p.Add(500)
	now = now.Add(2 * time.Minute)
	p.Skip(100) // a pull landing does not count as sent
	p.tick()
	got := b.String()
	last := got[strings.LastIndex(got, "uploading"):]
	if !strings.Contains(last, " 60% 600 B/1000 B · nothing sent from here for 2m0s") || strings.Contains(last, "ETA") {
		t.Errorf("bar while stalled: %q", last)
	}
	p.Done()
	p.tick() // a finished bar is not drawn again
	if b.String() != got+"uploading [==============          ]  60% 600 B/1000 B in 2m0s (3 B/s)\n" {
		t.Errorf("after Done: %q", b.String()[len(got):])
	}
}

func TestProgressFitsTheTerminal(t *testing.T) {
	// A line wider than the terminal wraps, and each redraw then leaves a
	// row behind, so the bar is cut to fit.
	var b bytes.Buffer
	p := newProgress(&b, "uploading", 1000, true)
	p.tty = true
	p.width = func() int { return 40 }
	p.Suffix("7 pieces copying between providers · 1 piece sent, waiting to go on chain")
	p.Add(10)
	for _, l := range strings.Split(b.String(), "\r\033[K")[1:] {
		if n := len([]rune(l)); n >= 40 {
			t.Errorf("%d columns: %q", n, l)
		}
	}
	if fit("short", 40) != "short" || fit("abcdef", 0) != "abcdef" {
		t.Error("fit changed a line that fits")
	}
}

func TestNotesGoAboveTheBar(t *testing.T) {
	// A note from a background step, such as a base fee pause, prints as
	// its own line through the bar rather than into the middle of it.
	var b bytes.Buffer
	p := newProgress(&b, "uploading", 1000, true)
	p.tty = true // as on a terminal: the bar redraws in place
	p.Suffix("2 pieces sent, waiting to go on chain")
	p.Add(500)
	o := &opts{stderr: &b}
	logTo(p)
	o.note("the network base fee is high")
	logTo(nil)
	o.note("plain")
	got := b.String()
	if !strings.Contains(got, "\r\033[Kthe network base fee is high\n") {
		t.Errorf("note not printed above the bar:\n%q", got)
	}
	if !strings.Contains(got, "· 2 pieces sent, waiting to go on chain") {
		t.Errorf("bar lacks its suffix:\n%q", got)
	}
	if !strings.HasSuffix(got, "plain\n") {
		t.Errorf("note without a bar:\n%q", got)
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 3 << 30: "3.0 GiB", 5 << 40: "5.0 TiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// fixture unpacks testdata/fixtures/<name>.tar.gz and returns its repo dir.
func fixture(t *testing.T, name string) string {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "fixtures", name+".tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return filepath.Join(dir, "repo")
		}
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, filepath.FromSlash(h.Name))
		if !strings.HasPrefix(p, dir+string(filepath.Separator)) {
			t.Fatalf("bad path %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(p, 0o755)
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
				var b []byte
				if b, err = io.ReadAll(tr); err == nil {
					err = os.WriteFile(p, b, 0o644)
				}
			}
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestWalletNew(t *testing.T) {
	t.Setenv("IPFS2FIL_KEY_FILE", "")
	t.Setenv("IPFS2FIL_PRIVATE_KEY", "")
	t.Setenv("IPFS2FIL_HOME", t.TempDir())
	out, _, err := ipfs2fil(t, "wallet", "new", "--network", "calibration")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"wallet.key", "address  0x", "t410f", "Back that file up"} {
		if !strings.Contains(out, want) {
			t.Errorf("wallet new output lacks %q:\n%s", want, out)
		}
	}
	if _, _, err := ipfs2fil(t, "wallet", "new"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second wallet new: %v", err)
	}
	if err := checkKey(); err != nil {
		t.Errorf("the new key is not found: %v", err)
	}
	other := filepath.Join(t.TempDir(), "other.key")
	if out, _, err := ipfs2fil(t, "wallet", "new", "--key-file", other); err != nil || !strings.Contains(out, "--key-file "+other) {
		t.Errorf("wallet new --key-file: %v\n%s", err, out)
	}
	if _, _, err := ipfs2fil(t, "wallet", "bogus"); err == nil || !strings.Contains(err.Error(), "wallet new") {
		t.Errorf("wallet bogus: %v", err)
	}
}

// TestPackCAR packs a CAR export of a kubo repo: every pin and the MFS root
// become roots of the CAR, and pack finds them all again.
func TestPackCAR(t *testing.T) {
	ctx := context.Background()
	r, err := repo.Open(filepath.Join(fixture(t, "v18")))
	if err != nil {
		t.Fatal(err)
	}
	pins, _ := r.Pins(ctx)
	var roots []cid.Cid
	for _, p := range pins {
		if p.Mode == repo.Recursive {
			roots = append(roots, p.Root)
		}
	}
	if mfs, ok, _ := r.MFSRoot(ctx); ok {
		roots = append(roots, mfs)
	}
	carPath := filepath.Join(t.TempDir(), "export.car")
	f, err := os.Create(carPath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := car.NewWriter(f, roots...)
	if err != nil {
		t.Fatal(err)
	}
	err = r.ForEachBlock(ctx, func(h multihash.Multihash, _ int) error {
		data, err := r.Get(ctx, h)
		if err != nil {
			return err
		}
		return w.Put(cid.NewCidV1(cid.Raw, h), data)
	})
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	out, _, err := ipfs2fil(t, "scan", "--repo", carPath)
	if err != nil || !strings.Contains(out, "(CAR)") {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	dir := t.TempDir()
	if _, _, err := ipfs2fil(t, "pack", "--repo", carPath, "--out", dir); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Read(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The repo's orphans come along as orphans; identity roots are skipped,
	// as for any repo.
	pinned := 0
	for _, rt := range m.Roots {
		switch {
		case rt.Kind == manifest.KindPinRecursive && rt.Name == "export":
			pinned++
		case rt.Kind != manifest.KindOrphan:
			t.Errorf("root %+v", rt)
		}
	}
	if m.Source.Kind != repo.KindCAR || pinned != len(roots)-1 {
		t.Errorf("source %+v, %d pinned roots, want %d", m.Source, pinned, len(roots)-1)
	}
}

// TestPackRPC packs a repo through a kubo RPC API and gets the archive the
// files give: same data root, same roots.
func TestPackRPC(t *testing.T) {
	dir := fixture(t, "v18")
	files := t.TempDir()
	if _, _, err := ipfs2fil(t, "pack", "--repo", dir, "--out", files); err != nil {
		t.Fatal(err)
	}
	want, err := manifest.Read(filepath.Join(files, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, old := range []bool{false, true} {
		srv := httptest.NewServer(repotest.KuboRPC(r, old))
		defer srv.Close()
		out, _, err := ipfs2fil(t, "scan", "--repo", srv.URL)
		if err != nil || !strings.Contains(out, "(kubo RPC 0.38.0)") {
			t.Fatalf("scan: %v\n%s", err, out)
		}
		rpc := t.TempDir()
		if _, _, err := ipfs2fil(t, "pack", "--repo", srv.URL, "--out", rpc); err != nil {
			t.Fatal(err)
		}
		got, err := manifest.Read(filepath.Join(rpc, "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		if got.Source.Kind != repo.KindRPC || len(got.Roots) != len(want.Roots) {
			t.Fatalf("old=%v: source %+v, %d roots, want %d", old, got.Source, len(got.Roots), len(want.Roots))
		}
		if old {
			continue // no pin names, so a different data root
		}
		if got.DataRoot != want.DataRoot {
			t.Errorf("data root %s through RPC, %s from the files", got.DataRoot, want.DataRoot)
		}
	}
}

// TestPackBluesky packs a Bluesky account: its commit is a pin named after
// the handle, its blobs come along, and the explorer gets a Posts page.
func TestPackBluesky(t *testing.T) {
	a := atprototest.New(t)
	t.Setenv("IPFS2FIL_HOME", t.TempDir())
	out, _, err := ipfs2fil(t, "scan", "--repo", "at://alice.test")
	if err != nil || !strings.Contains(out, "(Bluesky account @alice.test, rev 3zzzzzzzzzzzz)") {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	dir := t.TempDir()
	if _, _, err := ipfs2fil(t, "pack", "--repo", "at://alice.test", "--out", dir); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Read(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Source.Kind != repo.KindATProto || len(m.Roots) != 4 {
		t.Fatalf("source %+v, roots %+v", m.Source, m.Roots)
	}
	if r := m.Roots[0]; r.CID != a.Commit.String() || r.Name != "@alice.test" || r.Kind != manifest.KindPinRecursive {
		t.Errorf("first root %+v", r)
	}
	// The blob no post links any more is kept, as an orphan.
	if r := m.Roots[3]; r.Kind != manifest.KindOrphan || r.CID != a.Old.String() {
		t.Errorf("last root %+v", r)
	}
	idx, err := os.ReadFile(filepath.Join(dir, "explorer", "idx", "at", "0.js"))
	if err != nil || !strings.Contains(string(idx), `"x":"a picture"`) || !strings.Contains(string(idx), a.Image.String()) {
		t.Errorf("posts index: %v\n%s", err, idx)
	}
	meta, _ := os.ReadFile(filepath.Join(dir, "explorer", "idx", "meta.js"))
	if !strings.Contains(string(meta), `"accounts":[{"name":"@alice.test","did":"`+a.DID+`","posts":3,"handle":"alice.test"`) ||
		!strings.Contains(string(meta), `"path":"pins/recursive/`+a.Commit.String()+`"`) || !strings.Contains(string(meta), `"blobs":{"linked":1,"held":1`) {
		t.Errorf("meta: %s", meta)
	}
	// In Files, the account is a folder of its records and blobs.
	shards, _ := filepath.Glob(filepath.Join(dir, "explorer", "idx", "s", "*.js"))
	var rows string
	for _, f := range shards {
		b, _ := os.ReadFile(f)
		rows += string(b)
	}
	for _, want := range []string{"pins/recursive/" + a.Commit.String() + "/app.bsky.feed.post", `"3aaaaaaaaaaaa.json","post"`, `"self.json","record"`, `"profile avatar.png","image"`} {
		if !strings.Contains(rows, want) {
			t.Errorf("no %s in the index:\n%s", want, rows)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "explorer", "posts.html")); err != nil {
		t.Error(err)
	}
}
