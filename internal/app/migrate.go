package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (a *App) Migrate(ctx context.Context, opt InstallOptions) (err error) {
	inv, opt, e := a.PlanInstall(ctx, opt)
	if e != nil {
		return e
	}
	if a.S.Installed {
		return errors.New("新版已安装；使用repair/update")
	}
	if len(inv.Legacy) == 0 {
		return errors.New("未发现旧部署；使用install")
	}
	for _, s := range inv.Legacy {
		fmt.Fprintln(a.Out, "迁移检查:", s)
	}
	// Validate backups and configuration targets before pausing any service.
	type legacyEntryRecord struct {
		ex     Extension
		backup string
	}
	var entries []legacyEntryRecord
	for _, r := range opt.Roots {
		exts, e := scanRoot(r)
		if e != nil {
			return e
		}
		for _, ex := range exts {
			if !ex.Legacy {
				continue
			}
			backup := ex.Entry + ".real"
			head := readPrefix(backup, 2048)
			if !executable(backup) || legacyEntry(head) || strings.Contains(head, Marker) {
				return fmt.Errorf("原入口备份缺失或无效: %s；先重装扩展", backup)
			}
			entries = append(entries, legacyEntryRecord{ex, backup})
		}
	}
	settings := []LegacySetting{}
	for _, p := range []string{filepath.Join(a.P.Home, ".vscode-server/data/Machine/settings.json"), filepath.Join(a.P.Home, ".vscode-server-insiders/data/Machine/settings.json"), filepath.Join(a.P.Home, ".local/share/code-server/User/settings.json")} {
		raw, e := os.ReadFile(p)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		v, present, e := configValue(raw, "chatgpt.cliExecutable")
		if e != nil {
			return e
		}
		if present && v != "" {
			known := v == filepath.Join(a.P.Home, "apps/codex-daemon-bridge/codex") || v == filepath.Join(a.P.Home, ".local/share/codex-daemon-bridge/codex") || v == filepath.Join(a.P.Home, ".local/share/codex-background/codex")
			if !known {
				return fmt.Errorf("未知CLI覆盖 %s，保留并要求手动确认来源", p)
			}
			settings = append(settings, LegacySetting{p, v})
			fmt.Fprintln(a.Out, "移除已确认旧CLI覆盖:", p)
		}
	}
	if opt.DryRun {
		return nil
	}
	if !opt.Yes {
		return errors.New("未确认迁移")
	}
	oldSocket := filepath.Join(a.S.CodexHome, "app-server-control/app-server-control.sock")
	if _, e = os.Stat(oldSocket); e == nil && !opt.Force {
		probe := *a
		probe.P.Socket = oldSocket
		if busy, e := probe.Busy(ctx); e != nil || busy {
			return errors.New("旧后台忙碌或不能确认空闲；稍后迁移，或明确--force中断")
		}
	}
	unlock, e := a.Lock()
	if e != nil {
		return e
	}
	t, e := a.begin("migrate")
	if e != nil {
		unlock()
		return e
	}
	failed := true
	defer func() {
		if failed {
			if unlock == nil {
				var le error
				unlock, le = a.Lock()
				if le != nil {
					err = fmt.Errorf("%v；未获得恢复锁: %w", err, le)
					return
				}
			}
			if re := a.recoverLocked(context.Background()); re != nil {
				err = fmt.Errorf("%v；旧部署恢复未完成: %w", err, re)
			}
		}
		if unlock != nil {
			unlock()
		}
	}()
	// Systemd resources are touched only with the old installer markers.
	for _, name := range []string{"codex-extension-hook.timer", "codex-extension-hook.service", "codex-app-server.service", "codex-background.service"} {
		p := filepath.Join(a.P.Home, ".config/systemd/user", name)
		raw, e := os.ReadFile(p)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		known := strings.Contains(string(raw), "vscode-codex-background-deploy") || strings.Contains(string(raw), "Description=Codex app-server background runtime") || strings.Contains(string(raw), "Description=Codex background runtime (managed)")
		if !known {
			return fmt.Errorf("旧服务来源未知，保留 %s", p)
		}
		active, _ := a.Run(ctx, "systemctl", "--user", "is-active", name)
		enabled, _ := a.Run(ctx, "systemctl", "--user", "is-enabled", name)
		t.LegacyUnits = append(t.LegacyUnits, LegacyUnit{name, strings.TrimSpace(string(active)) == "active", strings.TrimSpace(string(enabled)) == "enabled"})
		if e = a.saveTx(t); e != nil {
			return e
		}
		if _, e = a.Run(ctx, "systemctl", "--user", "disable", "--now", name); e != nil {
			return e
		}
		b, e := a.BackupFile(p, "legacy-service", t.ID)
		if e != nil {
			return e
		}
		t.Resources = append(t.Resources, TxResource{Path: p, BackupID: b.ID, Deleted: true})
		if e = a.saveTx(t); e != nil {
			return e
		}
		if e = os.Remove(p); e != nil {
			return e
		}
	}
	if len(t.LegacyUnits) > 0 {
		if _, e = a.Run(ctx, "systemctl", "--user", "daemon-reload"); e != nil {
			return e
		}
	}
	oldControl := filepath.Join(a.P.Home, ".local/share/codex-background/payload/control.sh")
	if _, e = os.Stat(filepath.Join(a.P.Home, ".local/share/codex-background/managed-v3")); e == nil {
		source := filepath.Join(a.P.Installer, "legacy/v3/payload/control.sh")
		h1, e1 := Hash(source)
		h2, e2 := Hash(oldControl)
		if e1 != nil || e2 != nil || h1 != h2 {
			return errors.New("v3控制器与保存来源不一致；需人工校验后迁移")
		}
		t.LegacyControl = oldControl
		if e = a.saveTx(t); e != nil {
			return e
		}
		if _, e = a.Run(ctx, "/bin/bash", oldControl, "stop"); e != nil {
			return e
		}
	}
	for _, p := range []string{filepath.Join(a.P.Home, "entrypoint.d/05-start-codex-daemon.sh"), filepath.Join(a.P.Home, "entrypoint.d/20-codex-background.sh")} {
		raw, e := os.ReadFile(p)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		if !strings.Contains(string(raw), "codex") {
			return errors.New("未知容器启动脚本")
		}
		b, e := a.BackupFile(p, "legacy-entrypoint", t.ID)
		if e != nil {
			return e
		}
		t.Resources = append(t.Resources, TxResource{Path: p, BackupID: b.ID, Deleted: true})
		if e = a.saveTx(t); e != nil {
			return e
		}
		if e = os.Remove(p); e != nil {
			return e
		}
	}
	if _, e = os.Stat(oldSocket); e == nil {
		cli := opt.CodexPath
		if cli == "managed" {
			cli = filepath.Join(a.P.Home, ".codex/packages/standalone/current/codex")
		}
		t.LegacyDaemon = cli
		if e = a.saveTx(t); e != nil {
			return e
		}
		if _, e = a.Run(ctx, cli, "app-server", "daemon", "stop"); e != nil {
			return e
		}
	}
	for _, entry := range entries {
		b, e := a.BackupFile(entry.ex.Entry, "legacy-wrapper", t.ID)
		if e != nil {
			return e
		}
		original, e := a.BackupFile(entry.backup, "legacy-original", t.ID)
		if e != nil {
			return e
		}
		r := TxResource{Path: entry.ex.Entry, BackupID: b.ID, Hash: original.Hash}
		t.Resources = append(t.Resources, r)
		if e = a.saveTx(t); e != nil {
			return e
		}
		if e = a.ReplaceFile(original.DataPath, entry.ex.Entry, os.FileMode(original.Mode)); e != nil {
			return e
		}
		// The verified legacy original now lives centrally; do not leave another
		// installer backup beside the extension after successful migration.
		t.Resources = append(t.Resources, TxResource{Path: entry.backup, BackupID: original.ID, Deleted: true})
		if e = a.saveTx(t); e != nil {
			return e
		}
		if e = os.Remove(entry.backup); e != nil {
			return e
		}
	}
	for _, s := range settings {
		t.LegacySettings = append(t.LegacySettings, s)
		if e = a.saveTx(t); e != nil {
			return e
		}
		raw, e := os.ReadFile(s.Path)
		if e != nil {
			return e
		}
		out, e := editConfigKey(raw, "chatgpt.cliExecutable", nil)
		if e != nil {
			return e
		}
		info, e := os.Stat(s.Path)
		if e != nil {
			return e
		}
		if e = a.ReplaceBytes(s.Path, out, info.Mode().Perm()); e != nil {
			return e
		}
	}
	unlock()
	unlock = nil
	if e = a.installCore(ctx, opt); e != nil {
		return e
	}
	failed = false
	// Original legacy directories remain external archives; report paths for explicit cleanup.
	fmt.Fprintln(a.Out, "迁移完成；旧程序目录保留为复用/迁移来源，未删除外部Node、Conda或用户数据。")
	return a.installExtras(ctx, opt)
}

var _ = json.Valid
