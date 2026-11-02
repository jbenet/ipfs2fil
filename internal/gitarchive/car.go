package gitarchive

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"

	"github.com/fxamacker/cbor/v2"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

// WriteCAR streams blocks into out. The root is only known at the end, so
// the header is written with a placeholder of the same length and
// rewritten; every record CID is a 36-byte CIDv1 DAG-CBOR sha2-256.
func WriteCAR(out string, fn func(BlockSink) (*Result, error)) (*Result, error) {
	placeholder, _ := cid.V1Builder{Codec: cid.DagCBOR, MhType: multihash.SHA2_256}.Sum(nil)
	tmp := out + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	hdr, err := carHeader(placeholder)
	if err != nil {
		return nil, err
	}
	section := func(parts ...[]byte) error {
		n := 0
		for _, p := range parts {
			n += len(p)
		}
		var buf [binary.MaxVarintLen64]byte
		if _, err := w.Write(buf[:binary.PutUvarint(buf[:], uint64(n))]); err != nil {
			return err
		}
		for _, p := range parts {
			if _, err := w.Write(p); err != nil {
				return err
			}
		}
		return nil
	}
	if err := section(hdr); err != nil {
		return nil, err
	}
	res, err := fn(func(c cid.Cid, data []byte) error { return section(c.Bytes(), data) })
	if err != nil {
		return nil, err
	}
	if res.Unchanged {
		return res, nil
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	real, err := carHeader(res.Root)
	if err != nil {
		return nil, err
	}
	if len(real) != len(hdr) {
		return nil, fmt.Errorf("root %s has an unexpected shape", res.Root)
	}
	var buf [binary.MaxVarintLen64]byte
	pre := buf[:binary.PutUvarint(buf[:], uint64(len(real)))]
	if _, err := f.WriteAt(append(pre, real...), 0); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return res, os.Rename(tmp, out)
}

// carHeader is a CARv1 header naming one root.
func carHeader(root cid.Cid) ([]byte, error) {
	enc, _ := cbor.CanonicalEncOptions().EncMode()
	return enc.Marshal(struct {
		Roots   []Link `cbor:"roots"`
		Version int    `cbor:"version"`
	}{[]Link{{Cid: root}}, 1})
}
