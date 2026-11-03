package atproto

import (
	"reflect"
	"testing"
)

func TestEnvFirst(t *testing.T) {
	t.Setenv("IPFS2FIL_TEST_URL", "")
	if got := envFirst("IPFS2FIL_TEST_URL", "https://plc.directory"); got != "https://plc.directory" {
		t.Errorf("unset: %q", got)
	}
	t.Setenv("IPFS2FIL_TEST_URL", " http://localhost:2582/ ,")
	got := envFirst("IPFS2FIL_TEST_URL", "https://plc.directory")
	if want := []string{"http://localhost:2582", "https://plc.directory"}; !reflect.DeepEqual(endpoints(got), want) {
		t.Errorf("set: %q", got)
	}
}
