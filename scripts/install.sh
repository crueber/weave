#!/bin/sh
# Weave CLI installer (POSIX sh). Downloads the release archive for this OS/arch from the Forgejo
# release, verifies the signed checksum list and the archive's SHA-256, and installs `weave`.
#
#   curl -fsSL https://github.com/crueber/weave/releases/download/<tag>/install.sh | sh
#   sh install.sh [--version vX.Y.Z] [--dir DIR] [--skill|--no-skill] [--yes]
#
# Environment:
#   WEAVE_VERSION            release tag to install (default: latest release)
#   WEAVE_INSTALL_DIR        install directory (default: /usr/local/bin as root, else ~/.local/bin)
#   WEAVE_RELEASE_BASE       directory URL that holds the archives and SHA256SUMS.txt (testing / mirrors)
#   WEAVE_SERVER             Forgejo server (default https://github.com)
#   WEAVE_REPO               owner/repo (default crueber/weave)
#   WEAVE_INSTALL_SKILL      1 = also run `weave skill install` (every detected coding agent), 0 = never (default: ask on a terminal)
#   WEAVE_REQUIRE_SIGNATURE  1 = refuse to install unless SHA256SUMS.txt.sig verifies (needs OpenSSL >= 3)
#   WEAVE_INSECURE           1 = allow http:// download URLs (testing only; default: https only, TLS >= 1.2)
#   WEAVE_RELEASE_PUBKEY     path to a PEM public key to verify against (forks that sign with their own key)
#
# Signature: SHA256SUMS.txt is signed with ed25519 (SHA256SUMS.txt.sig). With OpenSSL >= 3 the
# signature is verified against the release key embedded below; without it the installer warns and
# continues (unless WEAVE_REQUIRE_SIGNATURE=1) -- the archive is still checked against SHA256SUMS.txt.
#
# Everything runs inside main(), called on the last line, so a truncated download executes nothing.

# base64 of the DER SubjectPublicKeyInfo of the release signing key (see internal/relsign).
WEAVE_RELEASE_PUBKEY_B64="MCowBQYDK2VwAyEAX2HjqHRYLLIO9QVyhCHsgJ8UqLT115QJqCMxxGhKkXk="

main() {
set -eu

SERVER="${WEAVE_SERVER:-https://github.com}"
REPO="${WEAVE_REPO:-crueber/weave}"
VERSION="${WEAVE_VERSION:-}"
DIR="${WEAVE_INSTALL_DIR:-}"
BASE="${WEAVE_RELEASE_BASE:-}"
SKILL="${WEAVE_INSTALL_SKILL:-ask}"
REQUIRE_SIG="${WEAVE_REQUIRE_SIGNATURE:-0}"
INSECURE="${WEAVE_INSECURE:-0}"

say() { printf '%s\n' "$*" >&2; }
die() { say "install.sh: $*"; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:?--version needs a value}"; shift 2 ;;
    --dir) DIR="${2:?--dir needs a value}"; shift 2 ;;
    --skill) SKILL=1; shift ;;
    --no-skill) SKILL=0; shift ;;
    --yes|-y) [ "$SKILL" = ask ] && SKILL=1; shift ;;
    -h|--help) sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown option $1" ;;
  esac
done

# --- transport: https only (TLS >= 1.2) unless WEAVE_INSECURE=1 ---
if [ "$INSECURE" != 1 ]; then
  for u in "$SERVER" "$BASE"; do
    case "$u" in ""|https://*) ;; *) die "refusing non-https URL $u (set WEAVE_INSECURE=1 for testing)" ;; esac
  done
fi

# --- platform ---
case "$(uname -s)" in
  Linux) OS=linux ;;
  Darwin) OS=darwin ;;
  *) die "unsupported OS $(uname -s); on Windows use install.ps1" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac

# --- tools ---
if command -v curl >/dev/null 2>&1; then
  if [ "$INSECURE" = 1 ]; then
    fetch() { curl -fsSL "$1" -o "$2"; }
    fetch_stdout() { curl -fsSL "$1"; }
  else
    fetch() { curl -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2 "$1" -o "$2"; }
    fetch_stdout() { curl -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2 "$1"; }
  fi
elif command -v wget >/dev/null 2>&1; then
  if [ "$INSECURE" = 1 ]; then
    fetch() { wget -q "$1" -O "$2"; }
    fetch_stdout() { wget -qO- "$1"; }
  else
    fetch() { wget -q --https-only --secure-protocol=TLSv1_2 "$1" -O "$2"; }
    fetch_stdout() { wget -qO- --https-only --secure-protocol=TLSv1_2 "$1"; }
  fi
else
  die "need curl or wget"
fi
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
elif command -v openssl >/dev/null 2>&1; then
  sha256() { openssl dgst -sha256 "$1" | sed 's/^.*= //'; }
else
  die "need sha256sum, shasum or openssl to verify the download"
fi

# openssl_ed25519: true when `openssl pkeyutl -verify -rawin` is available (OpenSSL >= 3; LibreSSL
# as shipped on macOS cannot do ed25519 with -rawin).
openssl_ed25519() {
  command -v openssl >/dev/null 2>&1 || return 1
  case "$(openssl version 2>/dev/null)" in OpenSSL\ 3.*|OpenSSL\ [4-9]*) return 0 ;; esac
  return 1
}

# --- where to download from ---
if [ -z "$BASE" ]; then
  if [ -z "$VERSION" ]; then
    say "looking up the latest release..."
    VERSION=$(fetch_stdout "$SERVER/api/v1/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name":"\([^"]*\)".*/\1/p' | head -n 1)
    [ -n "$VERSION" ] || die "cannot determine the latest release; pass --version vX.Y.Z"
  fi
  BASE="$SERVER/$REPO/releases/download/$VERSION"
fi
BASE="${BASE%/}"

TMP=$(mktemp -d 2>/dev/null || mktemp -d -t weave)
trap 'rm -rf "$TMP"' EXIT INT TERM

say "downloading checksums from $BASE"
fetch "$BASE/SHA256SUMS.txt" "$TMP/SHA256SUMS.txt" || die "cannot download $BASE/SHA256SUMS.txt"

# --- signature over SHA256SUMS.txt ---
if fetch "$BASE/SHA256SUMS.txt.sig" "$TMP/SHA256SUMS.txt.sig" 2>/dev/null; then
  if openssl_ed25519; then
    if [ -n "${WEAVE_RELEASE_PUBKEY:-}" ]; then
      cp "$WEAVE_RELEASE_PUBKEY" "$TMP/pub.pem" || die "cannot read WEAVE_RELEASE_PUBKEY"
    else
      { echo "-----BEGIN PUBLIC KEY-----"; echo "$WEAVE_RELEASE_PUBKEY_B64"; echo "-----END PUBLIC KEY-----"; } > "$TMP/pub.pem"
    fi
    openssl base64 -d -A -in "$TMP/SHA256SUMS.txt.sig" -out "$TMP/sig.bin" 2>/dev/null || die "SHA256SUMS.txt.sig is not valid base64"
    if openssl pkeyutl -verify -rawin -pubin -inkey "$TMP/pub.pem" -sigfile "$TMP/sig.bin" -in "$TMP/SHA256SUMS.txt" >/dev/null 2>&1; then
      say "signature ok (ed25519, release key)"
    else
      die "SIGNATURE VERIFICATION FAILED for SHA256SUMS.txt: do not install this release"
    fi
  elif [ "$REQUIRE_SIG" = 1 ]; then
    die "WEAVE_REQUIRE_SIGNATURE=1 but OpenSSL >= 3 is not available to verify the signature (macOS: brew install openssl; or use the weave verify-release command)"
  else
    say "warning: cannot verify the release signature (needs OpenSSL >= 3); continuing with the checksum only"
  fi
elif [ "$REQUIRE_SIG" = 1 ]; then
  die "WEAVE_REQUIRE_SIGNATURE=1 but $BASE/SHA256SUMS.txt.sig is not available"
else
  say "warning: this release has no signature file (SHA256SUMS.txt.sig); continuing with the checksum only"
fi

# the archive for this platform (weave_<tag>_<os>_<arch>.tar.gz)
ARCHIVE=$(awk -v suffix="_${OS}_${ARCH}.tar.gz" '{ n=$2 } index(n, "weave_")==1 && substr(n, length(n)-length(suffix)+1)==suffix { print n; exit }' "$TMP/SHA256SUMS.txt")
[ -n "$ARCHIVE" ] || die "no release archive for ${OS}/${ARCH} in $BASE/SHA256SUMS.txt"
WANT=$(awk -v n="$ARCHIVE" '$2==n { print $1; exit }' "$TMP/SHA256SUMS.txt")

say "downloading $ARCHIVE"
fetch "$BASE/$ARCHIVE" "$TMP/$ARCHIVE" || die "cannot download $BASE/$ARCHIVE"
GOT=$(sha256 "$TMP/$ARCHIVE")
[ "$GOT" = "$WANT" ] || die "checksum mismatch for $ARCHIVE (expected $WANT, got $GOT)"
say "checksum ok"

mkdir "$TMP/x"
tar -xzf "$TMP/$ARCHIVE" -C "$TMP/x"
[ -f "$TMP/x/weave" ] || die "archive does not contain a weave binary"

# --- install ---
if [ -z "$DIR" ]; then
  if [ "$(id -u)" = 0 ]; then DIR=/usr/local/bin; else DIR="$HOME/.local/bin"; fi
fi
mkdir -p "$DIR" || die "cannot create $DIR (set WEAVE_INSTALL_DIR or run with sudo)"
install -m 0755 "$TMP/x/weave" "$DIR/weave" 2>/dev/null || { cp "$TMP/x/weave" "$DIR/weave" && chmod 0755 "$DIR/weave"; } || die "cannot write $DIR/weave"
say "installed $DIR/weave"
case ":$PATH:" in *":$DIR:"*) ;; *) say "note: $DIR is not on your PATH; add it to your shell profile" ;; esac
"$DIR/weave" version >&2 || true

# --- optional: agent skill ---
if [ "$SKILL" = ask ]; then
  if [ -r /dev/tty ] && [ -w /dev/tty ]; then
    printf 'Install the Weave skill for every coding agent found here (Claude Code, Codex, OpenCode, pi, Gemini, Cursor, ...)? [y/N] ' >/dev/tty
    read ans </dev/tty || ans=n
    case "$ans" in y|Y|yes|YES) SKILL=1 ;; *) SKILL=0 ;; esac
  else
    SKILL=0
  fi
fi
if [ "$SKILL" = 1 ]; then
  "$DIR/weave" skill install || say "skill install failed; run \`weave skill install\` later"
fi

say ""
say "next: weave login --url https://your-weave-server --token wk_...   (then: weave doctor)"
}

main "$@"
