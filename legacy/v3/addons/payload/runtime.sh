#!/usr/bin/env bash
set -uo pipefail
DATA="${CODEX_BACKGROUND_ADDONS_DATA:-$HOME/.local/share/codex-background-addons}"
OWNER='codex-background-addons-v1'
ACTION="${1:-startup}"
LOCK=''
fail() { printf 'addons: %s\n' "$*" >&2; return 1; }
owned() { [ -f "$DATA/$1/.owner" ] && [ "$(cat "$DATA/$1/.owner")" = "$OWNER" ]; }
root_run() { if [ "$(id -u)" = 0 ]; then "$@"; else command -v sudo >/dev/null && sudo -n "$@"; fi; }
cleanup() { [ -z "$LOCK" ] || rmdir -- "$LOCK" 2>/dev/null || true; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

pid_matches() {
  local pid="$1" executable="$2" birth="$3" actual
  [[ "$pid" =~ ^[0-9]+$ ]] && [ -r "/proc/$pid/cmdline" ] && [ -r "/proc/$pid/stat" ] || return 1
  tr '\0' '\n' < "/proc/$pid/cmdline" | grep -Fxq -- "$executable" || return 1
  actual="$(sed 's/.*) //' "/proc/$pid/stat" | awk '{print $20}')"
  [ -n "$birth" ] && [ "$actual" = "$birth" ] && kill -0 "$pid" 2>/dev/null
}

start_codexapp() {
  local NODE_BIN='' WRAPPER='' PORT='' pid='' birth='' exe="$DATA/codexapp/node_modules/.bin/codexapp"
  . "$DATA/codexapp/config.sh" || return
  [ -x "$NODE_BIN" ] && [ -x "$WRAPPER" ] && [ -x "$exe" ] && [ -s "$DATA/codexapp/password" ] || { fail 'CodexApp 缺少 Node、核心 wrapper、程序或密码。'; return 1; }
  local control="$(dirname "$WRAPPER")/payload/control.sh"
  if [ -f "$control" ]; then
    # The image may run the addons hook before the core hook. Start/check the
    # daemon explicitly before allowing CodexApp to spawn its first bridge.
    bash "$control" start || { fail '核心后台尚未就绪，暂不启动 CodexApp。'; return 1; }
  fi
  if [ -f "$DATA/codexapp/process" ]; then
    read -r pid birth < "$DATA/codexapp/process" || true
    if pid_matches "$pid" "$exe" "$birth"; then printf 'CodexApp 已运行。\n'; return 0; fi
    rm -- "$DATA/codexapp/process" || return
  fi
  # Check the port before launching; do not mistake another HTTP server for ours.
  "$NODE_BIN" - "$PORT" <<'JS' || return
const net = require('node:net');
const s = net.createServer();
s.once('error', e => { console.error(`CodexApp port unavailable: ${e.message}`); process.exitCode=1; });
s.listen(Number(process.argv[2]), '0.0.0.0', () => s.close());
JS
  local password
  password="$(cat "$DATA/codexapp/password")" || return
  [ -n "$password" ] || { fail 'CodexApp 密码为空。'; return 1; }
  (
    cd "$DATA/codexapp" || exit 1
    export CODEXUI_CODEX_COMMAND="$WRAPPER"
    export PATH="$(dirname "$NODE_BIN"):$PATH"
    exec nohup "$NODE_BIN" "$exe" --no-login --no-tunnel --no-open --port "$PORT" --password "$password"
  ) >> "$DATA/codexapp/output.log" 2>&1 < /dev/null &
  pid=$!
  unset password
  birth="$(sed 's/.*) //' "/proc/$pid/stat" 2>/dev/null | awk '{print $20}')"
  printf '%s %s\n' "$pid" "$birth" > "$DATA/codexapp/process" || return
  local i
  for ((i=0;i<40;i++)); do
    if ! kill -0 "$pid" 2>/dev/null; then
      rm -f "$DATA/codexapp/process"
      fail "CodexApp 启动失败，见 $DATA/codexapp/output.log"; return 1
    fi
    if "$NODE_BIN" - "$PORT" <<'JS'
const http=require('node:http');
const req=http.get({host:'127.0.0.1',port:Number(process.argv[2]),path:'/',timeout:300},res=>{res.resume();process.exitCode=0;});
req.on('error',()=>{process.exitCode=1;});req.on('timeout',()=>req.destroy());
JS
    then
      pid_matches "$pid" "$exe" "$birth" || { fail 'HTTP 已响应，但进程身份不匹配。'; return 1; }
      printf 'CodexApp 已就绪，端口 %s。\n' "$PORT"; return 0
    fi
    sleep 0.25
  done
  fail "CodexApp 未就绪，保留进程和日志供诊断：$DATA/codexapp/output.log"
}

stop_codexapp() {
  local pid='' birth='' exe="$DATA/codexapp/node_modules/.bin/codexapp" i
  [ -f "$DATA/codexapp/process" ] || return 0
  read -r pid birth < "$DATA/codexapp/process" || true
  if ! pid_matches "$pid" "$exe" "$birth"; then
    if [[ "$pid" =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null; then
      fail 'PID 已被其他进程占用或身份无法验证；拒绝发送信号，保留记录。'; return 1
    fi
    rm -- "$DATA/codexapp/process"; return $?
  fi
  kill "$pid" || return
  for ((i=0;i<40;i++)); do
    if ! pid_matches "$pid" "$exe" "$birth"; then rm -- "$DATA/codexapp/process"; return $?; fi
    sleep 0.25
  done
  # Check identity immediately before escalation; never kill a recycled PID.
  pid_matches "$pid" "$exe" "$birth" && kill -KILL "$pid" || return
  for ((i=0;i<20;i++)); do
    if ! pid_matches "$pid" "$exe" "$birth"; then rm -- "$DATA/codexapp/process"; return $?; fi
    sleep 0.1
  done
  fail 'CodexApp 仍未退出，保留组件。'
}

start_branding() {
  local MEDIA_DIR='' file
  . "$DATA/branding/config.sh" || return
  [ -d "$MEDIA_DIR" ] || { fail "media 目录不存在：$MEDIA_DIR"; return 1; }
  for file in "$DATA/branding/assets/"*; do
    # At initial install and restart, reject externally changed files. An image
    # upgrade needs a fresh backup, not blindly restoring an old image's assets.
    if [ -e "$MEDIA_DIR/${file##*/}" ]; then
      cmp -s "$file" "$MEDIA_DIR/${file##*/}" || cmp -s "$DATA/branding/originals/${file##*/}" "$MEDIA_DIR/${file##*/}" || { fail "图标已变化，保留它：$MEDIA_DIR/${file##*/}"; return 1; }
    fi
  done
  for file in "$DATA/branding/assets/"*; do
    root_run cp -- "$file" "$MEDIA_DIR/${file##*/}" && root_run chmod 644 "$MEDIA_DIR/${file##*/}" || return
  done
}

stop_branding() {
  local MEDIA_DIR='' file current original
  . "$DATA/branding/config.sh" || return
  # Preflight all six before restoring any; keep backups if any conflict exists.
  for file in "$DATA/branding/assets/"*; do
    current="$MEDIA_DIR/${file##*/}"; original="$DATA/branding/originals/${file##*/}"
    if [ -e "$current" ]; then
      cmp -s "$file" "$current" || cmp -s "$original" "$current" || { fail "无法恢复已被外部修改的图标：$current；备份保留。"; return 1; }
    elif [ -e "$original" ]; then
      fail "原图标被外部删除：$current；备份保留。"; return 1
    fi
  done
  for file in "$DATA/branding/assets/"*; do
    current="$MEDIA_DIR/${file##*/}"; original="$DATA/branding/originals/${file##*/}"
    if [ -e "$original" ]; then root_run cp -p -- "$original" "$current" || return
    else root_run rm -f -- "$current" || return; fi
  done
}

main() {
  local addon rc=0
  if [ "$ACTION" = startup ]; then
    # A single real entrypoint file guarantees ordering even when the image uses
    # unsorted `find -exec`. Continue other addons, but report aggregate failure.
    for addon in apti branding codexapp; do
      owned "$addon" || continue
      if [ "$(cat "$DATA/$addon/status" 2>/dev/null)" != installed ]; then printf 'addons: 跳过未完成安装 %s\n' "$addon" >&2; rc=1; continue; fi
      CODEX_BACKGROUND_ADDONS_DATA="$DATA" bash "$DATA/runtime.sh" "start-$addon" || rc=1
    done
    return "$rc"
  fi
  case "$ACTION" in start-apti) owned apti && "$DATA/apti/apti" restore; return $?;; start-branding|stop-branding) addon=branding;; start-codexapp|stop-codexapp) addon=codexapp;; *) fail '未知运行命令。'; return 2;; esac
  owned "$addon" || { fail '无组件所有权记录。'; return 1; }
  mkdir "$DATA/$addon/.runtime-lock" 2>/dev/null || { fail "$addon 另一个启动/停止操作尚未结束。"; return 1; }
  LOCK="$DATA/$addon/.runtime-lock"
  "${ACTION//-/_}"
}
main
