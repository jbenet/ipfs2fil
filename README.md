# ipfs2fil

Archive an IPFS repo to Filecoin, and browse it from anywhere.

`ipfs2fil` reads a kubo or go-ipfs repo of any version (fs-repo 0 through 18),
links every pin, the MFS tree and leftover blocks under one root, cuts the
blocks into pieces, and stores 3 copies on Filecoin Onchain Cloud. Every
archive carries its own index and an HTML explorer, so any IPFS gateway can
serve the files and show what is where.

    ipfs2fil scan              # what would be archived, and what it costs
    ipfs2fil up                # pack and push in one go, after confirming the cost
    ipfs2fil status            # copies, funding, proofs
    ipfs2fil explore --serve   # browse the archive locally
    ipfs2fil explore --demo    # preview the explorer on a made-up archive

The repo is only ever read. Running again after the repo changes uploads only
the pieces that changed.

Status: under construction. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

- Plan: https://claude.ai/artifact/KfFPmyzZdSsVRbRD2mN2gE
- Explorer designs: https://claude.ai/artifact/XpKa96RnjJz32GJ5eqTQGK (sources in docs/boards)

## Install

```sh
curl -fsSL https://github.com/jbenet/ipfs2fil/releases/latest/download/install.sh | sh
```

or `go install github.com/jbenet/ipfs2fil/cmd/ipfs2fil@latest`, or download a
binary for your platform from [releases](https://github.com/jbenet/ipfs2fil/releases).
Each release lists SHA-256 checksums and signed build provenance
(`gh attestation verify <file> --repo jbenet/ipfs2fil`).

## Quickstart (5 minutes)

You need a kubo or go-ipfs repo (the daemon can keep running) and a Filecoin
wallet holding some FIL for gas and USDFC to pay for storage.

1. See what you have and what it costs. Nothing is written.

       ipfs2fil scan

   It reads `$IPFS_PATH`, else `~/.ipfs`; pass `--repo <path>` for another repo.

2. Give it your wallet key. The key is never printed or written anywhere.

       export IPFS2FIL_KEY_FILE=~/secrets/filecoin.key    # or IPFS2FIL_PRIVATE_KEY=<hex>

3. Archive. This packs the repo into `./ipfs2fil-archive`, shows the monthly
   cost and any deposit to make into Filecoin Pay, and uploads 3 copies once
   you say yes.

       ipfs2fil up

   Ctrl-C is safe: run `ipfs2fil up` again and it carries on. Run it again
   after the repo changes and only the changed pieces upload.

4. Check on it, and browse it.

       ipfs2fil status            # providers, pieces, last proof, days funded
       ipfs2fil verify            # fetch every piece back and check every block
       ipfs2fil explore --serve   # http://127.0.0.1:8484/explorer/

   Once stored, the archive root printed by `up` opens from any gateway,
   e.g. `https://dweb.link/ipfs/<root>/explorer/`.

The steps of `up` also run on their own: `ipfs2fil pack`, then `ipfs2fil push`.
Try it free on the Calibration testnet with `--network calibration`. Every
command explains itself: `ipfs2fil help <command>`.

## Many repos

Keep a list of repos and archive them together:

    ipfs2fil repos add ~/.ipfs
    ipfs2fil repos add /Volumes/old/go-ipfs --name old-laptop
    ipfs2fil up --all          # pack and push every repo, one confirmation
    ipfs2fil status --all      # every repo, its copies, proofs and funding

Repos in the list share one set of data sets (one per copy), so setup is paid
once, and a block an earlier repo already stores is not stored again by a later
one. The list and each repo's archive live in `~/.ipfs2fil` (or `--home`, or
`$IPFS2FIL_HOME`); the list holds repo paths and never leaves the machine.
`ipfs2fil repos ls` shows the list, and `ipfs2fil repos rm <name>` takes a repo
off it.

Repos on several machines: give every machine the same wallet key and the
same catalog, a git repository (say a private one on GitHub) that ipfs2fil
clones, pulls, commits to and pushes itself:

    ipfs2fil repos add ~/.ipfs --catalog git@github.com:you/ipfs2fil-catalog.git   # on each machine
    ipfs2fil up --all              # on each machine
    ipfs2fil status --all          # on any machine: every repo on every machine

The catalog is remembered after the first time. Each machine commits only its
own subfolder (named by its host name, or `--host`), holding its list and
each archive's manifest and block lists; the pieces stay where they were
packed. A machine leaves out blocks that repos on other machines have
already stored. A folder every machine already sees works as `--catalog` too.

## Releases

To cut a release: `git tag v0.1.0 && git push origin v0.1.0`.
