#!/bin/sh
# Builds internal/feedback/assets/feedback.js.gz and feedback.css: the client of
# github.com/jbenet/feedback-dev-kit (React and all) as one script, for
# 'ipfs2fil explore --feedback'. The outputs are committed, so a Go build needs
# no Node; rerun this (Node 20+) after changing internal/feedback/web or KIT_REF.
#
# Usage: scripts/build-feedback.sh [path to a feedback-dev-kit checkout]
set -eu
KIT_REF=${KIT_REF:-50576f48aa4d8b4297f99c8b188a5ebeff49e121}
cd "$(dirname "$0")/.."
out=internal/feedback/assets
kit=${1:-}
if [ -z "$kit" ]; then
	kit=$(mktemp -d)/feedback-dev-kit
	git clone -q https://github.com/jbenet/feedback-dev-kit "$kit"
	git -C "$kit" checkout -q "$KIT_REF"
fi
[ -d "$kit/node_modules" ] || (cd "$kit" && npm ci --ignore-scripts --no-audit --no-fund)
NODE_PATH="$kit/node_modules" "$kit/node_modules/.bin/esbuild" internal/feedback/web/main.tsx \
	--bundle --minify --format=iife --target=es2020 --jsx=automatic \
	--alias:@jbenet/feedback-react="$kit/packages/feedback-react/src/index.ts" \
	--define:process.env.NODE_ENV='"production"' \
	--resolve-extensions=.tsx,.ts,.js,.mjs \
	--legal-comments=eof \
	--outfile="$out/feedback.js"
gzip -9 -n -f "$out/feedback.js"
cp "$kit/packages/feedback-react/src/styles.css" "$out/feedback.css"
echo "built from feedback-dev-kit $(git -C "$kit" rev-parse HEAD)" > "$out/VERSION"
ls -l "$out"
