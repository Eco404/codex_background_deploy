package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (a *App) backupMeta(id string) string {
	return filepath.Join(a.P.Data, "backups", id, "backup.json")
}
func validID(id string) bool {
	if id == "" {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func (a *App) SaveBackup(b Backup) error {
	data, e := json.MarshalIndent(b, "", "  ")
	if e != nil {
		return e
	}
	return AtomicWrite(a.backupMeta(b.ID), data, 0600)
}
func (a *App) ReadBackup(id string) (Backup, error) {
	var b Backup
	if !validID(id) {
		return b, errors.New("无效备份ID")
	}
	raw, e := os.ReadFile(a.backupMeta(id))
	if e != nil {
		return b, e
	}
	e = json.Unmarshal(raw, &b)
	if e != nil {
		return b, e
	}
	if b.ID != id || !Within(filepath.Join(a.P.Data, "backups", id), b.DataPath) {
		return b, errors.New("备份目录记录无效")
	}
	return b, nil
}
func (a *App) Backups() ([]Backup, error) {
	entries, e := os.ReadDir(filepath.Join(a.P.Data, "backups"))
	if errors.Is(e, os.ErrNotExist) {
		return []Backup{}, nil
	}
	if e != nil {
		return nil, e
	}
	out := []Backup{}
	for _, entry := range entries {
		if !entry.IsDir() || !validID(entry.Name()) {
			continue
		}
		b, e := a.ReadBackup(entry.Name())
		if e != nil {
			return out, e
		}
		out = append(out, b)
	}
	return out, nil
}
func (a *App) BackupFile(path, kind, ref string) (Backup, error) {
	var b Backup
	if e := a.EnsureData(); e != nil {
		return b, e
	}
	info, e := os.Lstat(path)
	if e != nil {
		return b, e
	}
	if !info.Mode().IsRegular() {
		return b, fmt.Errorf("备份只接受普通文件: %s", path)
	}
	h, e := Hash(path)
	if e != nil {
		return b, e
	}
	items, e := a.Backups()
	if e != nil {
		return b, e
	}
	for _, old := range items {
		if !old.Deleted && old.Original == path && old.Hash == h && old.Ref == ref && old.Kind == kind {
			if got, e := Hash(old.DataPath); e == nil && got == h {
				return old, nil
			}
		}
	}
	b = Backup{ID: ID(), Original: path, Kind: kind, Hash: h, Mode: uint32(info.Mode().Perm()), Size: info.Size(), Created: time.Now().UTC(), Ref: ref}
	b.DataPath = filepath.Join(a.P.Data, "backups", b.ID, "content")
	if e = copyFile(path, b.DataPath, 0600); e != nil {
		return b, e
	}
	if check, e := Hash(b.DataPath); e != nil || check != h {
		return b, errors.New("备份校验失败")
	}
	if e = a.SaveBackup(b); e != nil {
		return b, e
	}
	return b, nil
}
func (a *App) VerifyBackup(id string) error {
	b, e := a.ReadBackup(id)
	if e != nil {
		return e
	}
	if b.Deleted {
		return errors.New("备份已删除，恢复能力缺失")
	}
	h, e := Hash(b.DataPath)
	if e != nil {
		return e
	}
	if h != b.Hash {
		return errors.New("备份校验不一致")
	}
	return nil
}
func (a *App) RestoreBackup(id string) error {
	b, e := a.ReadBackup(id)
	if e != nil {
		return e
	}
	if e = a.VerifyBackup(id); e != nil {
		return e
	}
	if e = a.ReplaceFile(b.DataPath, b.Original, os.FileMode(b.Mode)); e != nil {
		return e
	}
	h, e := Hash(b.Original)
	if e != nil || h != b.Hash {
		return errors.New("恢复后校验失败，备份已保留")
	}
	b.Ref = ""
	return a.SaveBackup(b)
}
func (a *App) backupReferenced(b Backup) bool {
	if b.Ref != "" {
		return true
	}
	for _, bind := range a.S.Bindings {
		if bind.BackupID == b.ID {
			return true
		}
	}
	for _, c := range a.S.Components {
		for _, id := range c.Backups {
			if id == b.ID {
				return true
			}
		}
	}
	return false
}
func (a *App) DeleteBackup(id string, force bool) error {
	b, e := a.ReadBackup(id)
	if e != nil {
		return e
	}
	if b.Deleted {
		return nil
	}
	if a.backupReferenced(b) && !force {
		return fmt.Errorf("备份仍被 %s 引用；若确实放弃恢复，需 --force 和显式确认", b.Ref)
	}
	// Write degradation first: a power loss must never claim recovery after deletion.
	if a.backupReferenced(b) {
		a.S.RecoveryMissing = appendUnique(a.S.RecoveryMissing, id)
		if e = a.Save(); e != nil {
			return e
		}
	}
	if e = os.Remove(b.DataPath); e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	b.Deleted = true
	if e = a.SaveBackup(b); e != nil {
		return e
	}
	a.event("backup_deleted", id)
	return nil
}
func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}
func (a *App) BackupAction(ctx context.Context, action, id string, force bool) error {
	switch action {
	case "list":
		b, e := a.Backups()
		if e != nil {
			return e
		}
		return json.NewEncoder(a.Out).Encode(b)
	case "show", "path", "verify":
		b, e := a.ReadBackup(id)
		if e != nil {
			return e
		}
		if action == "path" {
			fmt.Fprintln(a.Out, b.DataPath)
			return nil
		}
		if action == "verify" {
			return a.VerifyBackup(id)
		}
		return json.NewEncoder(a.Out).Encode(b)
	}
	unlock, e := a.Lock()
	if e != nil {
		return e
	}
	defer unlock()
	if e = a.EnsureData(); e != nil {
		return e
	}
	if hasPending(a.P.Data) {
		return errors.New("存在未完成事务，先恢复再管理备份")
	}
	switch action {
	case "delete":
		return a.DeleteBackup(id, force)
	case "prune":
		items, e := a.Backups()
		if e != nil {
			return e
		}
		for _, b := range items {
			if !a.backupReferenced(b) && !b.Deleted {
				if e = a.DeleteBackup(b.ID, false); e != nil {
					return e
				}
			}
		}
		return nil
	case "restore":
		// Active extension restores must also disable automatic re-hooking for that root.
		for _, bind := range a.S.Bindings {
			if bind.BackupID == id {
				return a.DetachRoot(ctx, bind.Root)
			}
		}
		b, e := a.ReadBackup(id)
		if e != nil {
			return e
		}
		if a.backupReferenced(b) {
			return fmt.Errorf("备份属于 %s，请先解除对应组件再恢复", b.Ref)
		}
		return a.RestoreBackup(id)
	default:
		return fmt.Errorf("未知备份操作: %s", action)
	}
}
func (a *App) DataAction(ctx context.Context, action, target string) error {
	if action == "path" {
		fmt.Fprintln(a.Out, a.P.Data)
		return nil
	}
	if action == "status" {
		for _, name := range []string{"backups", "logs", "cache", "tmp", "transactions", "history", "reports"} {
			var size int64
			_ = filepath.WalkDir(filepath.Join(a.P.Data, name), func(p string, d os.DirEntry, e error) error {
				if e == nil && !d.IsDir() {
					if i, e := d.Info(); e == nil {
						size += i.Size()
					}
				}
				return nil
			})
			fmt.Fprintf(a.Out, "%s\t%d bytes\t%s\n", name, size, filepath.Join(a.P.Data, name))
		}
		return nil
	}
	unlock, e := a.Lock()
	if e != nil {
		return e
	}
	defer unlock()
	if action == "relink" {
		if hasPending(a.P.Data) {
			return errors.New("先恢复未完成事务，再移动维护目录")
		}
		root, e := filepath.Abs(target)
		if e != nil {
			return e
		}
		if root == "/" {
			return errors.New("不能使用根目录")
		}
		data := filepath.Join(root, "installer-data")
		if info, e := os.Stat(data); e != nil || !info.IsDir() {
			return errors.New("目标必须已有 installer-data；先完整移动维护目录")
		}
		if a.S.DataID != "" {
			raw, e := os.ReadFile(filepath.Join(data, "directory-id"))
			if e != nil || strings.TrimSpace(string(raw)) != a.S.DataID {
				return errors.New("目标不是原维护目录；重新关联未执行")
			}
		}
		// Every currently referenced original must have moved with the directory.
		required := map[string]bool{}
		for _, b := range a.S.Bindings {
			required[b.BackupID] = true
		}
		for _, c := range a.S.Components {
			for _, id := range c.Backups {
				required[id] = true
			}
		}
		oldBackups, oldErr := a.Backups()
		if oldErr == nil {
			for _, b := range oldBackups {
				required[b.ID] = true
			}
		}
		for id := range required {
			raw, er := os.ReadFile(filepath.Join(data, "backups", id, "backup.json"))
			if er != nil {
				return fmt.Errorf("目标缺少备份%s；重新关联未执行", id)
			}
			var b Backup
			if er = json.Unmarshal(raw, &b); er != nil {
				return er
			}
			if b.ID != id {
				return errors.New("目标备份ID不符")
			}
			if !b.Deleted {
				h, er := Hash(filepath.Join(data, "backups", id, "content"))
				if er != nil || h != b.Hash {
					return fmt.Errorf("目标备份%s损坏", id)
				}
			}
		}
		old := a.P.Data
		a.P.Data = data
		// Update only registered backup paths, never execute imported configuration.
		bs, e := a.Backups()
		if e != nil { // metadata may still reference the previous root: repair below
			bs = nil
			entries, er := os.ReadDir(filepath.Join(data, "backups"))
			if er != nil {
				return er
			}
			for _, x := range entries {
				if !x.IsDir() || !validID(x.Name()) {
					continue
				}
				raw, er := os.ReadFile(filepath.Join(data, "backups", x.Name(), "backup.json"))
				if er != nil {
					return er
				}
				var b Backup
				if er = json.Unmarshal(raw, &b); er != nil {
					return er
				}
				b.DataPath = filepath.Join(data, "backups", b.ID, "content")
				if !b.Deleted {
					h, er := Hash(b.DataPath)
					if er != nil || h != b.Hash {
						return errors.New("重新关联备份校验失败")
					}
				}
				bs = append(bs, b)
			}
		}
		for _, b := range bs {
			b.DataPath = strings.Replace(b.DataPath, old, data, 1)
			if e = a.SaveBackup(b); e != nil {
				return e
			}
		}
		a.S.Installer = root
		if e = a.Save(); e != nil {
			return e
		}
		fmt.Fprintln(a.Out, "维护目录已重新关联；后台下次扫描读取新路径。")
		return nil
	}
	if e = a.EnsureData(); e != nil {
		return e
	}
	if hasPending(a.P.Data) {
		return errors.New("未完成事务阻止清理")
	}
	if action != "clean" {
		return errors.New("用法: data path|status|relink DIR|clean logs|cache|tmp")
	}
	if target != "logs" && target != "cache" && target != "tmp" {
		return errors.New("仅允许清理 logs/cache/tmp；备份使用 backup 管理")
	}
	dir := filepath.Join(a.P.Data, target)
	entries, e := os.ReadDir(dir)
	if e != nil {
		return e
	}
	for _, x := range entries {
		if target == "cache" && x.Name() == "codex-rollback" {
			fmt.Fprintln(a.Out, "保留仍用于版本回退的缓存: codex-rollback")
			continue
		}
		p := filepath.Join(dir, x.Name())
		if e = os.RemoveAll(p); e != nil {
			return e
		}
	}
	return nil
}
