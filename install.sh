#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
case "$(uname -s)" in Linux) ;; *) printf 'Only Linux is supported.\n' >&2; exit 1;; esac
case "$(uname -m)" in x86_64|amd64) ARCH=amd64;; aarch64|arm64) ARCH=arm64;; *) printf 'Unsupported architecture\n' >&2; exit 1;; esac
if [ -x "$ROOT/codex-bg" ]; then exec "$ROOT/codex-bg" --installer-dir "$ROOT" "$@"; fi
DATA="$ROOT/installer-data"
mkdir -p "$DATA/cache" "$DATA/tmp"
chmod 700 "$DATA" "$DATA/tmp"
PACKAGE="codex-bg-linux-$ARCH"
if [ -f "$ROOT/dist/$PACKAGE" ] && [ -f "$ROOT/dist/SHA256SUMS" ]; then
  SOURCE="$ROOT/dist/$PACKAGE"
  SUMS="$ROOT/dist/SHA256SUMS"
else
  : "${CODEX_BG_DOWNLOAD_BASE:?Provide a release download base URL or use a complete release archive}"
  case "$CODEX_BG_DOWNLOAD_BASE" in https://*) ;; *) printf 'Release download base must use HTTPS\n' >&2; exit 1;; esac
  command -v curl >/dev/null || { printf 'curl required for bootstrap download\n' >&2; exit 1; }
  curl -fL --retry 2 --connect-timeout 15 --max-time 180 "$CODEX_BG_DOWNLOAD_BASE/$PACKAGE" -o "$DATA/cache/$PACKAGE"
  curl -fL --retry 2 --connect-timeout 15 --max-time 30 "$CODEX_BG_DOWNLOAD_BASE/SHA256SUMS" -o "$DATA/cache/SHA256SUMS"
  SOURCE="$DATA/cache/$PACKAGE"
  SUMS="$DATA/cache/SHA256SUMS"
fi
EXPECTED=$(awk -v n="$PACKAGE" '$2==n {print $1}' "$SUMS")
[ "${#EXPECTED}" = 64 ] || { printf 'Invalid release checksum\n' >&2; exit 1; }
ACTUAL=$(sha256sum "$SOURCE"); ACTUAL=${ACTUAL%% *}
[ "$EXPECTED" = "$ACTUAL" ] || { printf 'Release checksum mismatch\n' >&2; exit 1; }
TEMP="$ROOT/.codex-bg-install-$$"
trap 'rm -f "$TEMP"' EXIT HUP INT TERM
cp "$SOURCE" "$TEMP"
chmod 755 "$TEMP"
mv "$TEMP" "$ROOT/codex-bg"
exec "$ROOT/codex-bg" --installer-dir "$ROOT" "$@"
