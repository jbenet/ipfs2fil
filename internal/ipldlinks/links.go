// Package ipldlinks reads the links out of IPLD blocks: dag-pb and
// dag-cbor; every other codec is a leaf.
package ipldlinks

import (
	"errors"

	mdag "github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/go-cid"
)

// Links returns the CIDs a block links to.
func Links(c cid.Cid, data []byte) ([]cid.Cid, error) {
	switch c.Type() {
	case cid.DagProtobuf:
		n, err := mdag.DecodeProtobuf(data)
		if err != nil {
			return nil, err
		}
		out := make([]cid.Cid, len(n.Links()))
		for i, l := range n.Links() {
			out[i] = l.Cid
		}
		return out, nil
	case cid.DagCBOR:
		var out []cid.Cid
		rest, err := CBORLinks(data, 0, &out)
		if err == nil && len(rest) != 0 {
			err = ErrCBOR
		}
		return out, err
	}
	return nil, nil
}

// ErrCBOR is a block that is not one well-formed dag-cbor item.
var ErrCBOR = errors.New("malformed dag-cbor")

// cborLinks reads one dag-cbor item from b, appends every tag-42 CID in it to
// out, and returns what follows the item.
func CBORLinks(b []byte, depth int, out *[]cid.Cid) ([]byte, error) {
	if len(b) == 0 || depth > 512 {
		return nil, ErrCBOR
	}
	major, info := b[0]>>5, b[0]&0x1f
	b = b[1:]
	var arg uint64
	switch {
	case info < 24:
		arg = uint64(info)
	case info <= 27:
		n := 1 << (info - 24)
		if len(b) < n {
			return nil, ErrCBOR
		}
		for _, x := range b[:n] {
			arg = arg<<8 | uint64(x)
		}
		b = b[n:]
	default:
		return nil, ErrCBOR // indefinite lengths are not dag-cbor
	}
	switch major {
	case 0, 1, 7:
		return b, nil
	case 2, 3:
		if uint64(len(b)) < arg {
			return nil, ErrCBOR
		}
		return b[arg:], nil
	case 4, 5:
		n := arg
		if major == 5 {
			n *= 2
		}
		if n > uint64(len(b)) {
			return nil, ErrCBOR
		}
		var err error
		for range n {
			if b, err = CBORLinks(b, depth+1, out); err != nil {
				return nil, err
			}
		}
		return b, nil
	case 6:
		if arg != 42 {
			return CBORLinks(b, depth+1, out)
		}
		// a CID: a byte string holding 0x00 then the CID bytes
		start := b
		rest, err := CBORLinks(b, depth+1, &[]cid.Cid{})
		if err != nil || start[0]>>5 != 2 {
			return nil, ErrCBOR
		}
		raw := start[:len(start)-len(rest)]
		hdr := 1
		switch raw[0] & 0x1f {
		case 24:
			hdr = 2
		case 25:
			hdr = 3
		case 26:
			hdr = 5
		case 27:
			hdr = 9
		}
		raw = raw[hdr:]
		if len(raw) < 2 || raw[0] != 0 {
			return nil, ErrCBOR
		}
		c, err := cid.Cast(raw[1:])
		if err != nil {
			return nil, ErrCBOR
		}
		*out = append(*out, c)
		return rest, nil
	}
	return nil, ErrCBOR
}
