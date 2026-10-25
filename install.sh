#!/bin/sh
# Install ipfs2fil from GitHub releases.
#
#   curl -fsSL https://github.com/jbenet/ipfs2fil/releases/latest/download/install.sh | sh
#
# IPFS2FIL_VERSION=v0.1.0 picks a version (default: latest).
# IPFS2FIL_BIN_DIR=/some/dir picks where it goes (default: /usr/local/bin if
# writable, else ~/.local/bin).
# IPFS2FIL_DOWNLOAD_URL=https://mirror/dir fetches the files from a mirror.
set -eu

repo=jbenet/ipfs2fil
version=${IPFS2FIL_VERSION:-latest}

say() { printf 'ipfs2fil install: %s\n' "$*" >&2; }
die() { say "$*"; exit 1; }

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  FreeBSD) os=freebsd ;;
  OpenBSD) os=openbsd ;;
  *) die "no build for $(uname -s); download one from https://github.com/$repo/releases or run: go install github.com/$repo/cmd/ipfs2fil@latest" ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  i386 | i686) arch=386 ;;
  armv6* | armv7* | arm) arch=arm ;;
  riscv64) arch=riscv64 ;;
  *) die "no build for $(uname -m); download one from https://github.com/$repo/releases" ;;
esac

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1" -o "$2"; }
  fetch_url() { curl -fsSLI -o /dev/null -w '%{url_effective}' "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO "$2" "$1"; }
  fetch_url() { wget -S --spider "$1" 2>&1 | sed -n 's/^ *Location: //p' | tail -1; }
else
  die "needs curl or wget"
fi

if [ "$version" = latest ]; then
  url=$(fetch_url "https://github.com/$repo/releases/latest")
  version=${url##*/}
  case $version in v*) ;; *) die "could not find the latest release; set IPFS2FIL_VERSION=vX.Y.Z" ;; esac
fi

name="ipfs2fil_${version}_${os}_${arch}"
base=${IPFS2FIL_DOWNLOAD_URL:-https://github.com/$repo/releases/download/$version}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "downloading $name.tar.gz"
fetch "$base/$name.tar.gz" "$tmp/$name.tar.gz" || die "download failed: $base/$name.tar.gz"
fetch "$base/ipfs2fil_${version}_checksums.txt" "$tmp/sums" || die "could not download checksums"

want=$(grep " $name.tar.gz\$" "$tmp/sums" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$name.tar.gz" | cut -d' ' -f1)
else
  got=$(shasum -a 256 "$tmp/$name.tar.gz" | cut -d' ' -f1)
fi
[ -n "$want" ] && [ "$want" = "$got" ] || die "checksum mismatch for $name.tar.gz; not installing"

tar -xzf "$tmp/$name.tar.gz" -C "$tmp" ipfs2fil

dir=${IPFS2FIL_BIN_DIR:-}
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir=$HOME/.local/bin; fi
fi
mkdir -p "$dir"
install -m 0755 "$tmp/ipfs2fil" "$dir/ipfs2fil" 2>/dev/null || { cp "$tmp/ipfs2fil" "$dir/ipfs2fil"; chmod 0755 "$dir/ipfs2fil"; }
say "installed $("$dir/ipfs2fil" version) to $dir/ipfs2fil"
case ":$PATH:" in *":$dir:"*) ;; *) say "add $dir to your PATH" ;; esac
