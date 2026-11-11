#!/usr/bin/env bash
# UI smoke test: fetch /login and / with the local `obscura` headless browser and
# screenshot them. Exits 0 (skipped) if obscura is not installed.
#   WEAVE_URL=http://localhost:8080 OUT=./shots scripts/ui-smoke.sh
set -u
URL="${WEAVE_URL:-http://localhost:8080}"
OUT="${OUT:-$(mktemp -d)}"
OB="$(command -v obscura || echo "$HOME/.local/bin/obscura")"
[ -x "$OB" ] || { echo "obscura not found; skipping UI smoke"; exit 0; }
mkdir -p "$OUT"
fails=0
check() { # name path expect-regex
  local name="$1" path="$2" re="$3"
  # screenshot and DOM dump are separate invocations (-s suppresses --dump output)
  "$OB" --allow-private-network fetch "$URL$path" -q --wait 2 -s "$OUT/$name.png" >/dev/null 2>"$OUT/$name.err"
  "$OB" --allow-private-network fetch "$URL$path" -q --dump html >"$OUT/$name.html" 2>>"$OUT/$name.err"
  if grep -Eqi "$re" "$OUT/$name.html" && [ -s "$OUT/$name.png" ]; then
    echo "ok    $path -> $OUT/$name.png"
  else
    echo "FAIL  $path (see $OUT/$name.err)" >&2; fails=$((fails+1))
  fi
}
check login '/login?loggedout=1' 'Sign in with SSO'
check home / '<title>|<html'      # unauthenticated / redirects to the login page
check healthz /healthz '>ok'
[ "$fails" -eq 0 ]
