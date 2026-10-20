# Codex Background 4.0

一个 Go 程序管理 Linux 中的 Codex 后台、IDE 接入、可选开发环境和恢复材料。支持 Linux amd64/arm64，systemd 用户服务及有启动钩子的容器。Go 核心不需要 Node、npm、Python 或 whiptail。

```bash
./install.sh        # 打开状态首页，不会自动安装
./codex-bg inspect
./codex-bg doctor
```

首页使用 Bubble Tea，支持方向键或 j/k、Enter/空格选择、Esc 返回上一级、q 退出；带有 `r 刷新` 提示的菜单可刷新当前页面的环境状态。没有安装记录也会扫描环境，显示旧部署和可复用环境；非交互运行只输出状态。选择明确的扩展目录后才能进入安装确认，未选中的其他主机、用户和 IDE 不受影响。

主菜单按任务分为三个分组和退出：

```text
部署管理
  安装后台 / 迁移现有部署（按检测结果显示其中一项；已安装后隐藏）
  可选组件 → 安装或复用 / 校验 / 卸载 / CodexApp 配置
    微软官方插件源（仅 code-server 容器，默认不选）
  下载设置 → 官方源或国内镜像 / 代理
  更新核心、更新历史、回滚核心、卸载核心（已安装时显示）
运行与维护
  后台服务 → 只读状态 / 启动 / 停止 / 重启
  只读诊断
  修复受管扩展入口
备份与文件
  备份管理 → 详情与路径 / 校验 / 恢复 / 删除
  查看日志
  文件清理 → 日志 / 无引用缓存 / 已结束操作的临时文件
  重新关联维护目录
退出
```

安装和迁移共用环境选择向导，但执行逻辑不同：检测到旧部署时，迁移会先验证旧原件、备份和服务来源，再处理旧服务、入口和已知 CLI 配置覆盖。向导标题和最终预览会明确标为“迁移”，不再同时提供两个相似入口。核心已安装后使用更新、修复等操作，不提供实际不支持的“重新配置”安装入口；CLI 的 `install` 和 `migrate` 仍保留独立命令。

输入和预览阶段可逐级返回；未完成配置的组件不会加入安装计划。修改仍需按 `y` 显式确认，强制操作保留二次确认。完成后返回所属菜单，修改操作会先重新读取安装状态；不会每次都跳回首页。

不可用操作显示原因并禁止执行：尚未部署时不能操作核心服务；已运行、正在启动或等待重试时不能重复启动，已停止或恢复失败时提供明确的启动入口。等待重试时可以停止后续尝试；停止未完成或失败时可再次停止，在确认退出前不能启动或重启。维护中不允许启动、停止或重启。已登记组件不再提供重复安装，部分安装状态只提供可执行的清理操作。CodexApp 要求核心已部署且就绪，Node >=20/npm 兼容；正在被它引用的受管 Node 不能直接删除。apti 检查容器、apt/dpkg 与权限，图标功能检查资源及写入能力。旧部署不会被服务菜单当作新版受管服务操作。发现未完成事务时先恢复，恢复入口位于“运行与维护”。

界面随终端宽度换行，长内容通过 PgUp/PgDn 翻页；输入框支持 Ctrl+U 清空。安装期间显示当前阶段、耗时、下载字节/百分比和子进程输出，记录于 `installer-data/logs/installer.log`。执行时按 Ctrl+C 会请求取消并等待子进程退出及恢复，不会直接跳过清理退出。

## 下载源与代理

首次确认安装、组件安装或需要下载的更新前，向导要求选择网络设置；也可在“部署管理 → 下载设置”修改。本次界面会话内复用已选设置，不写入全局 npm/Conda 配置或后台服务环境。

| 下载项 | 国内镜像预填值 |
|---|---|
| npm（Codex 原生包、CodexApp） | `https://repo.huaweicloud.com/repository/npm` |
| Node 安装包 | `https://repo.huaweicloud.com/nodejs` |
| Miniconda 安装包 | `https://mirrors.ustc.edu.cn/anaconda/miniconda` |

默认优先采用实测可用的华为云源。2026-10-13 检查时，测试到的华为 Miniconda 地址返回 HTML 网页而非安装脚本，因此 Miniconda 采用中科大；可手动改为同样验证过 x64/ARM64 安装包的北外源 `https://mirrors.bfsu.edu.cn/anaconda/miniconda`。这里配置的是安装包镜像，不是 Conda 软件包 channel。下载结果会检查脚本头，网页响应不会交给 shell 执行。

选择镜像后可逐项修改预填地址。Codex 镜像安装读取固定版本的原生架构包并校验 SHA512，核心仍不需要 Node/npm。镜像选择不会替换 IDE 插件库；code-server 的微软官方插件源是独立的可选组件。代理独立选择“不使用”“输入代理地址”或“继承当前环境”，支持 HTTP(S) 和 SOCKS5；预览与日志不会显示代理密码。

```bash
./codex-bg install --extension-root /path/extensions --mirrors --no-proxy
./codex-bg component install node --path managed --node-mirror https://repo.huaweicloud.com/nodejs
./codex-bg component install miniconda --path managed --proxy http://192.168.5.100:7890
```

CLI 默认使用官方源并沿用代理环境；修改仍需确认或显式 `--yes`。这些选项控制安装包下载与安装子进程，不修改日后的 apt、Conda channel 或其他用户软件配置。

## code-server 的微软官方插件源

在安装向导的“可选组件”，或“部署管理 → 可选组件”中选择“微软官方插件源 (marketplace)”。仅在容器内识别到 code-server 的安装目录时显示，默认不选。此选项把该 code-server 的整个插件库从 Open VSX 切换到微软官方 Marketplace，供用户在 IDE 中搜索、安装和更新插件；安装器不再单独下载或导入 Codex 插件，原 `extension install` 命令与 `--extension-source` 参数已移除。

```bash
./codex-bg component install marketplace --dry-run
./codex-bg component install marketplace
./codex-bg component repair marketplace    # 校验；原插件源被镜像重置时重新应用
./codex-bg component uninstall marketplace # 恢复原插件源，或解除复用记录
# 与核心一起选择
./codex-bg install --extension-root "$HOME/.local/share/code-server/extensions" --component marketplace
```

只备份并替换 code-server 自身 `lib/vscode/product.json` 的 `extensionsGallery` 对象，其他产品字段、用户设置和其他 IDE 不变。搜索地址为 `https://marketplace.visualstudio.com/_apis/public/gallery`；详情页使用 `https://marketplace.visualstudio.com/items`，资源使用微软的 `vscode-unpkg.net` 模板。原配置及校验信息登记在 `installer-data/backups`。已使用相同微软源时直接复用，不制造备份；解除引用也不会改动已有源。

**启用或恢复后需要手动重启 code-server。** 安装器不重启当前 IDE；“配置已校验”不代表正在运行的 IDE 已重新加载。若容器设置了 `EXTENSIONS_GALLERY` 覆盖，先在容器启动配置中处理它，否则程序拒绝声称切源成功。安装器的代理选项仅用于安装包下载，不自动配置 IDE 日后的插件网络代理。插件能否安装或更新仍取决于 Marketplace 可用性、插件发布者及 code-server 的兼容性。

在核心使用容器启动钩子、运行目录与维护目录均持久化的前提下，后台启动时会重新检查插件源：镜像重建将其恢复为已备份的原值时可重新应用；code-server 更新后的其他产品字段保留。遇到未知插件源、缺失备份或安装路径变化时停止改写并报告，需人工确认。复用的外部源只检查，不自动接管；源失效后可解除引用，再重新选择启用。仅单独启用本组件而未部署核心时，不创建额外启动钩子，可重建后手动执行 `component repair marketplace`。

卸载此组件仅恢复插件源字段，保留 IDE 已安装的插件和其他产品字段；核心卸载不会隐式删除可选组件，需分别选择卸载。以前由旧版本安装的 Codex 扩展及其历史记录继续保留。原有后台接管功能仍只处理用户明确选择的扩展根目录。

## 工作方式

扩展内置 `codex` 原件先备份，再原子替换成指向本工具的启动脚本。后台 Go bridge 把 JSONL 转换为 Unix WebSocket，连接独立的 Codex app-server。关闭编辑器只结束客户端连接，后台由 systemd 或容器监督进程管理。

所有 IDE 都使用入口接管，不依赖修改 `chatgpt.cliExecutable`。旧配置覆盖会在迁移计划中列出；未知值不会擅自删除。每个入口有独立ID，状态页区分“已配置”与“实际观察到连接”。扩展升级由文件监听与60秒扫描识别，只处理明确选择的根目录；备份不足或维护目录丢失时停止新接管。

代码不改变用户认证、模型、审批策略、sandbox 或 memories。`CODEX_HOME` 默认原用户的 `~/.codex`，该目录不随核心卸载删除。关闭主机或容器会中断正在执行的任务；重建后自启动不等于任务无缝续跑。

## 崩溃后的自动恢复

安装和迁移向导提供默认勾选的“崩溃后自动重启（最多 5 次）”；取消勾选则异常退出后等待手动启动。CLI 默认开启，使用 `--no-auto-restart` 关闭：

```bash
./codex-bg install --extension-root /path/extensions --no-auto-restart
./codex-bg migrate --extension-root /path/extensions --no-auto-restart --dry-run
```

监督器在“预期运行”时观察到 Codex 退出，最多自动重启 5 次，分别等待 2、4、8、16、30 秒。启动失败、启动后很快退出都累计失败；只有通过协议就绪检查、连续健康运行 2 分钟后才恢复额度。进程还活着但一次健康检查超时只报告异常，不直接强杀重启。额度耗尽后显示“重试额度已耗尽”，需要明确执行 `codex-bg start` 或在服务菜单选择“启动”，才会清除失败状态并重新尝试。

运行意图、失败次数和恢复状态保存在受管运行目录，监督器或容器重启不会自动抹去手动停止、维护或额度耗尽状态。容器启动钩子使用内部 `autostart` 入口，遵守已保存状态；它与用户明确执行的 `start` 不同。systemd 的外层进程重启也使用同一份状态，不绕过重试限制。

`stop` 先登记手动停止，再停止进程；升级、回退、迁移、卸载先登记维护意图，由相应操作负责恢复或结束服务。关闭 VSCode、桥接连接断开不会触发 Codex 重启。直接使用 `kill` 无法可靠表达用户意图；在“预期运行”状态下，非预期退出会被视为故障。由本工具以外的程序升级或替换 Codex 前，应先通过 `codex-bg stop` 停止，完成后再 `codex-bg start`。

首页、服务菜单和 `status` / `doctor` 区分正常运行、启动中、等待重试、额度耗尽、异常退出、手动停止和维护中，并显示自动重启开关、已用次数、下次尝试时间及最近退出的信号、版本和运行时长；`inspect --json` 的 `runtime_status` 提供同样信息。时间附带时区偏移。检查缺失或损坏的状态不会创建文件或重置额度。恢复日志仍集中在 `installer-data/logs`。自动恢复只恢复服务可用性，不自动重放请求，也不保证崩溃时的任务继续执行。

启动和清理 socket 时会通过 Linux `/proc` 和 `SOCK_DIAG` 校验子进程归属，兼容 Codex 将入口链接到内部 socket 的形式。校验成功后将监督器、子进程、内核启动标识及 socket 文件身份登记到运行目录的 `run/socket-owner.json`。监督器异常退出或容器重建后，只有确认原进程已退出、入口和目标未被替换、且没有活跃监听器，才清理已登记残留并按保存的运行意图启动；指向已消失 `/tmp` 目标的原符号链接也适用。未知、被替换或无法验证的文件继续保留，不重置停止状态和重试额度。

旧 Go 后台仍在运行时，可使用 `./codex-bg record-socket-owner --yes` 无重启登记当前归属。这只覆盖当时的进程和 socket；完整保护需要让新版监督器运行，以便子进程更换后持续刷新记录。内核不支持归属查询或权限不足时会报告并停止自动清理，不删除未知 socket。

## 安装、迁移与维护

先运行 `inspect` 查看候选；默认优先复用已有兼容 Codex，`--codex managed` 明确下载工具管理的版本。后台方式自动选择可用 systemd，否则识别容器；可明确指定。

```bash
./codex-bg install --extension-root "$HOME/.vscode-server/extensions" --dry-run
./codex-bg install --extension-root "$HOME/.vscode-server/extensions"
./codex-bg migrate --extension-root "$HOME/.vscode-server/extensions"

./codex-bg start
./codex-bg stop
./codex-bg repair
./codex-bg update                  # 更新本工具到当前运行的安装包版本
./codex-bg update --wait-idle 30m   # 最多等待30分钟，连续空闲后更新；仍需确认
./codex-bg update --version 0.153.4 # 仅工具管理的Codex支持升级
./codex-bg rollback TRANSACTION_ID
./codex-bg recover
./codex-bg uninstall
```

系统服务名为 `codex-bg.service`；容器创建 `$ENTRYPOINTD/codex-bg.sh`（也可 `--entrypoint-dir`）。保留原镜像entrypoint。systemd注销后持续运行可选 `--linger`。容器模式要求用户目录、运行目录、维护目录和启动钩子正确持久化；容器内无法替宿主保证挂载配置。

迁移、升级、停止和卸载默认检查忙碌状态；不能确认空闲时也拒绝隐式中断。`--force` 是明确中断选择，不跳过来源、恢复材料和依赖检查。修改CLI命令需要交互确认，自动化用 `--yes`；预览用 `--dry-run`（安装/迁移/组件）。普通CLI、help、schema等由桥接入口转发真实Codex，未知的窗口级app-server启动配置不能改变共享后台的配置。

“更新核心”也可选择等待空闲：观察连续 10 秒空闲后，再执行正常更新的管理锁和切换前检查，最长等待 30 分钟；CLI 用 `--wait-idle 24h` 等明确时限调整。等待在当前进程中进行，不自动创建常驻服务或启动项。退出、取消、超时均不会强制切换，`--wait-idle` 不能与 `--force` 同用。更新期间仍应暂停发起新任务；空闲采样不能阻止其他客户端在最后一次检查之后提交请求。

迁移识别原 `code-server-deploy`、`vscode-codex-background-deploy` 和 Bash统一版。校验旧入口原件和服务来源后才暂停服务并切换；失败恢复原服务、入口和目标设置。原程序目录、共享Node/Conda及用户数据不会在迁移中整目录删除。来源不明、原件缺失、未验证的外部控制器会阻止对应迁移，给出具体原因。

迁移预览会检查旧服务、原入口备份、CodexApp 依赖，以及旧后台的活跃任务。未选择的 IDE 如果仍引用同一个旧后台，会阻止停止旧后台，提示先处理该依赖；不会擅自修改它的配置。执行时在同一把管理锁下重新检查、备份、切换和提交，停止旧后台前再次检查是否空闲。请选择不再发送新请求的空闲窗口；这不是无中断热迁移。

检测到旧 CodexApp 依赖时，向导提供默认不选的“迁移现有 CodexApp（保留程序、端口和密码）”，普通新装 CodexApp 选项同时禁用。CLI 对应：

```bash
# 只读检查；仍有任务或无法核实依赖时会给出错误，不改变服务
./codex-bg migrate --backend container \
  --extension-root "$HOME/.local/share/code-server/extensions" \
  --migrate-codexapp --dry-run
# 确认空闲并检查预览后，去掉 --dry-run 进入确认
```

自动交接目前支持已核实的旧容器 `entrypoint.d/10-start-codexapp.sh` 实现；自定义或改动过的启动脚本、未知服务管理器、额外启动参数、身份不符的进程会阻止迁移。交接复用已有程序和固定 Node 路径，保留端口，将原密码复制到新受管运行目录，原密码文件与用户数据保留。旧启动脚本集中备份后停用，通过 PID、启动时间、UID、执行文件和参数核实旧进程，使用 Linux pidfd 停止，无法确认时不猜测发送信号。

旧 native daemon 的更新进程与模型服务相互独立，单独执行旧版 `daemon stop` 可能留下 updater。迁移现在将来源明确的 updater 一起交接：核对原启动项、PID/启动时间、内核启动标识、执行路径、HOME 和 socket，使用 pidfd 发送 SIGTERM；不按进程名称批量终止。停止后把 PID 记录及 updater 日志集中备份，清理原 socket；保留 Codex 程序、认证、会话和共享环境。迁移回滚会恢复并验证旧 updater。

已经迁移的部署可在“运行与维护 → 检查并清理旧 updater”检查残留（发现 PID 记录时显示），或执行：

```bash
./codex-bg cleanup-legacy-updater --dry-run
./codex-bg cleanup-legacy-updater --yes
```

清理必须有当前部署的已完成迁移记录和可校验的旧启动项备份。核实失败则保留，停止超时不强杀；失败或中断后 `recover` 继续同一清理。此独立清理不停止新 Go 后台和 CodexApp，也不重新启用旧自动更新。

核心后台和被迁移 CodexApp 都通过就绪检查后才提交；其中任一失败，会停止本次新建的服务并恢复旧入口、旧配置和原先运行的旧 CodexApp。恢复冲突会保留事务并报告，可用 `recover` 重试；不会声称迁移成功。新增的其他可选组件仍在核心迁移提交后独立安装，其失败单独报告。`--migrate-codexapp` 与 `--component codexapp` 不能同时使用。

## 可选组件与外部环境

Node、Miniconda、CodexApp 在普通Linux和容器都可选，默认关闭。容器额外提供满足能力条件的 apti、code-server 图标和微软官方插件源。已有兼容环境是默认推荐，亦可指定路径或安装受管版本。

```bash
./codex-bg component list
./codex-bg component install node --path /existing/bin/node
./codex-bg component install node --path managed
./codex-bg component install miniconda --path managed
./codex-bg component install codexapp --password-file /private/password --port 8214
# 密码留空：首次生成默认密码；复用现有 CodexApp 时 --configure 明确配置受管启动
./codex-bg component install codexapp --configure --port 8214
# 若明确允许同时补装私有Node：再加 --with-node
./codex-bg component configure codexapp --password-file /private/password --port 8214
./codex-bg component uninstall branding
```

Node/Conda新安装提供shell集成开关，默认开启；`--no-shell`关闭。受支持的bash/zsh只写一个可逆配置块，Conda不自动激活base，不复制整个profile到备份。Node版本默认v22.20.0，Codex默认0.153.4，CodexApp默认0.1.90；可传明确版本。Miniconda支持官方installer版本，默认latest。

复用环境只解除本工具引用，不删除其目录，不擅自升级或切换外部默认版本。Node出现用户新增global包、Conda有用户环境时，整目录删除会被阻止。组件安装失败保留可诊断状态，不误报成功；Node/Conda/CodexApp暂采用明确卸载后重装的版本维护方式，不提供原位升级。

受管组件目录属于安装器，不应用来保存项目或任意私人文件；保护检查覆盖常规Node全局包和Conda环境/base修改，不能识别所有任意文件的用途。

CodexApp 普通Linux使用独立 `codex-bg-codexapp.service`，容器由统一启动流程管理。复用已有程序也可以配置受管启动，但已有外部进程不会被自动终止或收编；它有自己的Node/npm、端口、密码及HTTP检查。CodexApp错误不使核心后台停机。

CodexApp 的 npm 版本通过 `package.json` 与实际 CLI 入口关联，并使用固定 Node 执行 `--help` 验证启动能力，不要求其支持 `--version`。不会因 NVM 浮动别名失效而忽略已经存在的兼容 Node；复用后保留验证过的 Node 路径。

WebUI 密码文件可留空：已有受管密码时保留，首次创建随机密码，文件权限 `0600`。界面显示密码保存路径，不在日志显示密码；默认路径为 `~/.local/share/codex-bg/components/codexapp/password`，复用外部程序时也保存在工具自己的运行目录。显式指定密码文件仍需绝对路径，源文件和外部程序目录不会改动。单独登记外部 CLI 的旧行为仍保留；选择界面安装或 CLI `--configure` 才配置受管启动。

apti只记录经它安装的包；卸载不卸载系统包。图标备份集中保存，外部图标变化时停止恢复并保留原件。程序自身下载使用 Go HTTP；官方 Codex 安装脚本及部分组件安装仍需 curl、shell、归档工具，CodexApp 需 Node/npm，系统包操作需要必要权限；它们不是 Go 核心的运行依赖。

## 文件位置

安装器目录是 `install.sh`／发布包程序所在位置，和当前工作目录无关。可用全局 `--installer-dir DIR` 明确指定；该选项放在命令前。所有维护资料集中为：

```text
codex-background-deploy/
├── install.sh
├── codex-bg
└── installer-data/
    ├── backups/
    ├── logs/
    ├── cache/
    ├── tmp/
    ├── transactions/
    ├── history/
    └── reports/
```

实际运行程序、当前配置、绑定、socket/锁、连接标记和受管组件在 `~/.local/share/codex-bg/`；外部复用环境留在原处。systemd unit、容器钩子、扩展入口及shell配置块处于各自必须的位置并记录归属。临时替换文件是跨文件系统原子替换的例外，正常结束立即删除。

目录、状态和备份默认仅当前用户可读。日志约2MiB轮换一次；bridge不记录prompt/response正文，Codex原生stderr可能含自身诊断信息。系统自行产生的journal/sudo/apt记录和Codex自身用户数据不是本工具的维护缓存，不能保证清除。

维护目录丢失时既有后台继续运行，日志丢弃，不另寻HOME或/tmp落盘；新备份/接管暂停。移动整个安装器后：

```bash
./codex-bg data relink /new/installer-directory
```

必须把原 `installer-data` 一起移动；重新关联会检查当前引用的备份，不能用空目录覆盖恢复记录。已运行程序的日志会跟随新路径。仅用于回退的旧Codex包位于cache/codex-rollback；普通缓存清理保留它，运行中的原路径保留引用链接。

## 备份与清理

```bash
./codex-bg backup list
./codex-bg backup show BACKUP_ID
./codex-bg backup path BACKUP_ID
./codex-bg backup verify BACKUP_ID
./codex-bg backup restore BACKUP_ID
./codex-bg backup delete BACKUP_ID
./codex-bg backup delete BACKUP_ID --force
./codex-bg backup prune
./codex-bg data status
./codex-bg data clean logs
./codex-bg data clean cache
./codex-bg data clean tmp
```

备份显示来源、用途、大小、时间、路径、校验及引用。普通删除和prune跳过仍用于恢复的材料；强删要求明确选择，写入“恢复材料缺失”状态后才删内容。之后不会假称能完整恢复，需补齐原件或重新安装扩展才能解除接管。恢复活跃扩展备份会同步解除其根目录接管，避免立即被自动重写。

卸载先恢复入口，再停止后台、删除受管运行文件。恢复冲突时保留依赖。维护资料不会随核心卸载隐式删除，最后可在安装器目录统一查看/清理。

## 构建与验证

源码可用Go >=1.24构建，无需在用户机器安装Go。仓库保留上一版源码于 `legacy/v3`，不是当前运行入口。

```bash
. scripts/go-env.sh
# 若未将Go放进installer-data/cache/toolchain/go，请先提供系统go
go test ./internal/app ./cmd/codex-bg
# 迁移事务：隔离真实 Node HTTP 进程和本地假 Codex，无外部模型/生产资源
go test ./internal/app -run 'TestMigration|TestLegacyCodexApp|TestUIMigration' -count=1
go build -trimpath -o codex-bg ./cmd/codex-bg
bash scripts/build.sh
# 可选真实CLI测试，不使用生产认证或Provider
CB_TEST_CODEX=/absolute/codex go test ./internal/app -run TestRuntimeRealCodex -count=1
/path/to/venv/bin/python scripts/test_background.py
# 安装界面：真实 PTY，模拟安装回调，不部署任何软件
go test ./internal/app -run 'TestUIAutoRestart|TestUIRetryStop|TestUICodexAppRequiresReady|TestUIServiceActions' -count=1
go test -c -o installer-data/tmp/ui-tests ./internal/app
/path/to/venv/bin/python scripts/test_ui.py
# 插件源：隔离临时安装目录内验证切换、恢复、镜像重置与失败保护
go test ./internal/app -run Marketplace -count=1
```

构建缓存、测试临时文件与报告均在installer-data下。发布物在dist含amd64/arm64二进制、SHA256SUMS、各架构完整tar.gz。安装包包含用于迁移来源比较的 v3 controller 原始源码；该副本不作为运行入口。安装包可直接解压运行；下载bootstrap通过 `CODEX_BG_DOWNLOAD_BASE=https://...` 指定真实发布地址，不硬编码未经发布的URL。包内checksum校验防止传输损坏，签名发布流程尚未建立。

隔离测试覆盖Go协议、EOF/握手超时、独立进程组、所有权和备份、迁移失败恢复、环境复用、组件与终端集成、TUI确认流程。端到端脚本用本地假Responses服务验证唯一客户端断开后任务完成、扩展更新自动接管、更新/回退/卸载以及无测试进程残留。

崩溃恢复测试覆盖禁用选项、最多五次重试、连续健康窗口、停止与维护抑制、监督进程重启后的额度、外部 socket 冲突和替换。端到端脚本还会终止测试专用的真实 Codex，确认自动恢复、已完成会话仍可读取、没有重放模型请求，并验证手动停止不会被容器启动钩子撤销。

真实systemd注销、宿主容器重建、目标VSCode界面和实际软件网络安装需目标环境验收；源码实现不等于已经部署到当前用户或SSH主机。本机操作的实际结果另记于 `installer-data/reports`，不以隔离测试代替生产验收。
