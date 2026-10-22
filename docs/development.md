# 构建与测试

[返回使用说明](../README.md) · [高级使用与维护](operations.md)

以下命令在源码仓库根目录执行。普通用户使用仓库附带的预编译程序即可，无需准备开发环境。

源码可用Go >=1.24构建，无需在用户机器安装Go。旧项目已从当前目录移除，完整源码保留在 Git 历史。迁移所需的少量原件保存在 `internal/app/legacydata/`，通过 `go:embed` 编入程序；发布包无需携带 `legacy/`。v3 迁移仍严格校验目标控制器，修改过或缺失的控制器会阻止迁移；目标机器上的旧运行文件仍须保留，以便停止旧后台和失败恢复。

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
go test ./internal/app -run '^TestUI' -count=1
go test -c -o installer-data/tmp/ui-tests ./internal/app
/path/to/venv/bin/python scripts/test_ui.py
# 插件源：隔离临时安装目录内验证切换、恢复、镜像重置与失败保护
go test ./internal/app -run Marketplace -count=1
```

构建缓存、测试临时文件与报告均在installer-data下。`dist/codex-bg-linux-amd64`、`dist/codex-bg-linux-arm64` 和 `dist/SHA256SUMS` 纳入 Git；修改 Go 源码后应运行 `bash scripts/build.sh`，将这三个文件一同提交。根目录的本机 `codex-bg` 副本、完整 tar.gz 安装包及维护资料仍忽略，不重复存入仓库。

构建脚本也生成各架构完整 tar.gz，包含 README 和 `docs/` 使用文档；迁移校验原件已内嵌于可执行文件，包内不再附带旧项目源码。完整安装包可直接解压运行。仅在单独使用 `install.sh`、没有本地程序或 `dist/` 预编译文件时，才需要通过 `CODEX_BG_DOWNLOAD_BASE=https://...` 指定真实下载地址。已有根目录 `codex-bg` 时脚本优先使用它；拉取新版预编译文件后，可移除安装器目录的该副本，再运行 `./install.sh` 重新校验提取。此操作不删除已部署到运行目录的后台程序，后台版本更新仍通过“更新核心”执行。包内checksum校验防止传输损坏，签名发布流程尚未建立。

隔离测试覆盖Go协议、EOF/握手超时、独立进程组、所有权和备份、迁移失败恢复、环境复用、组件与终端集成、TUI确认流程。端到端脚本用本地假Responses服务验证唯一客户端断开后任务完成、扩展更新自动接管、更新/回退/卸载以及无测试进程残留。

崩溃恢复测试覆盖禁用选项、最多五次重试、连续健康窗口、停止与维护抑制、监督进程重启后的额度、外部 socket 冲突和替换。端到端脚本还会终止测试专用的真实 Codex，确认自动恢复、已完成会话仍可读取、没有重放模型请求，并验证手动停止不会被容器启动钩子撤销。

真实systemd注销、宿主容器重建、目标VSCode界面和实际软件网络安装需目标环境验收；源码实现不等于已经部署到当前用户或SSH主机。本机操作的实际结果另记于 `installer-data/reports`，不以隔离测试代替生产验收。
