#!/usr/bin/env bash
# Regenerates the fs-repo fixtures in this directory with real go-ipfs and kubo
# binaries. Each fixture is <name>.tar.gz holding repo/ (the fs-repo) and
# expected/ (what the binary that made it reports):
#
#   expected/refs-local  `ipfs refs local`, sorted
#   expected/pins        `ipfs pin ls` as "<cid> <recursive|direct> [name]", sorted
#   expected/mfs         `ipfs files stat --hash /` (absent before repo 3)
#   expected/made-by     the binary and, for reverted repos, the reverts applied
#
# Repos 7 and 9..18 come straight from a release. go-ipfs releases before
# 0.4.17 have no GitHub assets, so repos 0..6 are made by reverting the repo 7
# fixture with fs-repo-migrations (built from source), and repo 8 by reverting
# repo 9. Their expected/ is the source fixture's: the reverts keep every block. Linux amd64 only; needs curl, git, go, python3, tar.
#
# usage: testdata/fixtures/make.sh [fixture...]   (default: all)
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
work=${WORK:-$(mktemp -d)}
bin=$work/bin
mkdir -p "$bin"
migrations_commit=0907f65847701cd9109a4d26b31933b4af5114ea

# version -> release that writes it natively
declare -A native=(
  [7]=v0.4.23 [9]=v0.5.1 [10]=v0.6.0 [11]=v0.8.0 [12]=v0.12.2 [13]=v0.18.1
  [14]=v0.21.1 [15]=v0.29.0 [16]=v0.36.0 [17]=v0.37.0 [18]=v0.43.1
)

fetch() { # fetch <release> -> path of its ipfs binary
  local v=$1 d=$bin/$1
  if [ ! -x "$d/ipfs" ]; then
    mkdir -p "$d"
    local name=kubo
    case $v in v0.4.*|v0.5.*|v0.6.*|v0.7.*|v0.8.*|v0.9.*|v0.1[0-2].*) name=go-ipfs ;; esac
    curl -sfL "https://github.com/ipfs/kubo/releases/download/$v/${name}_${v}_linux-amd64.tar.gz" |
      tar xz -C "$d" --strip-components=1
  fi
  echo "$d/ipfs"
}

migration() { # migration <from-to> -> path of the built binary
  local m=$1 src=$work/fs-repo-migrations
  if [ ! -d "$src" ]; then
    git clone -q https://github.com/ipfs/fs-repo-migrations "$src"
    git -C "$src" checkout -q $migrations_commit
  fi
  if [ ! -x "$bin/mig-$m" ]; then
    (cd "$src/fs-repo-$m" && GOFLAGS=-mod=vendor go build -o "$bin/mig-$m" .)
  fi
  echo "$bin/mig-$m"
}

data=$work/data
mkdata() {
  rm -rf "$data" && mkdir -p "$data"
  python3 - "$data" <<'EOF'
import os, random, sys
d = sys.argv[1]
r = random.Random(2026)
def w(p, b):
    os.makedirs(os.path.dirname(p), exist_ok=True)
    open(p, "wb").write(b)
w(f"{d}/site/index.html", b"<h1>fixture site</h1>\n")
w(f"{d}/site/about/me.txt", b"about me\n")
# Multi-chunk files that still compress well.
def text(n, tag):
    out, i = [], 0
    while sum(map(len, out)) < n:
        out.append(b"%s line %d %d\n" % (tag, i, r.randrange(10**9)))
        i += 1
    return b"".join(out)[:n]
w(f"{d}/site/img/big.bin", text(600_000, b"site"))
w(f"{d}/big.bin", text(800_000, b"big"))
for i in range(40):
    w(f"{d}/hamt/{'file-%02d-' % i + 'x' * 40}.txt", b"hamt %d\n" % i)
w(f"{d}/named/a.txt", b"named pin\n")
EOF
}

ver_ge() { # ver_ge <release> <min>: release >= min
  [ "$(printf '%s\n%s\n' "${2#v}" "${1#v}" | sort -V | head -1)" = "${2#v}" ]
}

populate() { # populate <release> <ipfs>; IPFS_PATH is set
  local v=$1 B=$2
  cd "$data"
  $B add -Q -r site >/dev/null
  local d o m
  d=$(echo direct-pin | $B add -Q --pin=false); $B pin add --recursive=false "$d" >/dev/null
  o=$(echo unpinned-orphan | $B add -Q --pin=false)
  $B files mkdir /notes
  m=$(echo mfs-file | $B add -Q --pin=false); $B files cp "/ipfs/$m" /notes/m.txt

  # HAMT directory
  if ver_ge "$v" v0.12.0; then
    if ver_ge "$v" v0.36.0; then
      $B config Import.UnixFSHAMTDirectorySizeThreshold 1KiB
    else
      $B config Internal.UnixFSShardingSizeThreshold 1KiB
    fi
    $B add -Q -r hamt >/dev/null
  else
    $B config --json Experimental.ShardingEnabled true
    $B add -Q -r hamt >/dev/null
    $B config --json Experimental.ShardingEnabled false
  fi

  # CIDv1, raw leaves and identity CIDs need repo 5+ (0.4.5+); the repo 7
  # fixture stays CIDv0 so the reverts to repo 4 and below keep every block.
  if ver_ge "$v" v0.5.0; then
    $B add -Q --raw-leaves --cid-version=1 big.bin >/dev/null
    echo inline-me | $B add -Q --inline >/dev/null
    $B files cp "/ipfs/$($B add -Q --pin=false --cid-version=1 --raw-leaves big.bin)" /big.bin
  fi
  if ver_ge "$v" v0.26.0; then
    $B pin add --name=my-named-pin "$($B add -Q --pin=false -r named)" >/dev/null
  fi
  cd - >/dev/null
}

record() { # record <ipfs> <expected-dir> <release>
  local B=$1 e=$2 v=$3
  mkdir -p "$e"
  $B refs local | sort >"$e/refs-local"
  if ver_ge "$v" v0.26.0; then
    $B pin ls --names | awk '{print $1, $2, $3}' | sed 's/ $//' | sort >"$e/pins"
  else
    $B pin ls | awk '{print $1, $2}' | sort >"$e/pins"
  fi
  grep -v ' indirect' "$e/pins" >"$e/pins.tmp" && mv "$e/pins.tmp" "$e/pins"
  $B files stat --hash / >"$e/mfs"
  echo "$v" >"$e/made-by"
}

pack() { # pack <name> <dir with repo/ and expected/>
  tar czf "$here/$1.tar.gz" --owner=0 --group=0 --sort=name -C "$2" repo expected
  echo "$1: repo $(cat "$2/repo/version" 2>/dev/null || echo 0), $(wc -l <"$2/expected/refs-local") blocks, $(wc -l <"$2/expected/pins") pins"
}

make_native() { # make_native <name> <repo version> [init profile] [release]
  local name=$1 rv=$2 profile=${3:-test} v=${4:-${native[$2]}} B
  B=$(fetch "$v")
  local dir=$work/$name
  rm -rf "$dir" && mkdir -p "$dir"
  export IPFS_PATH=$dir/repo
  $B init --profile="$profile" >/dev/null 2>&1
  [ "$profile" = test ] || $B config profile apply test >/dev/null
  [ "$(cat "$IPFS_PATH/version")" = "$rv" ] || { echo "$v made repo $(cat "$IPFS_PATH/version"), want $rv" >&2; exit 1; }
  populate "$v" "$B"
  record "$B" "$dir/expected" "$v"
  unset IPFS_PATH
  rm -f "$dir/repo/repo.lock" "$dir/repo/api"
  pack "$name" "$dir"
}

make_reverted() { # make_reverted <name> <repo version> <from fixture>
  local name=$1 rv=$2 from=$3 dir=$work/$1
  rm -rf "$dir" && cp -a "$work/$from" "$dir"
  local cur
  cur=$(cat "$dir/repo/version")
  while [ "$cur" -gt "$rv" ]; do
    if [ "$cur" = 2 ]; then # 1-to-2 moves the repo from .ipfs back to .go-ipfs
      mv "$dir/repo" "$dir/.ipfs"
      "$(migration 1-to-2)" -revert -path="$dir/.ipfs" >/dev/null
      mv "$dir/.go-ipfs" "$dir/repo"
    elif [ "$cur" = 1 ]; then # 0-to-1's revert locks the repo twice and fails; it only removes the version file
      rm "$dir/repo/version"
    else
      "$(migration "$((cur - 1))-to-$cur")" -revert -path="$dir/repo" >/dev/null
    fi
    echo "reverted $cur to $((cur - 1)) with fs-repo-migrations@${migrations_commit:0:7}" >>"$dir/expected/made-by"
    cur=$((cur - 1))
  done
  [ -f "$dir/repo/version" ] || cur=0
  [ "$cur" = "$rv" ] || { echo "$name: reverted to $cur, want $rv" >&2; exit 1; }
  # MFS (/local/filesroot) and the pin DAG arrived in repo 3.
  [ "$rv" -ge 3 ] || rm -f "$dir/expected/mfs"
  rm -f "$dir/repo/repo.lock" "$dir/repo/daemon.lock"
  pack "$name" "$dir"
}

custom_spec() { # repo 18 with blocks in prefix/3 flatfs at a custom path, meta at "meta"
  local B dir=$work/v18-custom-spec
  B=$(fetch "${native[18]}")
  rm -rf "$dir" && mkdir -p "$dir"
  export IPFS_PATH=$dir/repo
  $B init --profile=test --empty-repo >/dev/null 2>&1 || $B init --profile=test >/dev/null
  python3 - "$IPFS_PATH" <<'EOF'
import json, sys, shutil, os
r = sys.argv[1]
cfg = json.load(open(f"{r}/config"))
spec = {"type": "mount", "mounts": [
  {"mountpoint": "/blocks", "type": "measure", "prefix": "flatfs.datastore",
   "child": {"type": "flatfs", "path": "myblocks", "sync": False, "shardFunc": "/repo/flatfs/shard/v1/prefix/3"}},
  {"mountpoint": "/", "type": "measure", "prefix": "leveldb.datastore",
   "child": {"type": "levelds", "path": "meta", "compression": "none"}}]}
cfg["Datastore"]["Spec"] = spec
json.dump(cfg, open(f"{r}/config", "w"), indent=2)
json.dump({"mounts": [{"mountpoint": "/blocks", "path": "myblocks", "shardFunc": "/repo/flatfs/shard/v1/prefix/3", "type": "flatfs"},
                      {"mountpoint": "/", "path": "meta", "type": "levelds"}], "type": "mount"},
          open(f"{r}/datastore_spec", "w"), separators=(",", ":"))
shutil.rmtree(f"{r}/blocks"); shutil.rmtree(f"{r}/datastore")
EOF
  populate "${native[18]}" "$B"
  record "$B" "$dir/expected" "${native[18]}"
  echo "custom Datastore.Spec: flatfs prefix/3 at myblocks, levelds at meta" >>"$dir/expected/made-by"
  unset IPFS_PATH
  rm -f "$dir/repo/repo.lock"
  pack v18-custom-spec "$dir"
}

mkdata
want=${*:-all}
has() { [ "$want" = all ] || [[ " $want " == *" $1 "* ]]; }
has_old() { for o in v00 v01 v02 v03 v04 v05 v06; do has $o && return 0; done; return 1; }
for rv in 7 9 10 11 12 13 14 15 16 17 18; do
  n=$(printf 'v%02d' "$rv")
  if has "$n" || { [ "$rv" = 7 ] && has_old; } || { [ "$rv" = 9 ] && has v08; }; then make_native "$n" "$rv"; fi
done
for rv in 6 5 4 3 2 1 0; do
  n=$(printf 'v%02d' "$rv")
  has "$n" && make_reverted "$n" "$rv" v07
done
has v08 && make_reverted v08 8 v09
has v18-custom-spec && custom_spec
# Other datastores kubo offers: badger v1 (deprecated, still in 0.43) and pebble.
has v15-badgerds && make_native v15-badgerds 15 badgerds
has v18-badgerds && make_native v18-badgerds 18 badgerds
has v16-pebbleds && make_native v16-pebbleds 16 pebbleds v0.32.1
has v17-pebbleds && make_native v17-pebbleds 17 pebbleds
has v18-pebbleds && make_native v18-pebbleds 18 pebbleds
has v18-flatfs-pebbleds && make_native v18-flatfs-pebbleds 18 flatfs-pebbleds
echo "work dir: $work"
