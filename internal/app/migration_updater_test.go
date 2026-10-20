package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This copied test binary is a local, model-free native updater. It deliberately
// leaves its socket and PID file behind to exercise proven stale-file cleanup.
func legacyUpdaterFixtureMain() int {
	if len(os.Args) != 4 || os.Args[1] != "app-server" || os.Args[2] != "daemon" || os.Args[3] != "pid-update-loop" {
		return 81
	}
	root := filepath.Join(os.Getenv("CODEX_HOME"), "app-server-daemon")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: "app-server-updater.sock", Net: "unix"})
	if err != nil {
		return 82
	}
	listener.SetUnlinkOnClose(false)
	defer listener.Close()
	start, err := runtimeProcessStart(os.Getpid())
	if err != nil {
		return 83
	}
	boot, err := runtimeBootID()
	if err != nil {
		return 84
	}
	var record nativeUpdaterPID
	record.PID = os.Getpid()
	record.Identity.BootID = boot
	record.Identity.StartTicks, _ = strconv.ParseUint(start, 10, 64)
	raw, _ := json.Marshal(record)
	if err = AtomicWrite(filepath.Join(root, "app-server-updater.pid"), raw, 0600); err != nil {
		return 85
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	<-ctx.Done()
	return 0
}

func updaterFixture(t *testing.T) (*App, *exec.Cmd, *LegacyUpdaterPlan) {
	t.Helper()
	a := engineApp(t)
	// Short native sockets remain under the test's controlled maintenance tmp.
	cli := filepath.Join(a.P.Home, ".codex/packages/standalone/current/codex")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err = copyFile(self, cli, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(a.S.CodexHome, "app-server-daemon")
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(root, "app-server-updater.stderr.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = log.WriteString("updater log\n")
	engineFile(t, filepath.Join(root, "app-server-updater.stderr.log.previous"), "previous log\n")
	cmd := exec.Command(cli, "app-server", "daemon", "pid-update-loop")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CODEX_BG_UPDATER_INTEGRATION_HELPER=1", "HOME="+a.P.Home, "CODEX_HOME="+a.S.CodexHome)
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = log.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err = os.Stat(filepath.Join(root, "app-server-updater.pid")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture did not start:", err, engineRead(t, filepath.Join(root, "app-server-updater.stderr.log")))
		}
		time.Sleep(10 * time.Millisecond)
	}
	p, err := a.planLegacyUpdater(cli, "")
	if err != nil || p == nil {
		t.Fatalf("plan updater: %v, %+v", err, p)
	}
	return a, cmd, p
}

func updaterMigrationHistory(t *testing.T, a *App, p *LegacyUpdaterPlan) {
	t.Helper()
	tx, err := a.begin("migrate")
	if err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(a.P.Home, "entrypoint.d/05-start-codex-daemon.sh")
	engineFile(t, hook, "#!/bin/sh\nCODEX=$HOME/.codex/packages/standalone/current/codex\n$CODEX app-server daemon start\n")
	b, err := a.BackupFile(hook, "legacy-resource", tx.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx.LegacyDaemon = p.Daemon
	tx.Resources = []TxResource{{Path: hook, BackupID: b.ID, Deleted: true}, {Path: a.P.Binary(), Created: true}}
	if err = os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	a.S.Installed = true
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	if err = a.commit(tx); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyUpdaterCleanupPinsProcessAndPreservesCurrentRuntime(t *testing.T) {
	a, cmd, p := updaterFixture(t)
	updaterMigrationHistory(t, a, p)
	control, err := a.setRuntimeIntent("running", "normal", "", false)
	if err != nil {
		t.Fatal(err)
	}
	before := engineRead(t, a.runtimeControlFile())
	preview, err := a.PreviewLegacyUpdaterCleanup(context.Background())
	if err != nil || !strings.Contains(preview, "SIGTERM") {
		t.Fatalf("preview: %s %v", preview, err)
	}
	if hasPending(a.P.Data) || !legacyCodexAppSameProcess(p.Process) {
		t.Fatal("preview mutated runtime")
	}
	if err = a.CleanupLegacyUpdater(context.Background(), false); err == nil {
		t.Fatal("cleanup without confirmation")
	}
	if !legacyCodexAppSameProcess(p.Process) {
		t.Fatal("unconfirmed cleanup stopped updater")
	}
	if err = a.CleanupLegacyUpdater(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if legacyCodexAppSameProcess(p.Process) {
		t.Fatal("updater remained alive")
	}
	if hasPending(a.P.Data) {
		t.Fatal("cleanup transaction not committed")
	}
	if before != engineRead(t, a.runtimeControlFile()) {
		t.Fatalf("core runtime intent changed: %+v", control)
	}
	for _, path := range []string{p.PIDFile, p.Socket, filepath.Join(filepath.Dir(p.PIDFile), "app-server-updater.stderr.log"), filepath.Join(filepath.Dir(p.PIDFile), "app-server-updater.stderr.log.previous")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old artifact remains: %s %v", path, err)
		}
	}
	backups, err := a.Backups()
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) < 4 {
		t.Fatalf("insufficient centralized backups: %d", len(backups))
	}
	for _, b := range backups {
		if err = a.VerifyBackup(b.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = os.Stat(p.Daemon); err != nil {
		t.Fatal("reused CLI removed:", err)
	}
	if err = a.CleanupLegacyUpdater(context.Background(), true); err != nil {
		t.Fatal("cleanup should be idempotent:", err)
	}
}

func TestLegacyUpdaterRejectsChangedIdentityAndUnrelatedHistory(t *testing.T) {
	a, _, p := updaterFixture(t)
	if _, err := a.PreviewLegacyUpdaterCleanup(context.Background()); err == nil {
		t.Fatal("uninstalled/unknown provenance accepted")
	}
	updaterMigrationHistory(t, a, p)
	for _, tc := range []struct {
		name  string
		alter func(*LegacyUpdaterPlan)
	}{
		{"boot", func(p *LegacyUpdaterPlan) { p.BootID = "00000000-0000-0000-0000-000000000000" }},
		{"start", func(p *LegacyUpdaterPlan) { p.Process.Start = "1" }},
		{"uid", func(p *LegacyUpdaterPlan) { p.Process.UID++ }},
		{"exe", func(p *LegacyUpdaterPlan) { p.Daemon = "/bin/true" }},
		{"codex_home", func(p *LegacyUpdaterPlan) { p.CodexHome = filepath.Dir(p.CodexHome) }},
		{"record_hash", func(p *LegacyUpdaterPlan) { p.PIDHash = "changed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := *p
			identity := *p.Process
			copy.Process = &identity
			tc.alter(&copy)
			if err := a.validateLegacyUpdater(&copy); err == nil {
				t.Fatal("identity mutation accepted")
			}
			if !legacyCodexAppSameProcess(p.Process) {
				t.Fatal("read-only identity check killed process")
			}
		})
	}
	a.S.DataID = "different-installation"
	if _, err := a.PreviewLegacyUpdaterCleanup(context.Background()); err == nil {
		t.Fatal("unrelated migration accepted")
	}
}

func TestLegacyUpdaterCleanupResumesWithoutResignalingOrCoreRestart(t *testing.T) {
	a, cmd, p := updaterFixture(t)
	updaterMigrationHistory(t, a, p)
	tx, err := a.begin("legacy-updater-cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.prepareLegacyUpdater(tx, p); err != nil {
		t.Fatal(err)
	}
	// Durable intent followed by an external termination models a process crash
	// after SIGTERM but before archive or transaction commit.
	p.StopRequested = true
	if err = a.saveTx(tx); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err = a.recoverLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hasPending(a.P.Data) {
		t.Fatal("resumed cleanup did not commit")
	}
	if _, err = os.Lstat(p.PIDFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("PID record not archived")
	}
}

func TestLegacyUpdaterCleanupPreservesReplacedSocket(t *testing.T) {
	a, cmd, p := updaterFixture(t)
	updaterMigrationHistory(t, a, p)
	tx, err := a.begin("legacy-updater-cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.prepareLegacyUpdater(tx, p); err != nil {
		t.Fatal(err)
	}
	p.StopRequested = true
	if err = a.saveTx(tx); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err = os.Remove(p.Socket); err != nil {
		t.Fatal(err)
	}
	// /proc/self/fd provides a short name for bind without moving test data out
	// of installer-data/tmp; the cleanup still checks the original full path.
	dir, err := os.Open(filepath.Dir(p.Socket))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	shortSocket := "/proc/self/fd/" + strconv.FormatUint(uint64(dir.Fd()), 10) + "/app-server-updater.sock"
	listener, err := net.Listen("unix", shortSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err = a.recoverLegacyUpdaterCleanup(context.Background(), tx); err == nil {
		t.Fatal("foreign replacement socket removed")
	}
	conn, err := net.Dial("unix", shortSocket)
	if err != nil {
		t.Fatal("foreign listener damaged:", err)
	}
	conn.Close()
	if !hasPending(a.P.Data) {
		t.Fatal("failed cleanup falsely committed")
	}
}

func TestLegacyUpdaterMigrationRestoresRecordedFilesOnFailure(t *testing.T) {
	a, cmd, p := updaterFixture(t)
	unlock, err := a.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	tx, err := a.begin("migrate")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.prepareLegacyUpdater(tx, p); err != nil {
		t.Fatal(err)
	}
	tx.LegacyDaemon = p.Daemon
	if err = a.saveTx(tx); err != nil {
		t.Fatal(err)
	}
	if err = a.stopLegacyUpdater(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	// Files must already be journaled before deletion, allowing ordinary
	// migration recovery to restore them before native daemon start.
	if len(tx.Resources) != 3 {
		t.Fatalf("missing recovery resources: %+v", tx.Resources)
	}
	for _, r := range tx.Resources {
		if !r.Deleted || r.BackupID == "" {
			t.Fatalf("unrecoverable resource: %+v", r)
		}
	}
	var restored *exec.Cmd
	a.RunCommand = func(ctx context.Context, name string, args, env []string) ([]byte, error) {
		if name != p.Daemon || strings.Join(args, " ") != "app-server daemon start" {
			return nil, errors.New("unexpected recovery command")
		}
		// Native daemon startup recreates its updater after the ordinary file
		// recovery phase. The replacement PID/start record must be accepted.
		restored = exec.Command(p.Daemon, "app-server", "daemon", "pid-update-loop")
		restored.Dir = filepath.Dir(p.PIDFile)
		restored.Env = append(os.Environ(), "CODEX_BG_UPDATER_INTEGRATION_HELPER=1", "HOME="+a.P.Home, "CODEX_HOME="+a.S.CodexHome)
		return nil, restored.Start()
	}
	t.Cleanup(func() {
		if restored != nil && restored.Process != nil {
			_ = restored.Process.Kill()
			_ = restored.Wait()
		}
	})
	if err = a.recoverLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hasPending(a.P.Data) {
		t.Fatal("migration recovery did not commit")
	}
	if restored == nil || restored.Process == nil || restored.Process.Pid == p.Process.PID {
		t.Fatal("old updater capability was not restored")
	}
	if check, err := a.planLegacyUpdater(p.Daemon, ""); err != nil || check == nil || check.Process.PID != restored.Process.Pid {
		t.Fatalf("restored updater verification failed: %+v %v", check, err)
	}
	if engineRead(t, filepath.Join(filepath.Dir(p.PIDFile), "app-server-updater.stderr.log")) != "updater log\n" {
		t.Fatal("old log was not recoverable")
	}
}

func TestLegacyUpdaterMigrationRejectsUnownedNativeDaemon(t *testing.T) {
	a, opt, hook := migrationPreflightApp(t)
	migrationOldDaemonFixture(t, a, "idle")
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	engineFile(t, filepath.Join(a.P.Home, "apps/codex-daemon-bridge", "marker"), "legacy bridge")
	opt.DryRun = true
	if err := a.Migrate(context.Background(), opt); err == nil || !strings.Contains(err.Error(), "没有本次迁移拥有") {
		t.Fatalf("unrelated native daemon was adopted: %v", err)
	}
	if hasPending(a.P.Data) {
		t.Fatal("rejected preflight wrote transaction")
	}
}
