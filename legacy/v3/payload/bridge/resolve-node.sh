#!/usr/bin/env bash
# No nvm.sh, interactive shell initialization, npm, or alias resolution.
cb_valid_node() {
    local candidate=$1 major
    [[ "$candidate" = /* && -x "$candidate" && -f "$candidate" ]] || return 1
    major=$(/usr/bin/timeout 5 "$candidate" -p 'Number(process.versions.node.split(".")[0])' 2>/dev/null) || return 1
    [[ "$major" =~ ^[0-9]+$ && "$major" -ge 18 ]]
}

cb_resolve_node() {
    local candidate
    if [[ ${CODEX_NODE+x} ]]; then
        if cb_valid_node "$CODEX_NODE"; then
            printf '%s\n' "$CODEX_NODE"
            return 0
        fi
        printf 'codex-background: CODEX_NODE must be an executable absolute Node.js path (version >= 18).\n' >&2
        return 1
    fi
    if [[ -n ${CB_NODE_PIN:-} ]] && cb_valid_node "$CB_NODE_PIN"; then
        printf '%s\n' "$CB_NODE_PIN"
        return 0
    fi
    candidate=$(type -P node 2>/dev/null) || candidate=''
    if [[ "$candidate" != /* && -n "$candidate" ]]; then candidate="$PWD/$candidate"; fi
    if cb_valid_node "$candidate"; then
        printf '%s\n' "$candidate"
        return 0
    fi
    while IFS= read -r candidate; do
        if cb_valid_node "$candidate"; then
            printf '%s\n' "$candidate"
            return 0
        fi
    done < <(printf '%s\n' "${NVM_DIR:-$HOME/.nvm}"/versions/node/v*/bin/node | sort -Vr)
    printf 'codex-background: Node.js >= 18 is unavailable. Set CODEX_NODE to its absolute path or install the private Node addon.\n' >&2
    return 1
}
