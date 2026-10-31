package repo

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/fxamacker/cbor/v2"
	"github.com/ipfs/go-cid"
	"github.com/mr-tron/base58"
	"github.com/multiformats/go-multihash"
)

func (r *fsRepo) MFSRoot(ctx context.Context) (cid.Cid, bool, error) {
	if r.v < 3 {
		return cid.Undef, false, nil
	}
	b, err := r.ds.Get("/local/filesroot")
	if errors.Is(err, ErrNotFound) {
		return cid.Undef, false, nil
	}
	if err != nil {
		return cid.Undef, false, err
	}
	c, err := cid.Cast(b)
	if err != nil {
		return cid.Undef, false, fmt.Errorf("MFS root /local/filesroot is not a CID: %w", err)
	}
	return c, true, nil
}

// Pins lists recursive and direct pins, sorted by mode then CID.
func (r *fsRepo) Pins(ctx context.Context) ([]Pin, error) {
	var ps []Pin
	var err error
	switch {
	case r.v <= 2:
		ps, err = r.keyListPins()
	case r.v <= 10:
		ps, err = r.dagPins(ctx)
	default:
		ps, err = r.dsPins()
	}
	if err != nil {
		return nil, fmt.Errorf("reading pins: %w", err)
	}
	sortPins(ps)
	return ps, nil
}

// sortPins orders pins by mode, then CID.
func sortPins(ps []Pin) {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].Mode != ps[j].Mode {
			return ps[i].Mode < ps[j].Mode
		}
		return ps[i].Root.KeyString() < ps[j].Root.KeyString()
	})
}

// keyListPins reads repos 0..2: JSON lists of base58 multihashes.
func (r *fsRepo) keyListPins() ([]Pin, error) {
	var ps []Pin
	for _, m := range []PinMode{Recursive, Direct} {
		b, err := r.ds.Get("/local/pins/" + m.String() + "/keys")
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var keys []string
		if err := json.Unmarshal(b, &keys); err != nil {
			return nil, fmt.Errorf("%s pin list: %w", m, err)
		}
		for _, k := range keys {
			raw, err := base58.Decode(k)
			if err != nil {
				return nil, fmt.Errorf("%s pin %q: %w", m, k, err)
			}
			mh, err := multihash.Cast(raw)
			if err != nil {
				return nil, fmt.Errorf("%s pin %q: %w", m, k, err)
			}
			ps = append(ps, Pin{Root: cid.NewCidV0(mh), Mode: m})
		}
	}
	return ps, nil
}

// emptyNode is the empty dag-pb node the pin DAG uses for empty buckets.
var emptyNode = cid.MustParse("QmdfTbBqBPQ7VNxZEYEj14VmRuZBkqFbiwReogJgS1zR1n")

// dagPins reads repos 3..10: /local/pins names a dag-pb node whose "recursive"
// and "direct" links point to pin sets (go-ipfs pin/set.go).
func (r *fsRepo) dagPins(ctx context.Context) ([]Pin, error) {
	b, err := r.ds.Get("/local/pins")
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	root, err := cid.Cast(b)
	if err != nil {
		return nil, fmt.Errorf("/local/pins is not a CID: %w", err)
	}
	node, err := r.pbNode(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("pin root %s: %w", root, err)
	}
	var ps []Pin
	for _, m := range []PinMode{Recursive, Direct} {
		for _, l := range node.links {
			if l.name != m.String() {
				continue
			}
			if err := r.walkPinSet(ctx, l.cid, func(c cid.Cid) {
				ps = append(ps, Pin{Root: c, Mode: m})
			}); err != nil {
				return nil, fmt.Errorf("%s pin set: %w", m, err)
			}
		}
	}
	return ps, nil
}

// walkPinSet visits the items of a pin set node: a varint-prefixed header
// {version, fanout, seed}, then fanout bucket links, then the items.
func (r *fsRepo) walkPinSet(ctx context.Context, c cid.Cid, fn func(cid.Cid)) error {
	if c.Equals(emptyNode) {
		return nil
	}
	n, err := r.pbNode(ctx, c)
	if err != nil {
		return fmt.Errorf("%s: %w", c, err)
	}
	hlen, k := binary.Uvarint(n.data)
	if k <= 0 || hlen > uint64(len(n.data)-k) {
		return fmt.Errorf("%s: bad pin set header", c)
	}
	var version, fanout uint64
	err = pbFields(n.data[k:k+int(hlen)], func(f int, v uint64, _ []byte) error {
		switch f {
		case 1:
			version = v
		case 2:
			fanout = v
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%s: pin set header: %w", c, err)
	}
	if version != 1 || fanout > uint64(len(n.links)) {
		return fmt.Errorf("%s: unsupported pin set (version %d, fanout %d)", c, version, fanout)
	}
	for _, l := range n.links[fanout:] {
		fn(l.cid)
	}
	for _, l := range n.links[:fanout] {
		if err := r.walkPinSet(ctx, l.cid, fn); err != nil {
			return err
		}
	}
	return nil
}

// dsPins reads repos 11 and later: one CBOR record per pin under /pins/pin/
// (boxo dspinner). The /pins/index keys are derived from these and skipped.
func (r *fsRepo) dsPins() ([]Pin, error) {
	var ps []Pin
	var keys []string
	err := r.ds.Query("/pins/pin/", func(k string, _ int) error {
		keys = append(keys, k)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		b, err := r.ds.Get(k)
		if err != nil {
			return nil, err
		}
		var rec struct {
			Cid  []byte `cbor:"cid"`
			Mode int    `cbor:"mode"`
			Name string `cbor:"name"`
		}
		if err := cbor.Unmarshal(b, &rec); err != nil {
			return nil, fmt.Errorf("pin %s: %w", strings.TrimPrefix(k, "/pins/pin/"), err)
		}
		c, err := cid.Cast(rec.Cid)
		if err != nil {
			return nil, fmt.Errorf("pin %s: %w", strings.TrimPrefix(k, "/pins/pin/"), err)
		}
		// boxo pinner modes: 0 recursive, 1 direct.
		switch rec.Mode {
		case 0:
			ps = append(ps, Pin{Root: c, Mode: Recursive, Name: rec.Name})
		case 1:
			ps = append(ps, Pin{Root: c, Mode: Direct, Name: rec.Name})
		default:
			return nil, fmt.Errorf("pin %s has unknown mode %d", c, rec.Mode)
		}
	}
	return ps, nil
}

type pbLink struct {
	cid  cid.Cid
	name string
}

type pbNode struct {
	data  []byte
	links []pbLink
}

func (r *fsRepo) pbNode(ctx context.Context, c cid.Cid) (*pbNode, error) {
	if c.Type() != cid.DagProtobuf {
		return nil, fmt.Errorf("not dag-pb")
	}
	b, err := r.Get(ctx, c.Hash())
	if err != nil {
		return nil, err
	}
	if sum, err := c.Prefix().Sum(b); err != nil || !sum.Equals(c) {
		return nil, fmt.Errorf("block fails its hash (corrupt repo?)")
	}
	return decodePB(b)
}

// decodePB decodes a dag-pb node: Links = 2 (Hash = 1, Name = 2), Data = 1.
func decodePB(b []byte) (*pbNode, error) {
	n := &pbNode{}
	err := pbFields(b, func(f int, _ uint64, v []byte) error {
		switch f {
		case 1:
			n.data = v
		case 2:
			var l pbLink
			err := pbFields(v, func(f int, _ uint64, v []byte) error {
				var err error
				switch f {
				case 1:
					l.cid, err = cid.Cast(v)
				case 2:
					l.name = string(v)
				}
				return err
			})
			if err != nil {
				return err
			}
			n.links = append(n.links, l)
		}
		return nil
	})
	return n, err
}

// pbFields walks protobuf fields, passing varints as v and length-delimited
// fields as b.
func pbFields(b []byte, fn func(field int, v uint64, b []byte) error) error {
	for len(b) > 0 {
		tag, k := binary.Uvarint(b)
		if k <= 0 {
			return errors.New("bad protobuf tag")
		}
		b = b[k:]
		var v uint64
		var val []byte
		switch tag & 7 {
		case 0:
			v, k = binary.Uvarint(b)
			if k <= 0 {
				return errors.New("bad protobuf varint")
			}
			b = b[k:]
		case 1, 5:
			w := 8
			if tag&7 == 5 {
				w = 4
			}
			if len(b) < w {
				return errors.New("short protobuf field")
			}
			b = b[w:]
		case 2:
			l, k := binary.Uvarint(b)
			if k <= 0 || l > uint64(len(b)-k) {
				return errors.New("bad protobuf length")
			}
			val, b = b[k:k+int(l)], b[k+int(l):]
		default:
			return fmt.Errorf("protobuf wire type %d not supported", tag&7)
		}
		if err := fn(int(tag>>3), v, val); err != nil {
			return err
		}
	}
	return nil
}
