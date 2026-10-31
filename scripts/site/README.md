# Website media

How the recordings, screenshots and live demo in [site/](../../site/) were
made. Everything shows two made-up repos, never anyone's real data.

1. **Demo content.** In an empty folder: `python3 content.py` (photos and
   fractals, needs Pillow), then `node pdfs.js` (needs playwright). Add some
   video, audio, notes and code by hand or with ffmpeg.
2. **Demo repos.** With kubo, `ipfs init` two repos under a throwaway `$HOME`
   (`~/.ipfs` and `~/backups/old-laptop`), `ipfs add -r --pin-name <name>`
   folders into each (share a folder or two between them), and
   `ipfs files cp` a few into MFS.
3. **Terminal recordings.** These use a build with simulated storage, so
   repos of many GiB can be shown on mainnet without paying or uploading:
   `go build -tags sim -o bin/ipfs2fil ./cmd/ipfs2fil` (see
   cmd/ipfs2fil/sim.go; release builds leave it out). With `$HOME` set to
   that throwaway home, `bin` first on `$PATH`, `IPFS2FIL_HOST=studio` and
   `IPFS2FIL_SIM=$HOME/sim.json` (the simulated chain):

       COLS=100 ROWS=32 python3 rec.py scenes/up.scene up.cast
       node render.js up.cast out-up "ipfs2fil up --all  ·  storage simulated, sped up" 1.6 11 8

   `rec.py` types each command like a person and waits for the output;
   `render.js` plays the cast in xterm.js and writes `out.mp4`, `out.gif` and
   `last.png` (convert to webp for the site).
4. **Explorer screenshots.** Run `ipfs2fil explore` on the demo home and
   screenshot the pages with playwright at 1440x900, device scale 2.
5. **Live demo.** With `ipfs2fil explore` running on the demo home:

       node mirror.js http://127.0.0.1:8484 ../../site/demo

   It copies the explorer pages and index, and the files up to 6 MiB, so the
   demo works as plain static files on GitHub Pages.
6. **Public demo archive.** `demo-archive/make.sh <empty dir>` builds the
   content of the public sample archive on mainnet: open-license EPUBs,
   papers and RFCs, music, NASA photos and video, HTML games, and generated
   images and tracks. `demo-archive/content-README.md` lists every source
   and its license.
