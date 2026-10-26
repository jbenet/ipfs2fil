package ipni

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/routing/v1/providers/") {
		case "bafyA":
			fmt.Fprint(w, `{"Providers":[{"Schema":"peer","ID":"12D3KooW"}]}`)
		case "bafyEmpty":
			fmt.Fprint(w, `{"Providers":null}`)
		case "bafyDown":
			w.WriteHeader(http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	res := Check(context.Background(), srv.Client(), srv.URL+"/", []string{"bafyA", "bafyEmpty", "bafyNone", "bafyDown"}, 2)
	if res.Checked != 4 || res.Announced != 1 || len(res.Missing) != 3 {
		t.Fatalf("%+v", res)
	}
	want := []Miss{{"bafyEmpty", "no providers"}, {"bafyNone", "no providers"}, {"bafyDown", "HTTP 502"}}
	if fmt.Sprint(res.Missing) != fmt.Sprint(want) {
		t.Errorf("missing %v, want %v", res.Missing, want)
	}
}

func TestSample(t *testing.T) {
	m := &manifest.Manifest{Root: "r", DataRoot: "d"}
	for i := range 10 {
		m.Roots = append(m.Roots, manifest.Root{CID: fmt.Sprint("c", i)})
	}
	if got := fmt.Sprint(Sample(m, 4)); got != "[r d c0 c5]" {
		t.Errorf("Sample(4) = %s", got)
	}
	if got := len(Sample(m, 100)); got != 12 {
		t.Errorf("Sample(100) has %d CIDs, want 12", got)
	}
}
