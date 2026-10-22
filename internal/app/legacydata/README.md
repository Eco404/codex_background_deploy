# 旧部署迁移校验材料

此目录仅保存经审查的旧脚本原件，使用 `go:embed` 编入 Go 程序，用于核对目标机器上的旧部署。它们不会作为新后台的运行脚本安装或执行，也不会复制到发布包目录。

| 文件 | 校验用途 | 原始来源 |
|---|---|---|
| `v3-control.sh.txt` | v3 控制器；内容必须完全一致才允许迁移 | Git 提交 `8fea373` 的 `legacy/v3/payload/control.sh` |
| `code-server-codexapp-start.sh.txt` | 旧 code-server CodexApp 启动钩子 | 已审查的旧部署钩子，见 `migrate_codexapp.go` |

`v3-control.sh.txt` 保留原始字节，SHA256 为 `fdf98206f190f54209afc05fc03142063733f1015079415bc8814ad8a6ee96f6`。不要格式化或修改原件来适配未知的旧脚本；支持其他变体需要单独审查和测试。

v3 迁移仍通过目标机器上已有的控制器停止旧后台，并在失败时恢复旧后台；该控制器依赖的原安装文件需要保留。这里的原件仅用于校验，不用于补装或修复旧运行环境。

完整旧项目保留在 Git 历史，可用 `git show 8fea373:legacy/v3/README.md` 查看原说明。
