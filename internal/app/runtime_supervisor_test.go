package app

import (
	"context"
	"errors"
	"github.com/fsnotify/fsnotify"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRuntimeLockIsCloseOnExec(t *testing.T) {
	file, err := runtimeLock(filepath.Join(runtimeTestDir(t), "lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeUnlock(file)
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("lifetime lock leaks through exec")
	}
	if second, err := runtimeLock(file.Name()); err == nil {
		runtimeUnlock(second)
		t.Fatal("exclusive lock was not held")
	}
}
func TestRuntimeRejectsPIDReuse(t *testing.T) {
	started, err := runtimeProcessStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if runtimeValidPID(runtimePID{PID: os.Getpid(), Start: started + "0", UID: os.Getuid(), Binary: os.Args[0], Runtime: "/unused"}) {
		t.Fatal("reused PID accepted")
	}
	if runtimeValidPID(runtimePID{PID: os.Getpid(), Start: started, UID: os.Getuid() + 1, Binary: os.Args[0], Runtime: "/unused"}) {
		t.Fatal("wrong UID accepted")
	}
}
func TestRuntimeStartRefusesUnknownSocket(t *testing.T) {
	dir := runtimeTestDir(t)
	socket := filepath.Join(dir, "app.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	a := &App{P: Paths{Runtime: dir, Socket: socket}, S: State{Backend: "container"}}
	if err = a.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("unknown socket not refused: %v", err)
	}
}
func TestRuntimeStopChildCleansOrphans(t *testing.T) {
	dir := runtimeTestDir(t)
	pidfile := filepath.Join(dir, "grandchild.pid")
	cmd := exec.Command("/bin/bash", "-c", `sleep 120 & printf '%s' "$!" > "$1"; wait`, "bash", pidfile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	var child int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidfile)
		if err == nil {
			child, _ = strconv.Atoi(string(data))
			if child > 1 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child <= 1 {
		t.Fatal("helper did not start")
	}
	runtimeStopChild(cmd, done)
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := runtimeProcessStart(child); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("orphan descendant survived supervisor cleanup")
}

func TestRuntimeRestartBackoff(t *testing.T) {
	for i, want := range []time.Duration{2, 4, 8, 16, 30, 30, 30} {
		if got := runtimeBackoff(i + 1); got != want*time.Second {
			t.Fatalf("failure %d: %s", i+1, got)
		}
	}
}
func TestRuntimeWatcherUnavailableStillPolls(t *testing.T) {
	root := runtimeTestDir(t)
	a := &App{P: Paths{Runtime: root}}
	if err := os.MkdirAll(filepath.Join(root, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	// Missing state makes Reconcile fail. Polling must still attempt it, replacing
	// the initial watcher-unavailable record with an explicit reconcile failure.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.runtimeWatchWith(ctx, 20*time.Millisecond, func() (*fsnotify.Watcher, error) { return nil, errors.New("fixture watch unavailable") })
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(filepath.Join(root, "run", "reconcile.json"))
		if err == nil && strings.Contains(string(raw), "reconcile_failed") {
			if strings.Contains(string(raw), "fixture") || strings.Contains(string(raw), root) {
				t.Fatal("status leaked error body")
			}
			cancel()
			<-done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("polling stopped when fsnotify failed")
}
func TestRuntimeStatusDoesNotRecreateRemovedRuntime(t *testing.T) {
	root := runtimeTestDir(t)
	runtimeDir := filepath.Join(root, "gone")
	a := &App{P: Paths{Runtime: runtimeDir}}
	a.runtimeStatus("health", map[string]any{"status": "retrying"})
	if _, err := os.Stat(runtimeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("status recreated removed runtime")
	}
}

func TestRuntimeOptionalFailureIsRecordedWithoutFailingCore(t *testing.T) {
	root := runtimeTestDir(t)
	if err := os.MkdirAll(filepath.Join(root, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	a := &App{P: Paths{Runtime: root}, S: State{Backend: "container", Components: map[string]*Component{
		"codexapp": {Name: "codexapp", Status: "installed", Ownership: "reused"},
	}}}
	a.runtimeOptionalStart(context.Background())
	data, err := os.ReadFile(filepath.Join(root, "run", "extras.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "optional_start_failed") || !strings.Contains(string(data), `"ok":false`) {
		t.Fatal(string(data))
	}
}
