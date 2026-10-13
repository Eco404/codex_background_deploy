package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var componentVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([+-][A-Za-z0-9.-]+)?$`)

func platformArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "x64", nil
	case "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("unsupported architecture %s", runtime.GOARCH)
}
func (a *App) installNode(ctx context.Context, c *Component, opt ComponentOptions) error {
	version := opt.Version
	if version == "" {
		version = "v22.20.0"
	}
	if !componentVersion.MatchString(version) {
		return errors.New("invalid Node version")
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	arch, err := platformArch()
	if err != nil {
		return err
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return errors.New("Node addon currently supports Linux/macOS")
	}
	archive := "node-" + version + "-" + runtime.GOOS + "-" + arch + ".tar.gz"
	cache := filepath.Join(a.P.Data, "cache", "node-"+version)
	if err = os.MkdirAll(cache, 0700); err != nil {
		return err
	}
	tarPath := filepath.Join(cache, archive)
	sumPath := filepath.Join(cache, "SHASUMS256.txt")
	base := "https://nodejs.org/dist/" + version + "/"
	if err = a.componentDownload(ctx, base+archive, tarPath); err != nil {
		return err
	}
	if err = a.componentDownload(ctx, base+"SHASUMS256.txt", sumPath); err != nil {
		return err
	}
	sums, err := os.ReadFile(sumPath)
	if err != nil {
		return err
	}
	expected := ""
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == archive {
			if expected != "" {
				return errors.New("duplicate Node checksum")
			}
			expected = f[0]
		}
	}
	payload, err := os.ReadFile(tarPath)
	if err != nil {
		return err
	}
	if len(expected) != 64 || componentHash(payload) != expected {
		return errors.New("Node archive checksum mismatch")
	}
	root := filepath.Join(c.Path, "runtime")
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	if _, err = a.componentRun(ctx, nil, "tar", "-xzf", tarPath, "--strip-components=1", "-C", root); err != nil {
		return err
	}
	b, err := a.componentRun(ctx, nil, filepath.Join(root, "bin", "node"), "--version")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) != version {
		return errors.New("installed Node version mismatch")
	}
	c.Version = version
	for _, name := range []string{"npm", "corepack"} {
		path := filepath.Join(root, "lib", "node_modules", name, "package.json")
		if b, e := os.ReadFile(path); e == nil {
			c.Options["baseline_"+name] = componentHash(b)
		}
	}
	return nil
}
func (a *App) installConda(ctx context.Context, c *Component, opt ComponentOptions) error {
	arch, err := platformArch()
	if err != nil {
		return err
	}
	if arch == "x64" {
		arch = "x86_64"
	} else {
		arch = "aarch64"
	}
	platform := "Linux"
	if runtime.GOOS == "darwin" {
		platform = "MacOSX"
		if arch == "aarch64" {
			arch = "arm64"
		}
	} else if runtime.GOOS != "linux" {
		return errors.New("Conda addon currently supports Linux/macOS")
	}
	version := opt.Version
	if version == "" {
		version = "latest"
	}
	if strings.ContainsAny(version, "/\\\r\n ") {
		return errors.New("invalid Miniconda version")
	}
	installer := filepath.Join(a.P.Data, "cache", "Miniconda3-"+version+"-"+platform+"-"+arch+".sh")
	if err = a.componentDownload(ctx, "https://repo.anaconda.com/miniconda/"+filepath.Base(installer), installer); err != nil {
		return err
	}
	prefix := filepath.Join(c.Path, "runtime")
	installerHome, err := os.MkdirTemp(filepath.Join(a.P.Data, "tmp"), "conda-installer-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(installerHome)
	if _, err = a.componentRun(ctx, map[string]string{"HOME": installerHome, "BASH_ENV": "", "ENV": "", "CONDA_PKGS_DIRS": filepath.Join(a.P.Data, "cache", "conda")}, "bash", installer, "-b", "-p", prefix); err != nil {
		return err
	}
	b, err := a.componentRun(ctx, nil, filepath.Join(prefix, "bin", "conda"), "--version")
	if err != nil {
		return err
	}
	c.Version = strings.TrimSpace(string(b))
	if history, e := os.ReadFile(filepath.Join(prefix, "conda-meta", "history")); e == nil {
		c.Options["baseline_conda_history"] = componentHash(history)
	}
	return nil
}
func (a *App) installCodex(ctx context.Context, c *Component, version string) error {
	if version == "" {
		version = "0.153.4"
	}
	if !componentVersion.MatchString(version) {
		return errors.New("Codex requires a fixed version")
	}
	version = strings.TrimPrefix(version, "v")
	script := filepath.Join(a.P.Data, "cache", "codex-install.sh")
	if err := a.componentDownload(ctx, "https://chatgpt.com/codex/install.sh", script); err != nil {
		return err
	}
	home, err := os.MkdirTemp(filepath.Join(a.P.Data, "tmp"), "codex-installer-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	codeHome := filepath.Join(c.Path, "packages-home")
	bin := filepath.Join(c.Path, "bin")
	for _, d := range []string{home, codeHome, bin} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return err
		}
	}
	env := map[string]string{"HOME": home, "CODEX_HOME": codeHome, "CODEX_INSTALL_DIR": bin, "CODEX_NON_INTERACTIVE": "1", "CODEX_RELEASE": version, "CODEX_INSTALL_DAEMON_ONLY": "0", "CODEX_INSTALL_DEFER_SELECTION": "0", "CODEX_INSTALL_IF_LATEST": "0", "CODEX_INSTALL_IF_CURRENT": "0", "BASH_ENV": "", "ENV": "", "PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH")}
	if _, err := a.componentRun(ctx, env, "sh", script, "--release", version); err != nil {
		return err
	}
	b, err := a.componentRun(ctx, env, filepath.Join(bin, "codex"), "--version")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) != "codex-cli "+version {
		return errors.New("installed Codex version mismatch")
	}
	c.Version = version
	return nil
}
func (a *App) integrateShell(c *Component, opt ComponentOptions) error {
	shell := opt.Shell
	if shell == "" {
		shell = filepath.Base(os.Getenv("SHELL"))
	}
	if shell == "" {
		shell = "bash"
	}
	if shell != "bash" && shell != "zsh" {
		return errors.New("shell integration supports bash/zsh only; select --shell or disable integration")
	}
	rc := filepath.Join(a.P.Home, "."+shell+"rc")
	old, err := os.ReadFile(rc)
	created := os.IsNotExist(err)
	mode := os.FileMode(0600)
	if info, e := os.Stat(rc); e == nil {
		mode = info.Mode().Perm()
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	begin := "# >>> codex-bg " + c.Name + " >>>"
	end := "# <<< codex-bg " + c.Name + " <<<"
	if strings.Contains(string(old), begin) {
		return errors.New("existing shell block has no ownership record")
	}
	bin := filepath.Dir(a.componentBinary(c.Name))
	block := "\n" + begin + "\nexport PATH=" + shellQuote(bin) + ":\"$PATH\"\n"
	if c.Name == "miniconda" {
		prefix := filepath.Join(c.Path, "runtime")
		if c.Ownership == "reused" {
			prefix = filepath.Dir(filepath.Dir(c.Path))
		}
		block += "export CONDA_AUTO_ACTIVATE_BASE=false\n. " + shellQuote(filepath.Join(prefix, "etc", "profile.d", "conda.sh")) + "\n"
	}
	block += end + "\n"
	c.Options["shell_file"] = rc
	c.Options["shell_block"] = block
	c.Options["shell_created"] = fmt.Sprint(created)
	// Persist the exact reversible block before modifying the user's file; never
	// copy a full shell profile (which may contain credentials) into backups.
	if err = a.Save(); err != nil {
		return err
	}
	if err = a.ReplaceBytes(rc, append(old, []byte(block)...), mode); err != nil {
		return err
	}
	return a.Save()
}
func (a *App) removeShellIntegration(c *Component) error {
	p := c.Options["shell_file"]
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) && c.Options["shell_created"] == "true" {
		return nil
	}
	if err != nil {
		return err
	}
	block := c.Options["shell_block"]
	if !strings.Contains(string(b), "# >>> codex-bg "+c.Name+" >>>") {
		return nil
	}
	if strings.Count(string(b), block) != 1 {
		return fmt.Errorf("shell integration changed; preserve %s and backups", p)
	}
	rest := strings.Replace(string(b), block, "", 1)
	if c.Options["shell_created"] == "true" && rest == "" {
		return os.Remove(p)
	}
	mode := os.FileMode(0600)
	if info, e := os.Stat(p); e == nil {
		mode = info.Mode().Perm()
	}
	return AtomicWrite(p, []byte(rest), mode)
}
