package atproto

import (
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"

	"github.com/fxamacker/cbor/v2"
	"github.com/ipfs/go-cid"
)

// Commit is a signed repo commit: the account, the repo format version, the
// root of the record tree, and the revision.
type Commit struct {
	DID     string
	Version int
	Data    cid.Cid
	Rev     string
}

var dec, _ = cbor.DecOptions{
	DefaultMapType:  reflect.TypeOf(map[string]any(nil)),
	MaxNestedLevels: 256,
}.DecMode()

// decode reads dag-cbor into Go values; tag 42 stays a cbor.Tag.
func decode(data []byte) (any, error) {
	var v any
	if err := dec.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// link reads a tag-42 CID.
func link(v any) (cid.Cid, bool) {
	t, ok := v.(cbor.Tag)
	if !ok || t.Number != 42 {
		return cid.Undef, false
	}
	b, ok := t.Content.([]byte)
	if !ok || len(b) < 2 || b[0] != 0 {
		return cid.Undef, false
	}
	c, err := cid.Cast(b[1:])
	return c, err == nil
}

// DecodeCommit reads a commit block. It returns an error for any block that
// is not one, so it doubles as a test for atproto repo roots.
func DecodeCommit(data []byte) (*Commit, error) {
	v, err := decode(data)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("not an atproto commit")
	}
	c := &Commit{}
	c.DID, _ = m["did"].(string)
	c.Rev, _ = m["rev"].(string)
	ver, _ := m["version"].(uint64)
	c.Version = int(ver)
	data_, okData := link(m["data"])
	_, okSig := m["sig"].([]byte)
	if c.DID == "" || !okData || !okSig || c.Version < 2 {
		return nil, errors.New("not an atproto commit")
	}
	c.Data = data_
	return c, nil
}

// Walk visits every record under the tree rooted at root, in key order. A
// key is "collection/rkey". get returns a block's bytes; a missing tree node
// is an error, a missing record is the caller's business.
func Walk(get func(cid.Cid) ([]byte, error), root cid.Cid, fn func(key string, rec cid.Cid) error) error {
	return walk(get, root, fn, 0)
}

func walk(get func(cid.Cid) ([]byte, error), node cid.Cid, fn func(string, cid.Cid) error, depth int) error {
	if depth > 128 {
		return errors.New("record tree too deep")
	}
	data, err := get(node)
	if err != nil {
		return fmt.Errorf("tree node %s: %w", node, err)
	}
	v, err := decode(data)
	if err != nil {
		return fmt.Errorf("tree node %s: %w", node, err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("tree node %s is not a map", node)
	}
	if l, ok := link(m["l"]); ok {
		if err := walk(get, l, fn, depth+1); err != nil {
			return err
		}
	}
	es, _ := m["e"].([]any)
	var prev []byte
	for _, e := range es {
		em, ok := e.(map[string]any)
		if !ok {
			return fmt.Errorf("tree node %s: bad entry", node)
		}
		p, _ := em["p"].(uint64)
		k, _ := em["k"].([]byte)
		if int(p) > len(prev) {
			return fmt.Errorf("tree node %s: bad key prefix", node)
		}
		key := append(append([]byte(nil), prev[:p]...), k...)
		prev = key
		if rec, ok := link(em["v"]); ok {
			if err := fn(string(key), rec); err != nil {
				return err
			}
		}
		if t, ok := link(em["t"]); ok {
			if err := walk(get, t, fn, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// Record decodes a record into the atproto JSON form: links become
// {"$link": cid}, bytes {"$bytes": base64}.
func Record(data []byte) (map[string]any, error) {
	v, err := decode(data)
	if err != nil {
		return nil, err
	}
	m, ok := toJSON(v).(map[string]any)
	if !ok {
		return nil, errors.New("record is not a map")
	}
	return m, nil
}

func toJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = toJSON(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = toJSON(e)
		}
		return out
	case []byte:
		return map[string]any{"$bytes": base64.RawStdEncoding.EncodeToString(x)}
	case cbor.Tag:
		if c, ok := link(x); ok {
			return map[string]any{"$link": c.String()}
		}
		return nil
	}
	return v
}
