package blake3bao

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"lukechampine.com/blake3"
)

type vector struct {
	size     int
	root     [32]byte
	outboard []byte
}

// loadVectors reads testdata/bao-tree-vectors.txt, made by testdata/gen from
// n0's bao-tree crate with 16 KiB groups, the configuration iroh-blobs uses.
func loadVectors(t *testing.T) []vector {
	f, err := os.Open("testdata/bao-tree-vectors.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []vector
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		fs := strings.Fields(line)
		var v vector
		v.size, _ = strconv.Atoi(fs[0])
		r, _ := hex.DecodeString(fs[1])
		copy(v.root[:], r)
		if fs[2] != "-" {
			v.outboard, _ = hex.DecodeString(fs[2])
		}
		out = append(out, v)
	}
	return out
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

type store map[cid.Cid][]byte

func (s store) get(_ context.Context, c cid.Cid) ([]byte, error) {
	b, ok := s[c]
	if !ok {
		return nil, fmt.Errorf("missing %s", c)
	}
	return b, nil
}

func toStore(bs []blocks.Block) store {
	s := store{}
	for _, b := range bs {
		s[b.Cid()] = b.RawData()
	}
	return s
}

func TestBaoTreeVectors(t *testing.T) {
	ctx := context.Background()
	for _, v := range loadVectors(t) {
		t.Run(strconv.Itoa(v.size), func(t *testing.T) {
			data := pattern(v.size)
			root, bs, err := EncodeBytes(CodecRaw, data)
			if err != nil {
				t.Fatal(err)
			}
			// The root CID's digest is the iroh hash, which is BLAKE3(blob).
			h, _ := IrohHash(root)
			if h != v.root || h != blake3.Sum256(data) {
				t.Fatalf("root %x, want %x", h, v.root)
			}
			s := toStore(bs)
			// Every block checks alone, through the multihash registry too.
			for c, b := range s {
				if err := Verify(c, b); err != nil {
					t.Fatal(err)
				}
				c2, err := c.Prefix().Sum(b)
				if err != nil || !c2.Equals(c) {
					t.Fatalf("registry sum %s, want %s (%v)", c2, c, err)
				}
			}
			groups := max(1, (v.size+GroupSize-1)/GroupSize)
			if len(bs) != 2*groups-1 {
				t.Fatalf("%d blocks, want %d", len(bs), 2*groups-1)
			}
			// Parent bodies in pre-order are iroh's outboard, byte for byte.
			ob, err := Outboard(ctx, root, s.get)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(ob, v.outboard) {
				t.Fatalf("outboard differs from bao-tree (%d vs %d bytes)", len(ob), len(v.outboard))
			}
			// And iroh's outboard plus data rebuilds the same blocks.
			var bs2 []blocks.Block
			if err := FromOutboard(CodecRaw, v.root, data, v.outboard, func(b blocks.Block) error {
				bs2 = append(bs2, b)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(bs2) != len(bs) {
				t.Fatalf("FromOutboard made %d blocks, want %d", len(bs2), len(bs))
			}
			for _, b := range bs2 {
				if !bytes.Equal(s[b.Cid()], b.RawData()) {
					t.Fatalf("FromOutboard block %s differs", b.Cid())
				}
			}
			size, err := Size(ctx, root, s.get)
			if err != nil || size != uint64(v.size) {
				t.Fatalf("Size = %d, %v", size, err)
			}
			var buf bytes.Buffer
			if err := Read(ctx, root, size, s.get, &buf, 0, size); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buf.Bytes(), data) {
				t.Fatal("read back differs")
			}
		})
	}
}

func TestRangeRead(t *testing.T) {
	ctx := context.Background()
	data := pattern(5*GroupSize + 777)
	root, bs, _ := EncodeBytes(CodecRaw, data)
	s := toStore(bs)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		off := uint64(rng.Intn(len(data) + 1))
		n := uint64(rng.Intn(3 * GroupSize))
		// Count fetches: a range should touch only the blocks above it.
		fetched := 0
		get := func(ctx context.Context, c cid.Cid) ([]byte, error) { fetched++; return s.get(ctx, c) }
		var buf bytes.Buffer
		if err := Read(ctx, root, uint64(len(data)), get, &buf, off, n); err != nil {
			t.Fatal(err)
		}
		end := min(off+n, uint64(len(data)))
		if !bytes.Equal(buf.Bytes(), data[off:end]) {
			t.Fatalf("range %d+%d differs", off, n)
		}
		if fetched > len(bs) {
			t.Fatal("fetched more than the whole tree")
		}
	}
}

func TestTamper(t *testing.T) {
	data := pattern(3*GroupSize + 5)
	_, bs, _ := EncodeBytes(CodecRaw, data)
	for _, b := range bs {
		raw := append([]byte(nil), b.RawData()...)
		raw[len(raw)-1] ^= 1
		if err := Verify(b.Cid(), raw); !errors.Is(err, ErrMismatch) {
			t.Fatalf("flipped byte: %v", err)
		}
		// Moving a leaf (changing its start chunk) changes its hash.
		n, _ := Parse(b.RawData())
		if n.Kind == KindLeaf {
			n.StartChunk += 1 << ChunkLog
			if err := Verify(b.Cid(), n.Encode()); !errors.Is(err, ErrMismatch) {
				t.Fatalf("moved leaf: %v", err)
			}
		}
		// Dropping or adding the root flag changes the hash.
		raw = append([]byte(nil), b.RawData()...)
		if raw[0] == KindRootParent {
			raw[0] = KindParent
			if err := Verify(b.Cid(), raw); !errors.Is(err, ErrMismatch) {
				t.Fatalf("root flag: %v", err)
			}
		}
	}
}

// A tree whose blocks all hash correctly but whose shape is not BLAKE3's
// (a short leaf on the left) must be refused by Read, because its root would
// not be BLAKE3 of the content it spells.
func TestBadShape(t *testing.T) {
	ctx := context.Background()
	s := store{}
	put := func(n Node) [32]byte {
		h := n.Hash()
		s[CID(CodecRaw, h)] = n.Encode()
		return h
	}
	l := put(Node{Kind: KindLeaf, StartChunk: 0, Data: []byte("short")})
	r := put(Node{Kind: KindLeaf, StartChunk: 16, Data: pattern(GroupSize)})
	root := CID(CodecRaw, put(Node{Kind: KindRootParent, Left: l, Right: r}))
	size, err := Size(ctx, root, s.get)
	if err != nil {
		t.Fatal(err)
	}
	if err := Read(ctx, root, size, s.get, &bytes.Buffer{}, 0, size); !errors.Is(err, ErrShape) {
		t.Fatalf("bad shape read: %v", err)
	}
	// Claiming the wrong size fails too.
	data := pattern(2*GroupSize + 1)
	root, bs, _ := EncodeBytes(CodecRaw, data)
	if err := Read(ctx, root, uint64(len(data)-1), toStore(bs).get, &bytes.Buffer{}, 0, 1<<30); !errors.Is(err, ErrShape) {
		t.Fatalf("wrong size read: %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string][]byte{
		"empty":           {},
		"unknown kind":    {0x04, 1},
		"short parent":    append([]byte{KindParent}, make([]byte, 63)...),
		"long parent":     append([]byte{KindRootParent}, make([]byte, 65)...),
		"empty leaf":      {KindLeaf, 0x10},
		"unaligned leaf":  {KindLeaf, 0x01, 'x'},
		"nonminimal":      {KindLeaf, 0x90, 0x00, 'x'},
		"oversized leaf":  append([]byte{KindRootLeaf}, make([]byte, GroupSize+1)...),
		"oversized leaf2": append([]byte{KindLeaf, 0x00}, make([]byte, GroupSize+1)...),
	}
	for name, b := range cases {
		if _, err := Parse(b); !errors.Is(err, ErrBadBlock) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The empty blob is a root leaf with no data, and hashes to BLAKE3("").
	h, err := Hash([]byte{KindRootLeaf})
	if err != nil || h != blake3.Sum256(nil) {
		t.Fatalf("empty blob: %x %v", h, err)
	}
}

func TestHashSeq(t *testing.T) {
	ctx := context.Background()
	// A hash sequence of 1000 blobs spans several groups (32000 bytes).
	var seq []byte
	var kids []cid.Cid
	s := store{}
	for i := 0; i < 1000; i++ {
		c, bs, _ := EncodeBytes(CodecRaw, []byte(fmt.Sprintf("blob %d", i)))
		for _, b := range bs {
			s[b.Cid()] = b.RawData()
		}
		h, _ := IrohHash(c)
		seq = append(seq, h[:]...)
		kids = append(kids, c)
	}
	root, bs, err := EncodeBytes(CodecHashSeq, seq)
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := IrohHash(root); h != blake3.Sum256(seq) {
		t.Fatal("hashseq root is not BLAKE3 of the sequence")
	}
	for _, b := range bs {
		s[b.Cid()] = b.RawData()
	}
	// Walking links from the root reaches every child blob root, in order.
	var got []cid.Cid
	var walk func(c cid.Cid)
	walk = func(c cid.Cid) {
		b, _ := s.get(ctx, c)
		if err := Verify(c, b); err != nil {
			t.Fatal(err)
		}
		ls, err := Links(c.Type(), b)
		if err != nil {
			t.Fatal(err)
		}
		if c.Type() == CodecHashSeq && len(ls) != 2 {
			n, _ := Parse(b)
			if !n.IsParent() {
				got = append(got, ls...)
				return
			}
		}
		for _, l := range ls {
			walk(l)
		}
	}
	walk(root)
	if len(got) != len(kids) {
		t.Fatalf("%d links, want %d", len(got), len(kids))
	}
	for i := range kids {
		if !got[i].Equals(kids[i]) {
			t.Fatalf("link %d = %s, want %s", i, got[i], kids[i])
		}
	}
	if _, _, err := EncodeBytes(CodecHashSeq, []byte("not 32")); err == nil {
		t.Fatal("odd hashseq accepted")
	}
}

func BenchmarkEncode(b *testing.B) {
	data := pattern(64 << 20)
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		Encode(CodecRaw, bytes.NewReader(data), uint64(len(data)), func(blocks.Block) error { return nil })
	}
}
