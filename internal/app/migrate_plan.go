package app

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Preserve the reviewed v3 controller as immutable comparison data. Migration
// executes only the matching controller in the existing installation, never
// this embedded copy or a reference supplied by the installer directory.
//
//go:embed legacydata/v3-control.sh.txt
var legacyV3Controller []byte

type legacyEntryRecord struct {
	ex                      Extension
	backup                  string
	entryHash, originalHash string
}
type migrationFile struct{ Path, Hash string }
type migrationPlan struct {
	Options       InstallOptions
	Entries       []legacyEntryRecord
	Settings      []LegacySetting
	Units         []migrationFile
	Hooks         []migrationFile
	Control       string
	Daemon        string
	Socket        string
	UnitSockets   map[string]string
	ControlSocket string
	CodexApp      *LegacyCodexAppPlan
	Updater       *LegacyUpdaterPlan
}

// An empty unix:// address is the Codex default socket. Explicit legacy
// addresses are accepted only as absolute paths; shell expansions are not run.
func (a *App) legacyUnitSocket(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	match := regexp.MustCompile(`unix://([^"'[:space:]]*)`).FindStringSubmatch(string(raw))
	if len(match) != 2 {
		return ""
	}
	if match[1] == "" {
		return filepath.Join(a.S.CodexHome, "app-server-control/app-server-control.sock")
	}
	socket := strings.ReplaceAll(match[1], "%h", a.P.Home)
	if !filepath.IsAbs(socket) || strings.ContainsAny(socket, "$%\\") {
		return ""
	}
	return socket
}

func (a *App) checkLegacyUnitIdle(ctx context.Context, file migrationFile, socket string, force bool) error {
	name := filepath.Base(file.Path)
	if name != "codex-app-server.service" && name != "codex-background.service" {
		return nil
	}
	status, err := a.Run(ctx, "systemctl", "--user", "is-active", name)
	state := strings.TrimSpace(string(status))
	if state == "inactive" || state == "failed" {
		return nil
	}
	if state != "active" || err != nil || socket == "" {
		if force {
			fmt.Fprintln(a.Out, "明确强制迁移：无法确认旧服务空闲:", name)
			return nil
		}
		return fmt.Errorf("不能确认旧服务 %s 的运行状态或 socket；未迁移", name)
	}
	return a.checkMigrationIdle(ctx, socket, force)
}

func (a *App) checkMigrationPlanIdle(ctx context.Context, p *migrationPlan, force bool) error {
	if err := a.checkMigrationIdle(ctx, p.Socket, force); err != nil {
		return err
	}
	if err := a.checkMigrationIdle(ctx, p.ControlSocket, force); err != nil {
		return err
	}
	for _, file := range p.Units {
		if _, err := a.inspectLegacyUnitState(ctx, filepath.Base(file.Path)); err != nil {
			return err
		}
		if err := a.checkLegacyUnitIdle(ctx, file, p.UnitSockets[file.Path], force); err != nil {
			return err
		}
	}
	return nil
}

// Preview and execution share the complete read-only preflight. In particular,
// dry-run must not skip dependent services, original files or busy checks.
func (a *App) PreviewMigration(ctx context.Context, opt InstallOptions) (string, error) {
	copy := *a
	var output bytes.Buffer
	copy.Out, copy.Err = &output, &output
	_, err := copy.planMigration(ctx, opt)
	return output.String(), err
}

func (a *App) legacyCLI(path string) bool {
	for _, rel := range []string{"apps/codex-daemon-bridge/codex", ".local/share/codex-daemon-bridge/codex", ".local/share/codex-background/codex"} {
		if path == filepath.Join(a.P.Home, rel) {
			return true
		}
	}
	return false
}

func (a *App) migrationSettings(roots []string) ([]LegacySetting, error) {
	selected := map[string]bool{}
	for _, r := range roots {
		selected[filepath.Clean(r)] = true
	}
	settings := []LegacySetting{}
	for _, pair := range [][2]string{
		{".vscode-server/extensions", ".vscode-server/data/Machine/settings.json"},
		{".vscode-server-insiders/extensions", ".vscode-server-insiders/data/Machine/settings.json"},
		{".local/share/code-server/extensions", ".local/share/code-server/User/settings.json"},
		{".vscode/extensions", ".config/Code/User/settings.json"},
		{".vscode-insiders/extensions", ".config/Code - Insiders/User/settings.json"},
		{".vscode-oss/extensions", ".config/Code - OSS/User/settings.json"},
		{".vscode-codium/extensions", ".config/VSCodium/User/settings.json"},
	} {
		root, path := filepath.Join(a.P.Home, pair[0]), filepath.Join(a.P.Home, pair[1])
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		value, present, err := configValue(raw, "chatgpt.cliExecutable")
		if err != nil {
			return nil, fmt.Errorf("无法检查 IDE 配置 %s: %w", path, err)
		}
		if !present || value == "" {
			continue
		}
		if !selected[root] {
			if a.legacyCLI(value) {
				return nil, fmt.Errorf("未选择的 IDE 仍依赖旧后台: %s；请把 %s 纳入迁移或先解除其旧后台依赖", path, root)
			}
			continue
		}
		if !a.legacyCLI(value) {
			return nil, fmt.Errorf("未知CLI覆盖 %s，保留并要求手动确认来源", path)
		}
		if err := checkParents(path); err != nil {
			return nil, err
		}
		settings = append(settings, LegacySetting{path, value})
	}
	return settings, nil
}

func migrationFileInfo(path string) (migrationFile, error) {
	if err := checkParents(path); err != nil {
		return migrationFile{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return migrationFile{}, err
	}
	if !info.Mode().IsRegular() {
		return migrationFile{}, fmt.Errorf("迁移来源不是普通文件: %s", path)
	}
	hash, err := Hash(path)
	return migrationFile{path, hash}, err
}
func (f migrationFile) unchanged() error {
	current, err := migrationFileInfo(f.Path)
	if err != nil {
		return err
	}
	if current.Hash != f.Hash {
		return fmt.Errorf("预检查后文件已变化，未接管: %s", f.Path)
	}
	return nil
}

func (a *App) checkMigrationIdle(ctx context.Context, socket string, force bool) error {
	if socket == "" {
		return nil
	}
	probe := *a
	probe.P.Socket = socket
	busy, err := probe.Busy(ctx)
	if err != nil || busy {
		if force {
			fmt.Fprintln(a.Out, "明确强制迁移：将中断旧后台；活跃状态检查未通过。")
			return nil
		}
		if err != nil {
			return fmt.Errorf("不能确认旧后台空闲；未迁移: %w", err)
		}
		return errors.New("旧后台存在活跃任务；请等待任务结束并停止旧客户端发送新请求后迁移")
	}
	fmt.Fprintln(a.Out, "旧后台：当前空闲；切换前会再次检查，请勿同时发起新任务。")
	return nil
}

func (a *App) planMigration(ctx context.Context, opt InstallOptions) (*migrationPlan, error) {
	if opt.MigrateCodexApp && hasString(opt.Components, "codexapp") {
		return nil, errors.New("旧 CodexApp 迁移与新增 codexapp 组件不能同时选择；迁移请仅使用 --migrate-codexapp")
	}
	inv, opt, err := a.PlanInstall(ctx, opt)
	if err != nil {
		return nil, err
	}
	if a.S.Installed {
		return nil, errors.New("新版已安装；使用repair/update")
	}
	if hasPending(a.P.Data) {
		return nil, errors.New("存在未完成事务，先 recover")
	}
	if len(inv.Legacy) == 0 {
		return nil, errors.New("未发现旧部署；使用install")
	}
	p := &migrationPlan{Options: opt, UnitSockets: map[string]string{}}
	for _, s := range inv.Legacy {
		fmt.Fprintln(a.Out, "迁移检查:", s)
	}
	for _, ext := range inv.Extensions {
		if ext.Legacy && !hasString(opt.Roots, ext.Root) {
			return nil, fmt.Errorf("未选择的扩展仍接入旧后台: %s；请纳入迁移或先解除依赖", ext.Root)
		}
	}
	for _, root := range opt.Roots {
		exts, err := scanRoot(root)
		if err != nil {
			return nil, err
		}
		for _, ext := range exts {
			if !ext.Legacy {
				continue
			}
			backup := ext.Entry + ".real"
			head := readPrefix(backup, 2048)
			if !executable(backup) || legacyEntry(head) || strings.Contains(head, Marker) {
				return nil, fmt.Errorf("原入口备份缺失或无效: %s；先重装扩展", backup)
			}
			original, err := migrationFileInfo(backup)
			if err != nil {
				return nil, err
			}
			wrapper, err := migrationFileInfo(ext.Entry)
			if err != nil {
				return nil, err
			}
			p.Entries = append(p.Entries, legacyEntryRecord{ext, backup, wrapper.Hash, original.Hash})
		}
	}
	if p.Settings, err = a.migrationSettings(opt.Roots); err != nil {
		return nil, err
	}
	for _, setting := range p.Settings {
		fmt.Fprintln(a.Out, "计划移除已选 IDE 的旧CLI覆盖:", setting.Path)
	}
	for _, name := range []string{"codex-extension-hook.timer", "codex-extension-hook.service", "codex-app-server.service", "codex-background.service"} {
		path := filepath.Join(a.P.Home, ".config/systemd/user", name)
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		known := strings.Contains(string(raw), "vscode-codex-background-deploy") || strings.Contains(string(raw), "Description=Codex app-server background runtime") || strings.Contains(string(raw), "Description=Codex background runtime (managed)")
		if !known {
			return nil, fmt.Errorf("旧服务来源未知，保留 %s", path)
		}
		if !inv.Systemd {
			return nil, fmt.Errorf("存在旧 systemd 服务但用户管理器不可用: %s", path)
		}
		file, err := migrationFileInfo(path)
		if err != nil {
			return nil, err
		}
		p.Units = append(p.Units, file)
		p.UnitSockets[path] = a.legacyUnitSocket(path)
	}
	control := filepath.Join(a.P.Home, ".local/share/codex-background/payload/control.sh")
	if _, err := os.Stat(filepath.Join(a.P.Home, ".local/share/codex-background/managed-v3")); err == nil {
		hash, err := Hash(control)
		if err != nil || hash != HashBytes(legacyV3Controller) {
			return nil, errors.New("v3控制器与内置原件不一致或无法读取；需人工校验后迁移")
		}
		p.Control = control
		v3Socket := filepath.Join(a.P.Home, ".local/state/codex-background/app-server.sock")
		if _, err := os.Stat(v3Socket); err == nil {
			p.ControlSocket = v3Socket
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if _, err := os.Stat(filepath.Join(a.P.Home, ".local/state/codex-background/supervisor.pid")); err == nil {
			// A missing socket is not proof that a recorded supervisor is idle.
			// Probe the expected address so the normal unknown/busy gate applies.
			p.ControlSocket = v3Socket
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	dirs := []string{filepath.Join(a.P.Home, "entrypoint.d")}
	if opt.EntrypointDir != dirs[0] {
		dirs = append(dirs, opt.EntrypointDir)
	}
	for _, dir := range dirs {
		for _, name := range []string{"05-start-codex-daemon.sh", "20-codex-background.sh"} {
			path := filepath.Join(dir, name)
			raw, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			known := strings.Contains(string(raw), "app-server daemon start") && strings.Contains(string(raw), ".codex/packages/standalone/current/codex")
			known = known || p.Control != "" && strings.Contains(string(raw), "codex-background") && strings.Contains(string(raw), "control.sh")
			if !known {
				return nil, fmt.Errorf("未知容器启动脚本，保留 %s", path)
			}
			file, err := migrationFileInfo(path)
			if err != nil {
				return nil, err
			}
			p.Hooks = append(p.Hooks, file)
		}
	}
	// The v3 controller delegates to this same unit in systemd mode. The unit
	// is already stopped/restored by the transaction; do not invoke it twice or
	// probe its removed socket after stopping it.
	for _, unit := range p.Units {
		if filepath.Base(unit.Path) == "codex-background.service" {
			p.Control, p.ControlSocket = "", ""
		}
	}
	if opt.CodexPath != "managed" {
		if _, err = a.validateCodex(ctx, opt.CodexPath); err != nil {
			return nil, err
		}
	}
	p.CodexApp, err = a.planLegacyCodexApp(ctx, opt)
	if err != nil {
		return nil, err
	}
	if opt.MigrateCodexApp && p.CodexApp == nil {
		return nil, errors.New("未发现可交接的旧 CodexApp；新装或复用程序请使用可选组件")
	}
	socket := filepath.Join(a.S.CodexHome, "app-server-control/app-server-control.sock")
	if _, err := os.Stat(socket); err == nil {
		unitOwnsSocket := false
		for _, unitSocket := range p.UnitSockets {
			if unitSocket == socket {
				unitOwnsSocket = true
			}
		}
		if !unitOwnsSocket {
			knownNativeHook := false
			for _, hook := range p.Hooks {
				if filepath.Base(hook.Path) != "05-start-codex-daemon.sh" {
					continue
				}
				raw, readErr := os.ReadFile(hook.Path)
				if readErr != nil {
					return nil, readErr
				}
				if bytes.Contains(raw, []byte("app-server daemon start")) && bytes.Contains(raw, []byte(".codex/packages/standalone/current/codex")) {
					knownNativeHook = true
				}
			}
			if !knownNativeHook {
				return nil, errors.New("检测到 native daemon，但没有本次迁移拥有的旧启动项；保留该独立服务，请先明确其归属")
			}
			p.Socket = socket
			cli := filepath.Join(a.P.Home, ".codex/packages/standalone/current/codex")
			if !executable(cli) {
				cli = opt.CodexPath
			}
			if cli == "managed" {
				return nil, errors.New("无法定位旧 daemon 的控制程序")
			}
			if _, err := a.validateCodex(ctx, cli); err != nil {
				return nil, fmt.Errorf("旧 daemon 控制程序校验失败: %w", err)
			}
			p.Daemon = cli
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if p.Updater, err = a.planLegacyUpdater(p.Daemon, ""); err != nil {
		return nil, err
	}
	if err = a.checkMigrationPlanIdle(ctx, p, opt.Force); err != nil {
		return nil, err
	}
	fmt.Fprintln(a.Out, "迁移预检查完成；执行时将在同一管理锁下重新检查并切换。")
	return p, nil
}
