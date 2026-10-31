// Package repotest serves repos the way other programs would, for tests.
package repotest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/repo"
)

// KuboRPC answers the read-only kubo RPC commands ipfs2fil uses from r, the
// way kubo 0.38 does. Old turns on the pre-0.27 pin/ls answer: one object
// of all keys, no names.
func KuboRPC(r repo.Repo, old bool) http.Handler {
	ctx := context.Background()
	mux := http.NewServeMux()
	cidOf := func(h multihash.Multihash) cid.Cid { return cid.NewCidV1(cid.Raw, h) }
	fail := func(w http.ResponseWriter, msg string) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"Message": msg, "Code": 0, "Type": "error"})
	}
	mux.HandleFunc("/api/v0/version", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"Version": "0.38.0"})
	})
	mux.HandleFunc("/api/v0/refs/local", func(w http.ResponseWriter, _ *http.Request) {
		e := json.NewEncoder(w)
		r.ForEachBlock(ctx, func(h multihash.Multihash, _ int) error {
			return e.Encode(map[string]string{"Ref": cidOf(h).String(), "Err": ""})
		})
	})
	get := func(w http.ResponseWriter, req *http.Request) ([]byte, bool) {
		c, err := cid.Decode(req.URL.Query().Get("arg"))
		if err != nil {
			fail(w, err.Error())
			return nil, false
		}
		data, err := r.Get(ctx, c.Hash())
		if err == repo.ErrNotFound {
			fail(w, "block was not found locally (offline): ipld: could not find "+c.String())
			return nil, false
		} else if err != nil {
			fail(w, err.Error())
			return nil, false
		}
		return data, true
	}
	mux.HandleFunc("/api/v0/block/stat", func(w http.ResponseWriter, req *http.Request) {
		if data, ok := get(w, req); ok {
			json.NewEncoder(w).Encode(map[string]any{"Key": req.URL.Query().Get("arg"), "Size": len(data)})
		}
	})
	mux.HandleFunc("/api/v0/block/get", func(w http.ResponseWriter, req *http.Request) {
		if data, ok := get(w, req); ok {
			w.Write(data)
		}
	})
	mux.HandleFunc("/api/v0/pin/ls", func(w http.ResponseWriter, req *http.Request) {
		pins, err := r.Pins(ctx)
		if err != nil {
			fail(w, err.Error())
			return
		}
		typ := req.URL.Query().Get("type")
		keys := map[string]map[string]string{}
		e := json.NewEncoder(w)
		for _, p := range pins {
			if p.Mode.String() != typ {
				continue
			}
			if old {
				keys[p.Root.String()] = map[string]string{"Type": typ}
			} else {
				e.Encode(map[string]string{"Cid": p.Root.String(), "Name": p.Name, "Type": typ})
			}
		}
		if old {
			e.Encode(map[string]any{"Keys": keys})
		}
	})
	mux.HandleFunc("/api/v0/files/stat", func(w http.ResponseWriter, _ *http.Request) {
		c, ok, err := r.MFSRoot(ctx)
		if err != nil || !ok {
			fail(w, fmt.Sprint("file does not exist ", err))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"Hash": c.String(), "Type": "directory"})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || !strings.HasPrefix(req.URL.Path, "/api/v0/") {
			http.Error(w, "405 - Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		mux.ServeHTTP(w, req)
	})
}
