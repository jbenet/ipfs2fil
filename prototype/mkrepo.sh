#!/bin/bash
# usage: mkrepo.sh <ipfs-binary> <repo-path> <data-dir>
set -e
B=$1; export IPFS_PATH=$2; D=$3
rm -rf "$IPFS_PATH"; mkdir -p "$(dirname $IPFS_PATH)"
$B init --profile=test >/dev/null 2>&1
echo "repo version: $(cat $IPFS_PATH/version)"
cd "$D"
echo "site: $($B add -Q -r site)"
echo "big:  $($B add -Q --raw-leaves --cid-version=1 big.bin)"
d=$(echo direct-pin | $B add -Q --pin=false); $B pin add --recursive=false $d >/dev/null; echo "direct: $d"
echo "unpinned: $(echo unpinned-orphan | $B add -Q --pin=false)"
$B files mkdir /notes
m=$(echo mfs-file | $B add -Q --pin=false); $B files cp /ipfs/$m /notes/m.txt
echo "mfs: $($B files stat --hash /)"
echo "local blocks: $($B refs local | wc -l)"
