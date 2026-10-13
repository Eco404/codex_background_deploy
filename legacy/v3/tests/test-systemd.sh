#!/usr/bin/env bash
# Fully offline lifecycle tests: no real systemctl/loginctl/sudo/Codex/npm calls.
set -euo pipefail
unset BASH_ENV ENV
TEST_ROOT=$(cd -- "$(dirname -- "$0")/.." && pwd)
TEST_NODE=$(command -v node)
TEST_TMP=$(mktemp -d)
cleanup() {
    local pidfile pid
    for pidfile in "$TEST_TMP"/*/mock.pid; do
        [ -f "$pidfile" ] || continue
        read -r pid < "$pidfile"
        kill "$pid" 2>/dev/null || true
    done
    rm -rf -- "$TEST_TMP"
}
trap cleanup EXIT
mkdir -p "$TEST_TMP/bin"
export TEST_NODE
cat > "$TEST_TMP/bin/node" <<'MOCK'
#!/bin/bash
case "${1:-}" in
  */check.mjs) printf 'mock health\n' >> "$TEST_CASE/health.log"; exit 0;;
esac
exec "$TEST_NODE" "$@"
MOCK
cat > "$TEST_TMP/bin/npm" <<'MOCK'
#!/bin/bash
printf '%s\n' "$*" >> "$TEST_CASE/npm.log"
# Dependency acquisition is stubbed; separate bridge tests verify protocol behavior.
exit 0
MOCK
cat > "$TEST_TMP/bin/codex" <<'MOCK'
#!/bin/bash
case "$*" in
  --version) printf 'codex mock\n';;
  'app-server --help') printf 'app-server --listen unix://PATH\n';;
  *) printf 'UNEXPECTED CODEX INVOCATION\n' >&2; exit 99;;
esac
MOCK
cat > "$TEST_TMP/socket.mjs" <<'MOCK'
import net from 'node:net';
import fs from 'node:fs';
const socket = process.argv[2];
const server = net.createServer((connection) => connection.end());
server.listen(socket);
process.on('SIGTERM', () => server.close(() => { try { fs.unlinkSync(socket); } catch {} process.exit(0); }));
MOCK
cat > "$TEST_TMP/bin/systemctl" <<'MOCK'
#!/bin/bash
set -eu
printf '%s\n' "$*" >> "$TEST_CASE/systemctl.log"
[ "${1:-}" = --user ] || exit 96
command_name=${2:-}
if [ "$command_name" = "${TEST_FAIL_SYSTEMCTL:-}" ]; then exit 1; fi
case "$command_name" in
  show-environment|daemon-reload) exit 0;;
  enable) touch "$TEST_CASE/enabled";;
  disable) rm -f "$TEST_CASE/enabled";;
  start)
    "$TEST_NODE" - "$TEST_SOCKET_SCRIPT" "$HOME/.local/state/codex-background/app-server.sock" "$TEST_CASE/mock.pid" <<'NODE'
const {spawn}=require('node:child_process');
const fs=require('node:fs');
const child=spawn(process.execPath,[process.argv[2],process.argv[3]],{detached:true,stdio:'ignore'});
fs.writeFileSync(process.argv[4],String(child.pid));
child.unref();
NODE
    ;;
  stop)
    if [ -f "$TEST_CASE/mock.pid" ]; then
      read -r mock_pid < "$TEST_CASE/mock.pid" || true
      kill -TERM "$mock_pid" 2>/dev/null || true
      rm -f "$TEST_CASE/mock.pid"
    fi
    ;;
  *) printf 'Unexpected mock systemctl command: %s\n' "$*" >&2; exit 95;;
esac
MOCK
cat > "$TEST_TMP/bin/loginctl" <<'MOCK'
#!/bin/bash
printf '%s\n' "$*" >> "$TEST_CASE/loginctl.log"
case "${1:-}" in
  show-user) if [ -f "$TEST_CASE/linger" ]; then printf 'yes\n'; else printf 'no\n'; fi;;
  enable-linger) touch "$TEST_CASE/linger";;
  disable-linger) rm -f "$TEST_CASE/linger";;
  *) exit 94;;
esac
MOCK
cat > "$TEST_TMP/bin/sudo" <<'MOCK'
#!/bin/bash
printf '%s\n' "$*" >> "$TEST_CASE/sudo.log"
[ "${1:-}" = loginctl ] || exit 93
exec "$TEST_MOCK_BIN/loginctl" "${@:2}"
MOCK
chmod +x "$TEST_TMP/bin/"*
export TEST_MOCK_BIN="$TEST_TMP/bin" TEST_SOCKET_SCRIPT="$TEST_TMP/socket.mjs"

new_case() {
    TEST_CASE="$TEST_TMP/$1"; export TEST_CASE
    mkdir -p "$TEST_CASE/home"
    case_home="$TEST_CASE/home"
    case_settings="$case_home/settings.json"
    case_runtime="$case_home/.local/share/codex-background"
    case_state="$case_home/.local/state/codex-background"
    case_unit="$case_home/.config/systemd/user/codex-background.service"
    printf '{"secret":"not-a-backup", "chatgpt.cliExecutable":"/original", "other":1}\n' > "$case_settings"
    cp "$case_settings" "$TEST_CASE/original.json"
}
deploy() {
    /usr/bin/env -u CODEX_NODE -u CODEX_HOME -u ENTRYPOINTD HOME="$case_home" PATH="$TEST_MOCK_BIN:/usr/bin:/bin" \
        bash "$TEST_ROOT/deploy.sh" "$@"
}
install_case() { deploy install --backend systemd --node "$TEST_MOCK_BIN/node" --codex "$TEST_MOCK_BIN/codex" --settings "$case_settings" --yes "$@"; }
fail() { printf '[FAIL] %s\n' "$*" >&2; [ ! -f "$TEST_CASE/output" ] || cat "$TEST_CASE/output" >&2; exit 1; }
expect_one() {
    local rc=0
    "$@" > "$TEST_CASE/output" 2>&1 || rc=$?
    [ "$rc" = 1 ] || fail "expected rc=1, got $rc"
}
assert_rolled_back() {
    cmp "$TEST_CASE/original.json" "$case_settings" || fail 'settings changed after rollback'
    [ ! -e "$case_unit" ] || fail 'unit remains after rollback'
    [ ! -e "$case_runtime" ] || fail 'runtime remains after rollback'
    [ ! -e "$case_state/transaction" ] || fail 'transaction remains after successful rollback'
    [ ! -e "$case_home/.local/bin/codex-background" ] || fail 'launcher remains after rollback'
    [ ! -e "$TEST_CASE/enabled" ] || fail 'service remains enabled after rollback'
}

new_case plan
deploy plan --node "$TEST_MOCK_BIN/node" --codex "$TEST_MOCK_BIN/codex" --settings "$case_settings" > "$TEST_CASE/output" 2>&1 || fail 'plan failed'
grep -Fq '后台：systemd' "$TEST_CASE/output" || fail 'auto did not select available mock systemd'
[ ! -e "$case_runtime" ] && [ ! -e "$case_state" ] || fail 'plan wrote runtime state'
cmp "$TEST_CASE/original.json" "$case_settings" || fail 'plan changed settings'
printf '[PASS] systemd plan auto selection is read-only\n'

for failure in enable start; do
    new_case "fail-$failure"
    export TEST_FAIL_SYSTEMCTL="$failure"
    expect_one install_case
    unset TEST_FAIL_SYSTEMCTL
    assert_rolled_back
    printf '[PASS] systemd %s rc=1 propagates and auto rollback removes adapters\n' "$failure"
done

new_case settings-failure
printf '{broken JSONC\n' > "$TEST_CASE/bad.json"
expect_one install_case --settings "$TEST_CASE/bad.json"
assert_rolled_back
printf '[PASS] late settings failure restores previously modified settings\n'

new_case success
install_case --linger > "$TEST_CASE/output" 2>&1 || fail 'installation failed'
grep -Fqx "ExecStart=\"$TEST_MOCK_BIN/codex\" app-server --listen \"unix://$case_state/app-server.sock\"" "$case_unit" || fail 'wrong unit ExecStart'
grep -Fqx "Environment=\"CODEX_HOME=$case_home/.codex\"" "$case_unit" || fail 'wrong unit CODEX_HOME'
grep -Fqx 'UMask=0077' "$case_unit" || fail 'unit lacks private umask'
unit_key=$(printf '%s' "$case_unit" | sha256sum | cut -d ' ' -f 1)
[ -f "$case_state/files/$unit_key" ] || fail 'unit ownership record missing'
[ "$(sed -n '1p' "$case_state/files/$unit_key")" = "$case_unit" ] || fail 'wrong ownership path'
[ "$(sed -n '2p' "$case_state/files/$unit_key")" = "$(sha256sum "$case_unit" | cut -d ' ' -f 1)" ] || fail 'wrong ownership hash'
[ -f "$case_state/linger-owned" ] && [ -f "$TEST_CASE/linger" ] || fail 'linger ownership missing'
printf '[PASS] generated systemd unit and hashes, linger ownership\n'

cp "$TEST_CASE/systemctl.log" "$TEST_CASE/systemctl.before"
deploy doctor > "$TEST_CASE/output" 2>&1 || fail 'doctor failed'
cmp "$TEST_CASE/systemctl.before" "$TEST_CASE/systemctl.log" || fail 'doctor invoked service management'
# An old interrupted transaction must not make a read-only command perform recovery.
mkdir -p "$case_state/transaction"
printf 'leave me\n' > "$case_state/transaction/audit-sentinel"
expect_one deploy doctor
[ -f "$case_state/transaction/audit-sentinel" ] || fail 'doctor mutated an existing transaction'
cmp "$TEST_CASE/systemctl.before" "$TEST_CASE/systemctl.log" || fail 'doctor started/stopped service with pending transaction'
rm -rf "$case_state/transaction"
printf '[PASS] doctor does not start services or recover unrelated transactions\n'

cp "$case_settings" "$TEST_CASE/installed-settings.json"
printf '{"chatgpt.cliExecutable":"/user-change"}\n' > "$case_settings"
expect_one deploy uninstall --yes
[ -d "$case_runtime" ] && [ -f "$case_unit" ] || fail 'settings conflict removed runtime or unit'
cmp "$TEST_CASE/systemctl.before" "$TEST_CASE/systemctl.log" || fail 'settings conflict stopped service'
printf '[PASS] settings conflict preserves runtime and returns rc=1\n'

cp "$TEST_CASE/installed-settings.json" "$case_settings"
deploy uninstall --yes > "$TEST_CASE/output" 2>&1 || fail 'uninstall failed'
cmp "$TEST_CASE/original.json" "$case_settings" || fail 'original settings not restored'
[ ! -e "$case_runtime" ] && [ ! -e "$case_unit" ] && [ ! -e "$case_state" ] || fail 'managed files remain after uninstall'
[ ! -e "$TEST_CASE/linger" ] || fail 'owned linger not disabled'
[ -x "$TEST_MOCK_BIN/codex" ] && [ -x "$TEST_MOCK_BIN/node" ] || fail 'external runtime removed'
printf '[PASS] systemd uninstall restores settings and preserves external runtimes\n'
