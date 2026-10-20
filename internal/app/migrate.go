package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func (a *App) Migrate(ctx context.Context, opt InstallOptions) (err error) {
	plan, e := a.planMigration(ctx, opt)
	if e != nil {
		return e
	}
	if opt.DryRun {
		return nil
	}
	if !opt.Yes {
		return errors.New("未确认迁移")
	}
	unlock, e := a.Lock()
	if e != nil {
		return e
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	// Revalidate under the same lock that covers backups, old-service shutdown,
	// new startup and dependency handoff. A concurrent recover cannot interleave.
	plan, e = a.planMigration(ctx, opt)
	if e != nil {
		return e
	}
	opt = plan.Options
	t, e := a.begin("migrate")
	if e != nil {
		return e
	}
	committed := false
	defer func() {
		if !committed {
			if re := a.recoverLocked(context.Background()); re != nil {
				err = fmt.Errorf("%v；旧部署恢复未完成: %w", err, re)
			}
		}
	}()
	// Prepare a new managed CLI before stopping the old service. Reused CLIs
	// were already validated by the read-only preflight.
	if opt.CodexPath == "managed" {
		if _, e = a.InstallManagedCodex(ctx, opt.CodexVersion); e != nil {
			return e
		}
	}
	// Save all known originals before the first service is stopped.
	for _, file := range append(append([]migrationFile{}, plan.Units...), plan.Hooks...) {
		if e = file.unchanged(); e != nil {
			return e
		}
		b, backupErr := a.BackupFile(file.Path, "legacy-resource", t.ID)
		if backupErr != nil {
			return backupErr
		}
		if b.Hash != file.Hash {
			return errors.New("迁移备份期间来源文件变化")
		}
	}
	for _, entry := range plan.Entries {
		b, backupErr := a.BackupFile(entry.ex.Entry, "legacy-wrapper", t.ID)
		if backupErr != nil {
			return backupErr
		}
		if b.Hash != entry.entryHash {
			return errors.New("旧扩展入口在预检查后变化，未迁移")
		}
		original, backupErr := a.BackupFile(entry.backup, "legacy-original", t.ID)
		if backupErr != nil {
			return backupErr
		}
		if original.Hash != entry.originalHash {
			return errors.New("旧扩展原件在预检查后变化，未迁移")
		}
		if e = (migrationFile{entry.ex.Entry, entry.entryHash}).unchanged(); e != nil {
			return e
		}
		if e = (migrationFile{entry.backup, entry.originalHash}).unchanged(); e != nil {
			return e
		}
	}
	if e = a.prepareLegacyCodexApp(t, plan.CodexApp); e != nil {
		return e
	}
	if e = a.prepareLegacyUpdater(t, plan.Updater); e != nil {
		return e
	}
	if e = a.checkMigrationPlanIdle(ctx, plan, opt.Force); e != nil {
		return e
	}
	if e = a.stopLegacyCodexApp(ctx, t); e != nil {
		return e
	}
	// Systemd resources are touched only with the old installer markers.
	for _, file := range plan.Units {
		p, name := file.Path, filepath.Base(file.Path)
		if e = file.unchanged(); e != nil {
			return e
		}
		if e = a.checkLegacyUnitIdle(ctx, file, plan.UnitSockets[file.Path], opt.Force); e != nil {
			return e
		}
		unit, stateErr := a.inspectLegacyUnitState(ctx, name)
		if stateErr != nil {
			return stateErr
		}
		t.LegacyUnits = append(t.LegacyUnits, unit)
		if e = a.saveTx(t); e != nil {
			return e
		}
		if _, e = a.Run(ctx, "systemctl", "--user", "disable", "--now", name); e != nil {
			return e
		}
		b, e := a.BackupFile(p, "legacy-resource", t.ID)
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
	if plan.Control != "" {
		if e = a.checkMigrationIdle(ctx, plan.ControlSocket, opt.Force); e != nil {
			return e
		}
		t.LegacyControl = plan.Control
		if e = a.saveTx(t); e != nil {
			return e
		}
		if _, e = a.Run(ctx, "/bin/bash", plan.Control, "stop"); e != nil {
			return e
		}
	}
	for _, file := range plan.Hooks {
		p := file.Path
		if e = file.unchanged(); e != nil {
			return e
		}
		b, e := a.BackupFile(p, "legacy-resource", t.ID)
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
	if plan.Daemon != "" {
		// Recheck immediately before shutdown; forced migration is explicit.
		if e = a.checkMigrationIdle(ctx, plan.Socket, opt.Force); e != nil {
			return e
		}
		cli := plan.Daemon
		t.LegacyDaemon = cli
		if e = a.saveTx(t); e != nil {
			return e
		}
		if _, e = a.Run(ctx, cli, "app-server", "daemon", "stop"); e != nil {
			return e
		}
		// Stop the native updater only after the old model server stopped. If
		// anything fails, native daemon start must perform a fresh startup and
		// restore both processes, rather than returning early for a live server.
		if e = a.stopLegacyUpdater(ctx, t); e != nil {
			return e
		}
		if plan.Updater != nil {
			// The updater could finish an update between the first daemon stop
			// and its own SIGTERM. Recheck through the same native controller
			// after it is gone so no old model server survives the handoff.
			if _, e = a.Run(ctx, cli, "app-server", "daemon", "stop"); e != nil {
				return e
			}
		}
	}
	for _, entry := range plan.Entries {
		if e = (migrationFile{entry.ex.Entry, entry.entryHash}).unchanged(); e != nil {
			return e
		}
		if e = (migrationFile{entry.backup, entry.originalHash}).unchanged(); e != nil {
			return e
		}
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
	for _, s := range plan.Settings {
		raw, e := os.ReadFile(s.Path)
		if e != nil {
			return e
		}
		value, present, checkErr := configValue(raw, "chatgpt.cliExecutable")
		if checkErr != nil || !present || value != s.Value {
			return fmt.Errorf("IDE 配置在预检查后变化，保留 %s", s.Path)
		}
		out, e := editConfigKey(raw, "chatgpt.cliExecutable", nil)
		if e != nil {
			return e
		}
		info, e := os.Stat(s.Path)
		if e != nil {
			return e
		}
		t.LegacySettings = append(t.LegacySettings, s)
		if e = a.saveTx(t); e != nil {
			return e
		}
		if e = a.ReplaceBytes(s.Path, out, info.Mode().Perm()); e != nil {
			return e
		}
	}
	if e = a.installCoreLocked(ctx, opt, t); e != nil {
		return e
	}
	if e = a.configureMigratedCodexApp(ctx, t, plan.CodexApp); e != nil {
		return e
	}
	if e = a.commit(t); e != nil {
		return e
	}
	committed = true
	unlock()
	locked = false
	fmt.Fprintln(a.Out, "迁移完成；核心及已选择的旧 CodexApp 已验证，外部程序和用户数据保留。请 Reload Window。")
	return a.installExtras(ctx, opt)
}
