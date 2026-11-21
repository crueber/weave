#!/usr/bin/env bash
# site-stage.sh - stage the project website into ONE directory, exactly as GitHub Pages serves it.
#
#   scripts/site-stage.sh [OUT_DIR]          # default ./dist/site (git-ignored)
#
# Copies site/ and the demo video (docs/demo.webm, kept only once in the repo) next to index.html.
# Used by `make site`, `make site-preview` and the Pages workflow (scripts/publish/pages.yml), so what you
# preview locally is what deploys. Every URL in the site is relative, so any base path works.
set -euo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
OUT=${1:-$ROOT/dist/site}

[ -f "$ROOT/site/index.html" ] || { echo "site/index.html is missing" >&2; exit 1; }
[ -f "$ROOT/docs/demo.webm" ] || { echo "docs/demo.webm is missing (the site embeds it)" >&2; exit 1; }

[ -n "$OUT" ] || { echo "empty output directory" >&2; exit 2; }
mkdir -p "$OUT"
OUT=$(cd -- "$OUT" && pwd)   # resolve, so "site", "./" or ".." cannot slip past the guard
case "$OUT" in /|"$HOME"|"$ROOT"|"$ROOT/site"|"$ROOT/site/"*|"$ROOT/docs"|"$ROOT/docs/"*) echo "refusing to stage into $OUT" >&2; exit 2 ;; esac
case "$ROOT" in "$OUT"/*) echo "refusing to stage into $OUT (it contains the repository)" >&2; exit 2 ;; esac
rm -rf "$OUT"
mkdir -p "$OUT"
cp -R "$ROOT/site/." "$OUT/"
cp "$ROOT/docs/demo.webm" "$OUT/demo.webm"
touch "$OUT/.nojekyll"   # serve files as they are (no Jekyll processing)

echo "staged $(find "$OUT" -type f | wc -l | tr -d ' ') files, $(du -sk "$OUT" | cut -f1) KB -> $OUT"
