package repo

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

// rpcRepo reads a running kubo node through its RPC API (port 5001), for
// repos the file readers can't open: plugin datastores such as S3, nodes on
// other machines (through an ssh tunnel), apps that embed kubo. It only calls
// read-only commands.
type rpcRepo struct {
	api     string // http://host:port, no trailing slash
	user    *url.Userinfo
	client  *http.Client
	version string

	mu   sync.Mutex
	cids map[string]cid.Cid // multihash -> the CID refs/local gave, for block/get
}

// isRPC says whether path is a kubo RPC API address.
func isRPC(path string) bool {
	return strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://")
}

func openRPC(path string) (Repo, error) {
	u, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	r := &rpcRepo{user: u.User, client: &http.Client{}, cids: map[string]cid.Cid{}}
	u.User = nil
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/api/v0")
	r.api = strings.TrimSuffix(u.String(), "/")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var v struct{ Version string }
	if err := r.call(ctx, "version", nil, func(body io.Reader) error {
		return json.NewDecoder(body).Decode(&v)
	}); err != nil {
		return nil, fmt.Errorf("%s: no kubo RPC API answered: %w", path, err)
	}
	r.version = v.Version
	return r, nil
}

func (r *rpcRepo) Path() string     { return r.api }
func (r *rpcRepo) Kind() string     { return KindRPC }
func (r *rpcRepo) Version() Version { return 0 }
func (r *rpcRepo) Close() error     { return nil }

// rpcError is the body kubo sends with a failed command.
type rpcError struct {
	Message string
	Code    int
}

// call POSTs one command and hands the body to read.
func (r *rpcRepo) call(ctx context.Context, cmd string, args url.Values, read func(io.Reader) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.api+"/api/v0/"+cmd+"?"+args.Encode(), nil)
	if err != nil {
		return err
	}
	if r.user != nil {
		pw, _ := r.user.Password()
		req.SetBasicAuth(r.user.Username(), pw)
	}
	res, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		var e rpcError
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			return fmt.Errorf("%s: %s", cmd, e.Message)
		}
		return fmt.Errorf("%s: %s: %s", cmd, res.Status, strings.TrimSpace(string(b)))
	}
	return read(res.Body)
}

// jsonLines decodes a stream of JSON values, one at a time, into fn.
func jsonLines[T any](body io.Reader, fn func(T) error) error {
	d := json.NewDecoder(bufio.NewReader(body))
	for {
		var v T
		if err := d.Decode(&v); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if err := fn(v); err != nil {
			return err
		}
	}
}

// statWorkers is how many block/stat calls run at once while listing.
const statWorkers = 16

func (r *rpcRepo) ForEachBlock(ctx context.Context, fn func(multihash.Multihash, int) error) error {
	var refs []cid.Cid
	err := r.call(ctx, "refs/local", nil, func(body io.Reader) error {
		return jsonLines(body, func(v struct{ Ref, Err string }) error {
			if v.Err != "" {
				return errors.New(v.Err)
			}
			c, err := cid.Decode(v.Ref)
			if err != nil {
				return err
			}
			refs = append(refs, c)
			return nil
		})
	})
	if err != nil {
		return err
	}
	r.mu.Lock()
	for _, c := range refs {
		r.cids[string(c.Hash())] = c
	}
	r.mu.Unlock()

	// refs/local gives no sizes; ask block/stat for them, several at a
	// time, and report blocks in the order refs/local gave them.
	sizes := make([]int, len(refs))
	errs := make([]error, len(refs))
	done := make([]chan struct{}, len(refs))
	for i := range done {
		done[i] = make(chan struct{})
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	next := make(chan int)
	var wg sync.WaitGroup
	for range statWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				sizes[i], errs[i] = r.stat(ctx, refs[i])
				close(done[i])
			}
		}()
	}
	go func() {
		defer close(next)
		for i := range refs {
			select {
			case next <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	defer wg.Wait()
	for i, c := range refs {
		select {
		case <-done[i]:
		case <-ctx.Done():
			return ctx.Err()
		}
		if errs[i] != nil {
			return fmt.Errorf("block/stat %s: %w", c, errs[i])
		}
		if err := fn(c.Hash(), sizes[i]); err != nil {
			return err
		}
	}
	return nil
}

func (r *rpcRepo) stat(ctx context.Context, c cid.Cid) (int, error) {
	var v struct{ Size int }
	err := r.call(ctx, "block/stat", url.Values{"arg": {c.String()}}, func(body io.Reader) error {
		return json.NewDecoder(body).Decode(&v)
	})
	return v.Size, err
}

func (r *rpcRepo) Get(ctx context.Context, mh multihash.Multihash) ([]byte, error) {
	r.mu.Lock()
	c, ok := r.cids[string(mh)]
	r.mu.Unlock()
	try := []cid.Cid{c}
	if !ok {
		// Kubo finds blocks by multihash from fs-repo 12 on; older nodes need
		// the codec, so try the common ones.
		try = try[:0]
		for _, codec := range keyCodecs {
			try = append(try, cid.NewCidV1(codec, mh))
		}
	}
	var err error
	for _, c := range try {
		var data []byte
		err = r.call(ctx, "block/get", url.Values{"arg": {c.String()}, "offline": {"true"}}, func(body io.Reader) error {
			var err error
			data, err = io.ReadAll(body)
			return err
		})
		if err == nil {
			return data, nil
		}
	}
	if strings.Contains(err.Error(), "not found") {
		return nil, ErrNotFound
	}
	return nil, err
}

// pinLs lists one type of pin. Kubo 0.27 and later name pins and stream
// them one per line; older nodes send one object of all keys and ignore
// names, which then stay empty.
func (r *rpcRepo) pinLs(ctx context.Context, typ string, mode PinMode) ([]Pin, error) {
	var pins []Pin
	add := func(s, name string) error {
		c, err := cid.Decode(s)
		if err != nil {
			return err
		}
		pins = append(pins, Pin{Root: c, Mode: mode, Name: name})
		return nil
	}
	read := func(body io.Reader) error {
		return jsonLines(body, func(v struct {
			Cid, Name string
			Keys      map[string]struct{ Name string }
		}) error {
			if v.Cid != "" {
				return add(v.Cid, v.Name)
			}
			for k, e := range v.Keys {
				if err := add(k, e.Name); err != nil {
					return err
				}
			}
			return nil
		})
	}
	err := r.call(ctx, "pin/ls", url.Values{"type": {typ}, "stream": {"true"}, "names": {"true"}}, read)
	if err != nil && strings.Contains(err.Error(), "names") {
		// Nodes before names may refuse the option.
		pins = pins[:0]
		err = r.call(ctx, "pin/ls", url.Values{"type": {typ}}, read)
	}
	return pins, err
}

func (r *rpcRepo) Pins(ctx context.Context) ([]Pin, error) {
	rec, err := r.pinLs(ctx, "recursive", Recursive)
	if err != nil {
		return nil, err
	}
	dir, err := r.pinLs(ctx, "direct", Direct)
	if err != nil {
		return nil, err
	}
	return append(rec, dir...), nil
}

func (r *rpcRepo) MFSRoot(ctx context.Context) (cid.Cid, bool, error) {
	var v struct{ Hash string }
	err := r.call(ctx, "files/stat", url.Values{"arg": {"/"}}, func(body io.Reader) error {
		return json.NewDecoder(body).Decode(&v)
	})
	if err != nil {
		return cid.Undef, false, err
	}
	c, err := cid.Decode(v.Hash)
	if err != nil {
		return cid.Undef, false, err
	}
	return c, true, nil
}
