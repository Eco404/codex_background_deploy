package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func (a *App) Inspect(ctx context.Context) (Inventory, error) { return a.Discover(ctx) }
func (a *App) Status(ctx context.Context) string {
	inv, e := a.Discover(ctx)
	if e != nil {
		return e.Error()
	}
	status := "尚未接管"
	if len(inv.Legacy) > 0 {
		status = "发现旧部署"
	}
	if a.S.Installed {
		status = "已安装"
	}
	text := fmt.Sprintf("目标 %s@%s\n部署 %s\n维护资料 %s\n", inv.User, inv.Host, status, a.P.Data)
	for _, kind := range []string{"codex", "node", "conda", "codexapp"} {
		cs := inv.Candidates[kind]
		usable := "未发现兼容环境"
		for _, c := range cs {
			if c.Compatible {
				usable = c.Version + " · " + c.Path
				break
			}
		}
		text += kind + ": " + usable + "\n"
	}
	text += fmt.Sprintf("IDE: 发现%d个扩展，已绑定%d个入口\n", len(inv.Extensions), len(a.S.Bindings))
	if inv.Container && inv.CodeServerProduct != "" {
		text += "微软插件源: " + marketplaceSummary(a.S.Components["marketplace"]) + "\n"
	}
	if a.S.Installed {
		text += inv.RuntimeStatus.Summary(inv.ServiceState) + "\n"
		if inv.ServiceState == "running" || inv.ServiceState == "starting" || inv.ServiceState == "unknown" {
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if e = a.Health(c); e != nil {
				text += "协议检查: 不健康 · " + e.Error() + "\n"
			} else {
				text += "协议检查: initialize 正常\n"
			}
		}
	}
	if _, e = os.Stat(a.P.Data); e != nil && a.S.Installed {
		text += "警告: 维护目录不可用；后台可继续，备份/新接管暂停\n"
	}
	if hasPending(a.P.Data) {
		text += "警告: 有未完成事务，执行 recover\n"
	}
	if len(a.S.RecoveryMissing) > 0 {
		text += fmt.Sprintf("恢复材料缺失: %d项\n", len(a.S.RecoveryMissing))
	}
	for _, name := range []string{"health", "extras"} {
		if name == "health" && inv.RuntimeStatus != nil && inv.RuntimeStatus.Recorded {
			continue
		}
		if raw, e := os.ReadFile(filepath.Join(a.P.Runtime, "run", name+".json")); e == nil {
			text += name + ": " + string(raw) + "\n"
		}
	}
	for _, b := range a.S.Bindings {
		label := "已配置，尚未观察到扩展连接"
		if _, e = os.Stat(filepath.Join(a.P.Runtime, "connections", b.ID+".json")); e == nil {
			label = "已观察到该入口连接"
		}
		text += b.Entry + " · " + label + "\n"
	}
	if raw, e := os.ReadFile(filepath.Join(a.P.Runtime, "run", "reconcile.json")); e == nil {
		text += "入口维护: " + string(raw) + "\n"
	}
	for _, w := range inv.Warnings {
		text += "警告: " + w + "\n"
	}
	return text
}
func (a *App) Diagnose(ctx context.Context) string {
	text := a.Status(ctx)
	if a.S.Installed {
		if _, err := os.Lstat(filepath.Join(a.S.CodexHome, "app-server-daemon", "app-server-updater.pid")); err == nil {
			preview, err := a.PreviewLegacyUpdaterCleanup(ctx)
			text += "旧 updater 检查: " + preview
			if err != nil {
				text += err.Error() + "\n"
			} else {
				text += "可用 cleanup-legacy-updater --dry-run 检查清理计划。\n"
			}
		}
	}
	if c := a.S.Components["marketplace"]; c != nil {
		if err := a.checkMarketplace(c); err != nil {
			text += "插件源异常: " + err.Error() + "\n"
		} else {
			text += "插件源: 配置已校验；运行中的 IDE 是否已重启需单独确认\n"
		}
	}
	for _, f := range a.S.Files {
		h, e := Hash(f.Path)
		if e != nil || h != f.Hash {
			text += "文件变化/缺失: " + f.Path + "\n"
		}
	}
	for _, b := range a.S.Bindings {
		if e := a.VerifyBackup(b.BackupID); e != nil {
			text += "备份不可恢复: " + b.Entry + " · " + e.Error() + "\n"
		}
	}
	return text + "模型请求: 未执行\n"
}
func (a *App) checkManaged() error {
	for _, f := range a.S.Files {
		h, e := Hash(f.Path)
		if e != nil {
			return e
		}
		if h != f.Hash {
			return fmt.Errorf("受管文件外部修改，保留: %s", f.Path)
		}
	}
	return nil
}
func (a *App) PlanInstall(ctx context.Context, opt InstallOptions) (Inventory, InstallOptions, error) {
	inv, e := a.Discover(ctx)
	if e != nil {
		return inv, opt, e
	}
	if opt.Backend == "" || opt.Backend == "auto" {
		if inv.Systemd {
			opt.Backend = "systemd"
		} else if inv.Container {
			opt.Backend = "container"
		} else {
			return inv, opt, errors.New("无可用systemd；请明确配置支持启动钩子的容器环境")
		}
	}
	if opt.Backend != "systemd" && opt.Backend != "container" {
		return inv, opt, errors.New("后台必须为systemd或container")
	}
	if opt.Backend == "systemd" && !inv.Systemd {
		return inv, opt, errors.New("无法连接systemd用户管理器")
	}
	if opt.EntrypointDir == "" {
		opt.EntrypointDir = inv.EntrypointDir
	}
	if opt.EntrypointDir == "" {
		opt.EntrypointDir = filepath.Join(a.P.Home, "entrypoint.d")
	}
	if opt.CodexPath == "" {
		for _, c := range inv.Candidates["codex"] {
			if c.Compatible {
				opt.CodexPath = c.Path
				break
			}
		}
	}
	if opt.CodexPath == "" {
		opt.CodexPath = "managed"
	}
	if len(opt.Roots) == 0 {
		return inv, opt, errors.New("请明确选择扩展目录（--extension-root）；不会自动接管所有IDE")
	}
	for i, r := range opt.Roots {
		p, e := filepath.Abs(r)
		if e != nil {
			return inv, opt, e
		}
		if p == "/" || p == a.P.Home {
			return inv, opt, errors.New("扩展根目录范围过大")
		}
		if e = checkParents(p); e != nil {
			return inv, opt, e
		}
		opt.Roots[i] = p
		if opt.CodexPath != "managed" && Within(p, opt.CodexPath) {
			return inv, opt, errors.New("不能把将被接管的扩展目录内CLI作为后台；请选择独立CLI")
		}
	}
	fmt.Fprintf(a.Out, "后台: %s\nCodex: %s\n维护目录: %s\n", opt.Backend, opt.CodexPath, a.P.Data)
	if opt.NoAutoRestart {
		fmt.Fprintln(a.Out, "崩溃后自动重启: 关闭；异常退出后等待手动启动")
	} else {
		fmt.Fprintln(a.Out, "崩溃后自动重启: 开启；最多 5 次，退避 2/4/8/16/30 秒，连续健康 2 分钟后重置")
	}
	a.progress("下载设置：%s", a.Network.Summary())
	for _, r := range opt.Roots {
		fmt.Fprintf(a.Out, "接管扩展根目录: %s\n", r)
	}
	if opt.Backend == "container" {
		fmt.Fprintf(a.Out, "启动钩子: %s/codex-bg.sh；需持久化运行/用户/维护目录并由镜像执行\n", opt.EntrypointDir)
	}
	for _, name := range opt.Components {
		if name == "marketplace" {
			if e := a.planMarketplace(inv); e != nil {
				return inv, opt, e
			}
		}
	}
	return inv, opt, nil
}
func (a *App) validateCodex(ctx context.Context, path string) (Candidate, error) {
	p, e := filepath.Abs(path)
	if e != nil {
		return Candidate{}, e
	}
	real, e := filepath.EvalSymlinks(p)
	if e != nil {
		return Candidate{}, e
	}
	if real == a.P.Binary() {
		return Candidate{}, errors.New("不能把转接程序作为Codex")
	}
	f, e := os.Open(real)
	if e != nil {
		return Candidate{}, e
	}
	head := make([]byte, 2048)
	n, _ := f.Read(head)
	f.Close()
	if strings.Contains(string(head[:n]), "codex-bg") || strings.Contains(string(head[:n]), "codex-daemon-bridge") {
		return Candidate{}, errors.New("候选路径指向旧bridge而不是原始Codex")
	}
	c, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	v, e := a.Run(c, real, "--version")
	if e != nil {
		return Candidate{}, e
	}
	help, e := a.Run(c, real, "app-server", "--help")
	if e != nil || !strings.Contains(string(help), "unix://") {
		return Candidate{}, errors.New("Codex不支持Unix app-server")
	}
	version := regexp.MustCompile(`(?m)^codex-cli [^\r\n]+`).FindString(string(v))
	if version == "" {
		return Candidate{}, errors.New("Codex版本输出无法识别")
	}
	return Candidate{Kind: "codex", Path: real, Version: version, Source: "reused", Compatible: true}, nil
}
func (a *App) Install(ctx context.Context, opt InstallOptions) (err error) {
	if opt.MigrateCodexApp {
		return errors.New("旧 CodexApp 交接仅支持 migrate，不可用于 install")
	}
	a.progress("阶段 1/5 · 检查环境与安装计划")
	inv, opt, e := a.PlanInstall(ctx, opt)
	if e != nil {
		return e
	}
	if opt.DryRun {
		return nil
	}
	if !opt.Yes {
		return errors.New("未确认计划")
	}
	if a.S.Installed {
		return errors.New("已经安装；更新用update，重新扫描入口用repair")
	}
	if len(inv.Legacy) > 0 {
		return errors.New("检测到旧部署；请使用 migrate，避免两个后台争用同一CODEX_HOME")
	}
	if e = a.installCore(ctx, opt); e != nil {
		return e
	}
	return a.installExtras(ctx, opt)
}
func (a *App) installExtras(ctx context.Context, opt InstallOptions) error {
	for _, name := range opt.Components {
		a.progress("可选组件 · %s", name)
		co, ok := opt.ComponentOptions[name]
		if !ok {
			co = ComponentOptions{ShellIntegration: true}
		}
		co.Yes = true
		if e := a.ManagedComponent(ctx, "install", name, co); e != nil {
			return fmt.Errorf("核心已就绪，可选组件 %s 失败: %w", name, e)
		}
	}
	return nil
}
func (a *App) ManagedComponent(ctx context.Context, action, name string, opt ComponentOptions) error {
	if action == "status" || action == "list" || action == "plan" || opt.DryRun {
		return a.ComponentAction(ctx, action, name, opt)
	}
	unlock, e := a.Lock()
	if e != nil {
		return e
	}
	defer unlock()
	if hasPending(a.P.Data) {
		return errors.New("先recover未完成事务")
	}
	if name == "codexapp" {
		if err := a.CheckCodexAppReady(ctx, action, opt); err != nil {
			return err
		}
	}
	if name == "codexapp" && opt.InstallNode && (action == "install" || action == "configure") {
		if e = a.ComponentAction(ctx, "install", "node", ComponentOptions{Path: "managed", ShellIntegration: opt.ShellIntegration, Yes: opt.Yes}); e != nil {
			return e
		}
	}
	if name == "codexapp" && !opt.InstallNode && (action == "install" || action == "configure") && a.S.Components["node"] == nil {
		inv, err := a.Discover(ctx)
		if err != nil {
			return err
		}
		for _, c := range inv.Candidates["node"] {
			var major int
			fmt.Sscanf(strings.TrimPrefix(c.Version, "v"), "%d", &major)
			if c.Compatible && major >= 20 && executable(filepath.Join(filepath.Dir(c.Path), "npm")) {
				if err = a.ComponentAction(ctx, "install", "node", ComponentOptions{Path: c.Path, ShellIntegration: false, Yes: opt.Yes}); err != nil {
					return err
				}
				break
			}
		}
	}
	return a.ComponentAction(ctx, action, name, opt)
}
func (a *App) installCore(ctx context.Context, opt InstallOptions) (err error) {
	unlock, e := a.Lock()
	if e != nil {
		return e
	}
	defer unlock()
	if a.S.Installed {
		return errors.New("核心已被其他操作安装，请刷新状态")
	}
	t, e := a.begin("install")
	if e != nil {
		return e
	}
	defer func() {
		if err != nil {
			a.event("install_failed", err.Error())
			if re := a.recoverLocked(context.Background()); re != nil {
				err = fmt.Errorf("%w；自动恢复未完成: %v", err, re)
			}
		}
	}()
	if e = a.installCoreLocked(ctx, opt, t); e != nil {
		return e
	}
	if e = a.commit(t); e != nil {
		return e
	}
	a.event("installed", a.S.Backend)
	a.progress("阶段 5/5 · 核心安装完成")
	fmt.Fprintln(a.Out, "后台已安装；请Reload Window，首页会显示实际入口连接。")
	return nil
}

// Caller owns the management lock and transaction. Migration commits only after
// every dependent legacy service has also been handed over and verified.
func (a *App) installCoreLocked(ctx context.Context, opt InstallOptions, t *Transaction) error {
	var e error
	candidate := Candidate{}
	a.progress("阶段 2/5 · 准备 Codex 运行环境")
	if opt.CodexPath == "managed" {
		candidate, e = a.InstallManagedCodex(ctx, opt.CodexVersion)
	} else {
		candidate, e = a.validateCodex(ctx, opt.CodexPath)
	}
	if e != nil {
		return e
	}
	a.S.Codex = candidate
	a.progress("阶段 3/5 · 写入运行程序与后台配置")
	a.S.Backend = opt.Backend
	a.S.EntrypointDir = opt.EntrypointDir
	a.S.Installer = a.P.Installer
	a.S.SelectedRoots = append([]string{}, opt.Roots...)
	autoRestart := !opt.NoAutoRestart
	a.S.AutoRestart = &autoRestart
	if e = a.Save(); e != nil {
		return e
	}
	mode := "installing"
	if t.Action == "migrate" {
		mode = "migrating"
	}
	if _, e = a.setRuntimeIntent("stopped", mode, t.ID, true); e != nil {
		return e
	}
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	bin, e := os.ReadFile(exe)
	if e != nil {
		return e
	}
	if e = a.txWrite(t, a.P.Binary(), bin, 0755, "binary"); e != nil {
		return e
	}
	launcher := []byte("#!/bin/sh\nexec " + shellQuote(a.P.Binary()) + " --home " + shellQuote(a.P.Home) + " --runtime " + shellQuote(a.P.Runtime) + " proxy --binding codexapp -- \"$@\"\n")
	if e = a.txWrite(t, filepath.Join(a.P.Runtime, "codex"), launcher, 0755, "launcher"); e != nil {
		return e
	}
	if e = a.installAdapter(ctx, t); e != nil {
		return e
	}
	a.S.Installed = true
	if e = a.Save(); e != nil {
		return e
	}
	if _, e = a.setRuntimeIntent("running", mode, t.ID, true); e != nil {
		return e
	}
	if e = a.Start(ctx); e != nil {
		return e
	}
	a.progress("阶段 4/5 · 后台已就绪，备份并配置选定扩展入口")
	for _, r := range opt.Roots {
		exts, e := scanRoot(r)
		if e != nil {
			return e
		}
		for _, ex := range exts {
			if e = a.Attach(ctx, ex); e != nil {
				return e
			}
		}
	}
	if opt.Linger && a.S.Backend == "systemd" {
		v, e := a.Run(ctx, "loginctl", "show-user", os.Getenv("USER"), "-p", "Linger", "--value")
		if e != nil {
			return e
		}
		if strings.TrimSpace(string(v)) != "yes" {
			a.S.LingerOwned = true
			if e = a.Save(); e != nil {
				return e
			}
			if _, e = a.Run(ctx, "sudo", "loginctl", "enable-linger", os.Getenv("USER")); e != nil {
				return e
			}
		}
	}
	if e = a.Save(); e != nil {
		return e
	}
	return nil
}
func unitQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(s) + `"`
}
func (a *App) installAdapter(ctx context.Context, t *Transaction) error {
	if a.S.Backend == "systemd" {
		path := filepath.Join(a.P.Home, ".config", "systemd", "user", "codex-bg.service")
		unit := "[Unit]\nDescription=Codex background manager\nStartLimitIntervalSec=300\nStartLimitBurst=5\n\n[Service]\nType=simple\nExecStart=" + unitQuote(a.P.Binary()) + " --home " + unitQuote(a.P.Home) + " --runtime " + unitQuote(a.P.Runtime) + " serve\nRestart=on-failure\nRestartSec=3\nTimeoutStopSec=25\nKillMode=control-group\nUMask=0077\nStandardOutput=null\nStandardError=null\n\n[Install]\nWantedBy=default.target\n"
		if _, e := os.Stat(path); e == nil && !a.S.Installed {
			return errors.New("已有同名外部服务，拒绝覆盖")
		}
		if e := a.txWrite(t, path, []byte(unit), 0644, "service"); e != nil {
			return e
		}
		if _, e := a.Run(ctx, "systemctl", "--user", "daemon-reload"); e != nil {
			return e
		}
		_, e := a.Run(ctx, "systemctl", "--user", "enable", "codex-bg.service")
		return e
	}
	p := filepath.Join(a.S.EntrypointDir, "codex-bg.sh")
	if _, e := os.Stat(p); e == nil && !a.S.Installed {
		return errors.New("容器钩子已被其他安装占用")
	}
	content := []byte("#!/bin/sh\n# codex-bg managed container startup\n" + shellQuote(a.P.Binary()) + " --home " + shellQuote(a.P.Home) + " --runtime " + shellQuote(a.P.Runtime) + " autostart || { printf 'Codex background failed; inspect installer-data/logs\\n' >&2; exit 0; }\n")
	return a.txWrite(t, p, content, 0755, "entrypoint")
}
func (a *App) Recover(ctx context.Context) error {
	unlock, e := a.Lock()
	if e != nil {
		return e
	}
	defer unlock()
	return a.recoverLocked(ctx)
}
func (a *App) recoverLocked(ctx context.Context) error {
	raw, e := os.ReadFile(filepath.Join(a.P.Data, "transactions", "active.json"))
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	var t Transaction
	if e = json.Unmarshal(raw, &t); e != nil {
		return e
	}
	if t.Action == "legacy-updater-cleanup" {
		return a.recoverLegacyUpdaterCleanup(ctx, &t)
	}
	current, e := a.readRuntimeControl()
	if e != nil {
		return e
	}
	restoreRuntime := !t.Previous.Installed || t.Stage == "switching" || current.Operation == t.ID
	if restoreRuntime {
		if _, e = a.setRuntimeIntent("stopped", "recovering", t.ID, false); e != nil {
			return e
		}
	}
	if e = a.cleanupMigratedCodexApp(ctx, &t); e != nil {
		return e
	}
	// Restore bindings created by this transaction before dependencies disappear.
	old := map[string]bool{}
	for _, b := range t.Previous.Bindings {
		old[b.ID] = true
	}
	for _, b := range a.S.Bindings {
		if !old[b.ID] {
			if e = a.restoreBinding(b); e != nil {
				return e
			}
		}
	}
	if a.S.Installed && restoreRuntime {
		if e = a.Stop(ctx); e != nil {
			return e
		}
	}
	for i := len(t.Resources) - 1; i >= 0; i-- {
		r := t.Resources[i]
		h, e := Hash(r.Path)
		if r.Deleted && errors.Is(e, os.ErrNotExist) {
			if e = a.RestoreBackup(r.BackupID); e != nil {
				return e
			}
			continue
		}
		if errors.Is(e, os.ErrNotExist) && r.Created {
			continue
		}
		if e != nil {
			return e
		}
		if h != r.Hash {
			if r.BackupID != "" {
				b, er := a.ReadBackup(r.BackupID)
				if er == nil && h == b.Hash {
					continue
				}
			}
			return fmt.Errorf("恢复冲突，保留 %s", r.Path)
		}
		if r.Created {
			if e = os.Remove(r.Path); e != nil {
				return e
			}
		} else {
			if e = a.RestoreBackup(r.BackupID); e != nil {
				return e
			}
		}
	}
	if a.S.Backend == "systemd" {
		if !t.Previous.Installed {
			_, e = a.Run(ctx, "systemctl", "--user", "disable", "codex-bg.service")
			if e != nil {
				return e
			}
		}
		if _, e = a.Run(ctx, "systemctl", "--user", "daemon-reload"); e != nil {
			return e
		}
	}
	if a.S.LingerOwned && !t.Previous.LingerOwned {
		if _, e = a.Run(ctx, "sudo", "loginctl", "disable-linger", os.Getenv("USER")); e != nil {
			return e
		}
	}
	for _, s := range t.LegacySettings {
		raw, e := os.ReadFile(s.Path)
		if e != nil {
			return e
		}
		v, present, e := configValue(raw, "chatgpt.cliExecutable")
		if e != nil {
			return e
		}
		if present && v != s.Value {
			return fmt.Errorf("旧配置恢复冲突 %s", s.Path)
		}
		if !present {
			out, e := editConfigKey(raw, "chatgpt.cliExecutable", &s.Value)
			if e != nil {
				return e
			}
			info, _ := os.Stat(s.Path)
			if e = AtomicWrite(s.Path, out, info.Mode().Perm()); e != nil {
				return e
			}
		}
	}
	if len(t.LegacyUnits) > 0 {
		if _, e = a.Run(ctx, "systemctl", "--user", "daemon-reload"); e != nil {
			return e
		}
		for _, u := range t.LegacyUnits {
			if u.Enabled {
				if _, e = a.Run(ctx, "systemctl", "--user", "enable", u.Name); e != nil {
					return e
				}
			}
			if u.Active {
				if _, e = a.Run(ctx, "systemctl", "--user", "start", u.Name); e != nil {
					return e
				}
			}
		}
	}
	if t.LegacyDaemon != "" {
		if _, e = a.Run(ctx, t.LegacyDaemon, "app-server", "daemon", "start"); e != nil {
			return e
		}
	}
	if e = a.restoreLegacyUpdater(ctx, &t); e != nil {
		return e
	}
	if t.LegacyControl != "" {
		if _, e = a.Run(ctx, "/bin/bash", t.LegacyControl, "start"); e != nil {
			return e
		}
	}
	if e = a.restoreLegacyCodexApp(ctx, &t); e != nil {
		return e
	}
	// Keep explicitly installed components even when core selection fails.
	components := a.S.Components
	a.S = t.Previous
	if a.S.Components == nil {
		a.S.Components = map[string]*Component{}
	}
	for k, v := range components {
		if _, ok := a.S.Components[k]; !ok {
			a.S.Components[k] = v
		}
	}
	if e = a.Save(); e != nil {
		return e
	}
	if restoreRuntime {
		if e = a.restoreRuntimeIntent(ctx, &t); e != nil {
			return e
		}
	}
	if t.Action == "update" {
		t.Action = "update-restored"
	}
	if t.Action == "migrate" {
		t.Action = "migrate-restored"
	}
	return a.commit(&t)
}
func (a *App) Service(ctx context.Context, action string, force bool) error {
	unlock, e := a.Lock()
	if e != nil {
		return e
	}
	defer unlock()
	if !a.S.Installed {
		return errors.New("后台尚未安装；inspect可检查已有环境")
	}
	control, e := a.readRuntimeControl()
	if e != nil {
		return e
	}
	if hasPending(a.P.Data) || control.Mode != "normal" {
		return errors.New("存在未完成维护；先 recover，不能启动或重置重试额度")
	}
	switch action {
	case "start":
		if control.Status == "stopping" || control.Status == "stop_failed" {
			return errors.New("停止尚未确认完成，请先重试 stop")
		}
		// Starting an already live supervisor is idempotent. Changing its
		// generation here would interrupt the very tasks we are preserving.
		if control.Desired == "running" && (control.Status == "running" || control.Status == "starting" || control.Status == "retrying") {
			live := false
			if a.S.Backend == "systemd" {
				_, e := a.Run(ctx, "systemctl", "--user", "is-active", "--quiet", "codex-bg.service")
				live = e == nil
			} else if p, e := a.runtimeReadPID(); e == nil {
				live = runtimeValidPID(p)
			}
			if live {
				return a.Start(ctx)
			}
		}
	case "stop", "restart":
		if !force && !a.runtimeKnownNotRunning() {
			busy, e := a.Busy(ctx)
			if e != nil {
				return fmt.Errorf("不能确认后台空闲；明确 --force 才能中断: %w", e)
			}
			if busy {
				return errors.New("存在活跃任务；稍后操作，或明确 --force 中断")
			}
		}
	default:
		return errors.New("未知服务操作")
	}
	if e := a.stopRuntimeFor(ctx, "normal", ""); e != nil {
		return e
	}
	if action == "start" || action == "restart" {
		if _, e := a.setRuntimeIntent("running", "normal", "", true); e != nil {
			return e
		}
		if a.S.Backend == "systemd" {
			if _, e := a.Run(ctx, "systemctl", "--user", "reset-failed", "codex-bg.service"); e != nil {
				return e
			}
		}
		return a.Start(ctx)
	}
	return nil
}
func (a *App) Update(ctx context.Context, version string, force bool) (err error) {
	if !a.S.Installed {
		return errors.New("尚未安装")
	}
	if e := a.checkManaged(); e != nil {
		return e
	}
	if version != "" && a.S.Codex.Source != "managed" {
		return errors.New("外部Codex只复用，不自动升级")
	}
	if !force && !a.runtimeKnownNotRunning() {
		b, e := a.Busy(ctx)
		if e != nil || b {
			return fmt.Errorf("%w：后台忙碌或无法确认空闲；更新未应用", errUpdateBusy)
		}
	}
	unlock, e := a.Lock()
	if e != nil {
		return e
	}
	defer unlock()
	if !a.S.Installed {
		return errors.New("核心已被其他操作卸载")
	}
	if e = a.checkManaged(); e != nil {
		return e
	}
	t, e := a.begin("update")
	if e != nil {
		return e
	}
	defer func() {
		if err != nil {
			if re := a.recoverLocked(context.Background()); re != nil {
				err = fmt.Errorf("%w；恢复未完成 %v", err, re)
			}
		}
	}()
	if version != "" {
		if a.S.Codex.Source != "managed" {
			return errors.New("外部Codex只复用，不自动升级；先选择工具管理版本")
		}
		c, e := a.InstallManagedCodex(ctx, version)
		if e != nil {
			return e
		}
		a.S.Codex = c
	}
	// Pin the bytes of this running Linux executable. A long idle wait must
	// not silently deploy a different build later placed at the same pathname.
	data, e := os.ReadFile("/proc/self/exe")
	if e != nil {
		return e
	}
	if !force && !a.runtimeKnownNotRunning() {
		busy, e := a.Busy(ctx)
		if e != nil || busy {
			return fmt.Errorf("%w：准备期间出现活跃任务或无法确认空闲，暂不切换", errUpdateBusy)
		}
	}
	t.Stage = "switching"
	if e = a.saveTx(t); e != nil {
		return e
	}
	if e = a.stopRuntimeFor(ctx, "updating", t.ID); e != nil {
		return e
	}
	if a.S.AutoRestart == nil {
		enabled := true
		a.S.AutoRestart = &enabled
	}
	if e = a.txWrite(t, a.P.Binary(), data, 0755, "binary"); e != nil {
		return e
	}
	// Older Go hooks invoked an explicit start on boot. Refresh them as part
	// of the same transaction so stopped/exhausted intent survives rebuilds.
	if e = a.installAdapter(ctx, t); e != nil {
		return e
	}
	if e = a.Save(); e != nil {
		return e
	}
	if e = a.resumeRuntimeAfterMaintenance(ctx, t.PreviousControl, "updating", t.ID); e != nil {
		return e
	}
	return a.commit(t)
}
func (a *App) Rollback(ctx context.Context, id string, force bool) error {
	if !validID(id) {
		return errors.New("rollback需history事务ID")
	}
	raw, e := os.ReadFile(filepath.Join(a.P.Data, "history", id+".json"))
	if e != nil {
		return e
	}
	var t Transaction
	if e = json.Unmarshal(raw, &t); e != nil {
		return e
	}
	if t.Action != "update" {
		return errors.New("只能回退update事务")
	}
	if !force && !a.runtimeKnownNotRunning() {
		busy, e := a.Busy(ctx)
		if e != nil || busy {
			return errors.New("无法确认空闲，未回退")
		}
	}
	unlock, e := a.Lock()
	if e != nil {
		return e
	}
	defer unlock()
	if hasPending(a.P.Data) {
		return errors.New("先恢复未完成事务")
	}
	// Rollback restores old files, but preserves the user's current running
	// intent and retry budget, not those from the historical update day.
	control, e := a.readRuntimeControl()
	if e != nil {
		return e
	}
	if control.Mode != "normal" {
		return errors.New("后台处于维护状态，请先 recover")
	}
	t.PreviousControl = &control
	t.Stage = "switching"
	if e = a.saveTx(&t); e != nil {
		return e
	}
	if _, e = a.setRuntimeIntent("stopped", "rolling_back", t.ID, false); e != nil {
		return e
	}
	return a.recoverLocked(ctx)
}
func (a *App) Uninstall(ctx context.Context, force bool) error {
	if !a.S.Installed {
		return errors.New("未发现本工具管理的核心；不会删除外部环境")
	}
	if c := a.S.Components["codexapp"]; c != nil && c.Status == "installed" {
		return errors.New("CodexApp仍依赖核心；先停用或卸载该组件")
	}
	if e := a.checkManaged(); e != nil {
		return e
	}
	if !force && !a.runtimeKnownNotRunning() {
		busy, e := a.Busy(ctx)
		if e != nil || busy {
			return errors.New("后台忙碌或状态未知；未卸载")
		}
	}
	unlock, e := a.Lock()
	if e != nil {
		return e
	}
	defer unlock()
	if !a.S.Installed {
		return errors.New("核心已被其他操作卸载")
	}
	if e = a.checkManaged(); e != nil {
		return e
	}
	if hasPending(a.P.Data) {
		return errors.New("先recover")
	}
	for _, b := range a.S.Bindings {
		if h, e := Hash(b.Entry); e == nil && h == b.Hash {
			if e = a.VerifyBackup(b.BackupID); e != nil {
				return fmt.Errorf("恢复材料缺失 %s；保留后台", b.Entry)
			}
		}
	}
	if _, e = a.setRuntimeIntent("stopped", "uninstalling", "", false); e != nil {
		return e
	}
	for _, r := range append([]string{}, a.S.SelectedRoots...) {
		if e = a.DetachRoot(ctx, r); e != nil {
			return e
		}
	}
	if e := a.Stop(ctx); e != nil {
		return e
	}
	if a.S.Backend == "systemd" {
		if _, e := a.Run(ctx, "systemctl", "--user", "disable", "codex-bg.service"); e != nil {
			return e
		}
	}
	for _, f := range a.S.Files {
		if e := os.Remove(f.Path); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	if a.S.Backend == "systemd" {
		if _, e := a.Run(ctx, "systemctl", "--user", "daemon-reload"); e != nil {
			return e
		}
	}
	if a.S.LingerOwned {
		if _, e := a.Run(ctx, "sudo", "loginctl", "disable-linger", os.Getenv("USER")); e != nil {
			return e
		}
	}
	a.S.Installed = false
	a.S.Files = nil
	a.S.LingerOwned = false
	if _, e = a.setRuntimeIntent("stopped", "normal", "", false); e != nil {
		return e
	}
	if _, e = a.updateRuntimeControl("", func(c *runtimeControl) error { c.Status = "stopped"; c.Child = nil; return nil }); e != nil {
		return e
	}
	if e := a.Save(); e != nil {
		return e
	}
	// Components and their ownership records remain for separately selected cleanup.
	if len(a.S.Components) == 0 {
		for _, p := range []string{"run", "connections"} {
			if e := os.RemoveAll(filepath.Join(a.P.Runtime, p)); e != nil {
				return e
			}
		}
		if e := os.Remove(a.P.StateFile()); e != nil {
			return e
		}
		_ = os.Remove(filepath.Join(a.P.Runtime, "operation.lock"))
		_ = os.Remove(a.P.Runtime)
	}
	fmt.Fprintf(a.Out, "核心已卸载。维护资料保留在 %s；备份/日志可集中清理。\n", a.P.Data)
	return nil
}
func (a *App) Logs() string {
	dir := filepath.Join(a.P.Data, "logs")
	items, e := os.ReadDir(dir)
	if e != nil {
		return e.Error()
	}
	text := "日志目录: " + dir + "\n"
	for _, x := range items {
		if !x.IsDir() {
			i, _ := x.Info()
			text += fmt.Sprintf("%s · %d bytes\n", x.Name(), i.Size())
		}
	}
	return text
}
