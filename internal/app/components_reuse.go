package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (a *App) codexappRuntime(c *Component) string {
	if c.Ownership == "reused" {
		return a.componentRoot("codexapp")
	}
	return c.Path
}
func codexappExecutable(c *Component) string {
	if c.Ownership == "reused" {
		return c.Path
	}
	return filepath.Join(c.Path, "node_modules", ".bin", "codexapp")
}

// Detect an external process by an exact argv token resolved to the executable.
// We never adopt or signal a process merely because its command contains a name.
func (a *App) codexappExternalRunning(ctx context.Context, c *Component) (bool, error) {
	target := codexappExecutable(c)
	if resolved, e := filepath.EvalSymlinks(target); e == nil {
		target = resolved
	}
	expected := componentProcess{}
	if b, e := os.ReadFile(filepath.Join(a.codexappRuntime(c), "process.json")); e == nil {
		_ = json.Unmarshal(b, &expected)
	}
	if dirs, e := os.ReadDir("/proc"); e == nil {
		for _, d := range dirs {
			pid, e := strconv.Atoi(d.Name())
			if e != nil || pid == os.Getpid() {
				continue
			}
			if pid == expected.PID && componentProcessMatches(expected) {
				continue
			}
			argv, e := os.ReadFile(filepath.Join("/proc", d.Name(), "cmdline"))
			if e != nil {
				continue
			}
			for _, arg := range strings.Split(string(argv), "\x00") {
				if !filepath.IsAbs(arg) {
					continue
				}
				resolved, e := filepath.EvalSymlinks(arg)
				if e == nil && resolved == target {
					return true, nil
				}
			}
		}
		return false, nil
	}
	output, err := a.componentRun(ctx, nil, "ps", "-axo", "pid=,command=")
	if err != nil {
		return false, fmt.Errorf("cannot inspect external CodexApp processes: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, _ := strconv.Atoi(fields[0])
		if pid == os.Getpid() || (pid == expected.PID && componentProcessMatches(expected)) {
			continue
		}
		if strings.Contains(strings.Join(fields[1:], " "), target) {
			return true, nil
		}
	}
	return false, nil
}

func (a *App) configureCodexApp(ctx context.Context, c *Component, opt ComponentOptions) error {
	if c == nil {
		return errors.New("CodexApp is not registered; install or select --path first")
	}
	if running, err := a.codexappExternalRunning(ctx, c); err != nil {
		return err
	} else if running {
		return errors.New("external CodexApp is running; inspect and stop it through its original manager, then repeat configure; no external process was stopped")
	}
	if err := a.componentDirs(); err != nil {
		return err
	}
	root := a.codexappRuntime(c)
	if c.Ownership == "reused" && c.Options["managed_runtime"] != "true" {
		if _, e := os.Lstat(root); e == nil {
			return errors.New("private CodexApp metadata directory already exists without ownership")
		}
		if err := os.MkdirAll(root, 0700); err != nil {
			return err
		}
		c.Options["managed_runtime"] = "true"
		c.Status = "configured"
		if err := a.Save(); err != nil {
			return err
		}
	}
	if opt.PasswordFile == "" {
		if _, e := os.Stat(filepath.Join(root, "password")); e != nil {
			return errors.New("configure CodexApp requires --password-file")
		}
	}
	var password []byte
	var err error
	if opt.PasswordFile != "" {
		password, err = os.ReadFile(opt.PasswordFile)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(password)) == "" {
			return errors.New("empty CodexApp password")
		}
	}
	portText := opt.Port
	if portText == "" {
		portText = c.Options["port"]
	}
	port, err := componentPort(portText)
	if err != nil {
		return err
	}
	node := a.componentBinary("node")
	if node == "" {
		inv, e := a.Discover(ctx)
		if e != nil {
			return e
		}
		for _, candidate := range inv.Candidates["node"] {
			if candidate.Compatible {
				node = candidate.Path
				break
			}
		}
	}
	if node == "" {
		return errors.New("CodexApp needs a compatible Node selection")
	}
	if err = a.validateCodexAppNode(ctx, node); err != nil {
		return err
	}
	if c.Options["managed_runtime"] == "true" || c.Ownership == "managed" {
		if err = a.StopComponent(ctx, "codexapp"); err != nil {
			return err
		}
	}
	if err = componentPortFree(port); err != nil {
		return fmt.Errorf("CodexApp port already in use; external processes will not be stopped: %w", err)
	}
	if password != nil {
		if err = AtomicWrite(filepath.Join(root, "password"), password, 0600); err != nil {
			return err
		}
	}
	c.Options["node"] = node
	c.Options["port"] = strconv.Itoa(port)
	c.Options["managed_runtime"] = "true"
	c.Status = "configured"
	if err = a.Save(); err != nil {
		return err
	}
	if err = a.ensureCodexAppUnit(ctx, c); err != nil {
		return err
	}
	c.Status = "installed"
	if err = a.Save(); err != nil {
		return err
	}
	return a.StartComponent(ctx, "codexapp")
}

func (a *App) ensureCodexAppUnit(ctx context.Context, c *Component) error {
	if a.S.Backend != "systemd" {
		return nil
	}
	p := filepath.Join(a.P.Home, ".config", "systemd", "user", "codex-bg-codexapp.service")
	if b, err := os.ReadFile(p); err == nil {
		owned := false
		for _, f := range c.Files {
			if f.Path == p && componentHash(b) == f.Hash {
				owned = true
			}
		}
		if !owned {
			return errors.New("external CodexApp service exists; refusing to replace it")
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		unit := a.codexappUnit()
		if err = a.componentFile(c, p, []byte(unit), 0600); err != nil {
			return err
		}
	}
	if _, err := a.componentRun(ctx, nil, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	_, err := a.componentRun(ctx, nil, "systemctl", "--user", "enable", "codex-bg-codexapp.service")
	return err
}

func (a *App) checkComponentUserData(c *Component) error {
	if c.Name == "miniconda" {
		if original := c.Options["baseline_conda_history"]; original != "" {
			b, e := os.ReadFile(filepath.Join(c.Path, "runtime", "conda-meta", "history"))
			if e != nil || componentHash(b) != original {
				return errors.New("Conda base environment was modified after installation; export or relocate it before uninstall")
			}
		}
		dirs, err := os.ReadDir(filepath.Join(c.Path, "runtime", "envs"))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if len(dirs) > 0 {
			return errors.New("Conda contains user environments under runtime/envs; export or relocate them before uninstall")
		}
	}
	if c.Name == "node" {
		for _, name := range []string{"npm", "corepack"} {
			if original := c.Options["baseline_"+name]; original != "" {
				b, e := os.ReadFile(filepath.Join(c.Path, "runtime", "lib", "node_modules", name, "package.json"))
				if e != nil || componentHash(b) != original {
					return fmt.Errorf("Node bundled %s changed after installation; preserve/migrate it before uninstall", name)
				}
			}
		}
		for dir, allowed := range map[string]map[string]bool{"runtime/lib/node_modules": {"npm": true, "corepack": true}, "runtime/bin": {"node": true, "npm": true, "npx": true, "corepack": true, "yarn": true, "yarnpkg": true, "pnpm": true, "pnpx": true}} {
			entries, err := os.ReadDir(filepath.Join(c.Path, dir))
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			for _, entry := range entries {
				if !allowed[entry.Name()] {
					return fmt.Errorf("Node contains user-added global package or command %s; migrate it before uninstall", entry.Name())
				}
			}
		}
	}
	return nil
}
