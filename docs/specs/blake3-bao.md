# blake3-bao: an IPLD codec for iroh blobs

Status: draft, 2026-11-01. One-sided spec, open for review by n0 and anyone else. Codes are placeholders. Prototype: `internal/blake3bao`.

Use design A: each block is one node of the BLAKE3 tree, cut at iroh's 16 KiB chunk groups, so a blob's root CID carries its iroh hash as the digest and every block can be checked alone. That takes one new multihash and two new codecs. A Go prototype in `internal/blake3bao` matches n0's `bao-tree` crate byte for byte, including iroh's outboard.

## Design A or B

Choose A. B already exists in all but name: it is a UnixFS file with BLAKE3 leaves plus an outboard on the side.

The problem both designs solve: iroh names a blob of any size by `BLAKE3(blob)`. Inside BLAKE3, a subtree's hash (its chaining value, CV) depends on where it sits: a leaf's chunks are compressed with their chunk counters, and only the root gets the ROOT flag. So a 16 KiB piece of a blob does not hash to the CV the tree holds for it, and plain `raw` + `blake3` blocks cannot be tied to the iroh hash one block at a time.

|  | A: tree-node blocks (recommended) | B: plain blocks plus a mapping |
| --- | --- | --- |
| Blocks | One per BLAKE3 tree node: leaves carry their start chunk, parents carry two child CVs | `raw` + `blake3` leaves of 16 KiB, plus bao tree nodes, plus a record mapping the iroh hash to both |
| Root CID | Digest is the iroh hash | Some other hash; the iroh hash sits in the mapping record |
| Check one block alone | Yes, against its own CID | Leaf yes; its place in the iroh tree only after rehashing with counters |
| Find by iroh hash (IPNI, gateways) | Prefix the hash: one CID per hash | Needs the mapping record first |
| Range reads | Fetch the blocks above the range, check each | Fetch leaves, then check them against the outboard |
| Matches iroh's outboard | Parent blocks are iroh's outboard pairs, byte for byte | Outboard stored as a separate blob |
| Plain IPFS tools today | Can carry and check blocks once the multihash is registered; cannot render | Gateways can render the UnixFS view |
| New codes | 1 multihash, 2 codecs | None, or one codec for the mapping |

The one thing B has that A lacks is rendering in today's gateways. ipfs2fil's explorer can render A itself, and a UnixFS view can still be added later beside an A archive if we want gateway viewing.

## Blocks

A block is one tree node with a one-byte kind in front, and it holds everything its hash needs. The tree is BLAKE3's own tree, cut at chunk groups of 16 chunks (16 KiB), the group size iroh uses. The group size is fixed by this spec, so every CID has exactly one valid block.

| Kind | Node | Bytes after the kind byte | Hash (32-byte digest) |
| --- | --- | --- | --- |
| `0x00` | Leaf | uvarint start chunk, then 1 to 16384 bytes of data | CV of the data as a BLAKE3 subtree whose first chunk counter is the start chunk |
| `0x01` | Root leaf | 0 to 16384 bytes of data (start chunk is 0) | Same, with the ROOT flag: plain `BLAKE3(data)` |
| `0x02` | Parent | left child CV (32), right child CV (32) | BLAKE3 parent compression of the 64 bytes, PARENT flag |
| `0x03` | Root parent | left child CV (32), right child CV (32) | Same, with PARENT and ROOT flags: the blob's BLAKE3 hash |

Rules a block must meet, or it is invalid and has no hash:

- The start chunk is a minimal uvarint and a multiple of 16. A leaf never straddles a group boundary.
- A non-root leaf has 1 to 16384 bytes. Only the root leaf of the empty blob has 0 bytes.
- A parent body is exactly 64 bytes.
- In a hash sequence tree (next sections), leaf data is a multiple of 32 bytes. 16384 is a multiple of 32, so a hash never splits across leaves.

The multihash is a tree hash over a framed input, as `sha2-256-trunc254-padded` (Filecoin's CommP) is a tree hash over padded input. It is not `BLAKE3(block)`, so it needs its own code. Only the root kinds give a value equal to the iroh hash.

Codes. The prototype uses private-use placeholders until the multicodec table has entries:

| Name | Type | Placeholder | Meaning |
| --- | --- | --- | --- |
| `blake3-bao` | multihash | `0x3b3020` | CV of a framed BLAKE3 tree node, 32 bytes |
| `blake3-bao-raw` | ipld codec | `0x3b3000` | A tree node of a blob of opaque bytes (iroh `BlobFormat::Raw`) |
| `blake3-bao-hashseq` | ipld codec | `0x3b3001` | A tree node of a hash sequence (iroh `BlobFormat::HashSeq`); leaves link to blobs |

## At a glance

A 40 KiB blob (40 chunks) is five blocks. BLAKE3 splits 40 chunks as 32 on the left and 8 on the right, so the last leaf hangs one level higher than the others. The two parent bodies, in pre-order, are exactly iroh's outboard for this blob.

```
                 Root parent 0x03  (digest = BLAKE3(blob) = iroh hash)
                 /                              \
        Parent 0x02                        Leaf 0x00, chunk 32
        /          \                       bytes 32768..40959 (8 KiB)
Leaf 0x00, chunk 0   Leaf 0x00, chunk 16
bytes 0..16383       bytes 16384..32767
```

Every edge is a 32-byte CV. A child's CID is (codec, blake3-bao, CV), so each block checks alone.

## CIDs and iroh hashes

An iroh hash maps to a CID by adding a prefix, and back by dropping it. No lookup table is needed in either direction.

- A raw blob with iroh hash `h` is `CIDv1(blake3-bao-raw, blake3-bao, h)`.
- A hash sequence with iroh hash `h` is `CIDv1(blake3-bao-hashseq, blake3-bao, h)`.
- A parent's children are `CIDv1(<same codec>, blake3-bao, left)` and `(…, right)`. The child CID does not say whether the child is a leaf or a parent; its block's kind byte does.
- Each 32-byte hash in a hash sequence leaf is a link to `CIDv1(blake3-bao-raw, blake3-bao, hash)`. Like iroh, a hash sequence does not say its children's format; readers that know better (a collection's metadata blob, say) can reinterpret.

The codec says what the content is; the multihash says the block is a framed tree node. That keeps codecs decodable without looking at the hash, which IPLD expects.

**How this sits with `0x80`.** The table has `blake3-hashseq` (`0x80`, draft) and iroh writes collection CIDs as `CIDv1(0x80, blake3, h)`, and blob CIDs as `CIDv1(raw, blake3, h)`. Those CIDs name the whole blob as one block. They stay valid and have the same digest as ours, so they are aliases: swapping the prefix converts between them. They only travel well for blobs under about 1 MiB, the block limit of bitswap and gateways. We do not reuse `0x80` for tree nodes, because then the codec's meaning would depend on the multihash.

## Reading and verifying

A block store checks a block the usual way: hash it with the CID's multihash and compare. A reader that wants the blob also checks the tree's shape, because only BLAKE3's shape makes the root equal `BLAKE3(content)`.

1. **Find the size.** iroh carries a blob's size beside its hash, and ipfs2fil's manifest can too. Without it, walk the right edge from the root (one block per level): size = last leaf's start chunk × 1024 + its length.
2. **Walk with the expected shape.** A subtree of n bytes is a leaf if n ≤ 16384, otherwise a parent whose left side holds the largest power-of-two number of chunks that leaves at least one byte on the right (BLAKE3's rule). Each leaf must carry the start chunk and length its place implies. Only the root block may carry a root kind.
3. **Refuse anything else.** A tree of valid blocks in the wrong shape names content that is not what BLAKE3 would hash to that root, so it is an error, not a different blob.

With these checks, a full read proves the root digest is the BLAKE3 hash of the bytes read, so it equals the iroh hash. Checking leaf counters also pins the length, which is what bao's final-chunk check does.

**Ranges.** To read bytes `[off, off+len)`, fetch only the parents above that range and the leaves inside it, about two blocks per tree level plus the leaves. Each block is checked as it arrives, so an explorer can stream the middle of a 30 GB video without the rest.

## Cost

A blob of g groups becomes 2g − 1 blocks, and storage grows by about 0.9%. That is the price of 16 KiB blocks; it buys exact iroh interop.

| Measure | blake3-bao | For comparison |
| --- | --- | --- |
| Blocks for 1 GB | 122,071 (61,036 leaves, 61,035 parents) | UnixFS at 256 KiB: about 3,800 |
| Blocks for 30 GB | about 3.7 million | UnixFS at 256 KiB: about 115,000 |
| CAR overhead per 16 KiB | about 150 bytes (leaf 45, parent 104), 0.9% | iroh's own outboard: 64 bytes, 0.4% |
| Encode speed, prototype | 773 MB/s on a 4-core container (64 MiB benchmark) | |

The overhead assumes two-byte codes once registered; the placeholders are one byte longer. Block count matters most for indexes: ipfs2fil would announce only blob roots to IPNI, not every node, which keeps announcements at one per blob.

## Prototype

The Go package `internal/blake3bao` implements this spec. It is not wired into ipfs2fil and reads no iroh store.

- **Encode** streams a blob into blocks, children first, using one group of memory plus the tree depth.
- **Verify**, **Links** and a multihash registration let ordinary go-cid and blockstore code check and walk the blocks.
- **Size** and **Read** find the size from the right edge, read any byte range, and enforce the shape rules.
- **Outboard** and **FromOutboard** convert to and from iroh's pre-order outboard, which is how an iroh export would work.

What the tests check:

| Test | Checks |
| --- | --- |
| Reference vectors | 16 sizes from 0 bytes to 1.05 MiB: root equals `BLAKE3(data)` and the `bao-tree` 0.17 root; parent bodies in pre-order equal `bao-tree`'s `PreOrderMemOutboard` at chunk log 4, byte for byte; FromOutboard rebuilds the same blocks; 2g − 1 blocks |
| Range reads | 200 random ranges of a 5-group blob read back exactly |
| Tampering | A flipped byte, a moved leaf or a dropped root flag fails verification |
| Bad shape | A tree of valid blocks with a short left leaf is refused, and so is a wrong claimed size |
| Parsing | Unknown kinds, short or long parents, empty or unaligned leaves, non-minimal varints are refused |
| Hash sequences | 1,000 child blobs: the root equals `BLAKE3(sequence)`, and walking links reaches every child in order |

The vectors come from a small Rust program in `internal/blake3bao/testdata/gen` that uses n0's crates, so anyone can regenerate them.

## Open questions

For n0:

- Is 16 KiB the right fixed group? A larger group (256 KiB) means 16 times fewer blocks, but parent blocks would then be only the top of iroh's outboard, and a 16 KiB range read would fetch a 256 KiB leaf.
- Should root or non-root be told by two multihash codes instead of a kind bit? We chose the kind bit so there is one hash code to register.
- Is `CIDv1(0x80, blake3, h)` how iroh means `0x80` to be used, and would n0 prefer we register the tree codecs next to it?
- Keyed BLAKE3 (bao-tree's `create_keyed`) is out of scope. Is anyone using keyed blobs who would want it?

For the project:

- Register the three codes in the multicodec table once n0 has reviewed, or keep the placeholders until ipfs2fil ships iroh support?
- Where should the spec live publicly: this repo's `docs/specs/`, or an IPLD specs pull request?

## Sources

- [multicodec table](https://github.com/multiformats/multicodec/blob/master/table.csv): `blake3` `0x1e` and `blake3-hashseq` `0x80`, both draft
- [bao-tree](https://docs.rs/bao-tree) 0.17.0, n0's tree library used by iroh-blobs; source of the test vectors
- [iroh content addressing](https://www.iroh.computer/design/content-addressing): blobs and collections as BLAKE3 root hashes and CIDs
- [iroh-blobs DESIGN.md](https://docs.rs/crate/iroh-blobs/latest/source/DESIGN.md) and [blob store design](https://iroh.computer/blog/blob-store-design-challenges)
- [iroh DAG sync](https://iroh.computer/blog/iroh-dag-sync): why per-block IPLD hashes cannot check BLAKE3 incrementally, the gap this codec closes
- [bao docs](https://github.com/n0-computer/bao-docs) and the [BLAKE3 paper](https://github.com/BLAKE3-team/BLAKE3-specs/blob/master/blake3.pdf), section 6.4 on verified streaming
- [lukechampine.com/blake3](https://pkg.go.dev/lukechampine.com/blake3) `guts`, the Go BLAKE3 internals the prototype uses
