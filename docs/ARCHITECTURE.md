# Architecture

One Go binary, no cgo, built for every platform kubo ships for (see CI).

```
cmd/ipfs2fil        CLI: scan, pack, push, up, status, explore, verify
internal/repo       read fs-repo 0..18, read-only -> blocks by multihash, pins, MFS root
internal/archive    link roots, lay out blocks, cut pieces, write CARs + index/
internal/car        CARv1 writing and PieceCID (v2)
internal/manifest   index/manifest.json: the contract between the packages below
internal/store      Filecoin Onchain Cloud: data sets, uploads, status, cost estimates
internal/explorer   static HTML explorer written into explorer/
prototype/          the 2026-10-21 spike, kept for reference (build-ignored)
```

## Data flow

1. `repo.Open` detects the version and returns a `repo.Repo`.
2. `archive.Build` walks from every root, builds `data/`
   (`pins/recursive`, `pins/direct`, `mfs`, `orphans`), lays blocks out depth
   first with roots sorted by CID, cuts pieces (see package doc), writes CARs
   and `index/` (manifest, per-piece block lists, history), and asks
   `explorer.Write` for `explorer/`. The archive root links all three.
3. `store` finds or creates one data set per copy and uploads each piece whose
   PieceCID the data set does not have yet. It writes `Copies` back into the
   manifest. Data sets set `withIPFSIndexing` so providers announce blocks to
   IPNI; pieces carry `ipfsRootCID`.

## Rules every package follows

- The repo is never written. Open stores read-only; never run migrations.
- `data/` and piece cuts are a pure function of the repo's blocks and roots.
  Anything that varies (times, providers, piece ids) goes in `index/`.
- Every block written to a CAR is hash-checked first.
- Errors say what to do next in plain words.
- No cgo. synapse-go's BLS signer needs cgo blst, so go.mod replaces blst with
  a stub in third_party/blst: wallets are secp256k1 (0x / f1) keys only.

## Workstreams

Each is its own branch and PR against `main`. They meet only at the types in
`internal/repo`, `internal/manifest`, `internal/store` and `internal/explorer`;
change those in a small PR of their own first.

| Stream | Packages | Done when |
|---|---|---|
| Repo readers | repo, testdata/fixtures | Fixture repos for 0..18 open, list blocks, pins and MFS, and match `ipfs refs local` / `ipfs pin ls` |
| Archive and pieces | archive, car | `pack` on fixtures gives byte-identical pieces across runs; an edit changes only nearby pieces; `kubo dag import` serves every root |
| Explorer | explorer | Pages match docs/boards, work from a gateway with no server, keyboard shortcuts on every page |
| Filecoin storage | store, push/status/verify | Calibration run stores 3 copies, re-run uploads nothing, status reads proofs from chain |
| CLI and release | cmd, CI, releases | Friendly output and progress, resumable runs, signed release binaries for every platform |

Cloud sessions cannot reach Filecoin RPC (glif) or dist.ipfs.tech; calibration
runs happen on a machine that can.
