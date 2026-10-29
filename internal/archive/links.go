package archive

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	mdag "github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/boxo/ipld/unixfs"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/explorer"
)

// links returns the CIDs a block links to. dag-pb and dag-cbor are read;
// every other codec is a leaf.
func links(c cid.Cid, data []byte) ([]cid.Cid, error) {
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
		rest, err := cborLinks(data, 0, &out)
		if err == nil && len(rest) != 0 {
			err = errCBOR
		}
		return out, err
	}
	return nil, nil
}

// UnixFS node types the explorer cares about.
const (
	ufsNone = iota
	ufsFile
	ufsDir
	ufsShard
)

// ufsInfo is what the explorer index needs from one block.
type ufsInfo struct {
	kind  byte
	data  byte     // for file data: 1 + its sniffed kind's index in explorer.Kinds
	size  int64    // file bytes
	names []string // per link, for directories; "" marks a HAMT sub-shard
}

// sniffed records what file data looks like, so the explorer can type a file
// its name says nothing about without reading its first block again.
func sniffed(d []byte) byte {
	if len(d) == 0 {
		return 0
	}
	return byte(slices.Index(explorer.Kinds, explorer.Sniff(d)) + 1)
}

// unixfsInfo reads a block's UnixFS type, file size and link names.
func unixfsInfo(c cid.Cid, data []byte) ufsInfo {
	if c.Type() == cid.Raw {
		return ufsInfo{kind: ufsFile, data: sniffed(data), size: int64(len(data))}
	}
	if c.Type() != cid.DagProtobuf {
		return ufsInfo{}
	}
	n, err := mdag.DecodeProtobuf(data)
	if err != nil {
		return ufsInfo{}
	}
	fs, err := unixfs.FSNodeFromBytes(n.Data())
	if err != nil {
		return ufsInfo{}
	}
	switch fs.Type() {
	case unixfs.TFile, unixfs.TRaw:
		return ufsInfo{kind: ufsFile, data: sniffed(fs.Data()), size: int64(fs.FileSize())}
	case unixfs.TDirectory, unixfs.THAMTShard:
		info := ufsInfo{kind: ufsDir}
		pad := 0
		if fs.Type() == unixfs.THAMTShard {
			info.kind = ufsShard
			if fs.Fanout() < 2 {
				return ufsInfo{}
			}
			pad = len(fmt.Sprintf("%X", fs.Fanout()-1))
		}
		for _, l := range n.Links() {
			if len(l.Name) < pad {
				return ufsInfo{}
			}
			info.names = append(info.names, l.Name[pad:])
		}
		return info
	}
	return ufsInfo{}
}

// guess picks a CID for a block known only by multihash (an orphan; repo 12
// and later key blocks by multihash): dag-pb if it round-trips as dag-pb,
// dag-cbor if it parses as one CBOR item, raw otherwise. The multihash, which
// is what retrieval uses, is the same either way.
func guess(h multihash.Multihash, data []byte) cid.Cid {
	if n, err := mdag.DecodeProtobuf(data); err == nil {
		if enc, err := n.EncodeProtobuf(true); err == nil && bytes.Equal(enc, data) {
			return cid.NewCidV1(cid.DagProtobuf, h)
		}
	}
	var ls []cid.Cid
	if rest, err := cborLinks(data, 0, &ls); err == nil && len(rest) == 0 && len(ls) > 0 {
		return cid.NewCidV1(cid.DagCBOR, h)
	}
	return cid.NewCidV1(cid.Raw, h)
}

var errCBOR = errors.New("malformed dag-cbor")

// cborLinks reads one dag-cbor item from b, appends every tag-42 CID in it to
// out, and returns what follows the item.
func cborLinks(b []byte, depth int, out *[]cid.Cid) ([]byte, error) {
	if len(b) == 0 || depth > 512 {
		return nil, errCBOR
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
			return nil, errCBOR
		}
		for _, x := range b[:n] {
			arg = arg<<8 | uint64(x)
		}
		b = b[n:]
	default:
		return nil, errCBOR // indefinite lengths are not dag-cbor
	}
	switch major {
	case 0, 1, 7:
		return b, nil
	case 2, 3:
		if uint64(len(b)) < arg {
			return nil, errCBOR
		}
		return b[arg:], nil
	case 4, 5:
		n := arg
		if major == 5 {
			n *= 2
		}
		if n > uint64(len(b)) {
			return nil, errCBOR
		}
		var err error
		for range n {
			if b, err = cborLinks(b, depth+1, out); err != nil {
				return nil, err
			}
		}
		return b, nil
	case 6:
		if arg != 42 {
			return cborLinks(b, depth+1, out)
		}
		// a CID: a byte string holding 0x00 then the CID bytes
		start := b
		rest, err := cborLinks(b, depth+1, &[]cid.Cid{})
		if err != nil || start[0]>>5 != 2 {
			return nil, errCBOR
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
			return nil, errCBOR
		}
		c, err := cid.Cast(raw[1:])
		if err != nil {
			return nil, errCBOR
		}
		*out = append(*out, c)
		return rest, nil
	}
	return nil, errCBOR
}
