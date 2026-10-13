package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type replacementRecord struct{ Target, Temporary string }

// Write-ahead record covers the short-lived adjacent-file exception. The caller
// holds the management lock, so a later operation can safely clean an interrupted write.
func (a *App) replaceReader(path string, src io.Reader, mode os.FileMode) error {
	if e := a.EnsureData(); e != nil {
		return e
	}
	if e := checkParents(path); e != nil {
		return e
	}
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	id := ID()
	tmp := filepath.Join(filepath.Dir(path), ".codex-bg-replace-"+id)
	record := filepath.Join(a.P.Data, "transactions", "replacements", id+".json")
	raw, _ := json.Marshal(replacementRecord{path, tmp})
	if e := AtomicWrite(record, raw, 0600); e != nil {
		return e
	}
	finished := false
	defer func() {
		if e := os.Remove(tmp); e == nil || errors.Is(e, os.ErrNotExist) {
			_ = os.Remove(record)
		} else if finished {
			a.event("replacement_cleanup_failed", tmp)
		}
	}()
	f, e := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if e != nil {
		return e
	}
	if _, e = io.Copy(f, src); e != nil {
		f.Close()
		return e
	}
	if e = f.Chmod(mode); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(tmp, path); e != nil {
		return e
	}
	finished = true
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (a *App) ReplaceBytes(path string, b []byte, mode os.FileMode) error {
	return a.replaceReader(path, strings.NewReader(string(b)), mode)
}
func (a *App) ReplaceFile(src, dst string, mode os.FileMode) error {
	f, e := os.Open(src)
	if e != nil {
		return e
	}
	defer f.Close()
	return a.replaceReader(dst, f, mode)
}
func (a *App) cleanupReplacements() error {
	dir := filepath.Join(a.P.Data, "transactions", "replacements")
	entries, e := os.ReadDir(dir)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		var r replacementRecord
		if e = json.Unmarshal(b, &r); e != nil {
			return e
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validID(id) || !filepath.IsAbs(r.Target) || r.Temporary != filepath.Join(filepath.Dir(r.Target), ".codex-bg-replace-"+id) {
			return errors.New("原子替换恢复记录无效")
		}
		info, e := os.Lstat(r.Temporary)
		if errors.Is(e, os.ErrNotExist) {
			if e = os.Remove(path); e != nil {
				return e
			}
			continue
		}
		if e != nil {
			return e
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) != os.Getuid() || !info.Mode().IsRegular() {
			return fmt.Errorf("临时文件归属变化，保留 %s", r.Temporary)
		}
		if e = os.Remove(r.Temporary); e != nil {
			return e
		}
		if e = os.Remove(path); e != nil {
			return e
		}
	}
	return nil
}
