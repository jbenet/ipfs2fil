package archive

import (
	"bytes"
	"fmt"
	"slices"

	mdag "github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/boxo/ipld/unixfs"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/explorer"
	"github.com/jbenet/ipfs2fil/internal/ipldlinks"
)

// links returns the CIDs a block links to. dag-pb and dag-cbor are read;
// every other codec is a leaf.
func links(c cid.Cid, data []byte) ([]cid.Cid, error) { return ipldlinks.Links(c, data) }

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
	if rest, err := ipldlinks.CBORLinks(data, 0, &ls); err == nil && len(rest) == 0 && len(ls) > 0 {
		return cid.NewCidV1(cid.DagCBOR, h)
	}
	return cid.NewCidV1(cid.Raw, h)
}
