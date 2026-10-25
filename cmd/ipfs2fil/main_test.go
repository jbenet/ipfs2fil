package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		if n != "version" && !strings.Contains(out, "-repo") {
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
	for _, want := range []string{"[======                  ]  25% 250 B/1000 B ETA 30s", " 50% 500 B/1000 B in 20s"} {
		if !strings.Contains(got, want) {
			t.Errorf("progress lacks %q:\n%s", want, got)
		}
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
