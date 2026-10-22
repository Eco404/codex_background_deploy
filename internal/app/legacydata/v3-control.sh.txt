#!/usr/bin/env bash
set -uo pipefail
# The installer lock must not escape into a detached, long-lived process.
if [[ ${CB_LOCK_FD:-} =~ ^[0-9]+$ ]]; then exec {CB_LOCK_FD}>&-; unset CB_LOCK_FD; fi
CB_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd) || exit 1
. "$CB_ROOT/config.sh" || exit 1
. "$CB_ROOT/lib/common.sh" || exit 1
. "$CB_ROOT/payload/bridge/resolve-node.sh" || exit 1
export CODEX_HOME="$CB_CODEX_HOME"

cb_own_pid() {
    local pid=$1 expected=$2 started=${3:-} actual
    [[ $pid =~ ^[0-9]+$ ]] && [ -r "/proc/$pid/cmdline" ] || return 1
    [ "$(stat -c %u "/proc/$pid")" = "$(id -u)" ] || return 1
    actual=$(sed 's/.*) //' "/proc/$pid/stat" | awk '{print $20}') || return
    [ -n "$started" ] && [ "$started" = "$actual" ] || return 1
    local -a argv=()
    mapfile -d '' -t argv < "/proc/$pid/cmdline" || return 1
    [ "${argv[0]:-}" = /bin/bash ] && [ "${argv[1]:-}" = "$expected" ]
}
cb_health() {
    local node
    node=$(cb_resolve_node) || return 1
    CB_SOCKET="$CB_SOCKET" timeout 12 "$node" "$CB_ROOT/payload/bridge/check.mjs" "$CB_SOCKET"
}
cb_start() {
    if [ "$CB_BACKEND" = systemd ]; then
        timeout 20 systemctl --user start codex-background.service || return
    else
        local pid='' started=''
        [ ! -f "$CB_STATE/supervisor.pid" ] || read -r pid started < "$CB_STATE/supervisor.pid"
        if ! cb_own_pid "$pid" "$CB_ROOT/payload/supervise.sh" "$started"; then
            [ ! -S "$CB_SOCKET" ] || { cb_error 'socket 存在但无可验证的 supervisor；拒绝接管未知后台。'; return 1; }
            mkdir -p -- "$CB_STATE" || return
            # The child owns a flock for its entire lifetime; duplicate starts exit harmlessly.
            nohup setsid /bin/bash "$CB_ROOT/payload/supervise.sh" </dev/null >>"$CB_STATE/server.log" 2>&1 &
        fi
    fi
    local i
    for i in {1..40}; do
        if [ -S "$CB_SOCKET" ]; then cb_health; return $?; fi
        sleep .25
    done
    cb_error "后台握手失败。检查 $CB_STATE/server.log 或 journalctl --user -u codex-background.service"
}
cb_stop() {
    if [ "$CB_BACKEND" = systemd ]; then
        systemctl --user stop codex-background.service || return
    else
        local pid='' started='' i
        [ ! -f "$CB_STATE/supervisor.pid" ] || read -r pid started < "$CB_STATE/supervisor.pid"
        if cb_own_pid "$pid" "$CB_ROOT/payload/supervise.sh" "$started"; then
            kill -TERM "$pid" || return
            for i in {1..80}; do cb_own_pid "$pid" "$CB_ROOT/payload/supervise.sh" "$started" || break; sleep .25; done
            cb_own_pid "$pid" "$CB_ROOT/payload/supervise.sh" "$started" && { cb_error '后台进程未退出；保留所有文件。'; return 1; }
        fi
        [ ! -S "$CB_SOCKET" ] || { cb_error 'socket 仍存在，拒绝猜测删除。'; return 1; }
    fi
}
case "${1:-status}" in
    start) cb_start ;;
    stop) cb_stop ;;
    restart) cb_stop && cb_start ;;
    status|doctor|check)
        printf 'backend=%s\ncode_home=%s\nsocket=%s\n' "$CB_BACKEND" "$CB_CODEX_HOME" "$CB_SOCKET"
        cb_health
        ;;
    *) cb_error '用法：codex-background start|stop|restart|status|doctor' ;;
esac
