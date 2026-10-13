#!/usr/bin/env bash
# One installer, shared runtime, two lifecycle backends. No global errexit dependency.
set -uo pipefail
SOURCE_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd) || exit 1
. "$SOURCE_ROOT/lib/common.sh" || exit 1
. "$SOURCE_ROOT/lib/transaction.sh" || exit 1
CB_ROOT="$HOME/.local/share/codex-background"
CB_STATE="$HOME/.local/state/codex-background"
CB_CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"
CB_SOCKET="$CB_STATE/app-server.sock"
CB_LOG="$CB_STATE/bridge.log"
CB_BACKEND=auto
CB_CODEX=''
CB_NODE_PIN=''
CB_ENTRYPOINT_DIR="${ENTRYPOINTD:-$HOME/entrypoint.d}"
CB_YES=0
CB_RESTART=0
CB_INSTALL_NODE=0
CB_INSTALL_CODEX=0
CB_LINGER=0
CB_TXN_CREATED=0
SETTINGS=()
UNIT="$HOME/.config/systemd/user/codex-background.service"
LAUNCHER="$HOME/.local/bin/codex-background"
usage() {
cat <<'TXT'
统一 Codex 后台部署器 3.0.0
  ./deploy.sh                         显示菜单
  ./deploy.sh plan [options]          只读预览
  ./deploy.sh install [options]       安装，确认后才修改
  ./deploy.sh update [options]        更新并验证；需 --restart 才重启后台
  ./deploy.sh status|doctor           只读状态和 initialize 握手（无模型请求）
  ./deploy.sh start|stop|restart       明确管理后台进程
  ./deploy.sh uninstall [--yes]       恢复受管设置，再停止并移除核心
  ./deploy.sh legacy                  只读列出旧部署和残留 hook
  ./deploy.sh recover [--yes]         回滚未完成安装，恢复旧版本/设置
  ./deploy.sh addon <...>             可选组件，参见 addon help

options:
  --backend auto|systemd|container    auto 优先可用 systemd user manager
  --codex /absolute/codex             复用外部 standalone CLI，不收编或删除
  --node /absolute/node               显式 Node >=18，无效即报错
  --install-node                     缺 Node/npm 时安装私有 Node 附加组件
  --install-codex                    缺 Codex 时安装私有 CLI 附加组件
  --settings /absolute/settings.json 可重复，精确选择 IDE/Remote Machine 配置
  --entrypoint-dir /absolute/path     容器 ENTRYPOINTD（必须与镜像约定一致）
  --linger                           systemd 注销后继续运行（单独记录是否本工具启用）
  --restart                          更新后重启，将中断正在进行的任务
  --yes                              接受已打印计划；默认 EOF/无输入不执行

默认不改审批/沙箱策略，不调用模型，不安装 CodexApp/Conda/图标/apti。
首次安装复用现有 Codex CLI；可显式 --install-codex 安装独立 CLI。
TXT
}
cb_load_installed() {
    [ -f "$CB_ROOT/config.sh" ] || { cb_error '尚未安装。'; return 1; }
    local expected_root=$CB_ROOT expected_state=$CB_STATE
    . "$CB_ROOT/config.sh" || return
    [ "$CB_ROOT" = "$expected_root" ] && [ "$CB_STATE" = "$expected_state" ] || { cb_error '配置中的安装路径与固定受管路径不符。'; return 1; }
    cb_valid_path "$CB_CODEX_HOME" && cb_valid_path "$CB_ENTRYPOINT_DIR"
}
cb_settings_defaults() {
    [ "${#SETTINGS[@]}" -eq 0 ] || return 0
    local p
    if [ -f "$CB_STATE/settings-list" ]; then mapfile -t SETTINGS < "$CB_STATE/settings-list"; return; fi
    for p in "$HOME/.vscode-server/data/Machine/settings.json" "$HOME/.vscode-server-insiders/data/Machine/settings.json" \
      "$HOME/.local/share/code-server/User/settings.json" "$HOME/.config/Code/User/settings.json" \
      "$HOME/.config/Code - OSS/User/settings.json" "$HOME/.config/VSCodium/User/settings.json" \
      "$HOME/.config/Code - Insiders/User/settings.json"; do
        [ ! -d "$(dirname -- "$p")" ] || SETTINGS+=("$p")
    done
}
cb_legacy() {
    local p found=0 target
    for p in "$HOME/apps/codex-daemon-bridge" "$HOME/.local/share/codex-daemon-bridge" \
      "$HOME/.config/systemd/user/codex-app-server.service" \
      "$HOME/.config/systemd/user/codex-extension-hook.timer" \
      "$HOME/entrypoint.d/05-start-codex-daemon.sh"; do
        [ ! -e "$p" ] || { printf 'legacy: %s\n' "$p"; found=1; }
    done
    while IFS= read -r -d '' target; do
        if head -c 4096 -- "$target" | grep -Eq 'codex-daemon-bridge extension hook|vscode-codex-background-deploy extension hook'; then
            printf 'legacy hook: %s (backup=%s)\n' "$target" "$target.real"; found=1
        fi
    done < <(cb_find_bins)
    [ "$found" -eq 0 ] || printf '旧部署迁移见 README.md；不自动删除旧目录、停止旧服务或改写旧备份。\n'
}
cb_choose_runtime() {
    local tool
    for tool in timeout flock sha256sum mktemp; do command -v "$tool" >/dev/null || { cb_error "缺少基础命令：$tool"; return 1; }; done
    cb_valid_path "$CB_CODEX_HOME" || { cb_error 'CODEX_HOME 必须是绝对路径'; return 1; }
    . "$SOURCE_ROOT/payload/bridge/resolve-node.sh" || return
    if [ -f "$CB_ROOT/config.sh" ]; then
        cb_verify_files || return
        local saved_code saved_node saved_backend
        saved_code=$(bash -c '. "$1"; printf "%s" "$CB_CODEX"' bash "$CB_ROOT/config.sh") || return
        saved_node=$(bash -c '. "$1"; printf "%s" "$CB_NODE_PIN"' bash "$CB_ROOT/config.sh") || return
        saved_backend=$(bash -c '. "$1"; printf "%s" "$CB_BACKEND"' bash "$CB_ROOT/config.sh") || return
        [ -n "$CB_CODEX" ] || CB_CODEX=$saved_code
        [ -n "$CB_NODE_PIN" ] || CB_NODE_PIN=$saved_node
        [ "$CB_BACKEND" != auto ] || CB_BACKEND=$saved_backend
    fi
    if [ -z "$CB_CODEX" ]; then
        CB_CODEX="$HOME/.codex/packages/standalone/current/codex"
        if [ ! -x "$CB_CODEX" ] && [ -x "$HOME/.local/share/codex-background-addons/codex-cli/bin/codex" ]; then
            CB_CODEX="$HOME/.local/share/codex-background-addons/codex-cli/bin/codex"
        fi
    fi
    if [ ! -x "$CB_CODEX" ]; then
        [ "$CB_INSTALL_CODEX" = 1 ] || { cb_error "未找到 Codex：$CB_CODEX；用 --codex 或 --install-codex。"; return 1; }
        CB_CODEX="$HOME/.local/share/codex-background-addons/codex-cli/bin/codex"
    fi
    cb_valid_path "$CB_CODEX" || return 1
    local help
    if [ -x "$CB_CODEX" ]; then
        timeout 8 "$CB_CODEX" --version || return
        help=$(timeout 8 "$CB_CODEX" app-server --help) || return
        [[ $help == *unix://* ]] || { cb_error '此 Codex 不支持 Unix socket app-server。'; return 1; }
    fi
    if [ -n "$CB_NODE_PIN" ]; then
        CODEX_NODE="$CB_NODE_PIN" cb_resolve_node >/dev/null || return
    else
        if [ -x "$HOME/.local/share/codex-background-addons/node/runtime/bin/node" ]; then
            CB_NODE_PIN="$HOME/.local/share/codex-background-addons/node/runtime/bin/node"
        fi
        CB_NODE_PIN=$(cb_resolve_node) || {
            [ "$CB_INSTALL_NODE" = 1 ] || { cb_error '需要 Node >=18；可传 --node 或 --install-node。'; return 1; }
            CB_NODE_PIN="$HOME/.local/share/codex-background-addons/node/runtime/bin/node"
        }
    fi
    if [ "$CB_INSTALL_NODE" = 1 ] && [ ! -x "$(dirname -- "$CB_NODE_PIN")/npm" ] && ! command -v npm >/dev/null; then
        CB_NODE_PIN="$HOME/.local/share/codex-background-addons/node/runtime/bin/node"
    fi
    if [ "$CB_BACKEND" = auto ]; then
        if command -v systemctl >/dev/null && systemctl --user show-environment >/dev/null 2>&1; then CB_BACKEND=systemd
        elif [ -f /.dockerenv ] && [ -r /usr/bin/entrypoint.sh ] && grep -Fq ENTRYPOINTD /usr/bin/entrypoint.sh; then CB_BACKEND=container
        else cb_error '无法自动识别后台管理方式；指定 --backend 并检查启动契约。'; return 1; fi
    fi
    case "$CB_BACKEND" in
        systemd) systemctl --user show-environment >/dev/null || return ;;
        container) command -v setsid >/dev/null || return; cb_valid_path "$CB_ENTRYPOINT_DIR" || return ;;
        *) cb_error '无效 backend'; return 1 ;;
    esac
    if [ "$CB_LINGER" = 1 ] && [ "$CB_BACKEND" != systemd ]; then cb_error '--linger 仅适用于 systemd'; return 1; fi
    cb_settings_defaults
    [ "${#SETTINGS[@]}" -gt 0 ] || { cb_error '没有检测到 IDE 配置目录；用 --settings 明确指定目标。'; return 1; }
    local p
    for p in "${SETTINGS[@]}"; do cb_valid_path "$p" || { cb_error "需要绝对配置路径：$p"; return 1; }; done
}
cb_plan() {
    cb_choose_runtime || return
    printf '\n后台：%s\nCodex：%s\nNode：%s\n运行目录：%s\nsocket：%s\n' "$CB_BACKEND" "$CB_CODEX" "$CB_NODE_PIN" "$CB_ROOT" "$CB_SOCKET"
    printf '修改 chatgpt.cliExecutable → %s/codex：\n' "$CB_ROOT"
    printf '  %s\n' "${SETTINGS[@]}"
    if [ "$CB_BACKEND" = container ]; then printf '容器启动 hook：%s/20-codex-background.sh\n须由镜像执行 ENTRYPOINTD，HOME 必须持久化。\n' "$CB_ENTRYPOINT_DIR"; fi
    [ "$CB_INSTALL_NODE" != 1 ] || printf '允许缺少依赖时安装独立 Node 附加组件。\n'
    [ "$CB_INSTALL_CODEX" != 1 ] || printf '允许缺少依赖时安装独立 Codex CLI 附加组件。\n'
    [ "$CB_LINGER" != 1 ] || printf '允许启用当前用户 linger；只在原先未启用时记录所有权。\n'
    [ "$CB_RESTART" != 1 ] || printf '本次会停止并重启后台；正在运行的任务会中断。\n'
}
cb_write_config() {
    local destination=$1
    {
        local v
        for v in CB_ROOT CB_STATE CB_CODEX_HOME CB_SOCKET CB_LOG CB_BACKEND CB_CODEX CB_NODE_PIN CB_ENTRYPOINT_DIR; do printf '%s=%q\n' "$v" "${!v}"; done
    } | cb_atomic_text "$destination" 600
}
cb_record_file() {
    local p=$1 key
    key=$(printf '%s' "$p" | sha256sum | cut -d ' ' -f 1) || return
    printf '%s\n%s\n' "$p" "$(cb_hash "$p")" | cb_atomic_text "$CB_STATE/files/$key" 600
}
cb_verify_files() {
    local record p hash actual
    for record in "$CB_STATE/files/"*; do
        [ -f "$record" ] || continue
        { IFS= read -r p; IFS= read -r hash; } < "$record"
        [ -f "$p" ] || { cb_error "受管文件丢失：$p；先恢复或重新安装。"; return 1; }
        actual=$(cb_hash "$p") || return
        [ "$hash" = "$actual" ] || { cb_error "受管文件已被外部修改，保留：$p"; return 1; }
        case "$p" in
            "$CB_ROOT/codex"|"$CB_ROOT/payload/control.sh"|"$CB_ROOT/payload/supervise.sh"|"$LAUNCHER"|*/20-codex-background.sh)
                [ -x "$p" ] || { cb_error "受管入口失去可执行权限：$p"; return 1; };;
        esac
    done
}
cb_write_adapters() {
    mkdir -p -- "$(dirname -- "$LAUNCHER")" || return
    { printf '#!/bin/bash\nexec /bin/bash %q "$@"\n' "$CB_ROOT/payload/control.sh"; } | cb_atomic_text "$LAUNCHER" 755 || return
    cb_record_file "$LAUNCHER" || return
    if [ "$CB_BACKEND" = systemd ]; then
        # systemd's quoting differs from shell quoting. Reject ambiguous paths.
        [[ $CB_ROOT != *'%'* && $CB_ROOT != *'"'* && $CB_ROOT != *'\'* && $CB_CODEX != *'%'* && $CB_CODEX != *'"'* && $CB_CODEX != *'\'* && $CB_CODEX_HOME != *'%'* && $CB_CODEX_HOME != *'"'* && $CB_CODEX_HOME != *'\'* ]] || { cb_error 'systemd 路径不能包含 %、双引号或反斜线'; return 1; }
        {
            printf '[Unit]\nDescription=Codex background runtime (managed)\n\n[Service]\nType=simple\n'
            printf 'Environment="CODEX_HOME=%s"\n' "$CB_CODEX_HOME"
            printf 'ExecStart="%s" app-server --listen "unix://%s"\n' "$CB_CODEX" "$CB_SOCKET"
            printf 'Restart=on-failure\nRestartSec=2\nTimeoutStopSec=20\nUMask=0077\n\n[Install]\nWantedBy=default.target\n'
        } | cb_atomic_text "$UNIT" 644 || return
        cb_record_file "$UNIT" || return
        systemctl --user daemon-reload || return
        systemctl --user enable codex-background.service || return
    else
        {
            printf '#!/bin/bash\n# codex-background managed startup hook\n'
            printf '/bin/bash %q start || { printf "Codex background startup failed; see runtime logs\\n" >&2; exit 0; }\n' "$CB_ROOT/payload/control.sh"
        } | cb_atomic_text "$CB_ENTRYPOINT_DIR/20-codex-background.sh" 755 || return
        cb_record_file "$CB_ENTRYPOINT_DIR/20-codex-background.sh" || return
    fi
}
cb_legacy_conflicts() {
    local target
    if [ -S "$CB_CODEX_HOME/app-server-control/app-server-control.sock" ]; then
        cb_error '该 CODEX_HOME 已有默认 daemon socket；先确认并停用旧运行时，避免会话争用。'; return 1
    fi
    if [ -f "$HOME/.config/systemd/user/codex-app-server.service" ] || [ -f "$HOME/entrypoint.d/05-start-codex-daemon.sh" ]; then
        cb_error '发现旧后台启动器；请先按 README 迁移步骤停用旧启动器，避免同一 CODEX_HOME 两个运行时争用。'; return 1
    fi
    while IFS= read -r -d '' target; do
        if head -c 4096 -- "$target" | grep -Eq 'codex-daemon-bridge extension hook|vscode-codex-background-deploy extension hook'; then
            cb_error "旧扩展 hook 尚未恢复：$target；先恢复 codex.real。"; return 1
        fi
    done < <(cb_find_bins)
    return 0
}
cb_install() {
    [ ! -d "$CB_STATE/transaction" ] || { cb_error '存在未完成事务；先执行 recover。'; return 1; }
    cb_plan || return
    cb_legacy_conflicts || return
    local upgrade=0 p stage oldroot record node npm_bin current
    if [ -d "$CB_ROOT" ]; then
        [ -f "$CB_ROOT/managed-v3" ] || { cb_error '目标目录已有非本工具内容。'; return 1; }
        upgrade=1
        [ "$CB_RESTART" = 1 ] || { cb_error '更新需要显式 --restart；请先结束后台任务。'; return 1; }
        cb_verify_files || return
        # Backends/identity are immutable during updates; uninstall first to change them.
        current=$(bash -c '. "$1"; printf "%s|%s|%s|%s" "$CB_BACKEND" "$CB_CODEX_HOME" "$CB_ENTRYPOINT_DIR" "$CB_CODEX"' bash "$CB_ROOT/config.sh") || return
        [ "$current" = "$CB_BACKEND|$CB_CODEX_HOME|$CB_ENTRYPOINT_DIR|$CB_CODEX" ] || { cb_error '更新不能更换 backend/CODEX_HOME/entrypoint/Codex 路径；先卸载核心。'; return 1; }
    else
        for p in "$LAUNCHER" "$UNIT" "$CB_ENTRYPOINT_DIR/20-codex-background.sh"; do
            [ ! -e "$p" ] || { cb_error "存在非本工具资源，拒绝覆盖：$p"; return 1; }
        done
        [ ! -S "$CB_SOCKET" ] || { cb_error '目标 socket 已存在；拒绝接管未知进程。'; return 1; }
    fi
    cb_confirm || { cb_info '已取消；未安装。'; return 0; }
    cb_lock || return
    umask 077
    if [ ! -x "$CB_CODEX" ]; then
        [ "$CB_INSTALL_CODEX" = 1 ] || return 1
        bash "$SOURCE_ROOT/addons/deploy.sh" install codex-cli --yes || return
        local help
        help=$(timeout 8 "$CB_CODEX" app-server --help) || return
        [[ $help == *unix://* ]] || { cb_error '新 CLI 不支持 Unix socket。'; return 1; }
    fi
    if [ ! -x "$CB_NODE_PIN" ]; then
        [ "$CB_INSTALL_NODE" = 1 ] || return 1
        bash "$SOURCE_ROOT/addons/deploy.sh" install node --yes || return
    fi
    node=$CB_NODE_PIN
    CODEX_NODE="$node" cb_resolve_node >/dev/null || return
    mkdir -p -- "$(dirname -- "$CB_ROOT")" || return
    stage=$(mktemp -d "${CB_ROOT}.stage.XXXXXX") || return
    # Persist the transaction before touching integration or runtime.
    mkdir -p "$CB_STATE/transaction" || return
    CB_TXN_CREATED=1
    printf '%s\n' "$stage" > "$CB_STATE/transaction/stage" || return
    printf '%s\n' "$upgrade" > "$CB_STATE/transaction/upgrade" || return
    if [ -d "$CB_STATE/files" ]; then cp -a "$CB_STATE/files" "$CB_STATE/transaction/files" || return; fi
    mkdir -p "$CB_STATE/transaction/adapters" "$CB_STATE/transaction/new-settings" || return
    for p in "$LAUNCHER" "$UNIT" "$CB_ENTRYPOINT_DIR/20-codex-background.sh"; do
        record=$(printf '%s' "$p" | sha256sum | cut -d ' ' -f 1)
        printf '%s\n' "$p" > "$CB_STATE/transaction/adapters/$record.path" || return
        if [ -f "$p" ]; then cp -p -- "$p" "$CB_STATE/transaction/adapters/$record.original" || return; fi
    done
    cp -a "$SOURCE_ROOT/payload" "$SOURCE_ROOT/lib" "$stage/" || { rm -rf -- "$stage"; return 1; }
    # Archives and ACL-bearing workspaces may not preserve executable bits.
    chmod 755 "$stage/payload/control.sh" "$stage/payload/supervise.sh" "$stage/payload/bridge/wrapper.sh" || return
    cb_write_config "$stage/config.sh" || return
    printf '3.0.0\n' > "$stage/managed-v3" || return
    printf '#!/bin/bash\nexec /bin/bash %q "$@"\n' "$CB_ROOT/payload/bridge/wrapper.sh" | cb_atomic_text "$stage/codex" 755 || return
    if [ -d "$CB_ROOT/payload/bridge/node_modules" ]; then
        cp -a "$CB_ROOT/payload/bridge/node_modules" "$stage/payload/bridge/" || return
    fi
    npm_bin="$(dirname -- "$node")/npm"
    [ -x "$npm_bin" ] || npm_bin=$(command -v npm || true)
    if [ ! -x "$npm_bin" ]; then cb_error '安装私有 ws 依赖需要 npm；使用 --install-node 或提供带 npm 的 Node。'; return 1; fi
    PATH="$(dirname -- "$node"):$PATH" "$npm_bin" install --prefix "$stage/payload/bridge" --ignore-scripts --omit=dev --no-audit --no-fund || return
    "$node" --check "$stage/payload/bridge/bridge.mjs" || return
    # Validate config syntax before selecting the prepared payload.
    bash -n "$stage/config.sh" "$stage/codex" || return
    if [ "$upgrade" = 1 ]; then
        "$CB_ROOT/payload/control.sh" stop || return
        oldroot="${CB_ROOT}.previous"
        [ ! -e "$oldroot" ] || { cb_error "有待恢复的备份：$oldroot"; return 1; }
        printf '%s\n' "$oldroot" > "$CB_STATE/transaction/previous" || return
        mv -- "$CB_ROOT" "$oldroot" || return
    fi
    printf 'selecting\n' > "$CB_STATE/transaction/selecting" || return
    mv -- "$stage" "$CB_ROOT" || return
    printf '%s\n' "$upgrade" > "$CB_STATE/transaction/selected" || return
    cb_write_adapters || return
    "$CB_ROOT/payload/control.sh" start || return
    # Hook up IDEs only after the real app-server initialize handshake succeeds.
    mkdir -p "$CB_STATE/settings" || return
    for p in "${SETTINGS[@]}"; do
        record=$(printf '%s' "$p" | sha256sum | cut -d ' ' -f 1)
        if [ ! -f "$CB_STATE/settings/$record.json" ]; then printf '%s\n' "$p" > "$CB_STATE/transaction/new-settings/$record.path" || return; fi
        printf '%s\n' "$p" | cb_atomic_text "$CB_STATE/settings/$record.path" 600 || return
        "$node" "$CB_ROOT/payload/settings.mjs" set "$p" "$CB_STATE/settings/$record.json" "$CB_ROOT/codex" || return
    done
    printf '%s\n' "${SETTINGS[@]}" | cb_atomic_text "$CB_STATE/settings-list" 600 || return
    if [ "$CB_LINGER" = 1 ]; then
        current=$(loginctl show-user "$(id -un)" -p Linger --value) || return
        if [ "$current" != yes ]; then
            printf 'enabling\n' > "$CB_STATE/transaction/linger-new" || return
            sudo loginctl enable-linger "$(id -un)" || return
            [ "$(loginctl show-user "$(id -un)" -p Linger --value)" = yes ] || return 1
            printf 'enabled\n' > "$CB_STATE/linger-owned" || return
        fi
    fi
    # Full hashes make wrapper/doctor/config updates visible; ownership != health.
    while IFS= read -r -d '' p; do cb_record_file "$p" || return; done < <(find "$CB_ROOT" -type f ! -path '*/node_modules/*' -print0)
    rm -rf -- "$CB_STATE/transaction" || return
    [ "$upgrade" = 0 ] || rm -rf -- "${CB_ROOT}.previous" || return
    cb_info '安装完成。请 Reload Window；doctor 仅验证控制面，不代表模型任务已验证。'
}
cb_uninstall() {
    [ ! -d "$CB_STATE/transaction" ] || { cb_error '存在未完成事务；先执行 recover。'; return 1; }
    cb_load_installed || return
    cb_verify_files || return
    local addon_config="$HOME/.local/share/codex-background-addons/codexapp/config.sh"
    if [ -f "$addon_config" ] && grep -Fq "$CB_ROOT/codex" "$addon_config"; then
        cb_error 'CodexApp 附加组件仍依赖核心；先 addon uninstall codexapp。'; return 1
    fi
    printf '恢复受管 IDE 设置，停止后台（当前任务会中断），删除核心、启动器和日志。\n保留 Codex 用户数据、外部 CLI/Node、可选附加组件。\n'
    cb_confirm || { cb_info '已取消。'; return 0; }
    cb_lock || return
    local node record p
    . "$CB_ROOT/payload/bridge/resolve-node.sh" || return
    node=$(cb_resolve_node) || return
    # A conflict blocks dependency removal. Successfully restored settings stay restored.
    for record in "$CB_STATE/settings/"*.path; do
        [ -f "$record" ] || continue
        IFS= read -r p < "$record"
        if [ -f "${record%.path}.json" ]; then
            "$node" "$CB_ROOT/payload/settings.mjs" restore "$p" "${record%.path}.json" || return
        fi
        rm -f -- "$record" || return
    done
    "$CB_ROOT/payload/control.sh" stop || return
    if [ "$CB_BACKEND" = systemd ]; then systemctl --user disable codex-background.service || return; fi
    if [ -f "$CB_STATE/linger-owned" ]; then sudo loginctl disable-linger "$(id -un)" || return; fi
    # Never use the inventory to recursively remove unknown directories.
    rm -f -- "$LAUNCHER" || return
    if [ "$CB_BACKEND" = systemd ]; then
        rm -f -- "$UNIT" || return
        systemctl --user daemon-reload || return
    else rm -f -- "$CB_ENTRYPOINT_DIR/20-codex-background.sh" || return; fi
    rm -rf -- "$CB_ROOT" || return
    rm -rf -- "$CB_STATE" || return
    cb_info '核心已卸载；可选组件用 addon uninstall 单独管理。'
}
cb_status() {
    cb_load_installed || return
    cb_verify_files || return
    [ ! -d "$CB_STATE/transaction" ] || { cb_error '存在未完成事务；请查看 transaction 及 README 恢复流程。'; return 1; }
    "$CB_ROOT/payload/control.sh" doctor
}
main() {
    local cmd=${1:-menu}; [ "$#" = 0 ] || shift
    if [ "$cmd" = addon ]; then exec bash "$SOURCE_ROOT/addons/deploy.sh" "$@"; fi
    while [ "$#" -gt 0 ]; do
        case "$1" in
            --yes) CB_YES=1;; --restart) CB_RESTART=1;; --install-node) CB_INSTALL_NODE=1;; --install-codex) CB_INSTALL_CODEX=1;; --linger) CB_LINGER=1;;
            --backend|--codex|--node|--settings|--entrypoint-dir)
                [ "$#" -gt 1 ] || { cb_error "缺少参数：$1"; return 2; }
                case "$1" in --backend) CB_BACKEND=$2;; --codex) CB_CODEX=$2;; --node) CB_NODE_PIN=$2;; --settings) SETTINGS+=("$2");; --entrypoint-dir) CB_ENTRYPOINT_DIR=$2;; esac
                shift;;
            *) cb_error "未知参数：$1"; return 2;;
        esac
        shift
    done
    case "$cmd" in
        help|-h|--help) usage;;
        menu)
            [ -t 0 ] || { usage; return 0; }
            printf '1) 安装/更新  2) 诊断  3) 卸载  4) 帮助\n选择 [4]: ' >&2
            local choice; IFS= read -r choice || return 0
            case "$choice" in 1) cb_install;; 2) cb_status;; 3) cb_uninstall;; *) usage;; esac;;
        plan) cb_plan;;
        legacy) cb_legacy;;
        install|update) cb_install;;
        uninstall) cb_uninstall;;
        recover) cb_recover;;
        status|doctor) cb_status;;
        start|stop|restart)
            cb_load_installed || return
            if [ "$cmd" != start ]; then printf '停止/重启会中断正在运行的任务。\n'; cb_confirm || return 0; fi
            "$CB_ROOT/payload/control.sh" "$cmd";;
        *) cb_error "未知命令：$cmd"; return 2;;
    esac
}
if [[ ${BASH_SOURCE[0]} = "$0" ]]; then
    main "$@"
    rc=$?
    if [ "$rc" -ne 0 ] && [ "$CB_TXN_CREATED" = 1 ] && [ -d "$CB_STATE/transaction" ]; then
        cb_error "安装未完成（rc=$rc）。尝试回滚本次事务。" || true
        if [[ ${CB_LOCK_FD:-} =~ ^[0-9]+$ ]]; then exec {CB_LOCK_FD}>&-; unset CB_LOCK_FD; fi
        CB_YES=1 cb_recover || cb_error "自动回滚未完成；保留恢复材料：$CB_STATE/transaction。解决冲突后执行 recover。" || true
    fi
    exit "$rc"
fi
