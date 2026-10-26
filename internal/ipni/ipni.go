// Package ipni asks a delegated routing endpoint whether CIDs are announced,
// which is what makes them findable by CID on the IPFS network. Providers
// announce the blocks of data sets tagged withIPFSIndexing, a while after the
// pieces land, so a fresh archive reads as not announced yet. This is
// announcement, not possession: verify's fetch and the chain's proofs say
// whether the providers hold the bytes.
package ipni

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// DefaultEndpoint is the IPFS project's public delegated routing service.
const DefaultEndpoint = "https://delegated-ipfs.dev"

// Result is what a Check found.
type Result struct {
	Endpoint  string
	Checked   int
	Announced int
	Missing   []Miss // in the order checked
}

// Miss is a CID with no provider record, and why.
type Miss struct {
	CID    string
	Reason string
}

// Check asks endpoint for providers of each CID, concurrency at a time,
// using GET /routing/v1/providers/{cid} (IPIP-337). A CID with at least one
// provider record counts as announced.
func Check(ctx context.Context, client *http.Client, endpoint string, cids []string, concurrency int) Result {
	if client == nil {
		client = http.DefaultClient
	}
	base := strings.TrimRight(endpoint, "/")
	res := Result{Endpoint: base, Checked: len(cids)}
	reasons := make([]string, len(cids))
	work := make(chan int)
	var wg sync.WaitGroup
	for range max(1, min(concurrency, len(cids))) {
		wg.Go(func() {
			for i := range work {
				reasons[i] = lookup(ctx, client, base+"/routing/v1/providers/"+cids[i])
			}
		})
	}
	for i := range cids {
		work <- i
	}
	close(work)
	wg.Wait()
	for i, r := range reasons {
		if r == "" {
			res.Announced++
		} else {
			res.Missing = append(res.Missing, Miss{CID: cids[i], Reason: r})
		}
	}
	return res
}

// lookup returns "" if url lists a provider, else why not.
func lookup(ctx context.Context, client *http.Client, url string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err.Error()
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err.Error()
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "no providers"
	case resp.StatusCode != http.StatusOK:
		return fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	var body struct{ Providers []json.RawMessage }
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return "bad response: " + err.Error()
	}
	if len(body.Providers) == 0 {
		return "no providers"
	}
	return ""
}

// Sample picks up to n CIDs to check from m: the archive root, the data root,
// then roots spread evenly across m.Roots, so re-runs check the same ones.
func Sample(m *manifest.Manifest, n int) []string {
	var out []string
	seen := map[string]bool{}
	add := func(c string) {
		if c != "" && !seen[c] && len(out) < n {
			seen[c] = true
			out = append(out, c)
		}
	}
	add(m.Root)
	add(m.DataRoot)
	if left := n - len(out); left > 0 && len(m.Roots) > 0 {
		step := max(1, len(m.Roots)/left)
		for i := 0; i < len(m.Roots); i += step {
			add(m.Roots[i].CID)
		}
	}
	return out
}
