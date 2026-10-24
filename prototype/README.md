# ipfs2fil prototype (2026-10-21)

Throwaway Go prototype that checked the riskiest parts of the plan. Not the real tool.

What it does: opens a kubo repo read-only (flatfs + levelds only), indexes blocks by
multihash (repo 11 keys are CIDs, repo 12+ keys are multihashes), reads pins with boxo's
dspinner and the MFS root from `/local/filesroot`, finds orphan blocks, links everything
under one UnixFS directory (`pins/recursive`, `pins/direct`, `mfs`, `orphans`, `ipfs2fil.json`),
packs the DAG in DFS order into CARs under a target size, computes each CAR's PieceCID v2,
and verifies every block reachable from the new root is present and hash-checked.

Run (Go 1.26, GOTOOLCHAIN=auto):

    ./mkrepo.sh <ipfs binary> <repo dir> <data dir>   # data dir needs big.bin and site/
    go build -o p . && ./p -repo <repo dir> -out out -piece 1048576

Results: go-ipfs v0.11.0 (repo 11) and kubo v0.38.1 (repo 18) both read, packed into 5 CARs
and verified; the repo files hashed the same before and after; a fresh kubo `dag import` of the
CARs serves `<root>/mfs/notes/m.txt` and every pin. Known gap: orphan roots are not deduped
(an orphan linked by another orphan is listed too).
