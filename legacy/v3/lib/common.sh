#!/usr/bin/env bash
# Shared by the installer and the installed controller. Never depend on errexit.
cb_info() { printf '[INFO] %s\n' "$*" >&2; }
cb_error() { printf '[ERROR] %s\n' "$*" >&2; return 1; }
cb_hash() { sha256sum -- "$1" | cut -d ' ' -f 1; }
cb_atomic_copy() {
    local src=$1 dst=$2 mode=${3:-600} tmp
    mkdir -p -- "$(dirname -- "$dst")" || return
    tmp=$(mktemp "${dst}.tmp.XXXXXX") || return
    if cp -- "$src" "$tmp" && chmod "$mode" "$tmp" && mv -f -- "$tmp" "$dst"; then return 0; fi
    rm -f -- "$tmp"
    return 1
}
cb_atomic_text() {
    local dst=$1 mode=${2:-600} tmp
    mkdir -p -- "$(dirname -- "$dst")" || return
    tmp=$(mktemp "${dst}.tmp.XXXXXX") || return
    if cat > "$tmp" && chmod "$mode" "$tmp" && mv -f -- "$tmp" "$dst"; then return 0; fi
    rm -f -- "$tmp"
    return 1
}
cb_confirm() {
    [ "${CB_YES:-0}" = 1 ] && return 0
    local answer
    printf '确认执行？[y/N] ' >&2
    IFS= read -r answer || return 1
    [[ $answer = y || $answer = Y || $answer = yes ]]
}
cb_valid_path() { [[ $1 = /* && $1 != *$'\n'* && $1 != *$'\r'* && $1 != *$'\t'* ]]; }
cb_lock() {
    mkdir -p -- "$CB_STATE" || return
    chmod 700 "$CB_STATE" || return
    exec {CB_LOCK_FD}>"$CB_STATE/lock" || return
    flock -n "$CB_LOCK_FD" || { cb_error '另一个安装、卸载或扩展扫描正在运行。'; return 1; }
    export CB_LOCK_FD
}
cb_roots() {
    printf '%s\n' "$HOME/.vscode/extensions" "$HOME/.vscode-insiders/extensions" \
      "$HOME/.vscode-server/extensions" "$HOME/.vscode-server-insiders/extensions" \
      "$HOME/.local/share/code-server/extensions" \
      "$HOME/.var/app/com.visualstudio.code/data/vscode/extensions" \
      "$HOME/.var/app/com.vscodium.codium/data/codium/extensions"
    if [ -f "$CB_STATE/extension-roots" ]; then cat "$CB_STATE/extension-roots"; fi
}
cb_find_bins() {
    local root ext
    while IFS= read -r root; do
        [ -d "$root" ] || continue
        while IFS= read -r -d '' ext; do
            [ -d "$ext/bin" ] || continue
            find "$ext/bin" -mindepth 2 -maxdepth 2 -name codex -type f -print0
        done < <(find "$root" -mindepth 1 -maxdepth 1 -name 'openai.chatgpt-*' -type d -print0)
    done < <(cb_roots)
}
