#!/bin/bash
# Downloads the open-license part of the demo archive: fetch.sh <content dir> <rfc html dir>
set -u
C=$1 RFC=$2 HERE=$(cd "$(dirname "$0")" && pwd) UA='Mozilla/5.0 (ipfs2fil demo archive)'
get() { [ -s "$2" ] || curl -sSfL -A "$UA" --retry 3 --max-time 600 -o "$2" "$1" || echo "FAIL $1"; }
mkdir -p "$C"/books "$C"/papers "$C"/music/chopin "$C"/games "$RFC"
# EPUBs: Project Gutenberg (public-domain texts)
for n in "11 alices-adventures-in-wonderland" "201 flatland" "35 the-time-machine" "84 frankenstein" "1661 the-adventures-of-sherlock-holmes" "36 the-war-of-the-worlds" "164 twenty-thousand-leagues-under-the-sea" "2701 moby-dick" "1342 pride-and-prejudice" "74 tom-sawyer"; do
  read -r id name <<< "$n"; get "https://www.gutenberg.org/ebooks/$id.epub3.images" "$C/books/$name.epub"; done
get https://craphound.com/littlebrother/Cory_Doctorow_-_Little_Brother.epub "$C/books/little-brother-cory-doctorow.epub"
# Papers, and RFCs (printed to PDF by rfcpdf.js)
get https://arxiv.org/pdf/1407.3561 "$C/papers/ipfs-content-addressed-versioned-p2p-file-system.pdf"
get https://filecoin.io/filecoin.pdf "$C/papers/filecoin-a-decentralized-storage-network.pdf"
get https://bitcoin.org/bitcoin.pdf "$C/papers/bitcoin-a-peer-to-peer-electronic-cash-system.pdf"
for r in 1 675 791 1958 2324 2616 8890; do get "https://www.rfc-editor.org/rfc/rfc$r.html" "$RFC/rfc$r.html"; done
# Music: Musopen's Chopin collection (CC0)
while read -r f; do get "https://archive.org/download/musopen-chopin/$(python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1]))' "$f")" "$C/music/chopin/$f"; done < "$HERE/chopin.txt"
# Games: open-source HTML games, each with its LICENSE
for g in "gabrielecirulli/2048 2048 master" "hextris/hextris hextris gh-pages" "ellisonleao/clumsy-bird clumsy-bird" "jakesgordon/javascript-racer javascript-racer" "doublespeakgames/adarkroom a-dark-room"; do
  read -r repo dir branch <<< "$g"; [ -d "$C/games/$dir" ] && continue
  git clone -q --depth 1 ${branch:+-b "$branch"} "https://github.com/$repo" "$C/games/$dir" && rm -rf "$C/games/$dir/.git" || echo "FAIL $repo"; done
