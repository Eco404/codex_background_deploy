package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed branding/*
var componentAssets embed.FS

func componentHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (a *App) componentRoot(name string) string {
	return filepath.Join(a.P.Runtime, "components", name)
}
func (a *App) componentEnv(extra map[string]string) []string {
	m := map[string]string{}
	for _, s := range os.Environ() {
		k, v, ok := strings.Cut(s, "=")
		if ok {
			m[k] = v
		}
	}
	m["TMPDIR"] = filepath.Join(a.P.Data, "tmp")
	m["HOME"] = a.P.Home
	m["CODEX_HOME"] = a.S.CodexHome
	m["TMP"] = m["TMPDIR"]
	m["TEMP"] = m["TMPDIR"]
	m["npm_config_cache"] = filepath.Join(a.P.Data, "cache", "npm")
	m["PIP_CACHE_DIR"] = filepath.Join(a.P.Data, "cache", "pip")
	m["CONDA_PKGS_DIRS"] = filepath.Join(a.P.Data, "cache", "conda")
	for k, v := range extra {
		m[k] = v
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(m))
	for _, k := range keys {
		env = append(env, k+"="+m[k])
	}
	return env
}
func (a *App) componentRun(ctx context.Context, env map[string]string, name string, args ...string) ([]byte, error) {
	return a.componentRunOutput(ctx, env, false, name, args...)
}
func (a *App) componentRunOutput(ctx context.Context, env map[string]string, stream bool, name string, args ...string) ([]byte, error) {
	e := a.componentEnv(env)
	a.progress("执行 %s…", filepath.Base(name))
	if a.RunCommand != nil {
		return a.RunCommand(ctx, name, args, e)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = e
	// Cancelling a shell/npm installer must also stop its download/extraction
	// children; otherwise they can mutate files after rollback or hold pipes open.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	var b []byte
	var err error
	if stream {
		var buffer bytes.Buffer
		lines := &installOutput{a: a}
		cmd.Stdout = io.MultiWriter(&buffer, lines)
		cmd.Stderr = cmd.Stdout
		err = cmd.Run()
		lines.flush()
		b = buffer.Bytes()
	} else {
		b, err = cmd.CombinedOutput()
	}
	if err != nil {
		return b, fmt.Errorf("%s: %w: %s", name, err, a.Network.redact(strings.TrimSpace(string(b))))
	}
	a.progress("完成 %s", filepath.Base(name))
	return b, nil
}
func (a *App) componentDirs() error {
	if err := a.EnsureData(); err != nil {
		return err
	}
	for _, d := range []string{filepath.Join(a.P.Data, "tmp"), filepath.Join(a.P.Data, "cache"), filepath.Join(a.P.Data, "logs"), filepath.Join(a.P.Runtime, "components")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return err
		}
	}
	return nil
}
func (a *App) componentFile(c *Component, path string, b []byte, mode os.FileMode) error {
	if err := a.ReplaceBytes(path, b, mode); err != nil {
		return err
	}
	c.Files = append(c.Files, ManagedFile{Path: path, Hash: componentHash(b), Kind: c.Name})
	return a.Save()
}
func (a *App) componentDownload(ctx context.Context, url, path string) error {
	return a.download(ctx, url, path)
}
func (a *App) ComponentAction(ctx context.Context, action, name string, opt ComponentOptions) error {
	switch name {
	case "node", "miniconda", "codex-cli", "codexapp", "apti", "branding", "marketplace":
	default:
		return fmt.Errorf("unknown component %q", name)
	}
	if action == "status" {
		c := a.S.Components[name]
		if a.Out != nil {
			if c == nil {
				fmt.Fprintf(a.Out, "%s: absent\n", name)
			} else {
				fmt.Fprintf(a.Out, "%s: %s (%s) %s\n", name, c.Status, c.Ownership, c.Path)
			}
		}
		return nil
	}
	if action == "plan" || opt.DryRun {
		if name == "marketplace" && (action == "plan" || action == "install") {
			inv, err := a.Discover(ctx)
			if err != nil {
				return err
			}
			return a.planMarketplace(inv)
		}
		if a.Out != nil {
			fmt.Fprintf(a.Out, "%s %s: %s; external paths are reused and never deleted\n", action, name, a.componentRoot(name))
		}
		return nil
	}
	if !opt.Yes {
		return errors.New("component mutation requires explicit confirmation (--yes)")
	}
	if err := a.Network.Validate(); err != nil {
		return err
	}
	switch action {
	case "start":
		return a.StartComponent(ctx, name)
	case "stop":
		return a.StopComponent(ctx, name)
	case "uninstall":
		return a.removeComponent(ctx, name)
	case "configure":
		if name != "codexapp" {
			return errors.New("configure is supported for CodexApp; shell choices require explicit reinstall")
		}
		return a.configureCodexApp(ctx, a.S.Components[name], opt)
	case "repair":
		c := a.S.Components[name]
		if c == nil {
			return errors.New("component absent; use install")
		}
		if c.Status != "installed" {
			return errors.New("partial installation preserved; uninstall this component, then install again")
		}
		if name == "marketplace" {
			if c.Ownership == "managed" {
				return a.applyMarketplace(ctx, c)
			}
			return a.checkMarketplace(c)
		}
		for _, f := range c.Files {
			b, err := os.ReadFile(f.Path)
			if err != nil || componentHash(b) != f.Hash {
				return fmt.Errorf("component file changed or missing: %s; preserved for explicit recovery", f.Path)
			}
		}
		return nil
	case "install":
	case "update":
		return errors.New("component update requires uninstall then install; reused installations must be updated by their owner")
	default:
		return fmt.Errorf("unknown component action %s", action)
	}
	if c := a.S.Components[name]; c != nil {
		if c.Status == "installed" {
			return nil
		}
		return fmt.Errorf("%s has incomplete state; uninstall or repair before retry", name)
	}
	if name == "marketplace" {
		return a.installMarketplace(ctx)
	}
	forceManaged := opt.Path == "managed"
	if forceManaged {
		opt.Path = ""
	}
	if opt.Path != "" {
		return a.reuseComponent(ctx, name, opt)
	}
	if !forceManaged && (name == "node" || name == "miniconda" || name == "codexapp") && opt.Version == "" {
		inv, err := a.Discover(ctx)
		if err != nil {
			return err
		}
		kind := name
		if name == "miniconda" {
			kind = "conda"
		}
		for _, candidate := range inv.Candidates[kind] {
			if candidate.Compatible {
				opt.Path = candidate.Path
				return a.reuseComponent(ctx, name, opt)
			}
		}
	}
	if name == "codexapp" {
		if err := a.requireCodexAppCore(ctx); err != nil {
			return err
		}
	}
	if err := a.componentDirs(); err != nil {
		return err
	}
	root := a.componentRoot(name)
	if _, err := os.Lstat(root); err == nil {
		return fmt.Errorf("refusing to adopt existing directory %s", root)
	} else if !os.IsNotExist(err) {
		return err
	}
	if a.S.Components == nil {
		a.S.Components = map[string]*Component{}
	}
	c := &Component{Name: name, Path: root, Ownership: "managed", Status: "installing", Options: map[string]string{}}
	a.S.Components[name] = c
	if err := a.Save(); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	var err error
	switch name {
	case "node":
		err = a.installNode(ctx, c, opt)
	case "miniconda":
		err = a.installConda(ctx, c, opt)
	case "codex-cli":
		err = a.installCodex(ctx, c, opt.Version)
	case "codexapp":
		err = a.installCodexApp(ctx, c, opt)
	case "apti":
		err = a.installApti(ctx, c)
	case "branding":
		err = a.installBranding(ctx, c, opt)
	}
	if err != nil {
		c.Status = "failed"
		_ = a.Save()
		return err
	}
	if opt.ShellIntegration && (name == "node" || name == "miniconda") {
		if err = a.integrateShell(c, opt); err != nil {
			c.Status = "failed"
			_ = a.Save()
			return err
		}
	}
	c.Status = "installed"
	if err = a.Save(); err != nil {
		return err
	}
	if name == "codexapp" {
		return a.StartComponent(ctx, name)
	}
	return nil
}
func (a *App) reuseComponent(ctx context.Context, name string, opt ComponentOptions) error {
	if name != "node" && name != "miniconda" && name != "codex-cli" && name != "codexapp" {
		return errors.New("--path reuse is supported for Node, Conda, Codex CLI and CodexApp")
	}
	p, err := filepath.Abs(opt.Path)
	if err != nil {
		return err
	}
	if name == "miniconda" {
		if s, e := os.Stat(p); e == nil && s.IsDir() {
			p = filepath.Join(p, "bin", "conda")
		}
	}
	var b []byte
	var codexapp codexAppProbe
	if name == "codexapp" {
		codexapp, err = a.probeCodexAppDetails(ctx, p)
		b = []byte(codexapp.Version)
	} else {
		b, err = a.componentRun(ctx, nil, p, "--version")
	}
	if err != nil {
		return err
	}
	if name == "node" {
		var major int
		if _, e := fmt.Sscanf(strings.TrimSpace(string(b)), "v%d", &major); e != nil || major < 18 {
			return errors.New("compatible Node >=18 required")
		}
	}
	if a.S.Components == nil {
		a.S.Components = map[string]*Component{}
	}
	c := &Component{Name: name, Path: p, Version: strings.TrimSpace(string(b)), Ownership: "reused", Status: "installed", Options: map[string]string{}}
	if name == "codexapp" {
		c.Status = "available"
		if codexapp.Node != "" {
			c.Options["node"] = codexapp.Node
		}
	}
	a.S.Components[name] = c
	if err = a.Save(); err != nil {
		return err
	}
	if name == "codexapp" && (opt.PasswordFile != "" || opt.Configure) {
		return a.configureCodexApp(ctx, c, opt)
	}
	if opt.ShellIntegration && (name == "node" || name == "miniconda") {
		return a.integrateShell(c, opt)
	}
	return nil
}
func (a *App) InstallManagedCodex(ctx context.Context, version string) (Candidate, error) {
	if c := a.S.Components["codex-cli"]; c != nil && c.Ownership == "managed" && c.Status == "installed" {
		if err := a.verifyManagedCodexSelection(ctx, c); err != nil {
			return Candidate{}, err
		}
	}
	if c := a.S.Components["codex-cli"]; c != nil && c.Ownership == "managed" && c.Status == "installed" && version != "" && strings.TrimPrefix(version, "v") != c.Version {
		// The official installer keeps immutable release directories and switches
		// symlinks. Capture our narrow selection points so failure restores the
		// previously working CLI without deleting the auditable failed release.
		links := map[string]string{}
		for _, rel := range []string{"bin/codex", "bin/codex-code-mode-host", "packages-home/packages/standalone/current"} {
			p := filepath.Join(c.Path, rel)
			target, e := os.Readlink(p)
			if e == nil {
				links[p] = target
			} else if !os.IsNotExist(e) {
				return Candidate{}, fmt.Errorf("managed CLI link is not a symlink: %s", p)
			}
		}
		oldVersion := c.Version
		if err := a.installCodex(ctx, c, version); err != nil {
			var restoreErrors []error
			for p, target := range links {
				tmp := p + ".restore-" + ID()
				if e := os.Symlink(target, tmp); e != nil {
					restoreErrors = append(restoreErrors, e)
					continue
				}
				if e := os.Rename(tmp, p); e != nil {
					restoreErrors = append(restoreErrors, e)
				}
			}
			c.Version = oldVersion
			if archiveErr := a.archiveOldCodexPackages(c); archiveErr != nil {
				restoreErrors = append(restoreErrors, archiveErr)
			}
			return Candidate{}, errors.Join(append([]error{err}, restoreErrors...)...)
		}
		if err := a.Save(); err != nil {
			return Candidate{}, err
		}
	}
	err := a.ComponentAction(ctx, "install", "codex-cli", ComponentOptions{Version: version, Yes: true})
	if err != nil {
		return Candidate{}, err
	}
	c := a.S.Components["codex-cli"]
	p := c.Path
	if c.Ownership == "managed" {
		// A core rollback may restore its pinned State.Codex while a later CLI
		// selection link still points to a different release. Do not mislabel or
		// archive anything until the actual selected executable matches the record.
		if err := a.verifyManagedCodexSelection(ctx, c); err != nil {
			return Candidate{}, err
		}
		if err := a.archiveOldCodexPackages(c); err != nil {
			return Candidate{}, err
		}
		p = filepath.Join(p, "bin", "codex")
		if resolved, e := filepath.EvalSymlinks(p); e == nil {
			p = resolved
		}
	}
	source := "managed"
	if c.Ownership == "reused" {
		source = "reused"
	}
	return Candidate{Kind: "codex", Path: p, Version: c.Version, Source: source, Compatible: true}, nil
}

func (a *App) verifyManagedCodexSelection(ctx context.Context, c *Component) error {
	if err := a.EnsureData(); err != nil {
		return err
	}
	validationHome, err := os.MkdirTemp(filepath.Join(a.P.Data, "tmp"), "codex-selection-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(validationHome)
	output, err := a.componentRun(ctx, map[string]string{"HOME": validationHome, "CODEX_HOME": validationHome}, filepath.Join(c.Path, "bin", "codex"), "--version")
	if err != nil {
		return fmt.Errorf("managed Codex selection cannot be verified; inspect recorded runtime and recover pending operations: %w", err)
	}
	if strings.TrimSpace(string(output)) != "codex-cli "+c.Version {
		return fmt.Errorf("managed Codex selection differs from recorded version %s; inspect recorded runtime and update/rollback history before repairing selection links; no release was archived", c.Version)
	}
	return nil
}
func (a *App) componentBinary(name string) string {
	c := a.S.Components[name]
	if c == nil {
		return ""
	}
	if c.Ownership == "reused" {

		return c.Path
	}
	switch name {
	case "node":
		return filepath.Join(c.Path, "runtime", "bin", "node")
	case "miniconda":
		return filepath.Join(c.Path, "runtime", "bin", "conda")
	case "codex-cli":
		return filepath.Join(c.Path, "bin", "codex")
	}
	return ""
}
func (a *App) removeComponent(ctx context.Context, name string) error {
	c := a.S.Components[name]
	if c == nil {
		return nil
	}
	if c.Ownership == "reused" {
		if name == "codexapp" && c.Options["managed_runtime"] == "true" {
			if err := a.StopComponent(ctx, name); err != nil {
				return err
			}
			for _, f := range c.Files {
				b, err := os.ReadFile(f.Path)
				if os.IsNotExist(err) {
					continue
				}
				if err != nil || componentHash(b) != f.Hash {
					return fmt.Errorf("managed CodexApp file changed: %s", f.Path)
				}
				if err = os.Remove(f.Path); err != nil {
					return err
				}
			}
			if err := os.RemoveAll(a.codexappRuntime(c)); err != nil {
				return err
			}
			if a.S.Backend == "systemd" {
				if _, err := a.componentRun(ctx, nil, "systemctl", "--user", "daemon-reload"); err != nil {
					return err
				}
			}
		}
		if err := a.removeShellIntegration(c); err != nil {
			return err
		}
		delete(a.S.Components, name)
		return a.Save()
	}
	if c.Ownership != "managed" || filepath.Clean(c.Path) != a.componentRoot(name) {
		return errors.New("component ownership/path mismatch")
	}
	if err := a.checkComponentUserData(c); err != nil {
		return err
	}
	if name == "codex-cli" && a.S.Installed && strings.HasPrefix(a.S.Codex.Path, c.Path+string(os.PathSeparator)) {
		return errors.New("core still uses this Codex CLI")
	}
	if name == "node" {
		if app := a.S.Components["codexapp"]; app != nil && strings.HasPrefix(app.Options["node"], c.Path+string(os.PathSeparator)) {
			return errors.New("CodexApp still uses this Node")
		}
	}
	if err := a.StopComponent(ctx, name); err != nil {
		return err
	}
	if err := a.removeShellIntegration(c); err != nil {
		return err
	}
	if name == "branding" {
		originals := map[string]string{}
		for _, id := range c.Backups {
			if err := a.VerifyBackup(id); err != nil {
				return err
			}
			b, err := a.ReadBackup(id)
			if err != nil {
				return err
			}
			originals[b.Original] = b.Hash
		}
		for _, f := range c.Files {
			b, err := os.ReadFile(f.Path)
			if err != nil || (componentHash(b) != f.Hash && componentHash(b) != originals[f.Path]) {
				return fmt.Errorf("branding changed externally: %s; backups retained", f.Path)
			}
		}
		for _, id := range c.Backups {
			if err := a.restoreBrandingBackup(ctx, c, id); err != nil {
				return err
			}
		}
	}
	if name == "marketplace" {
		if err := a.restoreMarketplace(ctx, c); err != nil {
			return err
		}
	}
	for _, f := range c.Files {
		if name == "branding" || name == "marketplace" || strings.HasPrefix(f.Path, c.Path+string(os.PathSeparator)) {
			continue
		}
		b, err := os.ReadFile(f.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if componentHash(b) != f.Hash {
			return fmt.Errorf("managed file externally changed: %s", f.Path)
		}
		if err = os.Remove(f.Path); err != nil {
			return err
		}
	}
	if name == "apti" {
		p := filepath.Join(c.Path, "packages.txt")
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			if err = AtomicWrite(filepath.Join(a.P.Data, "backups", "apti-packages-retained.txt"), b, 0600); err != nil {
				return err
			}
		}
	}
	if err := os.RemoveAll(c.Path); err != nil {
		return err
	}
	if name == "codexapp" && a.S.Backend == "systemd" {
		if _, err := a.componentRun(ctx, nil, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
	}
	delete(a.S.Components, name)
	return a.Save()
}
func (a *App) RunComponent(ctx context.Context, name string) error {
	if name != "codexapp" {
		return errors.New("unknown component process")
	}
	c := a.S.Components[name]
	if c == nil {
		return errors.New("CodexApp is not installed")
	}
	return a.runCodexApp(ctx, c, true)
}
func (a *App) ContainerExtras(ctx context.Context) error {
	var failures []error
	for _, n := range []string{"apti", "branding", "marketplace", "codexapp"} {
		if c := a.S.Components[n]; c != nil && c.Status == "installed" {
			if err := a.StartComponent(ctx, n); err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", n, err))
			}
		}
	}
	return errors.Join(failures...)
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return AtomicWrite(path, b, 0600)
}
func copyComponentStream(dst string, r io.Reader) error {
	f, e := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if e != nil {
		return e
	}
	_, e = io.Copy(f, r)
	ce := f.Close()
	if e != nil {
		return e
	}
	return ce
}
func componentPort(s string) (int, error) {
	if s == "" {
		return 8214, nil
	}
	n, e := strconv.Atoi(s)
	if e != nil || n < 1 || n > 65535 {
		return 0, errors.New("invalid CodexApp port")
	}
	return n, nil
}
func componentPortFree(port int) error {
	l, e := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if e != nil {
		return e
	}
	return l.Close()
}
