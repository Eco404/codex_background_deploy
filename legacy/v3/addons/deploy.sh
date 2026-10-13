#!/usr/bin/env bash
# Optional tools; independent of the background server's installation and auth.
set -uo pipefail
SOURCE_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
USER_HOME="${CODEX_BACKGROUND_ADDONS_HOME:-$HOME}"
DATA="${CODEX_BACKGROUND_ADDONS_DATA:-$USER_HOME/.local/share/codex-background-addons}"
STATE="${CODEX_BACKGROUND_ADDONS_STATE:-$USER_HOME/.config/codex-background-addons}"
HOOK_DIR="${CODEX_BACKGROUND_ENTRYPOINT_DIR:-${ENTRYPOINTD:-$USER_HOME/entrypoint.d}}"
HOOK="$HOOK_DIR/codex-background-addons.sh"
WRAPPER="${CODEX_BACKGROUND_WRAPPER:-$USER_HOME/.local/share/codex-background/codex}"
BIN_DIR="$USER_HOME/.local/bin"
OWNER='codex-background-addons-v1'
STAGE=''
LOCKED=0
SHARED_CREATED=0
fail() { printf 'ERROR: %s\n' "$*" >&2; return 1; }
root_run() { if [ "$(id -u)" = 0 ]; then "$@"; else command -v sudo >/dev/null && sudo -n "$@"; fi; }
owned() { [ -f "$DATA/$1/.owner" ] && [ "$(cat "$DATA/$1/.owner")" = "$OWNER" ]; }
need() { command -v "$1" >/dev/null || fail "缺少 $1；请先自行安装，不会自动修改系统软件包。"; }
cleanup() {
  [ -z "$STAGE" ] || rm -rf -- "$STAGE"
  if [ "$SHARED_CREATED" = 1 ]; then
    local addon any=0
    for addon in apti node miniconda codex-cli codexapp branding; do owned "$addon" && any=1; done
    if [ "$any" = 0 ]; then
      rm -f -- "$DATA/runtime.sh" "$DATA/startup.sh" "$DATA/.owner"
      rmdir -- "$DATA" 2>/dev/null || true
    fi
  fi
  [ "$LOCKED" = 0 ] || rmdir -- "$STATE/.lock" 2>/dev/null || true
  [ "$LOCKED" = 0 ] || rmdir -- "$STATE" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

validate_paths() {
  local p
  for p in "$USER_HOME" "$DATA" "$STATE" "$HOOK_DIR" "$WRAPPER"; do
    [[ "$p" = /* && "$p" != / && "$p" != *$'\n'* ]] || { fail "需要非根目录的绝对路径：$p"; return 1; }
  done
  for p in "$DATA" "$STATE"; do
    case "$p" in "$USER_HOME"/*) :;; *) fail "专属数据/状态目录必须位于用户 HOME 下：$p"; return 1;; esac
    [[ "/$p/" != */../* && "/$p/" != */./* ]] || { fail "拒绝含 . 或 .. 的数据路径：$p"; return 1; }
  done
  [ "$DATA" != "$STATE" ] || { fail '数据和状态目录必须分开。'; return 1; }
  [ ! -L "$DATA" ] && [ ! -L "$STATE" ] || { fail '数据/状态根目录不能是符号链接。'; return 1; }
}

ensure_shared() {
  if [ -e "$DATA/.owner" ]; then
    [ "$(cat "$DATA/.owner")" = "$OWNER" ] || { fail "未知数据目录：$DATA"; return 1; }
  elif [ -d "$DATA" ] && [ -n "$(find "$DATA" -mindepth 1 -maxdepth 1 -print -quit)" ]; then
    fail "拒绝接管非空目录：$DATA"; return 1
  else
    SHARED_CREATED=1
  fi
  mkdir -p -- "$DATA" || return
  chmod 700 "$DATA" || return
  printf '%s\n' "$OWNER" > "$DATA/.owner" || return
  cp -- "$SOURCE_DIR/payload/runtime.sh" "$DATA/runtime.sh" || return
  chmod 700 "$DATA/runtime.sh" || return
  {
    printf '#!/usr/bin/env bash\n'
    printf 'export CODEX_BACKGROUND_ADDONS_DATA=%q\n' "$DATA"
    printf 'exec bash %q startup\n' "$DATA/runtime.sh"
  } > "$DATA/startup.sh" || return
  chmod 700 "$DATA/startup.sh"
}

ensure_hook() {
  if [ -e "$HOOK" ] || [ -L "$HOOK" ]; then
    [ -L "$HOOK" ] && [ "$(readlink "$HOOK")" = "$DATA/startup.sh" ] || { fail "启动钩子被其他程序占用：$HOOK"; return 1; }
  else
    mkdir -p -- "$HOOK_DIR" || return
    # code-server's find -type f does not run symlinks. Use a real tiny dispatcher.
    cp -- "$DATA/startup.sh" "$HOOK" || return
    chmod 700 "$HOOK" || return
  fi
  printf '容器启动契约：官方 entrypoint 必须执行 %s；DATA、STATE 和 hook 必须持久化。\n' "$HOOK"
}

hook_owned() { [ -f "$HOOK" ] && cmp -s -- "$DATA/startup.sh" "$HOOK"; }
ensure_hook_real() {
  if [ -e "$HOOK" ] || [ -L "$HOOK" ]; then
    hook_owned || { fail "启动钩子有外部修改，拒绝覆盖：$HOOK"; return 1; }
    return 0
  fi
  ensure_hook
}

write_setting() { printf '%s=%q\n' "$1" "$2" >> "$STAGE/config.sh"; }
fetch() { need curl && curl -fL --retry 2 --connect-timeout 15 --max-time 600 "$1" -o "$2"; }
architecture() {
  case "$(uname -m)" in x86_64) printf x64;; aarch64|arm64) printf arm64;; *) fail '仅支持 Linux x86_64 / arm64。';; esac
}

install_node() {
  local arch version archive checksum
  need tar && need gzip && need sha256sum || return
  arch="$(architecture)" || return
  version="${CODEX_ADDON_NODE_VERSION:-v22.20.0}"
  [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { fail '无效 Node 版本。'; return 1; }
  archive="node-$version-linux-$arch.tar.gz"
  fetch "https://nodejs.org/dist/$version/$archive" "$STAGE/node.tar.gz" || return
  fetch "https://nodejs.org/dist/$version/SHASUMS256.txt" "$STAGE/checksums" || return
  checksum="$(awk -v name="$archive" '$2 == name { print $1 }' "$STAGE/checksums")"
  [[ "$checksum" =~ ^[a-f0-9]{64}$ ]] || { fail 'Node SHA256 不存在或重复。'; return 1; }
  printf '%s  %s\n' "$checksum" "$STAGE/node.tar.gz" | sha256sum -c - >/dev/null || return
  mkdir "$STAGE/runtime" || return
  tar -xzf "$STAGE/node.tar.gz" --strip-components=1 -C "$STAGE/runtime" || return
  "$STAGE/runtime/bin/node" --version || return
  rm -- "$STAGE/node.tar.gz" "$STAGE/checksums" || return
}

install_miniconda() {
  local arch url
  arch="$(architecture)" || return
  case "$arch" in x64) arch=x86_64;; arm64) arch=aarch64;; esac
  url="https://repo.anaconda.com/miniconda/Miniconda3-latest-Linux-$arch.sh"
  fetch "$url" "$STAGE/installer.sh" || return
  # Conda embeds its prefix: install directly in the final, owned directory.
  mkdir -p "$DATA/miniconda" || return
  printf '%s\n' "$OWNER" > "$DATA/miniconda/.owner" || return
  printf 'installing\n' > "$DATA/miniconda/status" || return
  bash "$STAGE/installer.sh" -b -p "$DATA/miniconda/runtime" || return
  "$DATA/miniconda/runtime/bin/conda" --version || return
  rm -- "$STAGE/installer.sh" || return
}

install_codex_cli() {
  local version="${CODEX_ADDON_CODEX_VERSION:-0.153.4}" prefix="$DATA/codex-cli"
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-alpha(\.[0-9]+){0,2}|-beta(\.[0-9]+)?)?$ ]] || { fail 'Codex CLI 需要固定版本号。'; return 1; }
  fetch 'https://chatgpt.com/codex/install.sh' "$STAGE/installer.sh" || return
  sh -n "$STAGE/installer.sh" || { fail '下载的官方 Codex 安装器语法无效。'; return 1; }
  mkdir -p "$prefix/bin" "$prefix/installer-home" "$prefix/packages-home" || return
  chmod 700 "$prefix" "$prefix/installer-home" "$prefix/packages-home" || return
  printf '%s\n' "$OWNER" > "$prefix/.owner" || return
  printf 'installing\n' > "$prefix/status" || return
  # The official installer can edit a profile even when BIN_DIR is already in
  # PATH, if another npm Codex exists. Confine all installer home/config writes
  # to this owned prefix. Noninteractive mode rejects conflict uninstall/start.
  /usr/bin/env -u BASH_ENV -u ENV -u CODEX_INSTALL_DEFER_SELECTION \
    -u CODEX_INSTALL_IF_LATEST -u CODEX_INSTALL_IF_CURRENT -u CODEX_UPDATE_FROM_RELEASE \
    HOME="$prefix/installer-home" CODEX_HOME="$prefix/packages-home" \
    CODEX_INSTALL_DIR="$prefix/bin" CODEX_NON_INTERACTIVE=1 \
    CODEX_INSTALL_DAEMON_ONLY=0 CODEX_RELEASE="$version" \
    PATH="$prefix/bin:$PATH" sh "$STAGE/installer.sh" --release "$version" || return
  [ -x "$prefix/bin/codex" ] || { fail '官方安装器未生成私有 Codex 入口。'; return 1; }
  local installed_version
  installed_version="$(/usr/bin/env -u BASH_ENV -u ENV HOME="$prefix/installer-home" CODEX_HOME="$prefix/packages-home" "$prefix/bin/codex" --version)" || return
  [ "$installed_version" = "codex-cli $version" ] || { fail "Codex 版本不匹配：$installed_version"; return 1; }
  printf '%s\n' "$version" > "$prefix/version" || return
  rm -- "$STAGE/installer.sh" || return
  printf '私有 Codex 入口：%s/bin/codex\n' "$prefix"
}

resolve_node() {
  NODE_BIN="${CODEX_ADDON_NODE_BIN:-}"
  if [ -z "$NODE_BIN" ] && owned node; then NODE_BIN="$DATA/node/runtime/bin/node"; fi
  [ -n "$NODE_BIN" ] || NODE_BIN="$(command -v node || true)"
  [ -x "$NODE_BIN" ] || { fail 'CodexApp 需要现有 Node/npm；可单独安装 node 附加组件。'; return 1; }
  NODE_BIN="$(readlink -f "$NODE_BIN")" || return
  NPM_BIN="${CODEX_ADDON_NPM_BIN:-$(dirname "$NODE_BIN")/npm}"
  [ -x "$NPM_BIN" ] || { fail "缺少与 Node 匹配的 npm：$NPM_BIN"; return 1; }
  "$NODE_BIN" -e 'if(Number(process.versions.node.split(".")[0])<20)process.exit(1)' || { fail 'CodexApp 需要 Node >= 20。'; return 1; }
}

install_codexapp() {
  local version="${CODEX_ADDON_CODEXAPP_VERSION:-0.1.90}" password_file="${CODEXAPP_PASSWORD_FILE:-}"
  resolve_node || return
  [ -x "$WRAPPER" ] || { fail "请先安装后台核心 wrapper：$WRAPPER"; return 1; }
  [ -n "$password_file" ] && [ -s "$password_file" ] || { fail '请用 CODEXAPP_PASSWORD_FILE 指定非空密码文件。'; return 1; }
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([+-][A-Za-z0-9.-]+)?$ ]] || { fail 'CodexApp 需要固定版本号。'; return 1; }
  local port="${CODEXAPP_PORT:-8214}"
  [[ "$port" =~ ^[0-9]{1,5}$ ]] && (( 10#$port > 0 && 10#$port < 65536 )) || { fail '无效 CodexApp 端口。'; return 1; }
  printf '{"private":true}\n' > "$STAGE/package.json" || return
  PATH="$(dirname "$NODE_BIN"):$PATH" "$NPM_BIN" install --prefix "$STAGE" --no-audit --no-fund --save-exact "codexapp@$version" || return
  [ -x "$STAGE/node_modules/.bin/codexapp" ] || { fail 'CodexApp 安装后缺少可执行程序。'; return 1; }
  cp -- "$password_file" "$STAGE/password" && chmod 600 "$STAGE/password" || return
  write_setting NODE_BIN "$NODE_BIN" && write_setting WRAPPER "$WRAPPER" && write_setting PORT "$port" || return
}

install_apti() {
  need apt-get && need dpkg-query || return
  [ ! -e "$BIN_DIR/apti" ] && [ ! -L "$BIN_DIR/apti" ] || { fail "保留现有 apti：$BIN_DIR/apti；请先迁移旧版。"; return 1; }
  cp -- "$SOURCE_DIR/payload/apti" "$STAGE/apti" && chmod 700 "$STAGE/apti" || return
  : > "$STAGE/packages.txt" || return
}

install_branding() {
  local target="${CODE_SERVER_MEDIA_DIR:-/usr/lib/code-server/src/browser/media}" file
  [[ "$target" = /* ]] && [ -d "$target" ] || { fail "未找到 code-server media：$target"; return 1; }
  cp -a -- "$SOURCE_DIR/resources/vscode" "$STAGE/assets" || return
  mkdir "$STAGE/originals" || return
  for file in "$STAGE/assets/"*; do
    if [ -e "$target/${file##*/}" ]; then cp -p -- "$target/${file##*/}" "$STAGE/originals/${file##*/}" || return; fi
  done
  write_setting MEDIA_DIR "$target"
}

plan() {
  printf '附加组件：%s\n动作：%s\n专属目录：%s/%s\n' "$ADDON" "$ACTION" "$DATA" "$ADDON"
  case "$ADDON" in
    node) printf '独立 Node %s（不接管 NVM，不改 shell/npm 配置）。\n' "${CODEX_ADDON_NODE_VERSION:-v22.20.0}";;
    miniconda) printf '独立 Miniconda（不 conda init，不改镜像或既有环境）。\n';;
    codex-cli) printf '独立 Codex CLI %s；官方安装器在私有 HOME/CODEX_HOME 中运行，不接管已有 npm/Codex。\n' "${CODEX_ADDON_CODEX_VERSION:-0.153.4}";;
    codexapp) printf 'CodexApp %s；使用已有 Node/npm 与核心 wrapper，不写 Codex 认证或策略。\n' "${CODEX_ADDON_CODEXAPP_VERSION:-0.1.90}";;
    apti) printf 'apti 仅记录本次通过它安装的系统包；卸载组件不卸载系统包。\n';;
    branding) printf '保存并替换 code-server 六个图标；外部修改会阻止恢复。\n';;
  esac
  case "$ADDON" in apti|branding|codexapp) printf '唯一启动钩子：%s\nENTRYPOINTD 必须指向该目录且官方 entrypoint 必须执行它；不替换容器 entrypoint。\n' "$HOOK";; esac
}

install_addon() {
  if owned "$ADDON"; then
    [ "$(cat "$DATA/$ADDON/status" 2>/dev/null)" = installed ] && { printf '已安装；更新请先卸载此可选组件，再安装指定版本。\n'; return 0; }
    fail '发现未完成安装；先运行 uninstall 清理后重试。'; return 1
  fi
  [ ! -e "$DATA/$ADDON" ] && [ ! -L "$DATA/$ADDON" ] || { fail "拒绝接管外部目录：$DATA/$ADDON"; return 1; }
  ensure_shared || return
  STAGE="$(mktemp -d "$DATA/.stage-$ADDON.XXXXXX")" || return
  chmod 700 "$STAGE" || return
  printf '%s\n' "$OWNER" > "$STAGE/.owner" || return
  printf 'installing\n' > "$STAGE/status" || return
  : > "$STAGE/config.sh" || return
  "install_${ADDON//-/_}" || return
  if [ "$ADDON" = miniconda ] || [ "$ADDON" = codex-cli ]; then
    cp -- "$STAGE/config.sh" "$DATA/$ADDON/config.sh" || return
    rm -rf -- "$STAGE" || return
  else
    mv -- "$STAGE" "$DATA/$ADDON" || return
  fi
  STAGE=''
  if [ "$ADDON" = apti ]; then
    mkdir -p "$BIN_DIR" || return
    ln -s "$DATA/apti/apti" "$BIN_DIR/apti" || return
  fi
  case "$ADDON" in apti|branding|codexapp) ensure_hook_real || return;; esac
  case "$ADDON" in branding|codexapp)
    CODEX_BACKGROUND_ADDONS_DATA="$DATA" bash "$DATA/runtime.sh" "start-$ADDON" || return;;
  esac
  printf 'installed\n' > "$DATA/$ADDON/status" || return
  printf '已安装 %s。\n' "$ADDON"
}

uninstall_addon() {
  owned "$ADDON" || { fail '没有此组件的所有权记录，拒绝删除外部安装。'; return 1; }
  [ ! -d "$DATA/$ADDON/.runtime-lock" ] && [ ! -d "$DATA/$ADDON/.package-lock" ] || { fail '组件仍在启动、停止或安装系统包；待操作结束后再卸载。'; return 1; }
  if [ "$ADDON" = node ] && [ -f "$USER_HOME/.local/share/codex-background/config.sh" ]; then
    local encoded_node
    printf -v encoded_node '%q' "$DATA/node/runtime/bin/node"
    if grep -Fq "CB_NODE_PIN=$encoded_node" "$USER_HOME/.local/share/codex-background/config.sh"; then
      fail '后台核心仍引用此 Node，请先卸载核心或更换其 Node 路径。'; return 1
    fi
  fi
  if [ "$ADDON" = codex-cli ] && [ -f "$USER_HOME/.local/share/codex-background/config.sh" ]; then
    local encoded_prefix
    printf -v encoded_prefix '%q' "$DATA/codex-cli/"
    if grep -E '^CB_CODEX=' "$USER_HOME/.local/share/codex-background/config.sh" | grep -Fq "$encoded_prefix"; then
      fail '后台核心仍引用此 Codex CLI，请先卸载核心或更换其 CLI 路径。'; return 1
    fi
  fi
  if [ "$ADDON" = node ] && owned codexapp && [ -f "$DATA/codexapp/config.sh" ]; then
    # This file was generated by this installer, never a user-supplied env file.
    local NODE_BIN='' WRAPPER='' PORT=''
    . "$DATA/codexapp/config.sh" || return
    [[ "$NODE_BIN" != "$DATA/node/"* ]] || { fail 'CodexApp 仍依赖此 Node，请先卸载 CodexApp。'; return 1; }
  fi
  case "$ADDON" in
    codexapp|branding) CODEX_BACKGROUND_ADDONS_DATA="$DATA" bash "$DATA/runtime.sh" "stop-$ADDON" || return;;
    apti)
      if [ -L "$BIN_DIR/apti" ] && [ "$(readlink "$BIN_DIR/apti")" = "$DATA/apti/apti" ]; then rm -- "$BIN_DIR/apti" || return
      elif [ -e "$BIN_DIR/apti" ] || [ -L "$BIN_DIR/apti" ]; then fail 'apti 命令已被外部修改，保留组件待人工处理。'; return 1; fi
      if [ -s "$DATA/apti/packages.txt" ]; then
        cp -- "$DATA/apti/packages.txt" "$STATE/apti-packages-retained.txt" || return
        printf '保留系统软件包及安装清单：%s/apti-packages-retained.txt\n' "$STATE"
      fi;;
  esac
  rm -rf -- "$DATA/$ADDON" || return
  local other active=0 any=0
  for other in apti branding codexapp; do owned "$other" && active=1; done
  if [ "$active" = 0 ] && { [ -e "$HOOK" ] || [ -L "$HOOK" ]; }; then
    hook_owned || { fail "组件已移除，但外部修改的 hook 被保留：$HOOK"; return 1; }
    rm -- "$HOOK" || return
  fi
  for other in apti branding codexapp node miniconda codex-cli; do owned "$other" && any=1; done
  if [ "$any" = 0 ]; then
    rm -f -- "$DATA/runtime.sh" "$DATA/startup.sh" "$DATA/.owner" || return
    rmdir -- "$DATA" 2>/dev/null || { fail "保留未知数据：$DATA"; return 1; }
  fi
  printf '已卸载 %s；原有外部依赖及 Codex 用户数据保持不变。\n' "$ADDON"
}

main() {
  ACTION="${1:-}"; ADDON="${2:-}"; local yes="${3:-}" answer
  case "$ACTION" in help|-h|--help|'') printf '用法：addons/deploy.sh <plan|install|uninstall|status> <apti|node|miniconda|codex-cli|codexapp|branding> [--yes]\n'; return 0;; esac
  case "$ACTION" in plan|install|uninstall|status) :;; *) fail '用法：addons/deploy.sh <plan|install|uninstall|status> <apti|node|miniconda|codex-cli|codexapp|branding> [--yes]'; return 2;; esac
  case "$ADDON" in apti|node|miniconda|codex-cli|codexapp|branding) :;; *) fail '未知附加组件。'; return 2;; esac
  [ "$#" -le 3 ] && { [ -z "$yes" ] || [ "$yes" = --yes ]; } || { fail '未知参数。'; return 2; }
  validate_paths || return
  if [ "$ACTION" = status ]; then
    if owned "$ADDON"; then printf '%s: %s\n' "$ADDON" "$(cat "$DATA/$ADDON/status" 2>/dev/null || printf incomplete)"; else printf '%s: unmanaged / absent\n' "$ADDON"; fi
    return 0
  fi
  plan || return
  [ "$ACTION" != plan ] || return 0
  if [ "$yes" != --yes ]; then
    printf '输入 yes 确认：'
    IFS= read -r answer && [ "$answer" = yes ] || { printf '\n已取消。\n'; return 2; }
  fi
  mkdir -p "$STATE" && chmod 700 "$STATE" || return
  mkdir "$STATE/.lock" 2>/dev/null || { fail "另一个操作正在运行（锁：$STATE/.lock）；异常退出后请核实进程再手动移除锁。"; return 1; }
  LOCKED=1
  "${ACTION}_addon"
}
main "$@"
