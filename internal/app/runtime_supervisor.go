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
	if len([]byte(a.P.Socket)) > 107 {
		return errors.New("app-server Unix socket path exceeds Linux 107-byte limit; choose a shorter HOME/runtime path")
	}
	if a.S.Backend == "systemd" {
		if _, socketErr := os.Lstat(a.P.Socket); socketErr == nil {
			if _, activeErr := a.Run(ctx, "systemctl", "--user", "is-active", "--quiet", "codex-bg.service"); activeErr != nil {
				return errors.New("socket exists while managed systemd service is inactive; refusing to start")
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
			if _, socketErr := os.Lstat(a.P.Socket); socketErr == nil {
				return errors.New("socket exists without a verified supervisor; refusing to start")
			}
			if readErr == nil {
				_ = os.Remove(a.runtimePIDFile())
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
	healthCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		if err := a.Health(healthCtx); err == nil {
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
		if _, socketErr := os.Stat(a.P.Socket); socketErr == nil {
			return errors.New("socket exists without a known supervisor; refusing to remove or signal unknown service")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !runtimeValidPID(record) {
		return errors.New("supervisor PID identity changed; refusing to signal it")
	}
	if err = syscall.Kill(record.PID, syscall.SIGTERM); err != nil {
		return err
	}
	deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for runtimeValidPID(record) {
		if err = runtimeWait(deadline, 100*time.Millisecond); err != nil {
			return errors.New("supervisor did not stop; preserving runtime")
		}
	}
	// Serve removes its own socket and PID, after stopping the app-server group.
	if _, err = os.Stat(a.P.Socket); err == nil {
		return errors.New("socket remains after supervisor exit; preserve runtime and inspect")
	}
	return nil
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
	if _, err = os.Stat(a.P.Socket); err == nil {
		// Never unlink an existing responsive or unknown listener during startup.
		return errors.New("app-server socket already exists; inspect stale or competing runtime before starting")
	}
	started, err := runtimeProcessStart(os.Getpid())
	if err != nil {
		return err
	}
	record := runtimePID{PID: os.Getpid(), Start: started, UID: os.Getuid(), Binary: a.P.Binary(), Runtime: a.P.Runtime}
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
	failures := 0
	for {
		if err = ctx.Err(); err != nil {
			return nil
		}
		// Keep the selected CLI identity stable for this supervisor lifetime.
		// Maintenance explicitly restarts the supervisor after publishing changes.
		log := a.RuntimeLog("app-server")
		cmd := exec.Command(codexPath, "app-server", "--listen", "unix://"+a.P.Socket)
		cmd.Env = runtimeEnv(runtimeHome, codexHome)
		cmd.Stdin = nil
		cmd.Stdout = log
		cmd.Stderr = log
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		cmd.WaitDelay = time.Second
		launched := time.Now()
		startErr := cmd.Start()
		exitCode := -1
		if startErr == nil {
			a.runtimeStatus("health", map[string]any{"status": "running", "consecutive_failures": failures, "pid": cmd.Process.Pid})
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			stable := time.NewTimer(30 * time.Second)
			running := true
			for running {
				select {
				case <-ctx.Done():
					stable.Stop()
					runtimeStopChild(cmd, done)
					if st, e := os.Lstat(a.P.Socket); e == nil && st.Mode()&os.ModeSocket != 0 {
						_ = os.Remove(a.P.Socket)
					}
					log.Close()
					a.runtimeStatus("health", map[string]any{"status": "stopped", "consecutive_failures": 0})
					return nil
				case <-stable.C:
					failures = 0
					a.runtimeStatus("health", map[string]any{"status": "running", "consecutive_failures": 0, "pid": cmd.Process.Pid})
				case <-done:
					stable.Stop()
					exitCode = cmd.ProcessState.ExitCode()
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					running = false
				}
			}
		}
		log.Close()
		if time.Since(launched) >= 30*time.Second {
			failures = 0
		}
		failures++
		delay := runtimeBackoff(failures)
		a.runtimeStatus("health", map[string]any{"status": "retrying", "consecutive_failures": failures, "exit_code": exitCode, "retry_seconds": int(delay / time.Second), "start_failed": startErr != nil})

		// Codex removes its Unix socket on normal exit. A socket remaining after a
		// crash is only removed here, under our lifetime supervisor lock.
		if st, e := os.Lstat(a.P.Socket); e == nil && st.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(a.P.Socket)
		}
		if err = runtimeWait(ctx, delay); err != nil {
			return nil
		}
	}
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
