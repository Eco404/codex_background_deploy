package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A replacement is recorded before creating either temporary file. Recovery only
// removes these two exact paths; it never rewrites product.json or scans by glob.
type marketplaceReplacement struct {
	Target       string `json:"target"`
	Temporary    string `json:"temporary"`
	Source       string `json:"source"`
	BackupID     string `json:"backup_id"`
	OriginalHash string `json:"original_hash"`
	NewHash      string `json:"new_hash"`
	UID          uint32 `json:"uid"`
	GID          uint32 `json:"gid"`
	Mode         uint32 `json:"mode"`
}

func marketplaceHashValid(hash string) bool {
	b, err := hex.DecodeString(hash)
	return err == nil && len(b) == 32 && hash == strings.ToLower(hash)
}

func (a *App) marketplaceWriteBackup(c *Component, target string) (string, error) {
	if c == nil || c.Name != "marketplace" || !filepath.IsAbs(target) || filepath.Clean(target) != target || c.Options["product"] != target || filepath.Base(target) != "product.json" || strings.ContainsAny(target, "\x00\r\n") {
		return "", errors.New("拒绝写入未选中的 Marketplace product.json")
	}
	if err := checkParents(target); err != nil {
		return "", err
	}
	for _, id := range c.Backups {
		b, err := a.ReadBackup(id)
		if err == nil && b.Original == target {
			if err = a.VerifyBackup(id); err == nil {
				return id, nil
			}
		}
	}
	return "", errors.New("Marketplace 原文件没有可验证的登记备份")
}

func marketplaceTargetInfo(target, expectedHash string) (os.FileInfo, *syscall.Stat_t, error) {
	if err := checkParents(target); err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(target)
	if err != nil {
		return nil, nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok {
		return nil, nil, errors.New("Marketplace 目标不是普通文件")
	}
	hash, err := Hash(target)
	if err != nil {
		return nil, nil, err
	}
	if !marketplaceHashValid(expectedHash) || hash != expectedHash {
		return nil, nil, errors.New("Marketplace product.json 已被外部修改；保留当前文件")
	}
	return info, st, nil
}

// ReplaceBytes creates a new inode. Only use it when its uid/gid will match the
// original; a writable directory must not make a system-owned file user-owned.
func marketplaceLocalOwnership(target string, st *syscall.Stat_t) bool {
	if int(st.Uid) != os.Geteuid() {
		return false
	}
	parent, err := os.Stat(filepath.Dir(target))
	if err != nil {
		return false
	}
	group := os.Getegid()
	if parent.Mode()&os.ModeSetgid != 0 {
		pst, ok := parent.Sys().(*syscall.Stat_t)
		if !ok {
			return false
		}
		group = int(pst.Gid)
	}
	return int(st.Gid) == group
}

func (a *App) marketplaceSudo(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if os.Geteuid() == 0 {
		_, err := a.componentRun(ctx, nil, args[0], args[1:]...)
		return err
	}
	_, err := a.componentRun(ctx, nil, "sudo", append([]string{"-n", "--"}, args...)...)
	return err
}

func (a *App) writeMarketplaceProduct(ctx context.Context, c *Component, target string, data []byte, mode os.FileMode, expectedHash string) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	backupID, err := a.marketplaceWriteBackup(c, target)
	if err != nil {
		return err
	}
	info, st, err := marketplaceTargetInfo(target, expectedHash)
	if err != nil {
		return err
	}
	if mode.Perm() != info.Mode().Perm() || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return errors.New("Marketplace 替换必须保留原文件权限")
	}
	if err = a.EnsureData(); err != nil {
		return err
	}
	id := ID()
	r := marketplaceReplacement{
		Target: target, Temporary: filepath.Join(filepath.Dir(target), ".codex-bg-marketplace-"+id),
		Source: filepath.Join(a.P.Data, "tmp", "marketplace-source-"+id), BackupID: backupID,
		OriginalHash: expectedHash, NewHash: HashBytes(data), UID: st.Uid, GID: st.Gid, Mode: uint32(mode.Perm()),
	}
	record := filepath.Join(a.P.Data, "transactions", "marketplace-replacements", id+".json")
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err = AtomicWrite(record, raw, 0600); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, a.cleanupMarketplaceReplacement(record, r)) }()

	// Permission escalation is also necessary when a local rename would lose
	// ownership, even if the directory itself happens to be writable.
	err = os.ErrPermission
	if marketplaceLocalOwnership(target, st) {
		if err = marketplaceOriginalUnchanged(r); err != nil {
			return err
		}
		err = a.ReplaceBytes(target, data, mode.Perm())
	}
	if err != nil {
		if !errors.Is(err, os.ErrPermission) {
			return err
		}
		if err = a.prepareMarketplaceSource(r.Source, data); err != nil {
			return err
		}
		if err = checkParents(r.Temporary); err != nil {
			return err
		}
		if _, e := os.Lstat(r.Temporary); !errors.Is(e, os.ErrNotExist) {
			return errors.New("Marketplace 临时替换路径已存在或无法验证")
		}
		if err = a.marketplaceSudo(ctx, "install", "-m", strconv.FormatUint(uint64(r.Mode), 8), "-o", strconv.FormatUint(uint64(r.UID), 10), "-g", strconv.FormatUint(uint64(r.GID), 10), "--", r.Source, r.Temporary); err != nil {
			return err
		}
		if err = verifyMarketplaceReplacement(r.Temporary, r); err != nil {
			return err
		}
		// Installation can take time. Recheck the existing file immediately before
		// rename so a concurrent code-server update is never silently overwritten.
		if err = marketplaceOriginalUnchanged(r); err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = a.marketplaceSudo(ctx, "mv", "-f", "-T", "--", r.Temporary, target); err != nil {
			return err
		}
	}
	return verifyMarketplaceReplacement(target, r)
}

func marketplaceOriginalUnchanged(r marketplaceReplacement) error {
	info, st, err := marketplaceTargetInfo(r.Target, r.OriginalHash)
	if err != nil {
		return err
	}
	if st.Uid != r.UID || st.Gid != r.GID || uint32(info.Mode().Perm()) != r.Mode {
		return errors.New("Marketplace 原文件归属或权限发生变化")
	}
	return nil
}

func (a *App) prepareMarketplaceSource(path string, data []byte) error {
	if err := checkParents(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

func verifyMarketplaceReplacement(path string, r marketplaceReplacement) error {
	info, st, err := marketplaceTargetInfo(path, r.NewHash)
	if err != nil {
		return err
	}
	if st.Uid != r.UID || st.Gid != r.GID || uint32(info.Mode().Perm()) != r.Mode {
		return errors.New("Marketplace 替换后的归属或权限不一致")
	}
	return nil
}

func (a *App) validateMarketplaceReplacement(record string, r marketplaceReplacement) error {
	id := strings.TrimSuffix(filepath.Base(record), ".json")
	if !validID(id) || record != filepath.Join(a.P.Data, "transactions", "marketplace-replacements", id+".json") || !filepath.IsAbs(r.Target) || filepath.Clean(r.Target) != r.Target || filepath.Base(r.Target) != "product.json" || strings.ContainsAny(r.Target, "\x00\r\n") || r.Temporary != filepath.Join(filepath.Dir(r.Target), ".codex-bg-marketplace-"+id) || r.Source != filepath.Join(a.P.Data, "tmp", "marketplace-source-"+id) || !marketplaceHashValid(r.OriginalHash) || !marketplaceHashValid(r.NewHash) || r.Mode > 0777 {
		return errors.New("Marketplace 替换恢复记录无效")
	}
	for _, path := range []string{record, r.Target, r.Source, r.Temporary} {
		if err := checkParents(path); err != nil {
			return err
		}
	}
	backup, err := a.ReadBackup(r.BackupID)
	if err != nil || backup.Original != r.Target {
		return errors.New("Marketplace 替换恢复记录与备份不符")
	}
	return a.VerifyBackup(r.BackupID)
}

func (a *App) cleanupMarketplaceReplacement(record string, r marketplaceReplacement) error {
	if err := a.validateMarketplaceReplacement(record, r); err != nil {
		return err
	}
	// Validate both objects before removing either. Partial copies are permitted:
	// a killed install/write may leave incomplete content, but never a symlink.
	for _, path := range []string{r.Source, r.Temporary} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || (path == r.Source && int(st.Uid) != os.Geteuid()) || (path == r.Temporary && int(st.Uid) != os.Geteuid() && st.Uid != 0 && st.Uid != r.UID) {
			return fmt.Errorf("Marketplace 临时文件类型或归属变化，保留 %s", path)
		}
	}
	err := os.Remove(r.Temporary)
	if errors.Is(err, os.ErrPermission) {
		err = a.marketplaceSudo(context.Background(), "rm", "-f", "--", r.Temporary)
	}
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	sourceErr := os.Remove(r.Source)
	if errors.Is(sourceErr, os.ErrNotExist) {
		sourceErr = nil
	}
	if err = errors.Join(err, sourceErr); err != nil {
		return err
	}
	if err = os.Remove(record); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (a *App) cleanupMarketplaceReplacements() error {
	dir := filepath.Join(a.P.Data, "transactions", "marketplace-replacements")
	if err := checkParents(dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		record := filepath.Join(dir, entry.Name())
		if err = checkParents(record); err != nil {
			return err
		}
		info, err := os.Lstat(record)
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || int(st.Uid) != os.Geteuid() || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("Marketplace 替换记录类型或归属无效: %s", record)
		}
		raw, err := os.ReadFile(record)
		if err != nil {
			return err
		}
		var r marketplaceReplacement
		if err = json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if err = a.cleanupMarketplaceReplacement(record, r); err != nil {
			return err
		}
	}
	return nil
}
