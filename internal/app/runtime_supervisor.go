package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

type runtimePID struct {
	PID     int    `json:"pid"`
	Start   string `json:"start"`
	UID     int    `json:"uid"`
	Binary  string `json:"binary"`
	Runtime string `json:"runtime"`
	BootID  string `json:"boot_id,omitempty"`
}

func runtimeProcessStart(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	i := strings.LastIndex(string(data), ") ")
	if i < 0 {
		return "", errors.New("invalid process stat")
	}
	fields := strings.Fields(string(data)[i+2:])
	if len(fields) <= 19 || fields[0] == "Z" {
		return "", errors.New("process absent or zombie")
	}
	return fields[19], nil
}
func runtimeValidPID(record runtimePID) bool {
	if record.PID <= 1 || record.UID != os.Getuid() {
		return false
	}
	if record.BootID != "" {
		boot, err := runtimeBootID()
		if err != nil || boot != record.BootID {
			return false
		}
	}
	started, err := runtimeProcessStart(record.PID)
	if err != nil || started != record.Start {
		return false
	}
	stat, err := os.Stat(fmt.Sprintf("/proc/%d", record.PID))
	if err != nil {
		return false
	}
	sys, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || int(sys.Uid) != record.UID {
		return false
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", record.PID))
	if err != nil {
		return false
	}
	args := strings.Split(string(raw), "\x00")
	if len(args) < 2 || args[0] != record.Binary {
		return false
	}
	runtimeFound := false
	serveFound := false
	for i, arg := range args {
		if arg == "serve" {
			serveFound = true
		}
		if arg == "--runtime" && i+1 < len(args) && args[i+1] == record.Runtime {
			runtimeFound = true
		}
	}
	return runtimeFound && serveFound
}
func (a *App) runtimePIDFile() string { return filepath.Join(a.P.Runtime, "run", "supervisor.json") }
func (a *App) runtimeReadPID() (runtimePID, error) {
	var p runtimePID
	data, err := os.ReadFile(a.runtimePIDFile())
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(data, &p)
	if err == nil && (p.Runtime != a.P.Runtime || p.Binary != a.P.Binary()) {
		return p, errors.New("supervisor identity does not match installed runtime")
	}
	return p, err
}
func runtimeLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	// Go opens descriptors CLOEXEC. Explicitly retain it as a lifecycle invariant.
	unix.CloseOnExec(int(file.Fd()))
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
func runtimeUnlock(file *os.File) { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }
func runtimeWait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (a *App) Start(ctx context.Context) error {
	control, err := a.readRuntimeControl()
	if err != nil {
		return err
	}
	if !runtimeMayLaunch(control) {
		return fmt.Errorf("后台状态为 %s（%s），需显式启动或恢复维护", control.Status, control.Mode)
	}
	if len([]byte(a.P.Socket)) > 107 {
		return errors.New("app-server Unix socket path exceeds Linux 107-byte limit; choose a shorter HOME/runtime path")
	}
	if a.S.Backend == "systemd" {
		if _, socketErr := os.Lstat(a.P.Socket); socketErr == nil {
			if _, activeErr := a.Run(ctx, "systemctl", "--user", "is-active", "--quiet", "codex-bg.service"); activeErr != nil {
				if _, proofErr := a.readRuntimeSocketOwnership(); proofErr != nil {
					return errors.New("socket exists without recovery evidence; refusing to start")
				}
			}
		}
		if _, err := a.Run(ctx, "systemctl", "--user", "start", "codex-bg.service"); err != nil {
			return err
		}
	} else {
		if _, err := os.Stat(a.P.Runtime); err != nil {
			return errors.New("runtime is not installed")
		}
		if err := os.MkdirAll(filepath.Join(a.P.Runtime, "run"), 0700); err != nil {
			return err
		}
		record, readErr := a.runtimeReadPID()
		if readErr != nil || !runtimeValidPID(record) {
			// Serve owns the supervisor lock and validates registered stale socket
			// evidence before cleanup. The launcher cannot safely do either.
			if _, socketErr := os.Lstat(a.P.Socket); socketErr == nil {
				if _, proofErr := a.readRuntimeSocketOwnership(); proofErr != nil {
					return errors.New("socket exists without recovery evidence; refusing to start")
				}
			}
			cmd := exec.Command(a.P.Binary(), "--home", a.P.Home, "--runtime", a.P.Runtime, "serve")
			cmd.Env = runtimeEnv(a.P.Home, a.S.CodexHome)
			cmd.Stdin = nil
			// The launcher exits immediately; never pipe a detached process back
			// into Go copier goroutines owned by this short-lived launcher.
			cmd.Stdout = nil
			cmd.Stderr = nil
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				return err
			}
			go func() { _ = cmd.Wait() }()
		}
	}
	readyWait := 15 * time.Second
	if !control.Launch {
		if control.Status == "running" || control.Status == "starting" {
			readyWait += runtimeBackoff(control.Retries + 1)
		} else if remaining := time.Until(control.NextRetry); remaining > 0 {
			// A persisted retry timer may exceed the ordinary startup timeout.
			// Waiting never resets or advances the supervisor's retry allowance.
			if remaining > 30*time.Second {
				remaining = 30 * time.Second
			}
			readyWait += remaining
		}
	}
	healthCtx, cancel := context.WithTimeout(ctx, readyWait)
	defer cancel()
	for {
		current, err := a.readRuntimeControl()
		if err != nil {
			return err
		}
		if current.Generation != control.Generation {
			return errRuntimeSuperseded
		}
		if current.Desired != "running" || current.Status == "failed" || current.Status == "exhausted" {
			return fmt.Errorf("后台启动停止：%s；请检查状态与日志", current.Status)
		}
		if current.Status == "running" && current.Child != nil && a.Health(healthCtx) == nil {
			// A compatible listener is not enough: a foreign process may have
			// claimed this pathname while the child was launching. Confirm the
			// same generation and the kernel's child/socket ownership evidence.
			ready, err := a.readRuntimeControl()
			if err != nil {
				return err
			}
			if ready.Generation != control.Generation {
				return errRuntimeSuperseded
			}
			if ready.Status != "running" || ready.Desired != "running" || ready.Child == nil || *ready.Child != *current.Child || runtimeChildSocket(a.P.Socket, *ready.Child) == nil {
				if err := runtimeWait(healthCtx, 150*time.Millisecond); err != nil {
					return fmt.Errorf("app-server ownership did not become ready: %w", err)
				}
				continue
			}
			if a.S.Backend == "container" {
				a.runtimeOptionalStart(ctx)
			}
			return nil
		}
		if err := runtimeWait(healthCtx, 150*time.Millisecond); err != nil {
			return fmt.Errorf("app-server did not become ready: %w", err)
		}
	}
}
func (a *App) runtimeOptionalStart(ctx context.Context) {
	if extraErr := a.ContainerExtras(ctx); extraErr != nil {
		a.runtimeStatus("extras", map[string]any{"ok": false, "code": "optional_start_failed"})
		a.event("optional_start_failed", "Optional startup failed; inspect component status")
	} else {
		a.runtimeStatus("extras", map[string]any{"ok": true})
	}
}
func (a *App) Stop(ctx context.Context) error {
	if a.S.Backend == "container" {
		if err := a.StopComponent(ctx, "codexapp"); err != nil {
			return err
		}
	}
	if a.S.Backend == "systemd" {
		_, err := a.Run(ctx, "systemctl", "--user", "stop", "codex-bg.service")
		return err
	}
	record, err := a.runtimeReadPID()
	if errors.Is(err, os.ErrNotExist) {
		return a.runtimeCleanupStoppedSupervisor(nil)
	}
	if err != nil {
		return err
	}
	if !runtimeValidPID(record) {
		return a.runtimeCleanupStoppedSupervisor(&record)
	}
	if err = syscall.Kill(record.PID, syscall.SIGTERM); err != nil {
		if !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for runtimeValidPID(record) {
		if err = runtimeWait(deadline, 100*time.Millisecond); err != nil {
			return errors.New("supervisor did not stop; preserving runtime")
		}
	}
	// Serve removes its own socket and PID, after stopping the app-server group.
	return a.runtimeCleanupStoppedSupervisor(&record)
}

// Kill the private session created for exactly this child. Descendant git/helper
// processes inherit that group; killing only the leader leaks them on shutdown.
func runtimeStopChild(cmd *exec.Cmd, done <-chan error) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}
	// The leader may have exited before helpers. The still-owned group cannot be
	// reused while those descendants exist. SIGKILL completes bounded cleanup.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
func (a *App) Serve(ctx context.Context) error {
	if _, err := os.Stat(a.P.StateFile()); err != nil {
		return errors.New("runtime state is unavailable")
	}
	if err := os.MkdirAll(filepath.Join(a.P.Runtime, "run"), 0700); err != nil {
		return err
	}
	lock, err := runtimeLock(filepath.Join(a.P.Runtime, "run", "supervisor.lock"))
	if errors.Is(err, unix.EWOULDBLOCK) {
		return nil
	}
	if err != nil {
		return err
	}
	defer runtimeUnlock(lock)
	if len([]byte(a.P.Socket)) > 107 {
		return errors.New("Unix socket path exceeds 107 bytes")
	}
	control, err := a.readRuntimeControl()
	if err != nil {
		return err
	}
	if !runtimeMayLaunch(control) {
		a.runtimeControlStatus(control, 0, "")
		return nil
	}
	if err = a.recoverRuntimeSocketLocked(control.Generation); err != nil {
		a.runtimeStatus("socket-recovery", map[string]any{"ok": false, "code": "ownership_unavailable"})
		return err
	}

	started, err := runtimeProcessStart(os.Getpid())
	if err != nil {
		return err
	}
	bootID, err := runtimeBootID()
	if err != nil {
		return err
	}
	record := runtimePID{PID: os.Getpid(), Start: started, UID: os.Getuid(), Binary: a.P.Binary(), Runtime: a.P.Runtime, BootID: bootID}
	data, _ := json.Marshal(record)
	if err = os.WriteFile(a.runtimePIDFile(), data, 0600); err != nil {
		return err
	}
	defer os.Remove(a.runtimePIDFile())
	codexPath, codexHome, runtimeHome := a.S.Codex.Path, a.S.CodexHome, a.P.Home
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var watcherWG sync.WaitGroup
	watcherWG.Add(1)
	watchApp := *a
	go func() { defer watcherWG.Done(); _ = watchApp.Reload(); watchApp.runtimeWatch(runCtx) }()
	defer func() { cancel(); watcherWG.Wait() }()
	return a.superviseCodex(ctx, codexPath, codexHome, runtimeHome, defaultRuntimeRestartPolicy())
}

// Only bounded metadata is persisted here; never store an error's text because
// external paths/process output can contain user data. Missing runtime is not recreated.
func (a *App) runtimeStatus(name string, fields map[string]any) {
	dir := filepath.Join(a.P.Runtime, "run")
	if _, err := os.Stat(dir); err != nil {
		return
	}
	fields["time"] = time.Now().UTC()
	data, err := json.Marshal(fields)
	if err != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".status-")
	if err != nil {
		return
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return
	}
	if f.Close() != nil {
		return
	}
	_ = os.Rename(tmp, filepath.Join(dir, name+".json"))
}
func runtimeBackoff(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	if failures >= 5 {
		return 30 * time.Second
	}
	return time.Duration(1<<failures) * time.Second
}
func (a *App) runtimeWatch(ctx context.Context) {
	a.runtimeWatchWith(ctx, 60*time.Second, fsnotify.NewWatcher)
}
func (a *App) runtimeWatchWith(ctx context.Context, interval time.Duration, newWatcher func() (*fsnotify.Watcher, error)) {
	watcher, watchErr := newWatcher()
	var events <-chan fsnotify.Event
	var watchErrors <-chan error
	if watchErr == nil {
		defer watcher.Close()
		events = watcher.Events
		watchErrors = watcher.Errors
	}
	watched := map[string]bool{}
	addRoots := func() {
		if watcher == nil {
			return
		}
		for _, root := range a.S.SelectedRoots {
			for _, dir := range []string{root, filepath.Dir(root)} {
				if !watched[dir] {
					if watcher.Add(dir) == nil {
						watched[dir] = true
					}
				}
			}
		}
	}
	addRoots()
	writeStatus := func(ok bool, code string) {
		a.runtimeStatus("reconcile", map[string]any{"ok": ok, "code": code, "watcher_available": events != nil, "poll_seconds": int(interval / time.Second)})
	}
	if watchErr != nil {
		writeStatus(false, "watcher_unavailable_polling")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var pending <-chan time.Time
	var debounce *time.Timer
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()
	reconcile := func() {
		err := a.Reconcile(ctx)
		addRoots()
		if err != nil {
			writeStatus(false, "reconcile_failed")
		} else if events == nil {
			writeStatus(true, "polling_only")
		} else {
			writeStatus(true, "ok")
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		case event, ok := <-events:
			if !ok {
				events = nil
				writeStatus(false, "watcher_closed_polling")
				continue
			}
			if event.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Remove|fsnotify.Write) != 0 {
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.NewTimer(750 * time.Millisecond)
				pending = debounce.C
			}
		case <-pending:
			pending = nil
			reconcile()
		case _, ok := <-watchErrors:
			if !ok {
				watchErrors = nil
				continue
			}
			writeStatus(false, "watcher_error")
		}
	}
}
