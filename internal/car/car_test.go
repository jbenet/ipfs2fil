package car

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/ipfs/go-cid"
	carv2 "github.com/ipld/go-car/v2"
	"github.com/multiformats/go-multihash"
)

func block(t *testing.T, codec uint64, data []byte) cid.Cid {
	t.Helper()
	h, err := multihash.Sum(data, multihash.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	return cid.NewCidV1(codec, h)
}

func TestWriteRead(t *testing.T) {
	var cids []cid.Cid
	var datas [][]byte
	for i := range 50 {
		d := bytes.Repeat([]byte(fmt.Sprint(i)), 100*i+1)
		cids = append(cids, block(t, cid.Raw, d))
		datas = append(datas, d)
	}
	// a CIDv0 root, as kubo pins usually are
	h, _ := multihash.Sum([]byte("root"), multihash.SHA2_256, -1)
	root := cid.NewCidV0(h)

	var buf bytes.Buffer
	w, err := NewWriter(&buf, root)
	if err != nil {
		t.Fatal(err)
	}
	want := HeaderSize(root)
	for i, c := range cids {
		if err := w.Put(c, datas[i]); err != nil {
			t.Fatal(err)
		}
		want += EntrySize(c, len(datas[i]))
	}
	sum, err := w.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if sum.Bytes != want || int64(buf.Len()) != want || sum.Blocks != 50 {
		t.Fatalf("size %d, buffer %d, predicted %d, blocks %d", sum.Bytes, buf.Len(), want, sum.Blocks)
	}
	pc, n, err := PieceCID(bytes.NewReader(buf.Bytes()))
	if err != nil || n != want || !pc.Equals(sum.PieceCID) {
		t.Fatalf("PieceCID %s (%d bytes, %v), writer said %s", pc, n, err, sum.PieceCID)
	}

	// our reader
	r, err := NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Root.Equals(root) {
		t.Fatalf("root %s, want %s", r.Root, root)
	}
	for i := 0; ; i++ {
		c, d, err := r.Next()
		if err == io.EOF {
			if i != 50 {
				t.Fatalf("read %d blocks", i)
			}
			break
		}
		if err != nil || !c.Equals(cids[i]) || !bytes.Equal(d, datas[i]) {
			t.Fatalf("block %d: %s %v", i, c, err)
		}
	}

	// go-car, which kubo's dag import uses
	br, err := carv2.NewBlockReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(br.Roots) != 1 || !br.Roots[0].Equals(root) {
		t.Fatalf("go-car roots %v", br.Roots)
	}
	for i := 0; ; i++ {
		b, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil || !b.Cid().Equals(cids[i]) {
			t.Fatalf("go-car block %d: %v", i, err)
		}
	}
}

func TestTooSmall(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, block(t, cid.Raw, []byte("x")))
	if _, err := w.Finish(); err == nil {
		t.Fatal("a CAR under 65 bytes should not get a PieceCID")
	}
}

func TestBadHeader(t *testing.T) {
	if _, err := NewReader(bytes.NewReader([]byte{3, 1, 2, 3})); err == nil {
		t.Fatal("expected an error")
	}
}
