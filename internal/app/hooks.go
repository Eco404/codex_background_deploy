package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func scanRoot(root string) ([]Extension, error) {
	var out []Extension
	entries, e := os.ReadDir(root)
	if errors.Is(e, os.ErrNotExist) {
		return out, nil
	}
	if e != nil {
		return nil, e
	}
	for _, x := range entries {
		if !x.IsDir() || !strings.HasPrefix(x.Name(), "openai.chatgpt-") {
			continue
		}
		dir := filepath.Join(root, x.Name())
		platforms, e := os.ReadDir(filepath.Join(dir, "bin"))
		if e != nil {
			continue
		}
		for _, p := range platforms {
			if !p.IsDir() {
				continue
			}
			entry := filepath.Join(dir, "bin", p.Name(), "codex")
			i, e := os.Lstat(entry)
			if e != nil || !i.Mode().IsRegular() {
				continue
			}
			f, e := os.Open(entry)
			if e != nil {
				return nil, e
			}
			head := make([]byte, 2048)
			n, _ := f.Read(head)
			f.Close()
			s := string(head[:n])
			out = append(out, Extension{Root: root, Directory: dir, Entry: entry, Version: strings.TrimPrefix(x.Name(), "openai.chatgpt-"), Hooked: strings.Contains(s, Marker), Legacy: strings.Contains(s, "codex-daemon-bridge extension hook") || strings.Contains(s, "vscode-codex-background-deploy extension hook")})
		}
	}
	return out, nil
}
func (a *App) hookContent(id string) []byte {
	return []byte("#!/bin/sh\n# " + Marker + "\nexec " + shellQuote(a.P.Binary()) + " --home " + shellQuote(a.P.Home) + " --runtime " + shellQuote(a.P.Runtime) + " proxy --binding " + shellQuote(id) + " -- \"$@\"\n")
}
func (a *App) Attach(ctx context.Context, ex Extension) error {
	if e := a.EnsureData(); e != nil {
		return e
	}
	h, e := Hash(ex.Entry)
	if e != nil {
		return e
	}
	initial, e := os.Stat(ex.Entry)
	if e != nil {
		return e
	}
	for i := range a.S.Bindings {
		b := &a.S.Bindings[i]
		if b.Entry != ex.Entry {
			continue
		}
		if h == b.Hash {
			return a.VerifyBackup(b.BackupID)
		}
		if ex.Hooked || ex.Legacy {
			return fmt.Errorf("入口被其他管理者修改，保留: %s", ex.Entry)
		}
		// An extension update replaced the entry. Keep its earlier original as history.
		old, e := a.ReadBackup(b.BackupID)
		if e != nil {
			return e
		}
		old.Ref = ""
		if e = a.SaveBackup(old); e != nil {
			return e
		}
		a.S.Bindings = append(a.S.Bindings[:i], a.S.Bindings[i+1:]...)
		break
	}
	if ex.Hooked {
		return fmt.Errorf("入口已有标记但缺少所有权记录: %s", ex.Entry)
	}
	if ex.Legacy {
		return fmt.Errorf("旧入口需要 migrate: %s", ex.Entry)
	}
	// Extension managers copy large executables asynchronously. Do not replace
	// a file which is still being installed, even if its directory already exists.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(200 * time.Millisecond):
	}
	stable, e := os.Stat(ex.Entry)
	if e != nil {
		return e
	}
	if initial.Size() != stable.Size() || !initial.ModTime().Equal(stable.ModTime()) {
		return fmt.Errorf("扩展入口仍在变化，稍后重试: %s", ex.Entry)
	}
	i, e := os.Stat(ex.Entry)
	if e != nil {
		return e
	}
	if i.Mode()&0111 == 0 {
		return fmt.Errorf("入口不可执行: %s", ex.Entry)
	}
	id := ID()
	backup, e := a.BackupFile(ex.Entry, "extension-original", "binding:"+id)
	if e != nil {
		return e
	}
	if current, e := Hash(ex.Entry); e != nil || current != backup.Hash || current != h {
		return errors.New("备份期间入口变化，保留原入口并等待重试")
	}
	content := a.hookContent(id)
	binding := Binding{ID: id, Root: ex.Root, Entry: ex.Entry, Version: ex.Version, BackupID: backup.ID, Hash: HashBytes(content)}
	// Ownership is durable before the target is replaced. Recovery accepts original or wrapper.
	a.S.Bindings = append(a.S.Bindings, binding)
	if e = a.Save(); e != nil {
		return e
	}
	if e = a.ReplaceBytes(ex.Entry, content, i.Mode().Perm()); e != nil {
		return e
	}
	a.event("entry_attached", ex.Entry)
	return nil
}
func (a *App) restoreBinding(b Binding) error {
	backup, e := a.ReadBackup(b.BackupID)
	if e != nil {
		return e
	}
	h, e := Hash(b.Entry)
	if errors.Is(e, os.ErrNotExist) { // extension manager removed the old version; never resurrect it
		backup.Ref = ""
		return a.SaveBackup(backup)
	}
	if e != nil {
		return e
	}
	if h == backup.Hash {
		backup.Ref = ""
		return a.SaveBackup(backup)
	}
	if h != b.Hash { // A newer original is external and must remain intact.
		f, e := os.Open(b.Entry)
		if e != nil {
			return e
		}
		head := make([]byte, 2048)
		n, _ := f.Read(head)
		f.Close()
		if strings.Contains(string(head[:n]), Marker) {
			return fmt.Errorf("入口有外部改动，不能覆盖: %s", b.Entry)
		}
		backup.Ref = ""
		return a.SaveBackup(backup)
	}
	return a.RestoreBackup(b.BackupID)
}
func (a *App) DetachRoot(ctx context.Context, root string) error {
	var keep []Binding
	// Preflight every required original before changing any entry in this scope.
	for _, b := range a.S.Bindings {
		if b.Root != root {
			continue
		}
		if h, e := Hash(b.Entry); e == nil && h == b.Hash {
			if e = a.VerifyBackup(b.BackupID); e != nil {
				return fmt.Errorf("无法恢复 %s: %w", b.Entry, e)
			}
		}
	}
	for _, b := range a.S.Bindings {
		if b.Root != root {
			keep = append(keep, b)
			continue
		}
		if e := a.restoreBinding(b); e != nil {
			return e
		}
	}
	a.S.Bindings = keep
	roots := []string{}
	for _, r := range a.S.SelectedRoots {
		if r != root {
			roots = append(roots, r)
		}
	}
	a.S.SelectedRoots = roots
	return a.Save()
}
func (a *App) Reconcile(ctx context.Context) error {
	unlock, e := a.Lock()
	if e != nil {
		return e
	}
	defer unlock()
	if e = a.Reload(); e != nil {
		return e
	}
	if !a.S.Installed {
		return nil
	}
	if hasPending(a.P.Data) {
		return errors.New("有待恢复事务，暂停入口检查")
	}
	if e = a.EnsureData(); e != nil {
		return e
	}
	var issues []string
	for _, r := range a.S.SelectedRoots {
		exts, e := scanRoot(r)
		if e != nil {
			issues = append(issues, e.Error())
			continue
		}
		for _, ex := range exts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			// Incomplete writes are not hooked until file identity and size remain stable.
			if e = a.Attach(ctx, ex); e != nil {
				issues = append(issues, e.Error())
			}
		}
	}
	if len(issues) > 0 {
		return errors.New(strings.Join(issues, "; "))
	}
	return nil
}
