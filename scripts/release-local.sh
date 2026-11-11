#!/bin/sh
# Builds the whole release matrix locally into dist/ with the same packager CI uses
# (cmd/weave-release): 6 slim CLI targets as archives + raw binaries, linux server binaries,
# install scripts and SHA256SUMS.txt. With VERIFY=1 it also runs the linux binary, inspects the
# others and exercises install.sh against a local http.server.
#
#   scripts/release-local.sh                    # VERSION from git describe
#   VERSION=v0.0.1-test VERIFY=1 scripts/release-local.sh
set -eu
cd "$(dirname "$0")/.."
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT="${OUT:-dist}"

go run ./cmd/weave-skillgen -check || { echo "skills are stale: run 'make skill' first" >&2; exit 1; }
go run ./cmd/weave-release -out "$OUT" -version "$VERSION"
echo
ls -l "$OUT"

[ "${VERIFY:-0}" = 1 ] || exit 0

echo; echo "== verifying =="
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) arch=amd64 ;; esac
BIN="$OUT/weave-$os-$arch"
"$BIN" version
"$BIN" help | head -3
"$BIN" completion bash | head -2
for f in "$OUT"/weave-*; do
  case "$f" in *.tar.gz|*.zip) continue ;; esac
  if command -v file >/dev/null 2>&1; then file "$f"; fi
done
(cd "$OUT" && if command -v sha256sum >/dev/null 2>&1; then sha256sum -c SHA256SUMS.txt; else shasum -a 256 -c SHA256SUMS.txt; fi)

echo; echo "== install.sh against a local http.server =="
PORT="${PORT:-18765}"
(cd "$OUT" && python3 -m http.server "$PORT" --bind 127.0.0.1 >/dev/null 2>&1) &
SRV=$!
trap 'kill $SRV 2>/dev/null || true; rm -rf "$INSTALL_TMP"' EXIT INT TERM
INSTALL_TMP=$(mktemp -d)
sleep 1
WEAVE_INSECURE=1 WEAVE_RELEASE_BASE="http://127.0.0.1:$PORT" WEAVE_INSTALL_DIR="$INSTALL_TMP/bin" WEAVE_INSTALL_SKILL=0 sh "$OUT/install.sh"
"$INSTALL_TMP/bin/weave" version
HOME="$INSTALL_TMP/home" "$INSTALL_TMP/bin/weave" skill install
HOME="$INSTALL_TMP/home" "$INSTALL_TMP/bin/weave" skill check
echo "ok"
