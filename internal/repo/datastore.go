package repo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// kv is the read-only slice of a go-datastore this package needs. Keys are
// datastore key strings ("/local/filesroot"); before repo 4 a block key ends in
// raw multihash bytes, so keys are not always printable.
type kv interface {
	// Get returns ErrNotFound for a missing key.
	Get(key string) ([]byte, error)
	// Query calls fn for every key under prefix ("/" for all) with the size
	// of its value, in no particular order.
	Query(prefix string, fn func(key string, size int) error) error
	Close() error
}

// mount routes keys to child datastores by longest matching mountpoint, like
// go-datastore's mount.Datastore.
type mount []mountPoint

type mountPoint struct {
	point string // "/" or "/blocks"
	kv    kv
}

func newMount(ms []mountPoint) mount {
	sort.Slice(ms, func(i, j int) bool { return len(ms[i].point) > len(ms[j].point) })
	return ms
}

// lookup finds the mount holding key (or, for a query, the prefix) and the key
// inside that mount.
func (m mount) lookup(key string) (mountPoint, string, bool) {
	base := strings.TrimSuffix(key, "/")
	for _, mp := range m {
		switch {
		case mp.point == "/":
			return mp, key, true
		case base == mp.point:
			return mp, "/", true
		case strings.HasPrefix(base, mp.point+"/"):
			return mp, key[len(mp.point):], true
		}
	}
	return mountPoint{}, "", false
}

func (m mount) Get(key string) ([]byte, error) {
	mp, k, ok := m.lookup(key)
	if !ok {
		return nil, ErrNotFound
	}
	return mp.kv.Get(k)
}

// Query only looks in the mount that holds prefix, which is all this package
// needs: every prefix it queries ("/blocks/", "/pins/pin/") sits in one mount.
func (m mount) Query(prefix string, fn func(string, int) error) error {
	mp, k, ok := m.lookup(prefix)
	if !ok {
		return nil
	}
	outer := strings.TrimSuffix(mp.point, "/")
	return mp.kv.Query(k, func(key string, size int) error {
		return fn(outer+key, size)
	})
}

func (m mount) Close() error {
	var first error
	for _, mp := range m {
		if err := mp.kv.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// openDatastore opens the repo's datastore read-only. Repos 6 and later
// describe it in config's Datastore.Spec; earlier ones have a fixed layout.
func openDatastore(path string, v Version) (kv, error) {
	switch {
	case v <= 1:
		// Everything, blocks included, lives in leveldb.
		return openSpecs(path, v, []mountSpec{{"/", spec{"type": "levelds", "path": "datastore"}}})
	case v <= 5:
		return openSpecs(path, v, []mountSpec{
			{"/blocks", spec{"type": "flatfs", "path": "blocks"}},
			{"/", spec{"type": "levelds", "path": "datastore"}},
		})
	}
	b, err := os.ReadFile(filepath.Join(path, "config"))
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Datastore struct{ Spec spec }
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("%s: config is not valid JSON: %w", path, err)
	}
	if cfg.Datastore.Spec == nil {
		return nil, fmt.Errorf("%s: config has no Datastore.Spec", path)
	}
	ms, err := flattenSpec("/", cfg.Datastore.Spec)
	if err != nil {
		return nil, fmt.Errorf("%s: Datastore.Spec: %w", path, err)
	}
	return openSpecs(path, v, ms)
}

// spec is one node of a Datastore.Spec tree.
type spec map[string]any

func (s spec) str(k string) string {
	v, _ := s[k].(string)
	return v
}

type mountSpec struct {
	point string
	spec  spec
}

// flattenSpec turns a spec tree into leaf datastores and their mountpoints,
// looking through mount, measure and log wrappers.
func flattenSpec(point string, s spec) ([]mountSpec, error) {
	switch t := s.str("type"); t {
	case "mount":
		raw, _ := s["mounts"].([]any)
		var out []mountSpec
		for _, r := range raw {
			child, ok := r.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("mount entry is not an object")
			}
			mp := spec(child).str("mountpoint")
			if mp == "" {
				return nil, fmt.Errorf("mount entry has no mountpoint")
			}
			sub, err := flattenSpec(joinKey(point, mp), spec(child))
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		}
		return out, nil
	case "measure", "log":
		child, ok := s["child"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s datastore has no child", t)
		}
		return flattenSpec(point, spec(child))
	case "flatfs", "levelds", "badgerds", "pebbleds":
		return []mountSpec{{point, s}}, nil
	case "mem":
		return nil, nil // nothing on disk
	case "":
		return nil, fmt.Errorf("datastore with no type")
	default:
		return nil, fmt.Errorf("datastore type %q is not supported yet", t)
	}
}

func joinKey(a, b string) string {
	k := strings.TrimSuffix(a, "/") + "/" + strings.Trim(b, "/")
	if k != "/" {
		k = strings.TrimSuffix(k, "/")
	}
	return k
}

func openSpecs(repoPath string, v Version, ms []mountSpec) (kv, error) {
	var opened []mountPoint
	fail := func(err error) (kv, error) {
		mount(opened).Close()
		return nil, err
	}
	for _, m := range ms {
		p := m.spec.str("path")
		if p == "" {
			return fail(fmt.Errorf("%s datastore at %s has no path", m.spec.str("type"), m.point))
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(repoPath, p)
		}
		var d kv
		var err error
		switch m.spec.str("type") {
		case "flatfs":
			d, err = openFlatfs(p, v)
		case "levelds":
			d, err = openLevelDB(p)
		case "badgerds":
			d, err = openBadger(p)
		case "pebbleds":
			d, err = openPebble(p)
		default:
			err = fmt.Errorf("datastore type %q is not supported yet", m.spec.str("type"))
		}
		if err != nil {
			return fail(err)
		}
		opened = append(opened, mountPoint{m.point, d})
	}
	return newMount(opened), nil
}
