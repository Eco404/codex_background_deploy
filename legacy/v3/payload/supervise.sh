#!/usr/bin/env bash
# Detached container supervisor. Own only the child started by this process.
set -uo pipefail
CB_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd) || exit 1
. "$CB_ROOT/config.sh" || exit 1
export CODEX_HOME="$CB_CODEX_HOME"
mkdir -p -- "$CB_STATE" || exit 1
exec 9>"$CB_STATE/supervisor.lock" || exit 1
flock -n 9 || exit 0
started=$(sed 's/.*) //' "/proc/$$/stat" | awk '{print $20}') || exit 1
printf '%s %s\n' "$$" "$started" > "$CB_STATE/supervisor.pid" || exit 1
child=''
stopping=0
cleanup() {
    stopping=1
    trap '' TERM INT HUP
    # setsid makes this supervisor its own session/process-group leader.
    # Kill its group, including helpers orphaned by a previous app-server exit.
    local pgid sid
    read -r pgid sid < <(ps -o pgid=,sid= -p "$$")
    if [ "$pgid" = "$$" ] && [ "$sid" = "$$" ]; then kill -TERM -- "-$$" 2>/dev/null || true; fi
    if [ -n "$child" ]; then kill -TERM "$child" 2>/dev/null || true; wait "$child" 2>/dev/null || true; fi
    rm -f -- "$CB_STATE/supervisor.pid"
}
trap 'cleanup; exit 0' TERM INT HUP
trap cleanup EXIT
while [ "$stopping" = 0 ]; do
    "$CB_CODEX" app-server --listen "unix://$CB_SOCKET" 9>&- &
    child=$!
    wait "$child"
    rc=$?
    child=''
    printf '%s server exited rc=%s; retry in 2s\n' "$(date -Is)" "$rc"
    sleep 2 9>&- & child=$!; wait "$child"; child=''
done
