package repo

import (
	"strings"
	"testing"
)

func TestShardFuncs(t *testing.T) {
	cases := []struct {
		id, name, want string
	}{
		{"/repo/flatfs/shard/v1/next-to-last/2", "CIQABC", "AB"},
		{"/repo/flatfs/shard/v1/next-to-last/2", "A", "__"},
		{"/repo/flatfs/shard/v1/prefix/3", "CIQABC", "CIQ"},
		{"/repo/flatfs/shard/v1/prefix/3", "C", "C__"},
		{"/repo/flatfs/shard/v1/suffix/2", "CIQABC", "BC"},
		{"/repo/flatfs/shard/v1/suffix/2", "C", "_C"},
	}
	for _, c := range cases {
		f, err := parseShardFunc(c.id)
		if err != nil {
			t.Fatal(err)
		}
		if got := f(c.name); got != c.want {
			t.Errorf("%s(%s) = %s, want %s", c.id, c.name, got, c.want)
		}
	}
	for _, bad := range []string{"", "/repo/flatfs/shard/v2/prefix/2", "/repo/flatfs/shard/v1/middle/2", "/repo/flatfs/shard/v1/prefix/0"} {
		if _, err := parseShardFunc(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestFlattenSpec(t *testing.T) {
	s := spec{"type": "mount", "mounts": []any{
		map[string]any{"mountpoint": "/blocks", "type": "measure", "prefix": "x",
			"child": map[string]any{"type": "flatfs", "path": "blocks"}},
		map[string]any{"mountpoint": "/", "type": "log",
			"child": map[string]any{"type": "levelds", "path": "datastore"}},
		map[string]any{"mountpoint": "/tmp", "type": "mem"},
	}}
	ms, err := flattenSpec("/", s)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range ms {
		got = append(got, m.point+"="+m.spec.str("type")+":"+m.spec.str("path"))
	}
	if g := strings.Join(got, " "); g != "/blocks=flatfs:blocks /=levelds:datastore" {
		t.Errorf("got %s", g)
	}
	if _, err := flattenSpec("/", spec{"type": "s3ds"}); err == nil || !strings.Contains(err.Error(), `"s3ds"`) {
		t.Errorf("s3ds: %v", err)
	}
}
