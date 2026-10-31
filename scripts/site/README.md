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
5. **Live demo** (site/demo/). One home with a repo of every kind:
   `demo/make.sh` builds two kubo repos (the public demo archive's content
   from step 6, plus photos, notes and PDFs), a js-ipfs repo 12 and a Helia
   store made by their own code, `art.car` (images, video, texts, PDFs) and
   `dev-bundle.car` (code, logs, configs, data; three roots: a folder, a
   dag-cbor build record and a dag-json release). Five of @bsky.app's images
   and the NASA photos go into several of them with CIDv1, raw leaves and
   1 MiB chunks, so they are the same blocks everywhere and stored once.
   Install Helia and js-ipfs in separate folders (their dependencies clash):

       npm i helia @helia/unixfs @helia/mfs @helia/car blockstore-fs datastore-fs @ipld/car @ipld/dag-cbor @ipld/dag-json multiformats
       npm i ipfs-core@0.18.1
       demo/make.sh $HOME <step 6 content> <helia dir> <js-ipfs dir>

   With `$HOME` the throwaway home and the sim build (step 3), add the repos
   in this order, so later ones show what they share with earlier ones:

       ipfs2fil repos add ~/.ipfs; ipfs2fil repos add ~/backups/old-laptop
       ipfs2fil repos add at://bsky.app
       ipfs2fil repos add ~/.jsipfs --name js-ipfs
       ipfs2fil repos add ~/apps/photo-app/data --name helia-photo-app
       ipfs2fil repos add ~/exports/art.car; ipfs2fil repos add ~/exports/dev-bundle.car
       ipfs2fil up --all --yes && ipfs2fil explore

   Then mirror it as static files for GitHub Pages:

       node mirror.js http://127.0.0.1:8484 ../../site/demo 1572864

   It copies the explorer pages and index, the files up to 1.5 MiB, video up
   to 10 MiB and audio up to 4 MiB (the fourth and fifth arguments). The
   viewer says when a file was left out. To
   keep the site small, images over 120 KB in all/_i2f/content/ were then
   scaled to 700 to 900 px with Pillow (`demo/shrink.py`), so those copies no
   longer match their CIDs.
6. **Public demo archive.** `demo-archive/make.sh <empty dir>` builds the
   content of the public sample archive on mainnet: open-license EPUBs,
   papers and RFCs, music, NASA photos and video, HTML games, and generated
   images and tracks. `demo-archive/content-README.md` lists every source
   and its license.
