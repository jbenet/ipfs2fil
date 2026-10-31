#!/bin/bash
# Builds the public demo archive's content: make.sh <empty dir>
# Needs curl, git, python3 with numpy and Pillow, ffmpeg, and node with playwright.
set -eu
OUT=$(mkdir -p "$1" && cd "$1" && pwd) HERE=$(cd "$(dirname "$0")" && pwd)
TMP=$(mktemp -d)
"$HERE/fetch.sh" "$OUT" "$TMP/rfc"
python3 -I "$HERE/nasa.py" "$OUT"
python3 -I "$HERE/art.py" "$OUT"
python3 -I "$HERE/music.py" "$OUT"
mkdir -p "$OUT/papers/rfc" && node "$HERE/rfcpdf.js" "$TMP/rfc" "$OUT/papers/rfc"
cp "$HERE/content-README.md" "$OUT/README.md"
