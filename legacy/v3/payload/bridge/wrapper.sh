#!/usr/bin/env bash
set -euo pipefail
BRIDGE_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
CB_ROOT=${CB_ROOT:-$(cd -- "$BRIDGE_DIR/../.." && pwd -P)}
if [[ ! -f "$CB_ROOT/config.sh" ]]; then
    printf 'codex-background: missing runtime configuration: %s/config.sh\n' "$CB_ROOT" >&2
    exit 1
fi
# Installer-owned shell configuration, not user/provider content.
source "$CB_ROOT/config.sh"
source "$BRIDGE_DIR/resolve-node.sh"
: "${CB_CODEX:?missing CB_CODEX}"
export CODEX_HOME="${CB_CODEX_HOME:-${CODEX_HOME:-$HOME/.codex}}"

app_server=false
passthrough=false
skip=false
for arg in "$@"; do
    if $skip; then skip=false; continue; fi
    case "$arg" in
        -h|--help|-V|--version) passthrough=true ;;
    esac
    if ! $app_server; then
        case "$arg" in
            -c|--config|--enable|--disable|--code-mode-host) skip=true ;;
            -*) ;;
            app-server) app_server=true ;;
            *) passthrough=true; break ;;
        esac
    else
        case "$arg" in
            daemon|proxy|generate-ts|generate-json-schema|generate-internal-json-schema|help|version) passthrough=true ;;
            -c|--config|--enable|--disable|--code-mode-host) skip=true ;;
        esac
    fi
done
if $app_server && ! $passthrough; then
    : "${CB_SOCKET:?missing CB_SOCKET}"
    export CB_SOCKET CB_LOG="${CB_LOG:-}"
    node_path=$(cb_resolve_node) || exit 1
    exec "$node_path" "$BRIDGE_DIR/bridge.mjs"
fi
exec "$CB_CODEX" "$@"
