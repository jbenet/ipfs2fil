#!/bin/bash
# Builds the website demo's repos, one of each kind, under a throwaway home:
#
#   make.sh <home> <public content> <helia npm dir> <js-ipfs npm dir>
#
# <public content> is what demo-archive/make.sh built. The npm dirs have
#   npm i helia @helia/unixfs @helia/mfs @helia/car blockstore-fs datastore-fs @ipld/car @ipld/dag-cbor @ipld/dag-json multiformats
#   npm i ipfs-core@0.18.1
# (separate folders: their dependencies clash). Needs kubo's ipfs, python3
# with Pillow, node with playwright, ffmpeg and curl. It makes:
#
#   ~/.ipfs                 kubo: the public demo archive's content, photos, notes
#   ~/backups/old-laptop    kubo: an older repo sharing a folder with ~/.ipfs
#   ~/.jsipfs               js-ipfs repo 12
#   ~/apps/photo-app/data   a Helia store
#   ~/exports/art.car       images, video, texts and PDFs
#   ~/exports/dev-bundle.car code, logs, configs and data; three roots
#
# Plus at://bsky.app, added by ipfs2fil itself. Five of @bsky.app's images go
# into ~/.ipfs, the Helia store and art.car with CIDv1 and raw leaves, so they
# are the same blocks in four repos; so are the NASA photos in ~/.ipfs, the
# Helia store and art.car.
set -eu
H=$(mkdir -p "$1" && cd "$1" && pwd) PUB=$(cd "$2" && pwd) NH=$(cd "$3" && pwd) NJ=$(cd "$4" && pwd)
HERE=$(cd "$(dirname "$0")" && pwd)
C=$H/content
mkdir -p "$C" "$H/exports"
ln -sfn "$PUB" "$C/pub"

# Content: photos and fractals, PDFs, notes and data.
(cd "$C" && python3 -I "$HERE/../content.py" && node "$HERE/../pdfs.js" && python3 -I "$HERE/notes.py")
# Five of @bsky.app's own images (did:plc:z72i7hdynmk6r22z27h6tvur).
PDS=https://puffball.us-east.host.bsky.network DID=did:plc:z72i7hdynmk6r22z27h6tvur
mkdir -p "$C/from-bluesky"
while read -r name cid; do
  [ -s "$C/from-bluesky/$name" ] || curl -fsS -o "$C/from-bluesky/$name" "$PDS/xrpc/com.atproto.sync.getBlob?did=$DID&cid=$cid"
done <<'EOF'
bluesky-logo.jpg bafkreibmmpwets4yzpqjyewrvrr6wkfbswtvrknhbse5wa2zkkzuqhcetq
pins-to-bookmarks.jpg bafkreib7o3xfdmvbttjadfekinjd3f3wa2xj4rs6u7g74vpxn3c6dbm46u
release-1.93.jpg bafkreib2ibqpr5orloyfp6id4z4rkg7uehdibcdo7bukgpomwfwsht6isy
release-1.91.1.jpg bafkreibidzdbket2cxesqalxdfh3ngbgppsvvdjszqvhx7bhpqf3ou5wke
embed-a-post.jpg bafkreiaz5kruipoyvbimd2vilmddksvpe4cjxqbhrnu6xr5vwvd7es6eze
EOF

# art.car's folder: images, video, texts and PDFs.
A=$C/art-car
rm -rf "$A" && mkdir -p "$A"/{images,video,text,pdfs,from-bluesky}
cp "$C"/art/*.png "$PUB"/images/julia/julia-0{1,2,3,4}.jpg "$PUB"/images/truchet/truchet-0{1,2}.svg "$PUB"/images/animated/spirograph-1.gif "$A/images/"
cp "$PUB"/images/flow-fields/flow-0{1,2}.png "$A/images/"
cp "$PUB"/photos/nasa/webb/*.jpg "$A/images/"
ffmpeg -loglevel error -y -f lavfi -i "mandelbrot=size=640x360:rate=24:start_scale=3:end_scale=0.0002:end_pts=240:start_x=-0.743643887037151:start_y=0.131825904205330" \
  -t 8 -pix_fmt yuv420p -c:v libx264 -crf 30 "$A/video/seahorse-zoom.mp4"
cp "$PUB/video/nasa/ksc-69-71212-srgb.mp4" "$A/video/apollo-11-launch.mp4"
cp "$C"/art-texts/* "$A/text/"
cp "$C/papers/fractal-gallery.pdf" "$C/papers/notes-on-content-addressing.pdf" "$PUB/papers/ipfs-content-addressed-versioned-p2p-file-system.pdf" "$A/pdfs/"
cp "$C/from-bluesky/bluesky-logo.jpg" "$C/from-bluesky/pins-to-bookmarks.jpg" "$A/from-bluesky/"

# dev-bundle.car's folder.
D=$C/dev-bundle
rm -rf "$D" && python3 -I "$HERE/devassets.py" "$D" "$PUB/games/hextris/style/fonts/QuattrocentoSans-Regular.ttf"

# kubo repos.
export IPFS_PATH=$H/.ipfs
rm -rf "$IPFS_PATH" "$H/backups/old-laptop" && ipfs init --empty-repo >/dev/null
add() { ipfs add -r -Q --pin-name "$@"; }
for d in books music games images; do add "$d" "$PUB/$d" >/dev/null; done
add papers "$PUB/papers" >/dev/null
add nasa --cid-version 1 --chunker=size-1048576 "$PUB/photos/nasa" >/dev/null
add nasa-video --cid-version 1 --chunker=size-1048576 "$PUB/video/nasa" >/dev/null
add iceland-2024 "$C/photos/2024-iceland" >/dev/null
add saved-from-bluesky --cid-version 1 --chunker=size-1048576 "$C/from-bluesky" >/dev/null
for d in notes data papers; do ipfs files cp "/ipfs/$(ipfs add -r -Q --pin=false "$C/$d")" "/$d"; done
ipfs files mkdir -p /drafts && ipfs files write --create /drafts/README.md <<<'Unpinned drafts, kept in MFS.'
mkdir -p "$H/backups"
export IPFS_PATH=$H/backups/old-laptop
ipfs init --empty-repo >/dev/null
add iceland-2024 "$C/photos/2024-iceland" >/dev/null
add old-notes "$C/notes" >/dev/null
ipfs files cp "/ipfs/$(ipfs add -r -Q --pin=false "$C/art")" /art
unset IPFS_PATH

# js-ipfs, Helia and the CARs, made by their own code.
for f in jsipfs.mjs; do cp "$HERE/$f" "$NJ/"; done
for f in helia.mjs car.mjs devbundle.mjs; do cp "$HERE/$f" "$NH/"; done
rm -rf "$H/.jsipfs" "$H/apps/photo-app/data"
node "$NJ/jsipfs.mjs" "$H/.jsipfs" "$C"
node "$NH/helia.mjs" "$H/apps/photo-app/data" "$C"
node "$NH/car.mjs" "$A" "$H/exports/art.car"
node "$NH/devbundle.mjs" "$D" "$H/exports/dev-bundle.car"
echo "made $H"
