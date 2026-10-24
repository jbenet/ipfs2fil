package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DetectVersion reads the fs-repo version. A directory with a config file but
// no version file is a version 0 repo.
func DetectVersion(path string) (Version, error) {
	if _, err := os.Stat(filepath.Join(path, "config")); err != nil {
		return 0, fmt.Errorf("%s: %w", path, ErrNotRepo)
	}
	b, err := os.ReadFile(filepath.Join(path, "version"))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: bad version file %q", path, b)
	}
	if Version(n) > Latest {
		return Version(n), fmt.Errorf("%s: fs-repo %d is newer than %d: %w", path, n, Latest, ErrUnsupported)
	}
	return Version(n), nil
}
