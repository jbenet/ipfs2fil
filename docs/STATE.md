# ipfs2fil — STATE (handoff)

Updated 2026-10-21. Building starts 2026-10-22 (Juan: "we can build this tomorrow").

- Plan (rev 2): https://claude.ai/artifact/KfFPmyzZdSsVRbRD2mN2gE
- Explorer boards: https://claude.ai/artifact/XpKa96RnjJz32GJ5eqTQGK (Overview, Browse, Root, Pieces, Search, Mobile)
- Prototype: /mnt/project-files/ipfs2fil/prototype/ (Go; repos 11 and 18 read, packed, verified)

## Decided by Juan (2026-10-21)
- Support every fs-repo version, 0 (no version file, go-ipfs 0.2) through 18 (kubo 0.43.1).
- 3 copies. Orphans included by default. `--repo <path>` overrides discovery.
- Hosted top-up keeper = separate service (later). Code in a new GitHub repo `jbenet/ipfs2fil` (not created yet).
- Same repo state must give the same archive; re-runs must still be incremental -> content-defined piece cut points + skip pieces whose PieceCID is already stored.

## Next (M0)
1. Create jbenet/ipfs2fil (Go module), move the prototype in, add this STATE.md.
2. Fixtures 0..18: make a repo with go-ipfs 0.11, step down with fs-repo-migrations reverts (archived repo, still builds), save each.
3. Calibration spike needs a host that can reach api.calibration.node.glif.io (blocked in cloud sessions): synapse-go upload of 3 pieces sharing a root, fetch via gateway.

## Facts
- Block layouts: leveldb (0-1), old flatfs keys (2-3), base32 flatfs (4), flatfs+SHARDING (5+; CID keys <=11, multihash keys 12+).
- Pin layouts: datastore key lists (0-2), pin DAG /local/pins (3-10), dspinner /pins (11+). MFS root /local/filesroot (3+).
- synapse-go v0.7.0 caps a piece at 1 GiB*127/128; Synapse JS at 64 GiB*127/128.
