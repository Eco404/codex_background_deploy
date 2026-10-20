package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func legacyCodexAppFixture(t *testing.T) (*App, InstallOptions) {
	t.Helper()
	return legacyCodexAppFixtureAt(t, componentTestApp(t))
}
func legacyCodexAppFixtureAt(t *testing.T, a *App) (*App, InstallOptions) {
	t.Helper()
	entry := filepath.Join(a.P.Home, "apps/codexapp/node_modules/codexapp/cli.js")
	files := map[string][]byte{
		filepath.Join(a.P.Home, "entrypoint.d/10-start-codexapp.sh"): legacyCodexAppHook,
		entry: []byte("#!/usr/bin/env node\n// inert fixture\n"),
		filepath.Join(filepath.Dir(entry), "package.json"):         []byte(`{"name":"codexapp","version":"0.1.90","bin":{"codexapp":"cli.js"}}`),
		filepath.Join(a.P.Home, ".config/codexapp/webui-password"): []byte("test-password-kept\n"),
	}
	for p, b := range files {
		if err := AtomicWrite(p, b, 0700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(a.P.Home, "apps/codexapp/node_modules/.bin/codexapp")
	if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(entry, link); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(a.P.Home, "external-node/node")
	if err := AtomicWrite(node, []byte("fixture node"), 0700); err != nil {
		t.Fatal(err)
	}
	a.S.Components["node"] = &Component{Name: "node", Path: node, Ownership: "reused", Status: "installed"}
	a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
		if name == node {
			if len(args) == 1 && args[0] == "--version" {
				return []byte("v22.20.0"), nil
			}
			if len(args) == 2 && args[1] == "--help" {
				return []byte("--port --password --no-login --no-tunnel --no-open"), nil
			}
			return nil, nil
		}
		return nil, errors.New("unexpected command")
	}
	t.Setenv("CODEXAPP_PORT", passwordTestPort(t))
	return a, InstallOptions{Backend: "container", MigrateCodexApp: true, Yes: true}
}
func TestLegacyCodexAppPlanNeedsExplicitOptInAndWritesNothing(t *testing.T) {
	a, opt := legacyCodexAppFixture(t)
	opt.MigrateCodexApp = false
	if _, err := a.planLegacyCodexApp(context.Background(), opt); err == nil || !strings.Contains(err.Error(), "--migrate-codexapp") {
		t.Fatalf("missing opt-in guard: %v", err)
	}
	opt.MigrateCodexApp = true
	p, err := a.planLegacyCodexApp(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || p.Version != "0.1.90" || p.Process != nil {
		t.Fatalf("wrong plan: %+v", p)
	}
	for _, path := range []string{a.P.Runtime, a.P.Data} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("read-only planning created %s", path)
		}
	}
	raw, _ := json.Marshal(p)
	if bytes.Contains(raw, []byte("test-password-kept")) {
		t.Fatal("credential leaked to plan")
	}
}
func TestLegacyCodexAppPlanRejectsChangedHookAndUnknownDependency(t *testing.T) {
	t.Run("changed known hook", func(t *testing.T) {
		a, opt := legacyCodexAppFixture(t)
		path := filepath.Join(a.P.Home, "entrypoint.d/10-start-codexapp.sh")
		b := append(append([]byte{}, legacyCodexAppHook...), []byte("\n# local change\n")...)
		if err := os.WriteFile(path, b, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := a.planLegacyCodexApp(context.Background(), opt); err == nil || !strings.Contains(err.Error(), "differs") {
			t.Fatalf("changed hook accepted: %v", err)
		}
	})
	t.Run("custom dependency", func(t *testing.T) {
		a, opt := legacyCodexAppFixture(t)
		path := filepath.Join(a.P.Home, "entrypoint.d/custom.sh")
		if err := AtomicWrite(path, []byte("CODEXUI_CODEX_COMMAND=$HOME/apps/codex-daemon-bridge/codex\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := a.planLegacyCodexApp(context.Background(), opt); err == nil || !strings.Contains(err.Error(), "unrecognized") {
			t.Fatalf("custom dependency accepted: %v", err)
		}
	})
}
func TestLegacyCodexAppHandoffPreservesCredentialAndExternalFiles(t *testing.T) {
	a, opt := legacyCodexAppFixture(t)
	p, err := a.planLegacyCodexApp(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	oldEntry, _ := os.ReadFile(p.Entry)
	oldCredential, _ := os.ReadFile(p.PasswordFile)
	tx, err := a.begin("migrate")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.prepareLegacyCodexApp(tx, p); err != nil {
		t.Fatal(err)
	}
	backup, err := a.ReadBackup(p.HookBackupID)
	if err != nil {
		t.Fatal(err)
	}
	if backup.Hash != p.HookHash || !Within(a.P.Data, backup.DataPath) {
		t.Fatal("original hook not centrally backed up")
	}
	if _, err = os.Stat(p.Hook); !os.IsNotExist(err) {
		t.Fatal("old startup hook remains active")
	}
	if err = a.stopLegacyCodexApp(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	a.S.Installed = true
	a.S.Backend = "container"
	a.P.Socket = runtimeFixture(t, runtimeResponder("idle"))
	if err = a.configureMigratedCodexApp(context.Background(), tx, p); err != nil {
		t.Fatal(err)
	}
	c := a.S.Components["codexapp"]
	if c == nil || c.Ownership != "reused" || c.Path != p.Entry || c.Options["node"] != p.Node {
		t.Fatalf("unexpected component: %+v", c)
	}
	password, err := os.ReadFile(a.CodexAppPasswordPath())
	if err != nil || !bytes.Equal(password, oldCredential) {
		t.Fatal("password rotated")
	}
	info, _ := os.Stat(a.CodexAppPasswordPath())
	if info.Mode().Perm() != 0600 {
		t.Fatal("credential mode not private")
	}
	for _, path := range []string{a.P.StateFile(), filepath.Join(a.P.Data, "transactions/active.json")} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, bytes.TrimSpace(oldCredential)) {
			t.Fatal("credential leaked to state/transaction")
		}
	}
	if err = a.cleanupMigratedCodexApp(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(a.componentRoot("codexapp")); !os.IsNotExist(err) {
		t.Fatal("migration runtime retained after rollback cleanup")
	}
	for path, expected := range map[string][]byte{p.Entry: oldEntry, p.PasswordFile: oldCredential} {
		b, e := os.ReadFile(path)
		if e != nil || !bytes.Equal(b, expected) {
			t.Fatalf("external file changed: %s", path)
		}
	}
	if err = a.RestoreBackup(p.HookBackupID); err != nil {
		t.Fatal(err)
	}
	if err = a.restoreLegacyCodexApp(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
}
func TestLegacyCodexAppPrepareFailurePreservesRestoreMaterial(t *testing.T) {
	a, opt := legacyCodexAppFixture(t)
	p, err := a.planLegacyCodexApp(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := a.begin("migrate")
	if err != nil {
		t.Fatal(err)
	}
	// The runtime ownership conflict appears after preview. No process has been
	// stopped, and the removed hook must already have a durable restore record.
	if err = os.MkdirAll(a.componentRoot("codexapp"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = a.prepareLegacyCodexApp(tx, p); err == nil {
		t.Fatal("post-preview runtime conflict accepted")
	}
	if p.StopRequested {
		t.Fatal("preparation stopped a process")
	}
	b, err := a.ReadBackup(p.HookBackupID)
	if err != nil || b.Hash != p.HookHash {
		t.Fatal("failure lost restore material")
	}
	if err = a.RestoreBackup(p.HookBackupID); err != nil {
		t.Fatal(err)
	}
}
func TestLegacyCodexAppRollbackRefusesUnknownRuntime(t *testing.T) {
	a, opt := legacyCodexAppFixture(t)
	p, err := a.planLegacyCodexApp(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := a.begin("migrate")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.prepareLegacyCodexApp(tx, p); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(a.componentRoot("codexapp"), "migration-id")
	if err = os.WriteFile(marker, []byte("another-transaction"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.cleanupMigratedCodexApp(context.Background(), tx); err == nil {
		t.Fatal("unknown runtime removed")
	}
	if _, err = os.Stat(a.componentRoot("codexapp")); err != nil {
		t.Fatal("unknown runtime not preserved")
	}
}

func startLegacyCodexAppFixture(t *testing.T, a *App) (*exec.Cmd, string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("isolated process test requires existing Node")
	}
	node, err = filepath.EvalSymlinks(node)
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(a.P.Home, "apps/codexapp/node_modules/.bin/codexapp")
	// This local fake never loads Codex, authenticates, or reaches the network.
	js := `#!/usr/bin/env node
if(process.argv.includes('--help')) { console.log('--port --password --no-login --no-tunnel --no-open'); process.exit(0); }
const http=require('http');
const port=Number(process.argv[process.argv.indexOf('--port')+1]);
http.createServer((req,res)=>res.end('fixture')).listen(port,'127.0.0.1');
`
	resolved, _ := filepath.EvalSymlinks(entry)
	if err = os.WriteFile(resolved, []byte(js), 0700); err != nil {
		t.Fatal(err)
	}
	a.S.Components["node"] = &Component{Name: "node", Path: node, Ownership: "reused", Status: "installed"}
	a.RunCommand = nil
	port := os.Getenv("CODEXAPP_PORT")
	cmd := exec.Command(node, entry, "--no-login", "--no-tunnel", "--no-open", "--port", port, "--password", "test-password-kept")
	cmd.Env = append(os.Environ(), "HOME="+a.P.Home, "CODEX_HOME="+a.S.CodexHome, "CODEXUI_CODEX_COMMAND="+filepath.Join(a.P.Home, "apps/codex-daemon-bridge/codex"))
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	process, err := legacyCodexAppIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(port)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err = waitLegacyCodexAppHTTP(ctx, process, n); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(a.P.Home, ".local/state/codexapp/codexapp.pid")
	if err = AtomicWrite(pidfile, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return cmd, node
}
func TestLegacyCodexAppProcessIdentityStopAndRollback(t *testing.T) {
	a, opt := legacyCodexAppFixture(t)
	old, _ := startLegacyCodexAppFixture(t, a)
	p, err := a.planLegacyCodexApp(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if p.Process == nil || p.Process.PID != old.Process.Pid || p.Process.UID != uint32(os.Getuid()) {
		t.Fatalf("wrong identity: %+v", p.Process)
	}
	tx, err := a.begin("migrate")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.prepareLegacyCodexApp(tx, p); err != nil {
		t.Fatal(err)
	}
	// A forged start time must not signal the actual fixture process.
	original := p.Process.Start
	p.Process.Start = "wrong-start"
	if err = a.stopLegacyCodexApp(context.Background(), tx); err == nil {
		t.Fatal("wrong identity accepted")
	}
	p.Process.Start = original
	if !legacyCodexAppSameProcess(p.Process) {
		t.Fatal("wrong identity test stopped the fixture")
	}
	if err = a.stopLegacyCodexApp(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if legacyCodexAppSameProcess(p.Process) {
		t.Fatal("old fixture still running")
	}
	if err = a.cleanupMigratedCodexApp(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if err = a.RestoreBackup(p.HookBackupID); err != nil {
		t.Fatal(err)
	}
	if err = a.restoreLegacyCodexApp(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if p.RestoredProcess != nil && legacyCodexAppSameProcess(p.RestoredProcess) {
			process, _ := os.FindProcess(p.RestoredProcess.PID)
			_ = process.Kill()
		}
	})
	if !legacyCodexAppSameProcess(p.RestoredProcess) || p.RestoredProcess.PID == p.Process.PID {
		t.Fatal("rollback did not restore the fixture")
	}
	if err = a.restoreLegacyCodexApp(context.Background(), tx); err != nil {
		t.Fatal("recovery was not idempotent", err)
	}
}

func TestLegacyCodexAppStorageRootsMustMatch(t *testing.T) {
	a, opt := legacyCodexAppFixture(t)
	p, err := a.planLegacyCodexApp(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HOME", "CODEX_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Run(key, func(t *testing.T) {
			env := a.componentEnv(map[string]string{"CODEXUI_CODEX_COMMAND": p.Bridge})
			for i, v := range env {
				if strings.HasPrefix(v, key+"=") {
					env = append(env[:i], env[i+1:]...)
					break
				}
			}
			env = append(env, key+"=/unrelated-storage")
			if err := a.validateLegacyCodexAppEnvironment(p, []byte(strings.Join(env, "\x00"))); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("storage mismatch not refused: %v", err)
			}
		})
	}
	env := a.componentEnv(map[string]string{"CODEXUI_CODEX_COMMAND": p.Bridge})
	if err := a.validateLegacyCodexAppEnvironment(p, []byte(strings.Join(env, "\x00"))); err != nil {
		t.Fatal("matching roots rejected", err)
	}
}
