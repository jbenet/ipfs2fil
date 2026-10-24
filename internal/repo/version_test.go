package repo

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectVersion(t *testing.T) {
	cases := []struct {
		version string // "" means no version file
		want    Version
		err     error
	}{
		{"", 0, nil},
		{"11\n", 11, nil},
		{"18", 18, nil},
		{"19\n", 19, ErrUnsupported},
	}
	for _, c := range cases {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "config"), []byte("{}"), 0o600)
		if c.version != "" {
			os.WriteFile(filepath.Join(dir, "version"), []byte(c.version), 0o600)
		}
		got, err := DetectVersion(dir)
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("version %q: got %d, %v; want %d, %v", c.version, got, err, c.want, c.err)
		}
	}
	if _, err := DetectVersion(t.TempDir()); !errors.Is(err, ErrNotRepo) {
		t.Errorf("empty dir: got %v, want ErrNotRepo", err)
	}
}
