#!/bin/bash
# Starts the local servers the website demo's PDS and pinning-service repos
# are read from, after make.sh, and keeps them running until stopped:
#
#   services.sh <home> <pds npm dir>
#
# The pds npm dir has `npm i @atproto/pds @did-plc/server @atproto/api`.
# It starts:
#
#   :2582  a PLC directory (in memory) and :2583 a PDS with three made-up
#          accounts (@ada.test, @gardenlog.test, @cidlab.test) and @bsky-app.test,
#          a copy of @bsky.app's latest posts with the same images, pds.mjs
#   :2590  a pinning service listing three pins, psa.mjs
#   :8190  the gateway of a kubo node holding those pins (~/services/psa-node)
#
# then prints the environment ipfs2fil needs to reach them. Stop it with
# Ctrl-C; the PLC directory is in memory, so archive the PDS first.
set -eu
H=$(cd "$1" && pwd) NP=$(cd "$2" && pwd)
HERE=$(cd "$(dirname "$0")" && pwd)
S=$H/services
mkdir -p "$S"
trap 'kill $(jobs -p) 2>/dev/null' EXIT

# The pinning service's own node: a kubo repo holding what it pins.
export IPFS_PATH=$S/psa-node
if [ ! -d "$IPFS_PATH" ]; then
  ipfs init --empty-repo --profile test >/dev/null
  ipfs config Addresses.Gateway /ip4/127.0.0.1/tcp/8190
  ipfs config Addresses.API /ip4/127.0.0.1/tcp/5190
fi
papers=$(ipfs add -r -Q "$H/content/pub/papers")
iceland=$(ipfs add -r -Q "$H/content/photos/2024-iceland")
specs=$(ipfs dag import "$(readlink -f "$H/content/sites/specs.ipfs.tech.car")" | awk '/^Pinned root/{print $3}')
cat >"$S/pins.json" <<JSON
[
  {"cid": "$papers", "name": "papers", "created": "2026-09-14T10:02:11Z", "meta": {"app": "ada-desktop"}},
  {"cid": "$iceland", "name": "iceland-2024", "created": "2026-10-02T18:40:55Z", "meta": {"app": "ada-desktop", "album": "Iceland 2024"}},
  {"cid": "$specs", "name": "specs.ipfs.tech", "created": "2026-10-28T08:15:00Z", "meta": {"note": "a copy of specs.ipfs.tech"}}
]
JSON
ipfs daemon --offline >"$S/psa-node.log" 2>&1 &
node "$HERE/psa.mjs" "$S/pins.json" 2590 &
cp "$HERE/pds.mjs" "$NP/"
node "$NP/pds.mjs" "$S/pds" "$H/content" &
until curl -fsS -o /dev/null http://127.0.0.1:8190/ipfs/$papers?format=raw 2>/dev/null; do sleep 1; done
cat <<ENV

Running. In another shell, with \$HOME=$H:

  export IPFS2FIL_PLC_DIRECTORY=http://localhost:2582 IPFS2FIL_HANDLE_RESOLVER=http://localhost:2583
  export IPFS2FIL_GATEWAYS=http://127.0.0.1:8190 IPFS2FIL_PSA_TOKEN=demo
  ipfs2fil repos add pds:http://localhost:2583 --name pds
  ipfs2fil repos add psa+http://127.0.0.1:2590 --name pinning-service

ENV
wait
