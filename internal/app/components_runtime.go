package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CodexAppPasswordPath is the runtime credential location, including when the
// executable is reused from an installation that this tool must not modify.
func (a *App) CodexAppPasswordPath() string {
	root := a.componentRoot("codexapp")
	if c := a.S.Components["codexapp"]; c != nil {
		root = a.codexappRuntime(c)
	}
	return filepath.Join(root, "password")
}

// CheckCodexAppReady is run before acquiring dependencies or changing runtime
// configuration. Registering an external executable alone does not start it.
func (a *App) CheckCodexAppReady(ctx context.Context, action string, opt ComponentOptions) error {
	if action != "install" && action != "configure" && action != "start" {
		return nil
	}
	if action == "install" && !opt.Configure && opt.PasswordFile == "" && opt.Path != "managed" {
		return nil
	}
	return a.requireCodexAppCore(ctx)
}

func (a *App) requireCodexAppCore(ctx context.Context) error {
	if !a.S.Installed {
		return errors.New("CodexApp requires an installed core; install the background service first")
	}
	if err := a.Health(ctx); err != nil {
		return fmt.Errorf("CodexApp requires a ready core; start or repair the background service first: %w", err)
	}
	return nil
}

// Selecting the default preserves an existing credential. Only a missing
// managed credential generates a new secret; malformed/unreadable files must
// not silently rotate the password. This helper only prepares bytes in memory.
func codexAppPassword(source, destination string) ([]byte, error) {
	if source != "" && !filepath.IsAbs(source) {
		return nil, errors.New("CodexApp password file must be an absolute path")
	}
	path := source
	if path == "" {
		path = destination
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			secret := make([]byte, 32)
			if _, err = rand.Read(secret); err != nil {
				return nil, fmt.Errorf("generate CodexApp password: %w", err)
			}
			return []byte(base64.RawURLEncoding.EncodeToString(secret) + "\n"), nil
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("managed CodexApp password must be a regular file")
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("CodexApp password source must be a regular file")
	}
	password, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(password)) == "" {
		return nil, errors.New("empty CodexApp password; provide a non-empty password file")
	}
	return password, nil
}

func (a *App) installCodexApp(ctx context.Context, c *Component, opt ComponentOptions) error {
	node := a.componentBinary("node")
	if node == "" {
		var err error
		node, err = exec.LookPath("node")
		if err != nil {
			return errors.New("CodexApp requires Node; install/reuse node component first")
		}
	}
	if err := a.validateCodexAppNode(ctx, node); err != nil {
		return err
	}
	npm := filepath.Join(filepath.Dir(node), "npm")
	if _, err := os.Stat(npm); err != nil {
		return fmt.Errorf("npm matching Node unavailable: %w", err)
	}
	password, err := codexAppPassword(opt.PasswordFile, filepath.Join(c.Path, "password"))
	if err != nil {
		return err
	}
	port, err := componentPort(opt.Port)
	if err != nil {
		return err
	}
	if err = componentPortFree(port); err != nil {
		return err
	}
	version := opt.Version
	if version == "" {
		version = "0.1.90"
	}
	if !componentVersion.MatchString(version) {
		return errors.New("fixed CodexApp version required")
	}
	if err = AtomicWrite(filepath.Join(c.Path, "package.json"), []byte("{\"private\":true}\n"), 0600); err != nil {
		return err
	}
	env := map[string]string{"PATH": filepath.Dir(node) + string(os.PathListSeparator) + os.Getenv("PATH")}
	if _, err = a.installRun(ctx, env, npm, "install", "--prefix", c.Path, "--no-audit", "--no-fund", "--save-exact", "codexapp@"+version); err != nil {
		return err
	}
	if err = AtomicWrite(filepath.Join(c.Path, "password"), password, 0600); err != nil {
		return err
	}
	c.Options["node"] = node
	c.Options["port"] = strconv.Itoa(port)
	c.Version = version
	if a.S.Backend == "systemd" {
		unitPath := filepath.Join(a.P.Home, ".config", "systemd", "user", "codex-bg-codexapp.service")
		if _, err = os.Lstat(unitPath); err == nil {
			return errors.New("CodexApp service already exists; refusing overwrite")
		}
		unit := a.codexappUnit()
		if err = a.componentFile(c, unitPath, []byte(unit), 0600); err != nil {
			return err
		}
		if _, err = a.componentRun(ctx, nil, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		if _, err = a.componentRun(ctx, nil, "systemctl", "--user", "enable", "codex-bg-codexapp.service"); err != nil {
			return err
		}
	}
	return nil
}
func (a *App) validateCodexAppNode(ctx context.Context, node string) error {
	output, err := a.componentRun(ctx, nil, node, "--version")
	if err != nil {
		return err
	}
	var major int
	if _, err = fmt.Sscanf(strings.TrimSpace(string(output)), "v%d", &major); err != nil || major < 20 {
		return errors.New("CodexApp requires Node >=20; select a compatible runtime")
	}
	return nil
}
func systemdComponentQuote(s string) string {
	return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%").Replace(s) + "\""
}

type componentProcess struct {
	PID             int    `json:"pid"`
	Start           string `json:"start"`
	Executable      string `json:"executable"`
	OwnerPID        int    `json:"owner_pid,omitempty"`
	OwnerStart      string `json:"owner_start,omitempty"`
	OwnerExecutable string `json:"owner_executable,omitempty"`
}

func componentIdentity(pid int) (string, string, error) {
	// Linux's start-time field guards against PID reuse. macOS uses the stable
	// process start timestamp plus full argv from ps, without shell evaluation.
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err == nil {
		i := strings.LastIndex(string(b), ") ")
		if i < 0 {
			return "", "", errors.New("invalid process stat")
		}
		f := strings.Fields(string(b)[i+2:])
		if len(f) < 20 || f[0] == "Z" {
			return "", "", errors.New("short process stat")
		}
		cmd, e := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		return f[19], strings.ReplaceAll(string(cmd), "\x00", "\n"), e
	}
	start, e := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if e != nil {
		return "", "", e
	}
	cmd, e := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	return strings.TrimSpace(string(start)), strings.TrimSpace(string(cmd)), e
}
func componentProcessMatches(p componentProcess) bool {
	s, argv, err := componentIdentity(p.PID)
	return err == nil && s == p.Start && s != "" && (strings.Contains(argv, p.Executable+"\n") || strings.Contains(argv, p.Executable+" "))
}
func (a *App) runCodexApp(ctx context.Context, c *Component, foreground bool) error {
	if err := a.Health(ctx); err != nil {
		return fmt.Errorf("core not ready: %w", err)
	}
	if c.Ownership == "reused" && c.Options["managed_runtime"] != "true" {
		return errors.New("external CodexApp registered but not configured; use component configure with password/port")
	}
	if running, e := a.codexappExternalRunning(ctx, c); e != nil {
		return e
	} else if running {
		return errors.New("external CodexApp process is already running; original manager must stop it before managed start")
	}
	root := a.codexappRuntime(c)
	password, err := os.ReadFile(filepath.Join(root, "password"))
	if err != nil {
		return err
	}
	port, err := componentPort(c.Options["port"])
	if err != nil {
		return err
	}
	if err = componentPortFree(port); err != nil {
		return err
	}
	exe := codexappExecutable(c)
	args := []string{exe, "--no-login", "--no-tunnel", "--no-open", "--port", strconv.Itoa(port), "--password", strings.TrimRight(string(password), "\r\n")}
	env := map[string]string{"CODEXUI_CODEX_COMMAND": filepath.Join(a.P.Runtime, "codex"), "PATH": filepath.Dir(c.Options["node"]) + string(os.PathListSeparator) + os.Getenv("PATH")}
	if a.RunCommand != nil {
		_, err = a.componentRun(ctx, env, c.Options["node"], args...)
		return err
	}
	if foreground {
		return a.superviseCodexApp(ctx, c, c.Options["node"], args, env)
	}
	return a.launchCodexAppDetached(ctx, c, port)
}
func (a *App) StartComponent(ctx context.Context, name string) error {
	c := a.S.Components[name]
	if c == nil {
		return nil
	}
	if c.Ownership == "reused" && (name != "codexapp" || c.Options["managed_runtime"] != "true") {
		if name == "marketplace" {
			return a.checkMarketplace(c)
		}
		if name == "codexapp" {
			return errors.New("registered external CodexApp needs configure before managed start")
		}
		return nil
	}
	switch name {
	case "codexapp":
		if a.S.Backend == "systemd" {
			_, err := a.componentRun(ctx, nil, "systemctl", "--user", "start", "codex-bg-codexapp.service")
			return err
		}
		if b, e := os.ReadFile(filepath.Join(a.codexappRuntime(c), "process.json")); e == nil {
			var p componentProcess
			if json.Unmarshal(b, &p) == nil && componentOwnerMatches(p) {
				port, err := componentPort(c.Options["port"])
				if err != nil {
					return err
				}
				return a.waitCodexAppReady(ctx, c, port)
			}
		}
		return a.runCodexApp(ctx, c, false)
	case "apti":
		_, err := a.componentRun(ctx, nil, filepath.Join(c.Path, "apti"), "restore")
		return err
	case "branding":
		return a.applyBranding(ctx, c)
	case "marketplace":
		return a.applyMarketplace(ctx, c)
	}
	return nil
}
func (a *App) StopComponent(ctx context.Context, name string) error {
	c := a.S.Components[name]
	if c == nil || (c.Ownership == "reused" && c.Options["managed_runtime"] != "true") || name != "codexapp" {
		return nil
	}
	if a.S.Backend == "systemd" {
		unit := filepath.Join(a.P.Home, ".config", "systemd", "user", "codex-bg-codexapp.service")
		owned := false
		for _, f := range c.Files {
			if f.Path == unit {
				b, e := os.ReadFile(unit)
				if os.IsNotExist(e) {
					return nil
				}
				if e != nil || componentHash(b) != f.Hash {
					return errors.New("CodexApp unit changed externally; refusing to stop unknown service")
				}
				owned = true
			}
		}
		if !owned {
			return nil
		}
		_, err := a.componentRun(ctx, nil, "systemctl", "--user", "disable", "--now", "codex-bg-codexapp.service")
		return err
	}
	path := filepath.Join(a.codexappRuntime(c), "process.json")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var p componentProcess
	if err = json.Unmarshal(b, &p); err != nil {
		return err
	}
	if !componentOwnerMatches(p) {
		if _, _, e := componentIdentity(p.OwnerPID); e == nil {
			return errors.New("CodexApp PID identity mismatch; refusing signal")
		}
		return os.Remove(path)
	}
	process, err := os.FindProcess(p.OwnerPID)
	if err != nil {
		return err
	}
	if err = process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	for i := 0; i < 40; i++ {
		if !componentOwnerMatches(p) {
			err := os.Remove(path)
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return errors.New("CodexApp did not stop; preserve component")
}
