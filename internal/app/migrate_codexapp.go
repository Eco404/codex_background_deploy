package app

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// This is a reviewed copy of the legacy code-server installer hook. Only this
// exact implementation is supported; recognizing a few words is not ownership.
//
//go:embed legacydata/code-server-codexapp-start.sh.txt
var legacyCodexAppHook []byte

// LegacyCodexAppPlan intentionally contains no passwords, command lines, or
// process environment. It is persisted before the first destructive action.
type LegacyCodexAppPlan struct {
	Hook, HookHash, HookBackupID            string
	Entry, EntryHash, Version               string
	Node, NodeHash                          string
	Bridge, PasswordFile, PIDFile           string
	Port                                    int
	Process                                 *LegacyCodexAppProcess
	RuntimeOwned, StopRequested, Configured bool
	RestoredProcess                         *LegacyCodexAppProcess
}
type LegacyCodexAppProcess struct {
	PID        int
	Start      string
	UID        uint32
	Executable string
}

func legacyCodexAppIdentity(pid int) (*LegacyCodexAppProcess, error) {
	if pid <= 1 {
		return nil, errors.New("invalid legacy CodexApp PID")
	}
	start, _, err := componentIdentity(pid)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("cannot verify legacy CodexApp process UID")
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return nil, err
	}
	return &LegacyCodexAppProcess{PID: pid, Start: start, UID: stat.Uid, Executable: exe}, nil
}
func legacyCodexAppSameProcess(p *LegacyCodexAppProcess) bool {
	if p == nil {
		return false
	}
	current, err := legacyCodexAppIdentity(p.PID)
	return err == nil && *current == *p
}
func legacyBridge(home, path string) bool {
	for _, rel := range []string{"apps/codex-daemon-bridge/codex", ".local/share/codex-daemon-bridge/codex", ".local/share/codex-background/codex"} {
		if path == filepath.Join(home, rel) {
			return true
		}
	}
	return false
}
func sameExecutable(a, b string) bool {
	x, e1 := filepath.EvalSymlinks(a)
	y, e2 := filepath.EvalSymlinks(b)
	return e1 == nil && e2 == nil && x == y
}

// Read-only dependency discovery also catches running custom launchers, so a
// missing or renamed known hook cannot accidentally permit daemon shutdown.
func (a *App) legacyCodexAppProcesses(entry string) ([]*LegacyCodexAppProcess, error) {
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var result []*LegacyCodexAppProcess
	for _, d := range dirs {
		pid, e := strconv.Atoi(d.Name())
		if e != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		info, e := os.Stat(filepath.Join("/proc", d.Name()))
		if e != nil {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) {
			continue
		}
		raw, e := os.ReadFile(filepath.Join("/proc", d.Name(), "cmdline"))
		if e != nil || len(raw) == 0 {
			continue
		}
		argv := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		matchesEntry := len(argv) > 1 && filepath.IsAbs(argv[1]) && sameExecutable(argv[1], entry)
		env, e := os.ReadFile(filepath.Join("/proc", d.Name(), "environ"))
		if e != nil {
			if matchesEntry {
				return nil, fmt.Errorf("cannot inspect legacy CodexApp environment for PID %d", pid)
			}
			continue
		}
		depends := false
		for _, variable := range strings.Split(string(env), "\x00") {
			k, v, ok := strings.Cut(variable, "=")
			if ok && k == "CODEXUI_CODEX_COMMAND" && legacyBridge(a.P.Home, v) {
				depends = true
			}
		}
		if !matchesEntry && !depends {
			continue
		}
		if !matchesEntry || !depends {
			return nil, fmt.Errorf("CodexApp process %d has an unrecognized launcher or bridge; stop/migrate it through its original manager", pid)
		}
		identity, e := legacyCodexAppIdentity(pid)
		if e != nil {
			return nil, e
		}
		result = append(result, identity)
	}
	return result, nil
}

func (a *App) planLegacyCodexApp(ctx context.Context, opt InstallOptions) (*LegacyCodexAppPlan, error) {
	hook := filepath.Join(a.P.Home, "entrypoint.d/10-start-codexapp.sh")
	entry := filepath.Join(a.P.Home, "apps/codexapp/node_modules/.bin/codexapp")
	processes, err := a.legacyCodexAppProcesses(entry)
	if err != nil {
		return nil, err
	}
	var raw []byte
	directories := []string{filepath.Dir(hook), filepath.Join(a.P.Home, ".config/systemd/user")}
	if opt.EntrypointDir != "" && opt.EntrypointDir != directories[0] {
		directories = append(directories, opt.EntrypointDir)
	}
	for _, dir := range directories {
		files, e := os.ReadDir(dir)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		for _, f := range files {
			if f.IsDir() || !(strings.HasSuffix(f.Name(), ".sh") || strings.HasSuffix(f.Name(), ".service")) {
				continue
			}
			path := filepath.Join(dir, f.Name())
			b, e := os.ReadFile(path)
			if e != nil {
				return nil, e
			}
			text := string(b)
			if path == hook {
				raw = b
				continue
			}
			if (strings.Contains(strings.ToLower(text), "codexapp") || strings.Contains(text, "CODEXUI_CODEX_COMMAND")) && (strings.Contains(text, "codex-daemon-bridge") || strings.Contains(text, "codex-background")) {
				return nil, fmt.Errorf("unrecognized CodexApp dependency %s; resolve its old-backend dependency before migration", path)
			}
		}
	}
	if len(raw) == 0 && len(processes) == 0 {
		return nil, nil
	}
	if len(raw) == 0 {
		return nil, errors.New("running legacy CodexApp has no recognized startup hook; preserve the old backend and migrate it through its original manager")
	}
	if err = checkParents(hook); err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, legacyCodexAppHook) {
		return nil, fmt.Errorf("legacy CodexApp hook differs from the reviewed installer source: %s; automatic handoff refused", hook)
	}
	fmt.Fprintf(a.Out, "旧 CodexApp 依赖: hook=%s; 程序=%s; 旧 bridge=%s\n", hook, entry, filepath.Join(a.P.Home, "apps/codex-daemon-bridge/codex"))
	if !opt.MigrateCodexApp {
		return nil, errors.New("旧 CodexApp 仍依赖旧后台；请显式选择迁移 CodexApp（--migrate-codexapp），或先通过原管理方式解除依赖；本次未停止服务")
	}
	if opt.Backend != "container" {
		return nil, errors.New("legacy container CodexApp handoff requires the container backend")
	}
	if a.S.Components["codexapp"] != nil {
		return nil, errors.New("CodexApp already registered; resolve the existing component before legacy handoff")
	}
	if len(processes) > 1 {
		return nil, errors.New("multiple legacy CodexApp processes found; resolve duplicates before migration")
	}
	if _, e := os.Lstat(a.componentRoot("codexapp")); !os.IsNotExist(e) {
		return nil, errors.New("CodexApp runtime already exists without migration ownership")
	}
	probe, err := a.probeCodexAppDetails(ctx, entry)
	if err != nil {
		return nil, err
	}
	p := &LegacyCodexAppPlan{Hook: hook, HookHash: HashBytes(raw), Entry: entry, Version: probe.Version, Node: probe.Node, Bridge: filepath.Join(a.P.Home, "apps/codex-daemon-bridge/codex"), PasswordFile: filepath.Join(a.P.Home, ".config/codexapp/webui-password"), PIDFile: filepath.Join(a.P.Home, ".local/state/codexapp/codexapp.pid"), Port: 8214}
	if len(processes) > 0 {
		p.Process = processes[0]
		p.Node = p.Process.Executable
	}
	if p.Node == "" {
		return nil, errors.New("legacy CodexApp has no verified Node runtime")
	}
	if err = a.validateCodexAppNode(ctx, p.Node); err != nil {
		return nil, err
	}
	if p.EntryHash, err = Hash(p.Entry); err != nil {
		return nil, err
	}
	if p.NodeHash, err = Hash(p.Node); err != nil {
		return nil, err
	}
	if _, err = codexAppPassword(p.PasswordFile, ""); err != nil {
		return nil, fmt.Errorf("existing CodexApp credential must be preserved: %w", err)
	}
	if p.Process != nil {
		if p.Port, err = a.validateLegacyCodexAppProcess(p, p.Process); err != nil {
			return nil, err
		}
	} else {
		if port := os.Getenv("CODEXAPP_PORT"); port != "" {
			p.Port, err = componentPort(port)
			if err != nil {
				return nil, err
			}
		}
		if err = componentPortFree(p.Port); err != nil {
			return nil, fmt.Errorf("legacy CodexApp port conflict: %w", err)
		}
	}
	fmt.Fprintf(a.Out, "CodexApp 交接: 复用 %s；Node=%s；端口=%d；保留密码文件=%s；旧 hook 集中备份后停用\n", p.Version, p.Node, p.Port, p.PasswordFile)
	if p.Process != nil {
		fmt.Fprintf(a.Out, "CodexApp 进程: PID=%d UID=%d start=%s executable=%s\n", p.Process.PID, p.Process.UID, p.Process.Start, p.Process.Executable)
	}
	return p, nil
}

func (a *App) validateLegacyCodexAppProcess(p *LegacyCodexAppPlan, process *LegacyCodexAppProcess) (int, error) {
	if !legacyCodexAppSameProcess(process) || process.UID != uint32(os.Getuid()) || !sameExecutable(process.Executable, p.Node) {
		return 0, errors.New("legacy CodexApp process identity changed; no signal sent")
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", process.PID))
	if err != nil {
		return 0, err
	}
	argv := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if len(argv) != 9 || !sameExecutable(argv[1], p.Entry) || argv[2] != "--no-login" || argv[3] != "--no-tunnel" || argv[4] != "--no-open" || argv[5] != "--port" || argv[7] != "--password" {
		return 0, errors.New("legacy CodexApp has unsupported arguments; automatic handoff refused")
	}
	password, err := codexAppPassword(p.PasswordFile, "")
	if err != nil {
		return 0, err
	}
	if argv[8] != strings.TrimRight(string(password), "\r\n") {
		return 0, errors.New("legacy CodexApp credential differs from its managed file; resolve explicitly before migration")
	}
	env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", process.PID))
	if err != nil {
		return 0, err
	}
	if err = a.validateLegacyCodexAppEnvironment(p, env); err != nil {
		return 0, err
	}

	return componentPort(argv[6])
}
func (a *App) validateLegacyCodexAppEnvironment(p *LegacyCodexAppPlan, raw []byte) error {
	old := map[string]string{}
	for _, variable := range strings.Split(string(raw), "\x00") {
		k, v, ok := strings.Cut(variable, "=")
		if !ok {
			continue
		}
		old[k] = v
		if strings.HasPrefix(k, "CODEXUI_") && k != "CODEXUI_CODEX_COMMAND" {
			return errors.New("legacy CodexApp has custom CODEXUI settings; automatic handoff refused")
		}
	}
	if old["CODEXUI_CODEX_COMMAND"] != p.Bridge {
		return errors.New("legacy CodexApp bridge changed; automatic handoff refused")
	}
	next := map[string]string{}
	for _, variable := range a.componentEnv(nil) {
		k, v, ok := strings.Cut(variable, "=")
		if ok {
			next[k] = v
		}
	}
	// Storage roots are behavior, not just environment trivia. Never restart an
	// external UI against another HOME/config/session tree and call it migration.
	if old["HOME"] != a.P.Home {
		return errors.New("legacy CodexApp HOME differs from the selected home; automatic handoff refused")
	}
	defaults := map[string]string{"CODEX_HOME": ".codex", "XDG_DATA_HOME": ".local/share", "XDG_CONFIG_HOME": ".config", "XDG_STATE_HOME": ".local/state", "XDG_CACHE_HOME": ".cache"}
	for key, relative := range defaults {
		previous, current := old[key], next[key]
		if previous == "" {
			previous = filepath.Join(a.P.Home, relative)
		}
		if current == "" {
			current = filepath.Join(a.P.Home, relative)
		}
		if !filepath.IsAbs(previous) || !filepath.IsAbs(current) || filepath.Clean(previous) != filepath.Clean(current) {
			return fmt.Errorf("legacy CodexApp %s storage root differs from the new runtime; automatic handoff refused", key)
		}
	}
	return nil
}

func (a *App) verifyLegacyCodexAppFiles(p *LegacyCodexAppPlan) error {
	for path, expected := range map[string]string{p.Entry: p.EntryHash, p.Node: p.NodeHash} {
		hash, err := Hash(path)
		if err != nil || hash != expected {
			return fmt.Errorf("legacy CodexApp dependency changed: %s", path)
		}
	}
	return nil
}
func (a *App) prepareLegacyCodexApp(t *Transaction, p *LegacyCodexAppPlan) error {
	if p == nil {
		return nil
	}
	if err := a.verifyLegacyCodexAppFiles(p); err != nil {
		return err
	}
	hash, err := Hash(p.Hook)
	if err != nil || hash != p.HookHash {
		return errors.New("legacy CodexApp hook changed since preview")
	}
	if p.Process != nil {
		if _, err = a.validateLegacyCodexAppProcess(p, p.Process); err != nil {
			return err
		}
	}
	password, err := codexAppPassword(p.PasswordFile, "")
	if err != nil {
		return err
	}
	backup, err := a.BackupFile(p.Hook, "legacy-codexapp-entrypoint", t.ID)
	if err != nil {
		return err
	}
	if backup.Hash != p.HookHash {
		return errors.New("legacy CodexApp hook changed while backing up")
	}
	p.HookBackupID = backup.ID
	t.LegacyCodexApp = p
	t.Resources = append(t.Resources, TxResource{Path: p.Hook, BackupID: backup.ID, Deleted: true})
	if err = a.saveTx(t); err != nil {
		return err
	}
	// All restore material is durable before the hook is retired or a process is
	// stopped. The old credential remains untouched throughout the migration.
	if err = os.Remove(p.Hook); err != nil {
		return err
	}
	root := a.componentRoot("codexapp")
	if err = checkParents(root); err != nil {
		return err
	}
	if _, e := os.Lstat(root); !os.IsNotExist(e) {
		return errors.New("CodexApp runtime conflict appeared after preview")
	}
	p.RuntimeOwned = true
	if err = a.saveTx(t); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		return err
	}
	if err = os.Mkdir(root, 0700); err != nil {
		return err
	}
	if err = AtomicWrite(filepath.Join(root, "migration-id"), []byte(t.ID), 0600); err != nil {
		return err
	}
	return AtomicWrite(filepath.Join(root, "password"), password, 0600)
}
func (a *App) stopLegacyCodexApp(ctx context.Context, t *Transaction) error {
	p := t.LegacyCodexApp
	if p == nil {
		return nil
	}
	// Rescan even when the preview saw no process. Unexpected restarts are not
	// silently adopted by the migration.
	processes, err := a.legacyCodexAppProcesses(p.Entry)
	if err != nil {
		return err
	}
	if p.Process == nil {
		if len(processes) > 0 {
			return errors.New("legacy CodexApp started after preview; repeat migration preview")
		}
		return nil
	}
	if len(processes) != 1 || *processes[0] != *p.Process {
		return errors.New("legacy CodexApp process changed after preview; repeat migration preview")
	}
	if _, err = a.validateLegacyCodexAppProcess(p, p.Process); err != nil {
		return err
	}
	// pidfd pins the identity across the final check and signal; unsupported
	// kernels fail safely rather than falling back to racy kill(PID).
	fd, err := unix.PidfdOpen(p.Process.PID, 0)
	if err != nil {
		return fmt.Errorf("cannot pin legacy CodexApp process identity: %w", err)
	}
	defer unix.Close(fd)
	if !legacyCodexAppSameProcess(p.Process) {
		return errors.New("legacy CodexApp PID changed before stop")
	}
	p.StopRequested = true
	if err = a.saveTx(t); err != nil {
		return err
	}
	if err = unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0); err != nil && err != unix.ESRCH {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for legacyCodexAppSameProcess(p.Process) {
		if time.Now().After(deadline) {
			return errors.New("legacy CodexApp did not stop; no forced kill performed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	processes, err = a.legacyCodexAppProcesses(p.Entry)
	if err != nil {
		return err
	}
	if len(processes) > 0 {
		return errors.New("legacy CodexApp was restarted by another manager; restore old deployment before retry")
	}
	return componentPortFree(p.Port)
}
func (a *App) configureMigratedCodexApp(ctx context.Context, t *Transaction, p *LegacyCodexAppPlan) error {
	if p == nil {
		return nil
	}
	if err := a.verifyLegacyCodexAppFiles(p); err != nil {
		return err
	}
	if err := a.Health(ctx); err != nil {
		return fmt.Errorf("new core not ready for CodexApp handoff: %w", err)
	}
	processes, err := a.legacyCodexAppProcesses(p.Entry)
	if err != nil {
		return err
	}
	if len(processes) > 0 {
		return errors.New("legacy CodexApp restarted during migration; refusing second server")
	}
	if err = componentPortFree(p.Port); err != nil {
		return err
	}
	if a.S.Components == nil {
		a.S.Components = map[string]*Component{}
	}
	if a.S.Components["codexapp"] != nil {
		return errors.New("CodexApp component appeared during migration")
	}
	c := &Component{Name: "codexapp", Path: p.Entry, Version: p.Version, Ownership: "reused", Status: "configured", Options: map[string]string{"managed_runtime": "true", "node": p.Node, "port": strconv.Itoa(p.Port), "migration_id": t.ID}}
	a.S.Components["codexapp"] = c
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
	if err = a.StartComponent(ctx, "codexapp"); err != nil {
		return err
	}
	if a.RunCommand == nil {
		// Service startup alone is not handoff acceptance: a live 5xx Web UI
		// must roll back rather than commit an apparently successful migration.
		raw, e := os.ReadFile(filepath.Join(a.codexappRuntime(c), "process.json"))
		if e != nil {
			return e
		}
		var running componentProcess
		if json.Unmarshal(raw, &running) != nil || !componentOwnerMatches(running) || !componentProcessMatches(running) {
			return errors.New("migrated CodexApp process is not ready")
		}
		identity, e := legacyCodexAppIdentity(running.PID)
		if e != nil {
			return e
		}
		if e = waitLegacyCodexAppHTTP(ctx, identity, p.Port); e != nil {
			return fmt.Errorf("migrated CodexApp HTTP health failed: %w", e)
		}
	}
	p.Configured = true
	return a.saveTx(t)
}

// Call before stopping/removing the new core during recovery. Existing external
// programs and the original password are never cleanup targets.
func (a *App) cleanupMigratedCodexApp(ctx context.Context, t *Transaction) error {
	p := t.LegacyCodexApp
	if p == nil || !p.RuntimeOwned {
		return nil
	}
	c := a.S.Components["codexapp"]
	if c != nil {
		if c.Options["migration_id"] != t.ID || c.Path != p.Entry || c.Ownership != "reused" {
			return errors.New("CodexApp recovery ownership conflict")
		}
		if err := a.StopComponent(ctx, "codexapp"); err != nil {
			return err
		}
	}
	root := a.componentRoot("codexapp")
	marker, err := os.ReadFile(filepath.Join(root, "migration-id"))
	if os.IsNotExist(err) {
		entries, e := os.ReadDir(root)
		if os.IsNotExist(e) {
			delete(a.S.Components, "codexapp")
			return nil
		}
		if e != nil {
			return e
		}
		if len(entries) != 0 {
			return errors.New("CodexApp runtime lacks migration ownership marker; preserved")
		}
	} else if err != nil {
		return err
	} else if string(marker) != t.ID {
		return errors.New("CodexApp runtime migration ownership changed; preserved")
	}
	if err = os.RemoveAll(root); err != nil {
		return err
	}
	delete(a.S.Components, "codexapp")
	return a.Save()
}

// Call after transaction resources and the old backend have been restored. Do
// not execute a user shell hook: its reviewed Node/argv launch is reconstructed
// directly with detached /dev/null stdio. Only the recovery event, not the
// old application stdout/stderr, is recorded in the central maintenance logs.
func (a *App) restoreLegacyCodexApp(ctx context.Context, t *Transaction) error {
	p := t.LegacyCodexApp
	if p == nil || !p.StopRequested || p.Process == nil {
		return nil
	}
	if err := a.verifyLegacyCodexAppFiles(p); err != nil {
		return err
	}
	hash, err := Hash(p.Hook)
	if err != nil || hash != p.HookHash {
		return errors.New("legacy CodexApp hook must be restored before restart")
	}
	if legacyCodexAppSameProcess(p.Process) {
		return nil
	}
	if legacyCodexAppSameProcess(p.RestoredProcess) {
		return a.finishLegacyCodexAppRestore(ctx, p)
	}
	processes, err := a.legacyCodexAppProcesses(p.Entry)
	if err != nil {
		return err
	}
	if len(processes) > 0 {
		// An external supervisor may already have restored the exact reviewed
		// configuration. Validate it but never signal or replace it.
		if len(processes) != 1 {
			return errors.New("multiple CodexApp processes block recovery")
		}
		port, e := a.validateLegacyCodexAppProcess(p, processes[0])
		if e != nil || port != p.Port {
			return errors.New("external CodexApp restart conflicts with recovery")
		}
		p.RestoredProcess = processes[0]
		if err = a.saveTx(t); err != nil {
			return err
		}
		return a.finishLegacyCodexAppRestore(ctx, p)
	}
	if err = componentPortFree(p.Port); err != nil {
		return err
	}
	if err = legacyCodexAppPIDFileCheck(p, 0); err != nil {
		return err
	}
	password, err := codexAppPassword(p.PasswordFile, "")
	if err != nil {
		return err
	}
	args := []string{p.Entry, "--no-login", "--no-tunnel", "--no-open", "--port", strconv.Itoa(p.Port), "--password", strings.TrimRight(string(password), "\r\n")}
	env := map[string]string{"CODEXUI_CODEX_COMMAND": p.Bridge, "PATH": filepath.Dir(p.Node) + string(os.PathListSeparator) + os.Getenv("PATH")}
	if a.RunCommand != nil {
		_, err = a.componentRun(ctx, env, p.Node, args...)
		return err
	}
	cmd := exec.Command(p.Node, args...)
	cmd.Env = a.componentEnv(env)
	cmd.Dir = filepath.Join(a.P.Home, "apps/codexapp")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Recovery uses detached /dev/null stdio: pipes owned by this short-lived
	// installer would break the recovered server when the installer exits.
	if err = cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	log := a.RuntimeLog("codexapp-recovery")
	fmt.Fprintf(log, "restored legacy CodexApp PID=%d port=%d; credentials retained\n", cmd.Process.Pid, p.Port)
	_ = log.Close()
	p.RestoredProcess, err = legacyCodexAppIdentity(cmd.Process.Pid)
	if err != nil {
		return err
	}
	if err = a.saveTx(t); err != nil {
		return err
	}
	return a.finishLegacyCodexAppRestore(ctx, p)
}
func legacyCodexAppPIDFileCheck(p *LegacyCodexAppPlan, restoredPID int) error {
	// A conflicting file owner is preserved even if our recovered process is
	// already alive; retrying recovery must not turn a partial restore into success.
	b, e := os.ReadFile(p.PIDFile)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	value := strings.TrimSpace(string(b))
	if value != strconv.Itoa(p.Process.PID) && (restoredPID <= 1 || value != strconv.Itoa(restoredPID)) {
		return errors.New("legacy CodexApp PID file changed; preserved for inspection")
	}
	return nil
}
func (a *App) finishLegacyCodexAppRestore(ctx context.Context, p *LegacyCodexAppPlan) error {
	if p.RestoredProcess == nil {
		return errors.New("missing restored legacy CodexApp process")
	}
	port, err := a.validateLegacyCodexAppProcess(p, p.RestoredProcess)
	if err != nil {
		return err
	}
	if port != p.Port {
		return errors.New("restored legacy CodexApp port changed")
	}
	if err = legacyCodexAppPIDFileCheck(p, p.RestoredProcess.PID); err != nil {
		return err
	}
	if err = AtomicWrite(p.PIDFile, []byte(strconv.Itoa(p.RestoredProcess.PID)+"\n"), 0600); err != nil {
		return err
	}
	return waitLegacyCodexAppHTTP(ctx, p.RestoredProcess, p.Port)
}

func waitLegacyCodexAppHTTP(ctx context.Context, process *LegacyCodexAppProcess, port int) error {
	client := http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !legacyCodexAppSameProcess(process) {
			return errors.New("restored legacy CodexApp exited before HTTP readiness")
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode < 500 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("restored legacy CodexApp HTTP readiness timeout")
}
