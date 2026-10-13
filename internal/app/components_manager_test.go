package app

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func componentTestApp(t *testing.T) *App {
	t.Helper()
	root := t.TempDir()
	a, err := New(filepath.Join(root, "home"), filepath.Join(root, "installer"), filepath.Join(root, "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	a.Out = io.Discard
	a.Err = io.Discard
	return a
}
func testEnv(env []string, key string) string {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return strings.TrimPrefix(e, key+"=")
		}
	}
	return ""
}
func TestComponentsDryRunDoesNotWrite(t *testing.T) {
	a := componentTestApp(t)
	if err := a.ComponentAction(context.Background(), "install", "node", ComponentOptions{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.P.Runtime); !os.IsNotExist(err) {
		t.Fatal("dry run wrote runtime")
	}
}
func TestComponentsReusePreservesExternalAndShellChanges(t *testing.T) {
	a := componentTestApp(t)
	external := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(external, []byte("external"), 0700); err != nil {
		t.Fatal(err)
	}
	a.RunCommand = func(_ context.Context, n string, args, env []string) ([]byte, error) {
		if n != external {
			t.Fatalf("unexpected command %s", n)
		}
		return []byte("v22.20.0\n"), nil
	}
	rc := filepath.Join(a.P.Home, ".bashrc")
	if err := AtomicWrite(rc, []byte("export PRIVATE_TOKEN=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.ComponentAction(context.Background(), "install", "node", ComponentOptions{Path: external, Shell: "bash", ShellIntegration: true, Yes: true}); err != nil {
		t.Fatal(err)
	}
	if a.S.Components["node"].Ownership != "reused" {
		t.Fatal("external adopted")
	}
	if info, e := os.Stat(rc); e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("profile permissions weakened")
	}
	b, _ := os.ReadFile(rc)
	b = append(b, []byte("# later user change\n")...)
	if err := os.WriteFile(rc, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.ComponentAction(context.Background(), "uninstall", "node", ComponentOptions{Yes: true}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(rc)
	if string(b) != "export PRIVATE_TOKEN=secret\n# later user change\n" {
		t.Fatalf("user shell content lost: %s", b)
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatal("external node deleted")
	}
	if files, _ := filepath.Glob(filepath.Join(a.P.Data, "backups", "*", "content")); len(files) != 0 {
		t.Fatal("shell secrets copied into backups")
	}
}
func TestComponentsNodeChecksumFailureNeverExtracts(t *testing.T) {
	a := componentTestApp(t)
	a.RunCommand = func(_ context.Context, n string, args, env []string) ([]byte, error) {
		if n != "curl" {
			t.Fatalf("unexpected command after bad digest: %s", n)
		}
		target := args[len(args)-1]
		if err := os.WriteFile(target, []byte("invalid archive or manifest"), 0600); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(testEnv(env, "TMPDIR"), a.P.Data) {
			t.Fatal("tmp escaped data")
		}
		if testEnv(env, "npm_config_cache") != filepath.Join(a.P.Data, "cache", "npm") {
			t.Fatal("npm cache escaped data")
		}
		return nil, nil
	}
	err := a.ComponentAction(context.Background(), "install", "node", ComponentOptions{Path: "managed", Yes: true})
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected checksum failure: %v", err)
	}
	if a.S.Components["node"].Status != "failed" {
		t.Fatal("failed install claimed success")
	}
	if err = a.ComponentAction(context.Background(), "uninstall", "node", ComponentOptions{Yes: true}); err != nil {
		t.Fatal(err)
	}
}
func TestComponentsCodexInstallerPrivateHomeAndCoreReference(t *testing.T) {
	a := componentTestApp(t)
	a.RunCommand = func(_ context.Context, n string, args, env []string) ([]byte, error) {
		switch n {
		case "curl":
			return nil, os.WriteFile(args[len(args)-1], []byte("# mock installer"), 0600)
		case "sh":
			if testEnv(env, "HOME") == a.P.Home || !strings.HasPrefix(testEnv(env, "HOME"), filepath.Join(a.P.Data, "tmp")) {
				t.Fatal("installer home not private")
			}
			if testEnv(env, "CODEX_NON_INTERACTIVE") != "1" {
				t.Fatal("interactive installer")
			}
			if testEnv(env, "CODEX_HOME") == a.S.CodexHome {
				t.Fatal("real auth path used")
			}
			if args[len(args)-1] != "0.153.4" {
				t.Fatal("fixed version lost")
			}
			return nil, nil
		default:
			if strings.HasSuffix(n, "/bin/codex") {
				return []byte("codex-cli 0.153.4\n"), nil
			}
			return nil, errors.New("unexpected command")
		}
	}
	candidate, err := a.InstallManagedCodex(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	a.S.Installed = true
	a.S.Codex = candidate
	if err = a.ComponentAction(context.Background(), "uninstall", "codex-cli", ComponentOptions{Yes: true}); err == nil {
		t.Fatal("removed referenced CLI")
	}
	if _, err = os.Stat(a.S.CodexHome); !os.IsNotExist(err) {
		t.Fatal("real auth created")
	}
	a.S.Installed = false
	if err = a.ComponentAction(context.Background(), "uninstall", "codex-cli", ComponentOptions{Yes: true}); err != nil {
		t.Fatal(err)
	}
}
func TestComponentsOwnershipRefusesOutsidePath(t *testing.T) {
	a := componentTestApp(t)
	outside := t.TempDir()
	a.S.Components["node"] = &Component{Name: "node", Path: outside, Ownership: "managed", Status: "installed"}
	if err := a.ComponentAction(context.Background(), "uninstall", "node", ComponentOptions{Yes: true}); err == nil {
		t.Fatal("outside managed path accepted")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("outside deleted")
	}
}
func TestComponentsBrandingConflictKeepsBackup(t *testing.T) {
	a := componentTestApp(t)
	if err := a.componentDirs(); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "favicon.ico")
	if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	backup, err := a.BackupFile(target, "branding", "branding")
	if err != nil {
		t.Fatal(err)
	}
	c := &Component{Name: "branding", Path: a.componentRoot("branding"), Ownership: "managed", Status: "installed", Files: []ManagedFile{{Path: target, Hash: componentHash([]byte("managed")), Kind: "branding"}}, Backups: []string{backup.ID}, Options: map[string]string{}}
	a.S.Components["branding"] = c
	if err = os.WriteFile(target, []byte("user changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.ComponentAction(context.Background(), "uninstall", "branding", ComponentOptions{Yes: true}); err == nil {
		t.Fatal("external branding overwritten")
	}
	if err = a.VerifyBackup(backup.ID); err != nil {
		t.Fatal("backup lost")
	}
}

func TestComponentsCondaFailureKeepsNarrowOwnership(t *testing.T) {
	a := componentTestApp(t)
	a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
		if name == "curl" {
			return nil, os.WriteFile(args[len(args)-1], []byte("#!/bin/sh\n# Created by constructor\n"), 0600)
		}
		if name != "bash" {
			t.Fatalf("unexpected command %s", name)
		}
		if args[len(args)-1] != filepath.Join(a.componentRoot("miniconda"), "runtime") {
			t.Fatal("Conda prefix would be relocated")
		}
		if !strings.HasPrefix(testEnv(env, "HOME"), filepath.Join(a.P.Data, "tmp")) {
			t.Fatal("Conda installer could mutate real HOME")
		}
		return nil, errors.New("simulated partial install")
	}
	if err := a.ComponentAction(context.Background(), "install", "miniconda", ComponentOptions{Version: "fixture", Yes: true}); err == nil {
		t.Fatal("failed installer accepted")
	}
	if a.S.Components["miniconda"].Status != "failed" {
		t.Fatal("failure state lost")
	}
	if err := a.ComponentAction(context.Background(), "uninstall", "miniconda", ComponentOptions{Yes: true}); err != nil {
		t.Fatal(err)
	}
}

func TestComponentsCodexAppRequiresCoreAndOwnsService(t *testing.T) {
	a := componentTestApp(t)
	a.P.Socket = runtimeFixture(t, runtimeResponder("idle"))
	root := a.componentRoot("codexapp")
	c := &Component{Name: "codexapp", Path: root, Ownership: "managed", Status: "installed", Options: map[string]string{"node": "/fixture/node"}}
	a.S.Components["codexapp"] = c
	if err := a.componentDirs(); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(filepath.Join(root, "password"), []byte("fixture-password"), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	l.Close()
	c.Options["port"] = port
	called := false
	a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
		called = true
		if name != "/fixture/node" {
			t.Fatalf("unexpected process %s", name)
		}
		if testEnv(env, "CODEXUI_CODEX_COMMAND") != filepath.Join(a.P.Runtime, "codex") {
			t.Fatal("wrong core wrapper")
		}
		if args[len(args)-1] != "fixture-password" {
			t.Fatal("password missing")
		}
		return nil, nil
	}
	if err = a.StartComponent(context.Background(), "codexapp"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("app not started after core health")
	}
	// A failed installation must not disable a pre-existing external service.
	called = false
	a.S.Backend = "systemd"
	if err = a.StopComponent(context.Background(), "codexapp"); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("unowned service modified")
	}
}

func TestComponentsChangedShellBlockPreservesExternal(t *testing.T) {
	a := componentTestApp(t)
	external := filepath.Join(t.TempDir(), "node")
	a.RunCommand = func(context.Context, string, []string, []string) ([]byte, error) { return []byte("v22.20.0"), nil }
	if err := a.ComponentAction(context.Background(), "install", "node", ComponentOptions{Path: external, Shell: "bash", ShellIntegration: true, Yes: true}); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(a.P.Home, ".bashrc")
	b, _ := os.ReadFile(rc)
	b = []byte(strings.ReplaceAll(string(b), "export PATH=", "export CUSTOM_PATH="))
	if err := os.WriteFile(rc, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.ComponentAction(context.Background(), "uninstall", "node", ComponentOptions{Yes: true}); err == nil {
		t.Fatal("modified shell block was removed")
	}
	after, _ := os.ReadFile(rc)
	if string(after) != string(b) {
		t.Fatal("shell changed despite conflict")
	}
}

func TestComponentsCodexUpgradePinsOldReleaseAndRestoresFailedSelection(t *testing.T) {
	a := componentTestApp(t)
	failVersion := ""
	a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
		if name == "curl" {
			return nil, os.WriteFile(args[len(args)-1], []byte("installer fixture"), 0600)
		}
		if name == "sh" {
			version := args[len(args)-1]
			release := filepath.Join(a.componentRoot("codex-cli"), "packages-home", "packages", "standalone", "releases", version)
			if err := AtomicWrite(filepath.Join(release, "codex"), []byte(version), 0700); err != nil {
				return nil, err
			}
			for p, target := range map[string]string{filepath.Join(a.componentRoot("codex-cli"), "bin", "codex"): filepath.Join(release, "codex"), filepath.Join(a.componentRoot("codex-cli"), "packages-home", "packages", "standalone", "current"): release} {
				_ = os.Remove(p)
				if err := os.Symlink(target, p); err != nil {
					return nil, err
				}
			}
			if version == failVersion {
				return nil, errors.New("fixture failure after switch")
			}
			return nil, nil
		}
		if strings.HasSuffix(name, "/bin/codex") {
			b, err := os.ReadFile(name)
			return []byte("codex-cli " + string(b)), err
		}
		return nil, errors.New("unexpected command")
	}
	old, err := a.InstallManagedCodex(context.Background(), "0.153.4")
	if err != nil {
		t.Fatal(err)
	}
	upgraded, err := a.InstallManagedCodex(context.Background(), "0.153.5")
	if err != nil {
		t.Fatal(err)
	}
	if old.Path == upgraded.Path || !strings.Contains(old.Path, "/releases/0.153.4/") {
		t.Fatal("core candidate not pinned to immutable release")
	}
	if info, e := os.Lstat(filepath.Dir(old.Path)); e != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("old runtime release bytes were not archived")
	}
	if b, e := os.ReadFile(old.Path); e != nil || string(b) != "0.153.4" {
		t.Fatal("rollback path broken by archival")
	}
	failVersion = "0.153.6"
	if _, err = a.InstallManagedCodex(context.Background(), failVersion); err == nil {
		t.Fatal("failed upgrade accepted")
	}
	selected, err := filepath.EvalSymlinks(filepath.Join(a.componentRoot("codex-cli"), "bin", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if selected != upgraded.Path {
		t.Fatal("failed selection not restored")
	}
	if a.S.Components["codex-cli"].Version != "0.153.5" {
		t.Fatal("failed version overwritten")
	}
}

func TestComponentsReusedCodexAppConfiguredOutsideInstallation(t *testing.T) {
	a := componentTestApp(t)
	a.P.Socket = runtimeFixture(t, runtimeResponder("idle"))
	externalRoot := t.TempDir()
	external := filepath.Join(externalRoot, "codexapp")
	if err := os.WriteFile(external, []byte("external installation"), 0700); err != nil {
		t.Fatal(err)
	}
	a.S.Components["node"] = &Component{Name: "node", Path: "/fixture/node", Ownership: "reused", Status: "installed"}
	launched := false
	a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
		if name == external {
			return []byte("codexapp 0.1.90"), nil
		}
		if name == "/fixture/node" {
			if len(args) == 1 && args[0] == "--version" {
				return []byte("v22.20.0"), nil
			}
			if args[0] != external {
				t.Fatal("external entry not used")
			}
			launched = true
			return nil, nil
		}
		return nil, errors.New("unexpected command")
	}
	if err := a.ComponentAction(context.Background(), "install", "codexapp", ComponentOptions{Path: external, Yes: true}); err != nil {
		t.Fatal(err)
	}
	if a.S.Components["codexapp"].Status != "available" {
		t.Fatal("unconfigured external app claimed installed")
	}
	if err := a.StartComponent(context.Background(), "codexapp"); err == nil {
		t.Fatal("unconfigured external app started")
	}
	password := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(password, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	l.Close()
	if err = a.ComponentAction(context.Background(), "configure", "codexapp", ComponentOptions{PasswordFile: password, Port: port, Yes: true}); err != nil {
		t.Fatal(err)
	}
	if !launched {
		t.Fatal("configured external executable not launched")
	}
	entries, _ := os.ReadDir(externalRoot)
	if len(entries) != 1 {
		t.Fatal("external tree modified")
	}
	if err = a.ComponentAction(context.Background(), "uninstall", "codexapp", ComponentOptions{Yes: true}); err != nil {
		t.Fatal(err)
	}
	if b, e := os.ReadFile(external); e != nil || string(b) != "external installation" {
		t.Fatal("external installation removed")
	}
	if _, e := os.Stat(a.componentRoot("codexapp")); !os.IsNotExist(e) {
		t.Fatal("private runtime metadata left behind")
	}
}

func TestComponentsExternalCodexAppRunningRequiresSeparateStop(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node fixture unavailable")
	}
	external := filepath.Join(t.TempDir(), "external-app.js")
	if err = os.WriteFile(external, []byte("setInterval(()=>{},1000)"), 0600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(node, external)
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	a := componentTestApp(t)
	c := &Component{Name: "codexapp", Path: external, Ownership: "reused", Status: "available", Options: map[string]string{}}
	a.S.Components["codexapp"] = c
	if err = a.ComponentAction(context.Background(), "configure", "codexapp", ComponentOptions{Yes: true}); err == nil || !strings.Contains(err.Error(), "external CodexApp is running") {
		t.Fatalf("running external app not protected: %v", err)
	}
	if _, _, err = componentIdentity(child.Process.Pid); err != nil {
		t.Fatal("external process was stopped")
	}
}

func TestComponentsUserEnvironmentsBlockRemoval(t *testing.T) {
	for _, name := range []string{"miniconda", "node"} {
		t.Run(name, func(t *testing.T) {
			a := componentTestApp(t)
			root := a.componentRoot(name)
			extra := filepath.Join(root, "runtime", "envs", "user-project")
			if name == "node" {
				extra = filepath.Join(root, "runtime", "lib", "node_modules", "user-tool")
			}
			if err := os.MkdirAll(extra, 0700); err != nil {
				t.Fatal(err)
			}
			a.S.Components[name] = &Component{Name: name, Path: root, Ownership: "managed", Status: "installed", Options: map[string]string{}}
			if err := a.ComponentAction(context.Background(), "uninstall", name, ComponentOptions{Yes: true}); err == nil {
				t.Fatal("user environment deleted")
			}
			if _, err := os.Stat(extra); err != nil {
				t.Fatal("user content removed")
			}
		})
	}
}

func TestComponentsBrandingRebuildReappliesOnlyKnownOriginals(t *testing.T) {
	a := componentTestApp(t)
	if err := a.componentDirs(); err != nil {
		t.Fatal(err)
	}
	media := t.TempDir()
	c := &Component{Name: "branding", Path: a.componentRoot("branding"), Ownership: "managed", Status: "installed", Options: map[string]string{"media": media}}
	entries, _ := componentAssets.ReadDir("branding")
	for _, entry := range entries {
		p := filepath.Join(media, entry.Name())
		if err := os.WriteFile(p, []byte("image original "+entry.Name()), 0644); err != nil {
			t.Fatal(err)
		}
		backup, err := a.BackupFile(p, "branding", "branding")
		if err != nil {
			t.Fatal(err)
		}
		c.Backups = append(c.Backups, backup.ID)
	}
	a.S.Components["branding"] = c
	if err := a.applyBranding(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	for _, id := range c.Backups {
		b, _ := a.ReadBackup(id)
		original, _ := os.ReadFile(b.DataPath)
		if err := os.WriteFile(b.Original, original, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.applyBranding(context.Background(), c); err != nil {
		t.Fatalf("container originals should be reapplied: %v", err)
	}
	changed := filepath.Join(media, "favicon.ico")
	if err := os.WriteFile(changed, []byte("new upstream image"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := a.applyBranding(context.Background(), c); err == nil {
		t.Fatal("new image content overwritten")
	}
	b, _ := os.ReadFile(changed)
	if string(b) != "new upstream image" {
		t.Fatal("new image bytes lost")
	}
}

func TestComponentsPrivilegedBrandingFailureKeepsBackupsAndCleansTemporary(t *testing.T) {
	a := componentTestApp(t)
	if err := a.componentDirs(); err != nil {
		t.Fatal(err)
	}
	media := t.TempDir()
	target := filepath.Join(media, "favicon.ico")
	if err := os.WriteFile(target, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	backup, err := a.BackupFile(target, "branding", "branding")
	if err != nil {
		t.Fatal(err)
	}
	c := &Component{Name: "branding", Path: a.componentRoot("branding"), Ownership: "managed", Options: map[string]string{"media": media}, Backups: []string{backup.ID}}
	var adjacent string
	cleaned := false
	a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
		if name != "sudo" || args[0] != "-n" {
			t.Fatal("unexpected privilege command")
		}
		switch args[1] {
		case "install":
			adjacent = args[len(args)-1]
			if filepath.Dir(adjacent) != media || !strings.HasPrefix(filepath.Base(adjacent), ".codex-bg-branding-") {
				t.Fatal("unbounded target")
			}
			return nil, os.WriteFile(adjacent, []byte("staged"), 0644)
		case "mv":
			return nil, errors.New("simulated root rename failure")
		case "rm":
			if args[len(args)-1] != adjacent {
				t.Fatal("cleanup wrong file")
			}
			cleaned = true
			return nil, os.Remove(adjacent)
		}
		return nil, errors.New("unexpected sudo operation")
	}
	if err = a.privilegedBrandingWrite(context.Background(), c, target, []byte("managed"), 0644); err == nil {
		t.Fatal("privileged failure swallowed")
	}
	if !cleaned {
		t.Fatal("adjacent temp leaked")
	}
	if err = a.VerifyBackup(backup.ID); err != nil {
		t.Fatal("backup lost")
	}
	b, _ := os.ReadFile(target)
	if string(b) != "original" {
		t.Fatal("failed write changed target")
	}
	tmp, _ := os.ReadDir(filepath.Join(a.P.Data, "tmp"))
	if len(tmp) != 0 {
		t.Fatal("source temp leaked")
	}
	if err = a.privilegedBrandingWrite(context.Background(), c, filepath.Join(media, "not-an-icon"), nil, 0644); err == nil {
		t.Fatal("arbitrary root target accepted")
	}
}
