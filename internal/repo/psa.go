package repo

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

// PSADir holds pinning service tokens, under tokens/. Pins' DAGs go in
// DAGDir. The CLI sets it under its home directory.
var PSADir = filepath.Join(os.TempDir(), "ipfs2fil-psa")

// PSAFile names the direct pin that lists what a pinning service holds.
const PSAFile = "psa.json"

// PSAServices are the services psa:<name> stands for: endpoints of the IPFS
// Pinning Services API (https://ipfs.github.io/pinning-services-api-spec/).
var PSAServices = map[string]string{
	"pinata":    "https://api.pinata.cloud/psa",
	"filebase":  "https://api.filebase.io/v1/ipfs",
	"4everland": "https://api.4everland.dev",
}

// IsPSA says whether path names a pinning service: psa:<name> for a known
// service, or psa+https://endpoint for any service speaking the API.
func IsPSA(path string) bool { return psaEndpoint(path) != "" }

// psaEndpoint turns psa:<name> or psa+https://endpoint into the API's base
// URL, or "" if path is neither. User names and tokens in the URL are
// refused: tokens come from $IPFS2FIL_PSA_TOKEN or a token file.
func psaEndpoint(path string) string {
	if name, ok := strings.CutPrefix(path, "psa:"); ok && !strings.Contains(name, "/") {
		return PSAServices[strings.ToLower(name)]
	}
	s, ok := strings.CutPrefix(path, "psa+")
	if !ok {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	return strings.TrimSuffix(u.Scheme+"://"+strings.ToLower(u.Host)+u.Path, "/")
}

// PSATokenFile is where the token for a service's host is kept when
// $IPFS2FIL_PSA_TOKEN is not set: <PSADir>/tokens/<host>, one line.
func PSATokenFile(endpoint string) string {
	u, _ := url.Parse(endpoint)
	host := endpoint
	if u != nil && u.Host != "" {
		host = u.Host
	}
	return filepath.Join(PSADir, "tokens", safeName(host))
}

func psaToken(endpoint string) (string, error) {
	if t := strings.TrimSpace(os.Getenv("IPFS2FIL_PSA_TOKEN")); t != "" {
		return t, nil
	}
	b, err := os.ReadFile(PSATokenFile(endpoint))
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("no access token for %s: set $IPFS2FIL_PSA_TOKEN, or put it in %s", endpoint, PSATokenFile(endpoint))
	}
	return strings.TrimSpace(string(b)), err
}

// psaRepo is everything pinned on one pinning service account: each pin's
// whole DAG, fetched from IPFS gateways by CID (pinning services list pins
// but do not serve them), as a recursive pin under the pin's name.
type psaRepo struct {
	endpoint string

	once  sync.Once
	err   error
	car   *carRepo
	doc   []byte
	docMH multihash.Multihash
	pins  []Pin
}

// PSADoc is psa.json: the service and every pin it listed, with whether
// its DAG is in the archive. It holds what the service says, so the pins
// can be made again elsewhere with the same names and metadata.
type PSADoc struct {
	Type    string   `json:"$type"` // "ipfs2fil.psa"
	Service string   `json:"service"`
	Pins    []PSAPin `json:"pins"`
}

// PSAPin is one pin, as the service lists it.
type PSAPin struct {
	RequestID string            `json:"requestid"`
	Created   string            `json:"created,omitempty"`
	CID       string            `json:"cid"`
	Name      string            `json:"name,omitempty"`
	Origins   []string          `json:"origins,omitempty"`
	Meta      map[string]string `json:"meta,omitempty"`
	// Missing says why the pin's DAG is not in the archive: no gateway
	// served all of it.
	Missing string `json:"missing,omitempty"`
}

func openPSA(path string) (Repo, error) {
	ep := psaEndpoint(path)
	if ep == "" {
		return nil, ErrNotRepo
	}
	return &psaRepo{endpoint: ep}, nil
}

func (r *psaRepo) Path() string     { return "psa+" + r.endpoint }
func (r *psaRepo) Kind() string     { return KindPSA }
func (r *psaRepo) Version() Version { return 0 }

func (r *psaRepo) Close() error {
	if r.car != nil {
		return r.car.Close()
	}
	return nil
}

func (r *psaRepo) Describe() string {
	if err := r.load(); err != nil {
		return "pinning service " + r.endpoint
	}
	return fmt.Sprintf("pinning service %s, %d pins", r.endpoint, len(r.pins)-1)
}

func (r *psaRepo) load() error {
	r.once.Do(func() { r.err = r.fetch() })
	return r.err
}

// psaWorkers is how many pins' DAGs download at once.
const psaWorkers = 4

func (r *psaRepo) fetch() error {
	ctx := context.Background()
	token, err := psaToken(r.endpoint)
	if err != nil {
		return err
	}
	listed, err := listPins(ctx, r.endpoint, token)
	if err != nil {
		return err
	}
	logf("%s lists %d pins\n", r.endpoint, len(listed))

	cars := make([]string, len(listed))
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	sem := make(chan struct{}, psaWorkers)
	for i := range listed {
		c, err := cid.Decode(listed[i].CID)
		if err != nil {
			listed[i].Missing = "not a CID"
			continue
		}
		wg.Add(1)
		go func(i int, c cid.Cid) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			dest := dagFile(c)
			err := fetchDAG(ctx, c, dest)
			mu.Lock()
			defer mu.Unlock()
			done++
			if err != nil {
				listed[i].Missing = err.Error()
				logf("  %s (%s): %v\n", c, listed[i].Name, err)
			} else {
				cars[i] = dest
			}
			if done%25 == 0 || done == len(listed) {
				logf("  pins %d/%d\n", done, len(listed))
			}
		}(i, c)
	}
	wg.Wait()

	r.car = &carRepo{path: r.Path(), at: map[string]carBlock{}}
	seen := map[string]bool{}
	missing := 0
	for i, p := range listed {
		if cars[i] == "" {
			missing++
			continue
		}
		c, _ := cid.Decode(p.CID)
		name := p.Name
		if name == "" {
			name = p.CID
		}
		r.pins = append(r.pins, Pin{Root: c, Mode: Recursive, Name: name})
		if seen[cars[i]] {
			continue // two pins of one CID
		}
		seen[cars[i]] = true
		if err := r.car.index(cars[i], filepath.Dir(cars[i])); err != nil {
			return err
		}
	}
	r.car.pins = nil
	if missing > 0 {
		logf("  %d of %d pins could not be fetched from any gateway; psa.json in the archive names them\n", missing, len(listed))
	}
	if missing == len(listed) && missing > 0 {
		return fmt.Errorf("%s: no gateway served any of the %d pins", r.endpoint, missing)
	}
	doc := PSADoc{Type: "ipfs2fil.psa", Service: r.endpoint, Pins: listed}
	if r.doc, err = json.MarshalIndent(doc, "", "  "); err != nil {
		return err
	}
	sum := sha256.Sum256(r.doc)
	r.docMH, _ = multihash.Encode(sum[:], multihash.SHA2_256)
	r.pins = append(r.pins, Pin{Root: cid.NewCidV1(cid.Raw, r.docMH), Mode: Direct, Name: PSAFile})
	return nil
}

// maxPins bounds how many pins listPins reads from one account.
const maxPins = 1 << 20

// listPins lists every pin with status "pinned", newest first, following
// the API's paging (each page holds the pins created before the last one
// seen). Pins come back sorted by creation then request ID, so a listing
// that has not changed gives the same document.
func listPins(ctx context.Context, endpoint, token string) ([]PSAPin, error) {
	var out []PSAPin
	seen := map[string]bool{}
	before := ""
	for {
		q := url.Values{"status": {"pinned"}, "limit": {"1000"}}
		if before != "" {
			q.Set("before", before)
		}
		var page struct {
			Count   int `json:"count"`
			Results []struct {
				RequestID string `json:"requestid"`
				Status    string `json:"status"`
				Created   string `json:"created"`
				Pin       struct {
					CID     string            `json:"cid"`
					Name    string            `json:"name"`
					Origins []string          `json:"origins"`
					Meta    map[string]string `json:"meta"`
				} `json:"pin"`
			} `json:"results"`
		}
		if err := psaGet(ctx, endpoint+"/pins?"+q.Encode(), token, &page); err != nil {
			return nil, fmt.Errorf("listing the pins on %s: %w", endpoint, err)
		}
		added := 0
		for _, p := range page.Results {
			if seen[p.RequestID] || p.Status != "" && p.Status != "pinned" {
				continue
			}
			seen[p.RequestID] = true
			added++
			out = append(out, PSAPin{RequestID: p.RequestID, Created: p.Created, CID: p.Pin.CID, Name: p.Pin.Name, Origins: p.Pin.Origins, Meta: p.Pin.Meta})
			if before == "" || p.Created < before {
				before = p.Created
			}
		}
		if added == 0 || len(out) >= page.Count {
			break
		}
		if len(out) > maxPins {
			return nil, fmt.Errorf("%s lists over %d pins", endpoint, maxPins)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created < out[j].Created
		}
		return out[i].RequestID < out[j].RequestID
	})
	return out, nil
}

func psaGet(ctx context.Context, u, token string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	res, err := HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error struct{ Reason, Details string }
		}
		if json.Unmarshal(b, &e) == nil && e.Error.Reason != "" {
			return fmt.Errorf("%s: %s %s", res.Status, e.Error.Reason, e.Error.Details)
		}
		return fmt.Errorf("%s", res.Status)
	}
	return json.Unmarshal(b, v)
}

func (r *psaRepo) ForEachBlock(ctx context.Context, fn func(multihash.Multihash, int) error) error {
	if err := r.load(); err != nil {
		return err
	}
	if err := r.car.ForEachBlock(ctx, fn); err != nil {
		return err
	}
	if _, ok := r.car.at[string(r.docMH)]; ok {
		return nil
	}
	return fn(r.docMH, len(r.doc))
}

func (r *psaRepo) Get(ctx context.Context, mh multihash.Multihash) ([]byte, error) {
	if err := r.load(); err != nil {
		return nil, err
	}
	if string(mh) == string(r.docMH) {
		return r.doc, nil
	}
	return r.car.Get(ctx, mh)
}

func (r *psaRepo) Pins(context.Context) ([]Pin, error) {
	if err := r.load(); err != nil {
		return nil, err
	}
	return r.pins, nil
}

func (r *psaRepo) MFSRoot(context.Context) (cid.Cid, bool, error) {
	return cid.Undef, false, nil
}
