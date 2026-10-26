package collection

import "testing"

func TestIsGitURL(t *testing.T) {
	for in, want := range map[string]bool{
		"git@github.com:jbenet/catalog.git": true,
		"https://github.com/jbenet/catalog": true,
		"http://git.example.lan/catalog":    true,
		"ssh://host/srv/catalog":            true,
		"/srv/catalog.git":                  true,
		"/srv/catalog.git/":                 true,
		"~/Sync/ipfs2fil-catalog":           false,
		"/Volumes/shared/catalog":           false,
	} {
		if got := IsGitURL(in); got != want {
			t.Errorf("IsGitURL(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestResolveCatalog(t *testing.T) {
	dir := t.TempDir()
	if c, err := ResolveCatalog(dir, ""); c != "" || err != nil {
		t.Fatalf("nothing saved: %q %v", c, err)
	}
	if c, err := ResolveCatalog(dir, "git@h:c.git"); c != "git@h:c.git" || err != nil {
		t.Fatalf("given: %q %v", c, err)
	}
	if c, err := ResolveCatalog(dir, ""); c != "git@h:c.git" || err != nil {
		t.Fatalf("remembered: %q %v", c, err)
	}
}
