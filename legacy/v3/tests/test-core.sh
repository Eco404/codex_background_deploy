#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd -- "$(dirname -- "$0")/.." && pwd)
TMP=$(mktemp -d)
trap 'rm -rf -- "$TMP"' EXIT
export HOME="$TMP/home"
mkdir -p "$HOME"
. "$ROOT/deploy.sh"
# EOF is not consent, independent of terminal detection.
CB_YES=0
if cb_confirm </dev/null; then echo 'EOF must cancel' >&2; exit 1; fi
# Atomic copy must preserve old bytes and propagate a failure even in an if.
printf old > "$TMP/destination"
printf new > "$TMP/source"
cp() { return 23; }
if cb_atomic_copy "$TMP/source" "$TMP/destination" 600; then echo 'copy failure swallowed' >&2; exit 1; fi
[ "$(cat "$TMP/destination")" = old ]
unset -f cp
# A terminal-stage unselected item must never erase a failure.
cb_install() { return 1; }
if main install --yes; then echo 'generic install failure swallowed' >&2; exit 1; else [ "$?" = 1 ]; fi
# The transaction inventory checks content, not just a fixed marker.
mkdir -p "$CB_STATE/files"
printf wrapper > "$TMP/managed"
cb_record_file "$TMP/managed"
cb_verify_files
printf tampered > "$TMP/managed"
if cb_verify_files; then echo 'modified managed file accepted' >&2; exit 1; fi
# A noninteractive no-argument command displays help and cannot install.
bash "$ROOT/deploy.sh" </dev/null > "$TMP/help"
grep -Fq '统一 Codex 后台部署器' "$TMP/help"
printf '[PASS] core cancellation, failure propagation and ownership\n'
