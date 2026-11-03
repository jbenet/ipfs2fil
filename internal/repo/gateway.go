package repo

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	carv2 "github.com/ipld/go-car/v2"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/car"
	"github.com/jbenet/ipfs2fil/internal/ipldlinks"
)

// Gateways are the trustless IPFS gateways (https://specs.ipfs.tech/http-gateways/trustless-gateway/)
// the readers fetch content from by CID, tried in order. $IPFS2FIL_GATEWAYS,
// comma-separated, replaces them.
var Gateways = gatewaysFromEnv()

func gatewaysFromEnv() []string {
	if v := os.Getenv("IPFS2FIL_GATEWAYS"); v != "" {
		var out []string
		for _, g := range strings.Split(v, ",") {
			if g = strings.TrimSuffix(strings.TrimSpace(g), "/"); g != "" {
				out = append(out, g)
			}
		}
		return out
	}
	return []string{"https://trustless-gateway.link", "https://ipfs.io", "https://gateway.pinata.cloud", "https://ipfs.filebase.io", "https://4everland.io"}
}

// gatewayFails counts each gateway's failures in this run, so gateways that
// keep failing are tried last.
var (
	gatewayMu    sync.Mutex
	gatewayFails = map[string]int{}
)

// gatewayOrder is Gateways, the ones that failed least first.
func gatewayOrder() []string {
	gatewayMu.Lock()
	defer gatewayMu.Unlock()
	out := append([]string(nil), Gateways...)
	sort.SliceStable(out, func(i, j int) bool { return gatewayFails[out[i]] < gatewayFails[out[j]] })
	return out
}

// ErrIncomplete is a DAG a gateway did not send all of.
var ErrIncomplete = errors.New("the gateway sent part of the DAG")

// fetchTimeout bounds one gateway's answer for one DAG. Big DAGs stream
// for a long time; a stall is caught by idleTimeout.
var (
	fetchTimeout = 6 * time.Hour
	idleTimeout  = 2 * time.Minute
)

// fetchDAG downloads the whole DAG under root from the first gateway that
// has it, as a CAR, into dest. Every block's hash is checked, and every
// block the DAG links to must arrive, so dest holds the complete DAG or is
// not written. A dest that already exists is kept: a CID's DAG never
// changes.
func fetchDAG(ctx context.Context, root cid.Cid, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	var errs []error
	for _, gw := range gatewayOrder() {
		err := fetchFrom(ctx, gw, root, dest)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		gatewayMu.Lock()
		gatewayFails[gw]++
		gatewayMu.Unlock()
		errs = append(errs, fmt.Errorf("%s: %w", gw, err))
	}
	if len(errs) == 0 {
		return errors.New("no gateways to fetch from (set $IPFS2FIL_GATEWAYS)")
	}
	return fmt.Errorf("fetching %s: %w", root, errors.Join(errs...))
}

// idleReader fails a read that waits longer than idleTimeout.
type idleReader struct {
	r     io.Reader
	timer *time.Timer
}

func (i *idleReader) Read(p []byte) (int, error) {
	i.timer.Reset(idleTimeout)
	return i.r.Read(p)
}

func fetchFrom(ctx context.Context, gw string, root cid.Cid, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gw+"/ipfs/"+root.String()+"?format=car&dag-scope=all", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.ipld.car;version=1;order=dfs;dups=n")
	res, err := HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", res.Status)
	}
	timer := time.AfterFunc(idleTimeout, cancel)
	defer timer.Stop()
	br, err := carv2.NewBlockReader(&idleReader{r: res.Body, timer: timer})
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".fetch-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	w := bufio.NewWriterSize(tmp, 1<<20)
	section := func(parts ...[]byte) error {
		n := 0
		for _, p := range parts {
			n += len(p)
		}
		var vb [binary.MaxVarintLen64]byte
		if _, err := w.Write(vb[:binary.PutUvarint(vb[:], uint64(n))]); err != nil {
			return err
		}
		for _, p := range parts {
			if _, err := w.Write(p); err != nil {
				return err
			}
		}
		return nil
	}
	if err := section(car.Header(root)); err != nil {
		return err
	}
	have, need := map[string]bool{}, map[string]bool{string(root.Hash()): true}
	for {
		b, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		c, data := b.Cid(), b.RawData()
		if ok, err := sameHash(c, data); err != nil || !ok {
			return fmt.Errorf("block %s does not match its hash", c)
		}
		if have[string(c.Hash())] {
			continue
		}
		have[string(c.Hash())] = true
		if err := section(c.Bytes(), data); err != nil {
			return err
		}
		ls, err := ipldlinks.Links(c, data)
		if err != nil {
			return fmt.Errorf("block %s: %w", c, err)
		}
		for _, l := range ls {
			if l.Prefix().MhType != multihash.IDENTITY {
				need[string(l.Hash())] = true
			}
		}
	}
	missing := 0
	for k := range need {
		if !have[k] {
			missing++
		}
	}
	if missing > 0 {
		return fmt.Errorf("%w: %d blocks missing", ErrIncomplete, missing)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

// sameHash says whether data hashes to c.
func sameHash(c cid.Cid, data []byte) (bool, error) {
	d, err := multihash.Decode(c.Hash())
	if err != nil {
		return false, err
	}
	if d.Code == multihash.IDENTITY {
		return string(d.Digest) == string(data), nil
	}
	got, err := multihash.Sum(data, d.Code, d.Length)
	if err != nil {
		return false, err
	}
	return string(got) == string(c.Hash()), nil
}
