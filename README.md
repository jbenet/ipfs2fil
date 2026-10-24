# ipfs2fil

Archive an IPFS repo to Filecoin, and browse it from anywhere.

`ipfs2fil` reads a kubo or go-ipfs repo of any version (fs-repo 0 through 18),
links every pin, the MFS tree and leftover blocks under one root, cuts the
blocks into pieces, and stores 3 copies on Filecoin Onchain Cloud. Every
archive carries its own index and an HTML explorer, so any IPFS gateway can
serve the files and show what is where.

    ipfs2fil scan              # what would be archived, and what it costs
    ipfs2fil pack              # build the archive and its pieces on disk
    ipfs2fil push              # upload what is not stored yet
    ipfs2fil status            # copies, funding, proofs
    ipfs2fil explore --serve   # browse the archive locally

The repo is only ever read. Running again after the repo changes uploads only
the pieces that changed.

Status: under construction. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

- Plan: https://claude.ai/artifact/KfFPmyzZdSsVRbRD2mN2gE
- Explorer designs: https://claude.ai/artifact/XpKa96RnjJz32GJ5eqTQGK (sources in docs/boards)
