#!/usr/bin/env bash
set -Eeuo pipefail
# Ignore injected shell hooks when running command fixtures.
unset BASH_ENV ENV
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
REAL_NODE="$(command -v node)"
cleanup() {
  if [ -f "$TMP/home/.local/share/codex-background-addons/codexapp/process" ]; then
    CODEX_BACKGROUND_ADDONS_DATA="$TMP/home/.local/share/codex-background-addons" bash "$ROOT/payload/runtime.sh" stop-codexapp || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT
export CODEX_BACKGROUND_ADDONS_HOME="$TMP/home"
export CODEX_BACKGROUND_ADDONS_DATA="$TMP/home/.local/share/codex-background-addons"
export CODEX_BACKGROUND_ADDONS_STATE="$TMP/home/.config/codex-background-addons"
export CODEX_BACKGROUND_ENTRYPOINT_DIR="$TMP/hooks"
export CODEX_BACKGROUND_WRAPPER="$TMP/wrapper"
export TEST_ADDON_TMP="$TMP"
DATA="$CODEX_BACKGROUND_ADDONS_DATA"
mkdir -p "$TMP/home" "$TMP/mock" "$TMP/media"
export CODE_SERVER_MEDIA_DIR="$TMP/media"
cat > "$TMP/mock/sudo" <<'SH'
#!/usr/bin/env bash
[ "$1" != -n ] || shift
exec "$@"
SH
cat > "$TMP/mock/apt-get" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$TEST_ADDON_TMP/apt-calls"
[ "${TEST_APT_FAIL:-0}" != 1 ]
SH
cat > "$TMP/mock/dpkg-query" <<'SH'
#!/usr/bin/env bash
exit 1
SH
cat > "$TMP/wrapper" <<'SH'
#!/usr/bin/env bash
exit 0
SH
chmod +x "$TMP/mock/"* "$TMP/wrapper"
export PATH="$TMP/mock:$PATH"
run() { "$ROOT/deploy.sh" "$@" > "$TMP/result" 2>&1; }
reject() { if run "$@"; then cat "$TMP/result"; printf 'expected failure: %s\n' "$*" >&2; exit 1; fi; }

run plan apti
[ ! -e "$DATA" ]
reject install apti < /dev/null
[ ! -e "$DATA" ]
run install apti --yes
[ -f "$TMP/hooks/codex-background-addons.sh" ] && [ ! -L "$TMP/hooks/codex-background-addons.sh" ]
[ ! -e "$TMP/apt-calls" ]
"$TMP/home/.local/bin/apti" ripgrep >/dev/null
grep -Fxq ripgrep "$DATA/apti/packages.txt"
grep -Fq 'install -y -- ripgrep' "$TMP/apt-calls"
if TEST_APT_FAIL=1 "$TMP/home/.local/bin/apti" curl >/dev/null 2>&1; then exit 1; fi
! grep -Fxq curl "$DATA/apti/packages.txt"
if "$TMP/home/.local/bin/apti" --bad >/dev/null 2>&1; then exit 1; fi
run uninstall apti --yes
[ ! -e "$TMP/home/.local/bin/apti" ] && [ ! -e "$TMP/hooks/codex-background-addons.sh" ]
grep -Fxq ripgrep "$CODEX_BACKGROUND_ADDONS_STATE/apti-packages-retained.txt"

# External commands and directories are never adopted/deleted.
mkdir -p "$TMP/home/.local/bin"
printf external > "$TMP/home/.local/bin/apti"
reject install apti --yes
[ "$(cat "$TMP/home/.local/bin/apti")" = external ]
reject uninstall node --yes
rm "$TMP/home/.local/bin/apti"

# Branding restores originals and rejects user changes before deleting backups.
for f in "$ROOT/resources/vscode/"*; do printf 'original %s' "${f##*/}" > "$TMP/media/${f##*/}"; done
cp -a "$TMP/media" "$TMP/original-media"
run install branding --yes
cmp "$ROOT/resources/vscode/favicon.ico" "$TMP/media/favicon.ico"
printf external-change > "$TMP/media/favicon.ico"
reject uninstall branding --yes
[ -f "$DATA/branding/originals/favicon.ico" ]
cmp "$ROOT/resources/vscode/pwa-icon-192.png" "$TMP/media/pwa-icon-192.png"
cp "$ROOT/resources/vscode/favicon.ico" "$TMP/media/favicon.ico"
run uninstall branding --yes
diff -r "$TMP/media" "$TMP/original-media"

# Node: exercise verified archive install offline, including failed checksum.
mkdir -p "$TMP/node-fixture/node-v22.20.0-linux-x64/bin"
printf '#!/usr/bin/env bash\nprintf "v22.20.0\\n"\n' > "$TMP/node-fixture/node-v22.20.0-linux-x64/bin/node"
chmod +x "$TMP/node-fixture/node-v22.20.0-linux-x64/bin/node"
tar -czf "$TMP/node.tar.gz" -C "$TMP/node-fixture" node-v22.20.0-linux-x64
printf '%s  node-v22.20.0-linux-x64.tar.gz\n' "$(sha256sum "$TMP/node.tar.gz" | awk '{print $1}')" > "$TMP/SHASUMS256.txt"
cat > "$TMP/mock/uname" <<'SH'
#!/usr/bin/env bash
printf 'x86_64\n'
SH
cat > "$TMP/mock/curl" <<'SH'
#!/usr/bin/env bash
output='' url=''
while [ "$#" -gt 0 ]; do
  case "$1" in -o) output="$2"; shift 2;; https://*) url="$1"; shift;; *) shift;; esac
done
case "$url" in
  */SHASUMS256.txt) cp "$TEST_ADDON_TMP/SHASUMS256.txt" "$output";;
  *.tar.gz) cp "$TEST_ADDON_TMP/node.tar.gz" "$output";;
  *) exit 1;;
esac
SH
chmod +x "$TMP/mock/uname" "$TMP/mock/curl"
cp "$TMP/SHASUMS256.txt" "$TMP/good-sha"
printf '%064d  node-v22.20.0-linux-x64.tar.gz\n' 0 > "$TMP/SHASUMS256.txt"
reject install node --yes
[ ! -e "$DATA/node" ]
cp "$TMP/good-sha" "$TMP/SHASUMS256.txt"
run install node --yes
[ -x "$DATA/node/runtime/bin/node" ]
mkdir -p "$TMP/home/.local/share/codex-background"
printf 'CB_NODE_PIN=%q\n' "$DATA/node/runtime/bin/node" > "$TMP/home/.local/share/codex-background/config.sh"
reject uninstall node --yes
[ -x "$DATA/node/runtime/bin/node" ]
rm "$TMP/home/.local/share/codex-background/config.sh"
run uninstall node --yes
[ ! -e "$DATA/node" ]

# Failed Conda installs retain a narrow ownership marker, allowing safe cleanup.
cat > "$TMP/mock/curl" <<'SH'
#!/usr/bin/env bash
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then output="$2"; shift 2; else shift; fi
done
cat > "$output" <<'INSTALLER'
#!/usr/bin/env bash
while [ "$#" -gt 0 ]; do
  if [ "$1" = -p ]; then prefix="$2"; shift 2; else shift; fi
done
mkdir -p "$prefix/bin"
[ "${TEST_CONDA_FAIL:-0}" != 1 ] || exit 19
printf '#!/usr/bin/env bash\nprintf "conda fixture\\n"\n' > "$prefix/bin/conda"
chmod +x "$prefix/bin/conda"
INSTALLER
SH
if TEST_CONDA_FAIL=1 run install miniconda --yes; then exit 1; fi
[ -f "$DATA/miniconda/.owner" ]
grep -Fxq installing "$DATA/miniconda/status"
run uninstall miniconda --yes
run install miniconda --yes
[ -x "$DATA/miniconda/runtime/bin/conda" ]
run uninstall miniconda --yes
[ ! -e "$TMP/home/.bashrc" ] && [ ! -e "$TMP/home/.condarc" ]

# Official CLI installer fixture proves all installer writes stay in its own
# home, fixed versions propagate, and failed/active installs are not misreported.
cat > "$TMP/mock/codex" <<'SH'
#!/usr/bin/env node
// Deliberately resembles an external npm-managed Codex; never execute it.
SH
chmod +x "$TMP/mock/codex"
cp "$TMP/mock/codex" "$TMP/external-codex-original"
cat > "$TMP/mock/curl" <<'SH'
#!/usr/bin/env bash
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then output="$2"; shift 2; else shift; fi
done
cat > "$output" <<'INSTALLER'
#!/bin/sh
set -eu
[ "$CODEX_NON_INTERACTIVE" = 1 ]
[ "$CODEX_INSTALL_DAEMON_ONLY" = 0 ]
[ "$1" = --release ] && [ "$2" = "$CODEX_RELEASE" ]
case "$HOME" in */codex-cli/installer-home) :;; *) exit 21;; esac
case "$CODEX_HOME" in */codex-cli/packages-home) :;; *) exit 22;; esac
case "$CODEX_INSTALL_DIR" in */codex-cli/bin) :;; *) exit 23;; esac
printf 'private installer profile\n' > "$HOME/.bashrc"
mkdir -p "$CODEX_HOME/packages/standalone/current"
[ "${TEST_CODEX_FAIL:-0}" != 1 ] || exit 19
printf '#!/bin/sh\nprintf "codex-cli %s\\n"\n' "$CODEX_RELEASE" > "$CODEX_HOME/packages/standalone/current/codex"
chmod +x "$CODEX_HOME/packages/standalone/current/codex"
ln -s "$CODEX_HOME/packages/standalone/current/codex" "$CODEX_INSTALL_DIR/codex"
INSTALLER
SH
if TEST_CODEX_FAIL=1 run install codex-cli --yes; then exit 1; fi
grep -Fxq installing "$DATA/codex-cli/status"
run uninstall codex-cli --yes
CODEX_ADDON_CODEX_VERSION=0.153.4 run install codex-cli --yes
[ "$("$DATA/codex-cli/bin/codex" --version)" = 'codex-cli 0.153.4' ]
[ -f "$DATA/codex-cli/installer-home/.bashrc" ]
[ ! -e "$TMP/home/.bashrc" ] && [ ! -e "$TMP/home/.codex" ]
cmp "$TMP/mock/codex" "$TMP/external-codex-original"
printf 'CB_CODEX=%q\n' "$DATA/codex-cli/bin/codex" > "$TMP/home/.local/share/codex-background/config.sh"
reject uninstall codex-cli --yes
rm "$TMP/home/.local/share/codex-background/config.sh"
run uninstall codex-cli --yes
cmp "$TMP/mock/codex" "$TMP/external-codex-original"
[ ! -e "$DATA/codex-cli" ]

# Mock npm delivers a tiny local HTTP process, not a model/real CodexApp request.
cat > "$TMP/mock/npm-fixture" <<'SH'
#!/usr/bin/env bash
while [ "$#" -gt 0 ]; do
  if [ "$1" = --prefix ]; then target="$2"; shift 2; else shift; fi
done
mkdir -p "$target/node_modules/.bin"
cat > "$target/node_modules/.bin/codexapp" <<'JS'
const http=require('node:http');
const args=process.argv.slice(2);
http.createServer((req,res)=>res.end('fixture')).listen(Number(args[args.indexOf('--port')+1]),'127.0.0.1');
JS
chmod +x "$target/node_modules/.bin/codexapp"
SH
chmod +x "$TMP/mock/npm-fixture"
export CODEX_ADDON_NODE_BIN="$REAL_NODE"
export CODEX_ADDON_NPM_BIN="$TMP/mock/npm-fixture"
export CODEXAPP_PASSWORD_FILE="$TMP/password"
printf 'temporary fixture password' > "$TMP/password"
export CODEXAPP_PORT
CODEXAPP_PORT="$("$REAL_NODE" -e 'const n=require("net");const s=n.createServer();s.listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})')"
mkdir "$TMP/payload"
cat > "$TMP/payload/control.sh" <<'SH'
#!/usr/bin/env bash
[ "$1" = start ] || exit 1
printf 'core-ready\n' > "$TEST_ADDON_TMP/core-ready"
SH
run install codexapp --yes
[ -f "$TMP/core-ready" ]
read -r pid birth < "$DATA/codexapp/process"
kill -0 "$pid"
# A stale record pointing to this test shell must never send it a signal.
cp "$DATA/codexapp/process" "$TMP/process-good"
printf '%s 0\n' "$$" > "$DATA/codexapp/process"
reject uninstall codexapp --yes
kill -0 "$$"
cp "$TMP/process-good" "$DATA/codexapp/process"
run uninstall codexapp --yes
[ ! -e "$DATA/codexapp" ]
! kill -0 "$pid" 2>/dev/null || [ ! -s "/proc/$pid/cmdline" ]
[ ! -e "$TMP/home/.codex" ]
printf 'addons: all offline regression cases passed\n'
