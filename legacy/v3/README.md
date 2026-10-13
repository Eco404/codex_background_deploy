# Codex Background Deploy 3.0.0

把两套旧工具的后台运行逻辑合并为一个核心：同一份 Node bridge、IDE 配置适配器和维护命令，分别使用 systemd 用户服务或容器监督进程。旧目录保留为审计与迁移参考；新的改动集中在本目录。

## 运行方式

```text
VS Code / Remote SSH / code-server / Code OSS / VSCodium Codex 扩展
  → chatgpt.cliExecutable（精确修改配置项）
  → codex wrapper → 私有 ws bridge → 私有 Unix socket
  → 独立于编辑器生命周期的 Codex app-server
```

关闭客户端只关闭它的 bridge 连接，后台 app-server 由 systemd 或容器监督进程继续管理。关闭容器／主机、显式停止后台、模型错误和需要人工审批的任务属于不同情况。多窗口共享同一 CODEX_HOME 与运行时；这个工具不修改 approval_policy、sandbox_mode、认证、模型或 memories。

`chatgpt.cliExecutable` 是官方标为 development-only 的扩展设置，自定义 CLI 与扩展升级存在兼容性边界；本工具使用它是为了避免改写扩展 bundled binary。官方说明：[IDE settings](https://developers.openai.com/codex/ide/settings/)、[app-server](https://developers.openai.com/codex/app-server/)。配置级别可能被 workspace/profile 设置覆盖；安装后须 Reload Window，并在真实 IDE 验证接入。

## 安装

基础要求：Linux、Bash 4+、GNU coreutils（含 timeout/sha256sum）、util-linux flock；容器模式另需 setsid/procps。运行时需要支持 `app-server --listen unix://PATH` 的 Codex CLI、Node >=18 和私有 `ws@8.21.3`。安装私有 ws 需要 npm；运行时不需要 npm、NVM、Python、jq、socat 或 CodexApp。

首次安装支持复用已有 standalone Codex。工具不接管或删除外部 CLI／Node，不改变 NVM 别名、shell 初始化或系统软件源。缺少 CLI 时可显式 `--install-codex`，安装固定版本到附加组件私有前缀；不会替换全局 codex。

```bash
./deploy.sh                     # 简单数字菜单
./deploy.sh plan                # 检测环境并预览，无安装写入
./deploy.sh install             # 预览后确认；EOF/默认回答取消
# 全新环境，明确允许下载独立依赖：
./deploy.sh install --install-codex --install-node
```

指定依赖和准确 IDE 配置路径：

```bash
./deploy.sh install \
  --codex "$HOME/.codex/packages/standalone/current/codex" \
  --node "$HOME/.nvm/versions/node/v22.23.2/bin/node" \
  --settings "$HOME/.vscode-server/data/Machine/settings.json"
```

`--settings` 可重复；默认仅从已存在的常见 IDE 配置目录中选择。远程机器应修改该远端 Machine 配置，不要把远端路径填进本地机器的 User Settings。Code OSS/VSCodium 还需自行安装兼容的 Codex 扩展，本工具不处理扩展市场可用性。

没有可用 Node/npm 时，可明确加 `--install-node`；它调用私有 Node 附加组件。`CODEX_NODE` 是最高优先级的显式运行时覆盖，无效即报错；否则依次尝试已记录的绝对路径、PATH、NVM 已安装版本目录。不加载 nvm.sh、不依赖 default/lts 别名，每个候选实际执行版本验证。

systemd：

```bash
./deploy.sh install --backend systemd --linger
```

`--linger` 让退出登录后 user manager 继续运行，默认不改变该用户级设置。仅本工具新启用的 linger 会在卸载时关闭；这可能影响后来添加的其他 user services。服务独立命名为 `codex-background.service`，不覆盖旧的同名外部服务。

code-server 容器：

```bash
./deploy.sh install --backend container --entrypoint-dir "$HOME/entrypoint.d"
```

镜像必须执行真实 `ENTRYPOINTD` 中的可执行文件，HOME 与启动钩子需要宿主持久挂载。工具不能从容器内部保证宿主挂载；没有这个契约时仅本次启动有效。保留官方镜像 entrypoint，不依赖其 `find -exec` 顺序。核心只放一个真实钩子；监督器经 setsid 脱离编辑器进程，拥有自己的锁和进程组，重启意外退出的 app-server。

## 维护

```bash
./deploy.sh status
./deploy.sh doctor
./deploy.sh start
./deploy.sh stop
./deploy.sh restart
./deploy.sh update --restart
./deploy.sh recover
./deploy.sh uninstall
```

- `status/doctor` 检查受管文件完整性并进行有总超时的真实 `initialize` 握手，不启动服务、不更改配置、不发模型请求。连接检查会被 app-server 自身记录到其日志。
- `start` 幂等；`stop/restart` 提示任务中断并要求确认。自动化可传 `--yes`。
- `update` 更新本工具 payload 与固定 ws，不更新外部 Codex/Node；当前采用显式 `--restart`，避免后台任务被暗中重启。不使用旧版“marker 存在即 current”的判断。
- `recover` 回滚未完成的安装。正常失败立即尝试回滚；进程被强杀等情况下保留 write-ahead 事务，由该命令恢复。任何配置冲突或文件恢复失败都会返回非零并保留恢复材料。
- 卸载先恢复受管设置，再停止后台和删除依赖。若用户后来改了该设置、受管文件发生外部变化、或 CodexApp 附加组件仍依赖核心，则停止卸载并说明冲突，不删除仍被引用的运行时。

配置备份只含原始目标键与工具写入值，不复制整份 settings/config/auth。JSONC 编辑保留其他配置、注释和特殊字符串。目录中不得放个人数据；`~/.codex`、外部 CLI/Node 及可选组件都不随核心卸载删除。

默认路径：

| 内容 | 路径 |
|---|---|
| 核心、私有 ws | `~/.local/share/codex-background/` |
| 控制器 | `~/.local/bin/codex-background` |
| socket、状态、快照、事务、日志 | `~/.local/state/codex-background/` |
| systemd unit | `~/.config/systemd/user/codex-background.service` |
| 容器 hook | `$ENTRYPOINTD/20-codex-background.sh` |

bridge 日志仅记录连接生命周期、数量及错误代码，约 256 KiB 轮换一次。systemd 后台日志用 journal；容器原生 Codex 输出在 `server.log`，可能包含 Codex 自身诊断信息，应按用户数据管理。日志与运行目录权限仅当前用户可读。

## 可选附加组件

apti、Node、Codex CLI、Miniconda、CodexApp、branding 保留为独立功能，不随核心默认安装。参见 [addons/README.md](addons/README.md)。核心安装失败回滚不会连带卸载这些显式安装的独立组件；可分别卸载。

```bash
./deploy.sh addon plan apti
./deploy.sh addon install apti
./deploy.sh addon install miniconda
./deploy.sh addon uninstall branding
```

apti 使用独立持久清单；卸载不删除实际 apt 软件包。Conda 不 conda init、不接管已有环境。branding 恢复原图时检查目标是否被外部改动，冲突保留备份。

## 旧版迁移

```bash
./deploy.sh legacy
```

迁移涉及停止旧后台并中断任务，不能仅靠改变 wrapper 路径无感完成。新安装检测到旧启动器／旧扩展 hook 会阻止继续，避免同一 CODEX_HOME 被两个 app-server 同时占用。**不要直接运行旧版的全量卸载**，其已知恢复／依赖删除问题见审计报告。

建议在任务结束后按实际 inventory 做一次受控切换：

1. 记录旧后台版本、接入方式和 settings 目标值；保留旧工具、备份与用户数据。
2. systemd 旧方案停止并禁用 `codex-extension-hook.timer`、`codex-extension-hook.service` 和 `codex-app-server.service`；容器旧方案停止其 managed daemon，并把旧 `05-start-codex-daemon.sh` 移到**启动目录外**的备份位置（仅改文件名仍会被 find 执行）。先确认目标确属于旧部署。
3. 对每个旧扩展 hook 核对 marker 与 `codex.real`。备份存在且确为原始程序时原子恢复；缺失备份则重新安装该扩展，不能用 wrapper 充当原始程序。移走旧 systemd unit 文件至备份位置并 daemon-reload。所有这些操作均应在目标机器明确执行，不由本次源码合并自动操作。
4. 在 IDE 设置中恢复旧工具接管前的原值；原值本来不存在的只移除该键，避免新工具卸载时恢复成已停用的旧 bridge。保留其他设置。关闭旧 CodexApp 或稍后按附加组件迁移，防止它继续访问旧 daemon。
5. 运行新安装，检查 doctor，再 Reload Window，在一次新测试任务中关闭界面并重连确认完成。未验证前保留旧目录，失败可恢复旧设置、旧启动器和原始版本。
6. 新方案稳定后，按旧部署精确清单清理旧 bridge/ws/启动脚本；不删除整个 `.codex`、NVM 或 Conda。旧 apti 包清单、branding 原始备份和 CodexApp 密码需单独迁移，禁止覆盖／丢弃原件。

这是显式、可审计的迁移边界。当前版本不自动执行跨旧状态库的破坏性迁移或推断未知设置原值。

## 验证

```bash
bash tests/run-static.sh
# bridge tests 需要现有 ws 8.21.3，可通过 NODE_PATH 指向其 node_modules 父目录
NODE_PATH=/path/to/node_modules bash tests/run-static.sh

# 可选：实际本地 Codex + Node，所有 HOME/socket/配置/进程在临时目录
CB_TEST_CODEX=/absolute/codex CB_TEST_NODE=/absolute/node CB_TEST_WS=/path/to/ws \
  /path/to/venv/bin/python tests/test-integration.py
# 唯一客户端断开后，本地假 Responses 服务返回结果，重连读取 completed turn：
CB_TEST_CODEX=/absolute/codex CB_TEST_NODE=/absolute/node CB_TEST_WS=/path/to/ws \
  /path/to/venv/bin/python tests/test-background-turn.py
```

离线测试覆盖配置编辑与冲突恢复、原子复制失败、非交互取消、Node 路径解析、真实握手超时、bridge 关闭、可选组件所有权等。实际隔离集成验证安装→连接→断开重连→更新→卸载与进程清理。实际 IDE UI、目标 systemd 用户会话、容器重建及第三方 Provider 仍需目标环境验收。

普通 app-server 启动由一个共享后台承接，因此客户端传给 app-server 的启动级参数（包括 `-c`）不能为每个窗口单独重配已运行的服务；共享配置来自 CODEX_HOME。普通 CLI／daemon／schema／help 等管理命令保持原参数直达外部 Codex。
