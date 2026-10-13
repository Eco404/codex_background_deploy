# 可选附加组件

这些组件与 Codex 后台核心分开管理；安装一个组件不会安装其他组件。不会修改 Codex 的认证、approval、sandbox、memories 配置，也不会改动既有 NVM、Conda、npm/pip/apt 镜像或 shell 初始化配置。

```bash
./addons/deploy.sh plan node
./addons/deploy.sh install node
./addons/deploy.sh status node
./addons/deploy.sh uninstall node
```

支持 `apti`、`node`、`miniconda`、`codex-cli`、`codexapp`、`branding`。安装、卸载先展示计划，再要求输入 `yes`；EOF 会取消。自动执行可明确传 `--yes`。`plan`、`status` 只读。当前不提供原位升级；已安装组件再次安装会明确提示，升级需先卸载该组件再安装指定版本。Node 如被核心或 CodexApp 使用，会拒绝卸载；Codex CLI 如被核心引用，同样拒绝卸载。

| 组件 | 依赖与行为 |
|---|---|
| node | curl、tar、gzip、sha256sum；下载官方固定版本 `v22.20.0` 的 tar.gz，核对同版本 SHASUMS256.txt；不需要 xz、NVM 或 sudo。可用 `CODEX_ADDON_NODE_VERSION=v22.x.y` 指定实际版本。 |
| miniconda | curl、Bash 及官方安装器的系统要求；安装官方 latest 到专属前缀，不执行 conda init。上游 latest 未固定；不宣称可重现。 |
| codex-cli | curl、sh 及官方安装器的系统要求；使用 `https://chatgpt.com/codex/install.sh` 安装固定 Codex `0.153.4`，可用 `CODEX_ADDON_CODEX_VERSION` 指定固定版本。安装器脚本 URL 本身未固定摘要，归档校验由官方安装器执行；不宣称安装器供应链已锁定。 |
| apti | apt-get、dpkg-query；安装/恢复系统包需要 root 或 `sudo -n`。只接受标准包名，不接受选项或版本约束。不修改 apt 软件源。安装组件本身不会安装包。 |
| codexapp | 已存在 Node >= 20、配套 npm、核心 wrapper；固定 CodexApp `0.1.90`，可用 `CODEX_ADDON_CODEXAPP_VERSION` 指定版本。不会自动安装 Node。 |
| branding | 当前 code-server media 目录和写入权限；先保存六个图标，再替换。不属于 Codex 后台运行必要条件。 |

专属目录默认为 `~/.local/share/codex-background-addons/`；状态/保留清单在 `~/.config/codex-background-addons/`。组件只删除有自身 ownership 标记的专属目录，外部依赖不被接管。安装失败会返回非零；已写入目标的部分保留为 `installing`，可检查后卸载清理。组件目录是安装器专属，不应在其中存放个人文件或 Conda 工作环境（用 `conda create --prefix /你的工作目录/...`）。

Node 使用绝对路径；需要临时放进当前命令的 PATH 时：

```bash
/usr/bin/env PATH="$HOME/.local/share/codex-background-addons/node/runtime/bin:$PATH" node --version
```

Miniconda 入口为 `~/.local/share/codex-background-addons/miniconda/runtime/bin/conda`。它直接在最终前缀安装，避免移动环境导致嵌入路径失效。

Codex CLI 入口为 `~/.local/share/codex-background-addons/codex-cli/bin/codex`。安装器仅在子进程中使用组件内的私有 `HOME`、`CODEX_HOME`、`CODEX_INSTALL_DIR`；原有 `~/.codex`、全局命令符号链接和 shell profile 不被修改。官方安装器的非交互模式拒绝卸载已有 npm/Bun/Homebrew Codex，也不启动交互会话；即使因检测到外部 npm 版本而写 profile，也只会写私有 installer-home。安装直接使用最终目录，不搬迁包含绝对符号链接的安装树。正常运行该入口时使用调用者的真实 HOME/CODEX_HOME；私有安装 HOME 不会附带到核心运行环境。卸载不删除真实用户会话或认证数据。

CodexApp 安装示例（先用编辑器创建仅当前用户可读的密码文件）：

```bash
CODEXAPP_PASSWORD_FILE="$HOME/.config/codexapp-password" \
  ./addons/deploy.sh install codexapp
```

可用 `CODEXAPP_PORT` 改默认端口 8214；`CODEX_BACKGROUND_WRAPPER` 改核心入口；`CODEX_ADDON_NODE_BIN`、`CODEX_ADDON_NPM_BIN` 指定已有 Node/npm。密码复制进权限为 700 的组件目录，文件权限 600；CodexApp 的密码选项会进入进程参数，服务器管理员仍可读取。安装会启动本地 CodexApp，并检查进程身份、端口占用和 HTTP 就绪；关闭时核对 PID、命令行和进程启动时间，防止误杀复用的 PID。日志在 `codexapp/output.log`，卸载组件会移除它和组件密码副本；输入密码文件由用户管理。

容器内 `apti`、`branding`、`codexapp` 共用一个**真实文件** `codex-background-addons.sh` 启动钩子。其目录按 `CODEX_BACKGROUND_ENTRYPOINT_DIR`、`ENTRYPOINTD`、`~/entrypoint.d` 的顺序确定。官方 code-server entrypoint 的 `find -type f -exec` 不保证文件名排序，也不会运行符号链接；因此钩子在内部固定按 apti → branding → CodexApp 执行，失败保留日志并汇总返回非零。它不替换官方 entrypoint；CodexApp 启动前会调用核心 `payload/control.sh start`，确保后台就绪，不依赖核心 hook 与附加 hook 的执行先后。自定义 wrapper 没有对应 control 时，其服务生命周期由调用者负责。

**自启动依赖外部容器契约：** 官方 entrypoint 必须执行该钩子目录，HOME/数据目录/状态目录/钩子目录必须正确持久化。脚本不能从容器内部保证宿主机挂载正确。没有容器钩子执行器的普通 Linux 可手动执行 `~/.local/share/codex-background-addons/startup.sh`；附加组件不安装 systemd 服务。

卸载 apti 不卸载系统包；非空包清单保存为 `~/.config/codex-background-addons/apti-packages-retained.txt`。branding 卸载只有在目标仍等于本工具写入值或原值时恢复；外部变化、容器镜像升级、恢复权限失败均保留备份并报错。最后一个需要启动的组件卸载后移除未被改动的钩子；最后一个组件卸载后移除共享 runtime。外部改动的钩子和未知数据被保留并报错。

测试：`bash addons/tests/test-addons.sh`。测试使用临时数据目录、模拟下载/安装命令、六个临时图标和本地 HTTP fixture，不安装真实软件包，不调用模型或真实 CodexApp 服务。
