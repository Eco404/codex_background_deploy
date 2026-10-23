package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"codex-bg/internal/app"
	"golang.org/x/term"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if e := run(ctx, os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, "错误:", e)
		var exit *exec.ExitError
		if errors.As(e, &exit) {
			os.Exit(exit.ExitCode())
		}
		os.Exit(1)
	}
}
func confirm(yes bool, message string) bool {
	if yes {
		return true
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr, "非交互操作需要 --yes；未执行。")
		return false
	}
	fmt.Fprintf(os.Stderr, "%s\n输入 yes 确认: ", message)
	var reply string
	fmt.Fscanln(os.Stdin, &reply)
	return reply == "yes"
}
func run(ctx context.Context, args []string) error {
	home, installer, runtimeDir := "", "", ""
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "--") {
		key := args[i]
		if key == "--version" {
			fmt.Println(app.Version)
			return nil
		}
		if key != "--home" && key != "--installer-dir" && key != "--runtime" {
			break
		}
		if i+1 >= len(args) {
			return errors.New("缺少路径参数")
		}
		switch key {
		case "--home":
			home = args[i+1]
		case "--installer-dir":
			installer = args[i+1]
		case "--runtime":
			runtimeDir = args[i+1]
		}
		i += 2
	}
	args = args[i:]
	a, e := app.New(home, installer, runtimeDir)
	if e != nil {
		return e
	}
	if len(args) == 0 {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Print(a.Status(ctx))
			return nil
		}
		return app.RunUI(ctx, a, app.UIActions{PreviewLegacyUpdater: a.PreviewLegacyUpdaterCleanup, CleanupLegacyUpdater: a.CleanupLegacyUpdater, UpdateWhenIdle: a.UpdateWhenIdle, Inventory: func(c context.Context) (app.Inventory, error) {
			if err := a.Reload(); err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					return app.Inventory{}, err
				}
				a.S = app.State{Schema: 1, Installer: a.P.Installer, CodexHome: filepath.Join(a.P.Home, ".codex"), Components: map[string]*app.Component{}}
				a.P.Data = filepath.Join(a.P.Installer, "installer-data")
			}
			return a.Discover(c)
		}, Status: a.Status, Install: a.Install, Migrate: a.Migrate, PreviewMigration: a.PreviewMigration, Diagnose: a.Diagnose, Repair: a.Reconcile, Recover: a.Recover, Service: a.Service, Update: a.Update, Rollback: a.Rollback, Uninstall: a.Uninstall, Component: a.ManagedComponent, Backups: a.Backups, BackupAction: a.BackupAction, DataAction: a.DataAction, Logs: a.Logs})
	}
	command := args[0]
	args = args[1:]
	for _, arg := range args {
		if arg == "--no-auto-restart" && command != "install" && command != "migrate" {
			return errors.New("--no-auto-restart 仅用于 install 或 migrate")
		}
		if arg == "--wait-idle" && command != "update" {
			return errors.New("--wait-idle 仅用于 update")
		}
	}
	if command == "autostart" {
		if len(args) != 0 {
			return errors.New("autostart 不接受参数")
		}
		return a.AutoStart(ctx)
	}
	if command == "serve" {
		return a.Serve(ctx)
	}
	if command == "component-run" {
		if len(args) != 1 {
			return errors.New("component-run需要组件")
		}
		return a.RunComponent(ctx, args[0])
	}
	if command == "proxy" {
		binding := ""
		if len(args) >= 2 && args[0] == "--binding" {
			binding = args[1]
			args = args[2:]
		}
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		return a.Proxy(ctx, binding, args)
	}
	yes, force, dry, jsonOut := false, false, false, false
	var waitIdle time.Duration
	opt := app.InstallOptions{Backend: "auto"}
	cop := app.ComponentOptions{ShellIntegration: true}
	network := app.DefaultNetworkOptions()
	positional := []string{}
	for j := 0; j < len(args); j++ {
		arg := args[j]
		switch arg {
		case "--yes":
			yes = true
		case "--force":
			force = true
		case "--dry-run":
			dry = true
		case "--json":
			jsonOut = true
		case "--linger":
			opt.Linger = true
		case "--migrate-codexapp":
			opt.MigrateCodexApp = true
		case "--no-auto-restart":
			opt.NoAutoRestart = true
		case "--no-shell":
			cop.ShellIntegration = false
		case "--with-node":
			cop.InstallNode = true
		case "--configure":
			cop.Configure = true
		case "--mirrors":
			network.UseMirrors = true
		case "--no-proxy":
			network.ProxyMode = "none"
		case "--wait-idle":
			if j+1 >= len(args) {
				return errors.New("--wait-idle 需要时限，例如 30m 或 24h")
			}
			j++
			waitIdle, e = time.ParseDuration(args[j])
			if e != nil || waitIdle <= 0 {
				return errors.New("--wait-idle 需要正数时限，例如 30m 或 24h")
			}
		case "--backend", "--codex", "--version", "--extension-root", "--entrypoint-dir", "--path", "--password-file", "--port", "--media-dir", "--shell", "--component", "--npm-registry", "--node-mirror", "--conda-mirror", "--proxy":
			if j+1 >= len(args) {
				return fmt.Errorf("%s缺少参数", arg)
			}
			j++
			v := args[j]
			switch arg {
			case "--backend":
				opt.Backend = v
			case "--codex":
				opt.CodexPath = v
			case "--version":
				opt.CodexVersion = v
				cop.Version = v
			case "--extension-root":
				opt.Roots = append(opt.Roots, v)
			case "--entrypoint-dir":
				opt.EntrypointDir = v
			case "--path":
				cop.Path = v
			case "--password-file":
				cop.PasswordFile = v
			case "--port":
				cop.Port = v
			case "--media-dir":
				cop.MediaDir = v
			case "--shell":
				cop.Shell = v
			case "--component":
				opt.Components = append(opt.Components, v)
			case "--npm-registry":
				network.UseMirrors, network.NPMRegistry = true, v
			case "--node-mirror":
				network.UseMirrors, network.NodeMirror = true, v
			case "--conda-mirror":
				network.UseMirrors, network.CondaMirror = true, v
			case "--proxy":
				network.ProxyMode, network.Proxy = "custom", v
			}
		default:
			if strings.HasPrefix(arg, "--") {
				return fmt.Errorf("未知选项%s", arg)
			}
			positional = append(positional, arg)
		}
	}
	opt.Yes = yes
	opt.Force = force
	opt.DryRun = dry
	if waitIdle > 0 && force {
		return errors.New("--wait-idle 不能与 --force 同时使用")
	}
	if opt.MigrateCodexApp && command != "migrate" {
		return errors.New("--migrate-codexapp 仅用于 migrate；新装 CodexApp 使用 --component codexapp")
	}
	cop.Yes = yes
	cop.DryRun = dry
	if err := network.Validate(); err != nil {
		return err
	}
	a.Network = network
	opt.ComponentOptions = map[string]app.ComponentOptions{}
	for _, name := range opt.Components {
		opt.ComponentOptions[name] = cop
	}
	switch command {
	case "help", "--help", "-h":
		fmt.Print(help)
		return nil
	case "inspect":
		inv, e := a.Discover(ctx)
		if e != nil {
			return e
		}
		if jsonOut {
			return json.NewEncoder(os.Stdout).Encode(inv)
		}
		fmt.Print(a.Status(ctx))
		return nil
	case "status":
		fmt.Print(a.Status(ctx))
		return nil
	case "doctor":
		fmt.Print(a.Diagnose(ctx))
		return nil
	case "install", "migrate":
		if !yes && !dry {
			if command == "migrate" {
				preview, err := a.PreviewMigration(ctx, opt)
				fmt.Print(preview)
				if err != nil {
					return err
				}
			} else {
				if _, _, e = a.PlanInstall(ctx, opt); e != nil {
					return e
				}
			}
			if !confirm(false, "确认上述部署计划？") {
				return nil
			}
			opt.Yes = true
		}
		if command == "migrate" {
			return a.Migrate(ctx, opt)
		}
		return a.Install(ctx, opt)
	case "repair":
		if !confirm(yes, "检查并修复已选择的扩展入口？") {
			return nil
		}
		return a.Reconcile(ctx)
	case "recover":
		if !confirm(yes, "恢复未完成事务？可能重启本次管理的后台。") {
			return nil
		}
		return a.Recover(ctx)
	case "cleanup-legacy-updater":
		preview, err := a.PreviewLegacyUpdaterCleanup(ctx)
		fmt.Print(preview)
		if err != nil || dry {
			return err
		}
		if !confirm(yes, "清理已核实的旧版更新进程及其 PID/socket；保留程序和用户数据？") {
			return nil
		}
		return a.CleanupLegacyUpdater(ctx, true)
	case "record-socket-owner":
		if dry {
			return errors.New("record-socket-owner 不支持 --dry-run；不会写入登记")
		}
		if !confirm(yes, "是否验证并登记本工具当前后台的 socket 归属？") {
			return nil
		}
		return a.RecordRuntimeSocketOwnership()
	case "start", "stop", "restart":
		if command != "start" && !confirm(yes, "此操作可能中断当前任务。") {
			return nil
		}
		return a.Service(ctx, command, force)
	case "update":
		if dry {
			return errors.New("update 不支持 --dry-run；不会执行更新")
		}
		if opt.CodexVersion != "" {
			fmt.Println(network.Summary())
		}
		if !confirm(yes, "更新本工具及由它安装的 Codex CLI；空闲时切换，--force允许中断任务。") {
			return nil
		}
		if waitIdle > 0 {
			return a.UpdateWhenIdle(ctx, opt.CodexVersion, waitIdle)
		}
		return a.Update(ctx, opt.CodexVersion, force)
	case "rollback":
		if len(positional) != 1 {
			return errors.New("rollback需要history事务ID")
		}
		if !confirm(yes, "回退到指定升级前的版本？") {
			return nil
		}
		return a.Rollback(ctx, positional[0], force)
	case "uninstall":
		if !confirm(yes, "恢复扩展入口并卸载核心；外部环境和维护资料保留。") {
			return nil
		}
		return a.Uninstall(ctx, force)
	case "component":
		if len(positional) == 1 && positional[0] == "list" {
			return json.NewEncoder(os.Stdout).Encode(a.S.Components)
		}
		if len(positional) != 2 {
			return errors.New("component list/install/configure/uninstall NAME")
		}
		if positional[0] != "list" && positional[0] != "status" && positional[0] != "plan" && !dry {
			if positional[0] == "install" && positional[1] == "marketplace" {
				if err := a.ManagedComponent(ctx, "plan", "marketplace", cop); err != nil {
					return err
				}
			}
			if (positional[0] == "install" && positional[1] != "marketplace") || cop.InstallNode {
				fmt.Println(network.Summary())
			}
			if !confirm(yes, "确认组件操作："+strings.Join(positional, " ")) {
				return nil
			}
			cop.Yes = true
		}
		return a.ManagedComponent(ctx, positional[0], positional[1], cop)
	case "backup":
		if len(positional) < 1 {
			return errors.New("backup list/show/path/verify/restore/delete/prune [ID]")
		}
		action := positional[0]
		id := ""
		if len(positional) > 1 {
			id = positional[1]
		}
		if action == "delete" || action == "restore" || action == "prune" {
			if id != "" {
				_ = a.BackupAction(ctx, "show", id, false)
			}
			if !confirm(yes, "确认备份操作？--force 删除引用备份将失去恢复能力。") {
				return nil
			}
		}
		return a.BackupAction(ctx, action, id, force)
	case "data":
		if len(positional) == 0 {
			return a.DataAction(ctx, "status", "")
		}
		action := positional[0]
		target := ""
		if len(positional) > 1 {
			target = positional[1]
		}
		if action == "clean" || action == "relink" {
			if !confirm(yes, "确认维护资料操作？") {
				return nil
			}
		}
		return a.DataAction(ctx, action, target)
	case "logs":
		fmt.Print(a.Logs())
		return nil
	case "version":
		fmt.Println(app.Version)
		return nil
	default:
		return fmt.Errorf("未知命令%s；运行 help", command)
	}
}

var _ = filepath.Separator

const help = `Codex 后台管理 4.0
无参数：状态首页和交互向导；非交互：只读状态。
全局参数（命令前）：--home DIR --installer-dir DIR --runtime DIR

inspect [--json] | status | doctor | logs
install | migrate --extension-root DIR [--codex PATH|managed] [--backend auto|systemd|container]
  [--entrypoint-dir DIR] [--component NAME] [--linger] [--no-auto-restart] [--dry-run] [--yes]
  崩溃后默认最多自动重启 5 次；--no-auto-restart 关闭部署的自动恢复
  migrate --migrate-codexapp: 显式交接已识别的旧 CodexApp，保留程序、端口和密码
  migrate --dry-run: 检查恢复材料、其他 IDE/服务依赖与活跃任务，不修改部署
repair | recover | start | stop | restart [--yes] [--force]
update [--version VERSION] [--wait-idle 30m] [--yes] [--force]
  --wait-idle: 在当前进程等待连续空闲，超时退出；不能与 --force 同用
rollback TRANSACTION_ID [--yes] [--force]
cleanup-legacy-updater [--dry-run] [--yes] # 清理已迁移部署的旧 updater
record-socket-owner [--yes] # 不重启，仅校验并登记当前 socket 归属
uninstall [--yes] [--force]
component install|configure|repair|uninstall|status|plan NAME [--path PATH] [--version VERSION]
  [--no-shell] [--password-file FILE] [--port PORT] [--configure] [--dry-run] [--yes]
  marketplace: 仅 code-server 容器，选择微软官方插件源；不安装插件
  install/migrate --component marketplace 可与核心一起选择
安装/组件/更新下载选项（仅本次操作，不修改全局配置）：
  --mirrors [--npm-registry URL] [--node-mirror URL] [--conda-mirror URL]
  [--proxy http(s)://HOST:PORT|socks5://HOST:PORT | --no-proxy]
默认国内镜像：npm/Node 使用华为云；Miniconda 使用中科大。
backup list|show|path|verify|restore|delete|prune [ID] [--force] [--yes]
data status|path|clean logs|cache|tmp|relink INSTALLER_DIR [--yes]

外部环境只复用；强删备份不可恢复；状态/诊断不会启动服务或请求模型。
`
