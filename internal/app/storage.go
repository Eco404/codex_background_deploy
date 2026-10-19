package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

func New(home, installer, runtimeDir string) (*App, error) {
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}
	if installer == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		installer = filepath.Dir(exe)
	}
	home, _ = filepath.Abs(home)
	installer, _ = filepath.Abs(installer)
	if runtimeDir == "" {
		runtimeDir = filepath.Join(home, ".local", "share", "codex-bg")
	}
	runtimeDir, _ = filepath.Abs(runtimeDir)
	for _, p := range []string{home, installer, runtimeDir} {
		if p == "/" || strings.ContainsAny(p, "\x00\r\n") {
			return nil, fmt.Errorf("不安全的目录: %q", p)
		}
	}
	a := &App{P: Paths{Home: home, Installer: installer, Data: filepath.Join(installer, "installer-data"), Runtime: runtimeDir, Socket: filepath.Join(runtimeDir, "run", "app-server.sock")}, Out: os.Stdout, Err: os.Stderr}
	a.S = State{Schema: 1, Installer: installer, CodexHome: filepath.Join(home, ".codex"), Components: map[string]*Component{}}
	if err := a.Reload(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return a, nil
}
func (a *App) Reload() error {
	b, err := os.ReadFile(a.P.StateFile())
	if err != nil {
		return err
	}
	var s State
	if err = json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("状态损坏，保留文件以便恢复: %w", err)
	}
	if s.Schema != 1 {
		return fmt.Errorf("不支持的状态版本 %d", s.Schema)
	}
	if !filepath.IsAbs(s.Installer) || !filepath.IsAbs(s.CodexHome) {
		return errors.New("状态中的路径不是绝对路径")
	}
	if s.Components == nil {
		s.Components = map[string]*Component{}
	}
	a.S = s
	a.P.Data = filepath.Join(s.Installer, "installer-data")
	return nil
}
func (a *App) Save() error {
	if err := os.MkdirAll(a.P.Runtime, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a.S, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWrite(a.P.StateFile(), append(b, '\n'), 0600)
}
func ID() string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func Hash(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func HashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func Within(base, path string) bool {
	r, e := filepath.Rel(base, path)
	return e == nil && r != ".." && !strings.HasPrefix(r, ".."+string(os.PathSeparator))
}
func checkParents(path string) error {
	for p := filepath.Clean(path); p != "/" && p != "."; p = filepath.Dir(p) {
		i, e := os.Lstat(p)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		if i.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("拒绝经过符号链接写入: %s", p)
		}
	}
	return nil
}

// An adjacent replacement is the sole permitted temporary-file exception.
func AtomicWrite(path string, b []byte, mode os.FileMode) error {
	if err := checkParents(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.OpenFile(filepath.Join(filepath.Dir(path), ".codex-bg-replace-"+ID()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	n := tmp.Name()
	defer os.Remove(n)
	if _, err = tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(n, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err == nil {
		defer d.Close()
		return d.Sync()
	}
	return err
}
func copyFile(src, dst string, mode os.FileMode) error {
	if err := checkParents(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.OpenFile(filepath.Join(filepath.Dir(dst), ".codex-bg-replace-"+ID()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
func (a *App) EnsureData() error {
	// Missing registered roots must be explicitly relinked, never silently recreated.
	if a.S.Installed {
		if _, err := os.Stat(a.P.Data); err != nil {
			return fmt.Errorf("维护目录不可用 %s；使用 data relink: %w", a.P.Data, err)
		}
	}
	if err := checkParents(a.P.Data); err != nil {
		return err
	}
	for _, n := range []string{"backups", "logs", "cache", "tmp", "transactions", "history", "reports"} {
		if err := os.MkdirAll(filepath.Join(a.P.Data, n), 0700); err != nil {
			return err
		}
	}
	marker := filepath.Join(a.P.Data, "directory-id")
	if a.S.DataID != "" {
		raw, e := os.ReadFile(marker)
		if e != nil || strings.TrimSpace(string(raw)) != a.S.DataID {
			return errors.New("维护目录身份不符；不能重建或替换已登记的维护目录")
		}
	}
	if a.S.DataID == "" {
		if raw, e := os.ReadFile(marker); e == nil {
			a.S.DataID = strings.TrimSpace(string(raw))
		} else if errors.Is(e, os.ErrNotExist) {
			a.S.DataID = ID()
			if e = AtomicWrite(marker, []byte(a.S.DataID+"\n"), 0600); e != nil {
				return e
			}
		} else {
			return e
		}
	}
	return nil
}
func (a *App) Lock() (func(), error) {
	if err := os.MkdirAll(a.P.Runtime, 0700); err != nil {
		return nil, err
	}
	p := filepath.Join(a.P.Runtime, "operation.lock")
	if err := checkParents(p); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("另一个管理操作正在运行")
	}
	if err = a.Reload(); err != nil && !errors.Is(err, os.ErrNotExist) {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, err
	}
	if err = a.cleanupReplacements(); err != nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, err
	}
	if err = a.cleanupMarketplaceReplacements(); err != nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
func (a *App) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	env := os.Environ()
	env = setEnv(env, "HOME", a.P.Home)
	env = setEnv(env, "CODEX_HOME", a.S.CodexHome)
	if a.RunCommand != nil {
		return a.RunCommand(ctx, name, args, env)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	return cmd.CombinedOutput()
}
func setEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, v := range env {
		if !strings.HasPrefix(v, key+"=") {
			out = append(out, v)
		}
	}
	return append(out, key+"="+value)
}
func (a *App) ManagedEnv() []string {
	env := os.Environ()
	for k, v := range map[string]string{"HOME": a.P.Home, "CODEX_HOME": a.S.CodexHome, "TMPDIR": filepath.Join(a.P.Data, "tmp"), "TMP": filepath.Join(a.P.Data, "tmp"), "TEMP": filepath.Join(a.P.Data, "tmp"), "npm_config_cache": filepath.Join(a.P.Data, "cache", "npm"), "PIP_CACHE_DIR": filepath.Join(a.P.Data, "cache", "pip"), "CONDA_PKGS_DIRS": filepath.Join(a.P.Data, "cache", "conda")} {
		env = setEnv(env, k, v)
	}
	return env
}

type discardCloser struct{ io.Writer }

func (discardCloser) Close() error { return nil }

type rotatingLog struct {
	mu              sync.Mutex
	path, root      string
	statePath, name string
}

func (l *rotatingLog) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.statePath != "" {
		if raw, e := os.ReadFile(l.statePath); e == nil {
			var state State
			if json.Unmarshal(raw, &state) == nil && filepath.IsAbs(state.Installer) {
				l.root = filepath.Join(state.Installer, "installer-data", "logs")
				l.path = filepath.Join(l.root, l.name+".log")
			}
		}
	}
	if _, e := os.Stat(l.root); e != nil {
		return len(b), nil
	}
	if s, e := os.Stat(l.path); e == nil && s.Size() > 2<<20 {
		os.Remove(l.path + ".1")
		_ = os.Rename(l.path, l.path+".1")
	}
	f, e := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return len(b), nil
	}
	defer f.Close()
	return f.Write(b)
}
func (l *rotatingLog) Close() error { return nil }
func (a *App) RuntimeLog(name string) io.WriteCloser {
	if filepath.Base(name) != name {
		return discardCloser{io.Discard}
	}
	root := filepath.Join(a.P.Data, "logs")
	if _, e := os.Stat(root); e != nil {
		return discardCloser{io.Discard}
	}
	return &rotatingLog{path: filepath.Join(root, name+".log"), root: root, statePath: a.P.StateFile(), name: name}
}
func (a *App) event(kind, message string) {
	l := a.RuntimeLog("operations")
	defer l.Close()
	b, _ := json.Marshal(map[string]any{"time": time.Now().UTC(), "event": kind, "message": message})
	l.Write(append(b, '\n'))
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
