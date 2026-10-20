package app

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestRuntimeSocketProcessHelper(t *testing.T) {
	if os.Getenv("CODEX_BG_SOCKET_HELPER") != "1" {
		return
	}
	if gate := os.Getenv("CODEX_BG_SOCKET_GATE"); gate != "" {
		for {
			if _, err := os.Stat(gate); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: os.Getenv("CODEX_BG_SOCKET_PATH"), Net: "unix"})
	if err != nil {
		os.Exit(25)
	}
	l.SetUnlinkOnClose(false)
	if marker := os.Getenv("CODEX_BG_SOCKET_READY"); marker != "" {
		_ = os.WriteFile(marker, []byte("ready"), 0600)
	}
	for {
		time.Sleep(time.Second)
	}
}

func socketChildFixture(t *testing.T, gate string) (*App, string, runtimeRestartPolicy) {
	t.Helper()
	a, cli, p := restartFixture(t, "exit 0", true)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cli, []byte("#!/bin/sh\nexec "+shellQuote(exe)+" -test.run=^TestRuntimeSocketProcessHelper$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_BG_SOCKET_HELPER", "1")
	t.Setenv("CODEX_BG_SOCKET_PATH", a.P.Socket)
	t.Setenv("CODEX_BG_SOCKET_GATE", gate)
	p.health = func(context.Context) error { return nil }
	return a, cli, p
}

func TestRuntimeSocketKernelOwnershipAndReplacement(t *testing.T) {
	path := filepath.Join(runtimeTestDir(t), "owned.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	defer l.Close()
	start, err := runtimeProcessStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	owned := runtimeChildSocket(path, runtimeChild{PID: os.Getpid(), Start: start})
	if owned == nil {

		t.Fatal("kernel could not prove own socket filesystem identity")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	a := &App{P: Paths{Socket: path}}
	a.runtimeRemoveChildSocket(owned)
	if _, err = os.Lstat(path); err != nil {
		t.Fatal("cleanup removed replacement listener")
	}
	a.runtimeRemoveChildSocket(nil)
	if _, err = os.Lstat(path); err != nil {
		t.Fatal("unknown cleanup removed listener")
	}
}

func TestRuntimeSocketUnknownListenerDuringBackoffStopsRecovery(t *testing.T) {
	a, cli, p := restartFixture(t, "exit 12", true)
	p.backoff = func(int) time.Duration { return 40 * time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.superviseCodex(ctx, cli, "", a.P.Home, p) }()
	waitRestartControl(t, a, func(c runtimeControl) bool { return c.Status == "retrying" })
	l, err := net.Listen("unix", a.P.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("socket conflict retried")
	}
	c := readRestartControl(t, a)
	if c.Status != "failed" || c.LastExit == nil || c.LastExit.Reason != "socket_conflict" {
		t.Fatalf("conflict not persisted: %+v", c)
	}
	conn, err := net.Dial("unix", a.P.Socket)
	if err != nil {
		t.Fatalf("outside listener was removed: %v", err)
	}
	conn.Close()
}

func TestRuntimeSocketForeignBindAfterLaunchIsNeverOwnedOrRemoved(t *testing.T) {
	gate := filepath.Join(runtimeTestDir(t), "allow-bind")
	a, cli, p := socketChildFixture(t, gate)
	p.backoff = func(int) time.Duration { return time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.superviseCodex(ctx, cli, "", a.P.Home, p) }()
	c := waitRestartControl(t, a, func(c runtimeControl) bool { return c.Child != nil })
	l, err := net.Listen("unix", a.P.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if proof := runtimeChildSocket(a.P.Socket, *c.Child); proof != nil {
		t.Fatal("foreign listener was attributed to child")
	}
	if err = os.WriteFile(gate, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("failed bind did not stop recovery")
	}
	current := readRestartControl(t, a)
	if current.Status != "failed" || current.LastExit.Reason != "socket_conflict" {
		t.Fatalf("bind race not surfaced: %+v", current)
	}
	conn, err := net.Dial("unix", a.P.Socket)
	if err != nil {
		t.Fatalf("bind failure unlinked foreign socket: %v", err)
	}
	conn.Close()
}

func TestRuntimeSocketOwnedCrashIsCleanedAndRecoveryContinues(t *testing.T) {
	a, cli, p := socketChildFixture(t, "")
	p.backoff = func(int) time.Duration { return 20 * time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.superviseCodex(ctx, cli, "", a.P.Home, p) }()
	c := waitRestartControl(t, a, func(c runtimeControl) bool { return c.Child != nil })
	deadline := time.Now().Add(2 * time.Second)
	for runtimeChildSocket(a.P.Socket, *c.Child) == nil {
		if time.Now().After(deadline) {
			t.Fatal("owned child did not bind")
		}
		time.Sleep(time.Millisecond)
	}
	// Let the supervisor's poll capture the same independently proven identity.
	time.Sleep(15 * time.Millisecond)
	if err := syscall.Kill(c.Child.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	newControl := waitRestartControl(t, a, func(n runtimeControl) bool { return n.Child != nil && n.Child.PID != c.Child.PID })
	if newControl.Retries != 1 {
		t.Fatalf("owned crash not recovered exactly once: %+v", newControl)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup blocked")
	}
	if _, err := os.Lstat(a.P.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned socket was left behind: %v", err)
	}
}

func TestRuntimeSocketOwnershipDoesNotTrustUnrelatedProcess(t *testing.T) {
	path := filepath.Join(runtimeTestDir(t), "foreign.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cmd := exec.Command("sleep", "10")
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	start, err := runtimeProcessStart(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if proof := runtimeChildSocket(path, runtimeChild{PID: cmd.Process.Pid, Start: start}); proof != nil {
		t.Fatal("path sampling falsely attributed another process's socket")
	}
}

func TestRuntimeSocketSymlinkUsesVerifiedTargetIdentity(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "original"
		if changed {
			name = "replacement"
		}
		t.Run(name, func(t *testing.T) {
			root := runtimeTestDir(t)
			target := filepath.Join(root, "target.sock")
			link := filepath.Join(root, "entry.sock")
			l, err := net.ListenUnix("unix", &net.UnixAddr{Name: target, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			l.SetUnlinkOnClose(false)
			defer l.Close()
			if err = os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			start, err := runtimeProcessStart(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			proof := runtimeChildSocket(link, runtimeChild{PID: os.Getpid(), Start: start})
			if proof == nil || proof.target == nil {
				t.Fatal("published symlink ownership unavailable")
			}
			if changed {
				if err = os.Remove(target); err != nil {
					t.Fatal(err)
				}
				replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: target, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				defer replacement.Close()
			}
			a := &App{P: Paths{Socket: link}}
			a.runtimeRemoveChildSocket(proof)
			if _, err = os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned entry remains: %v", err)
			}
			_, err = os.Lstat(target)
			if changed && err != nil {
				t.Fatalf("target replacement removed: %v", err)
			}
			if !changed && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned target remains: %v", err)
			}
		})
	}
}

func TestRuntimeSocketSymlinkReplacementPreservesBothTargets(t *testing.T) {
	root := runtimeTestDir(t)
	target := filepath.Join(root, "target.sock")
	link := filepath.Join(root, "entry.sock")
	other := filepath.Join(root, "other.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: target, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	foreign, err := net.ListenUnix("unix", &net.UnixAddr{Name: other, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if err = os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	start, err := runtimeProcessStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	proof := runtimeChildSocket(link, runtimeChild{PID: os.Getpid(), Start: start})
	if proof == nil {
		t.Fatal("published symlink ownership unavailable")
	}
	if err = os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	a := &App{P: Paths{Socket: link}}
	a.runtimeRemoveChildSocket(proof)
	for _, path := range []string{link, target, other} {
		if _, err = os.Lstat(path); err != nil {
			t.Fatalf("replacement cleanup changed %s: %v", path, err)
		}
	}
}

func TestRuntimeSocketReplacedListenerCannotResetHealthyBudget(t *testing.T) {
	a, cli, p := socketChildFixture(t, "")
	p.stable = 50 * time.Millisecond
	if _, err := a.updateRuntimeControl("fixture", func(c *runtimeControl) error { c.Retries = 3; return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.superviseCodex(ctx, cli, "", a.P.Home, p) }()
	c := waitRestartControl(t, a, func(c runtimeControl) bool { return c.Status == "running" && c.Child != nil })
	deadline := time.Now().Add(time.Second)
	for runtimeChildSocket(a.P.Socket, *c.Child) == nil {
		if time.Now().After(deadline) {
			t.Fatal("child did not bind")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.Remove(a.P.Socket); err != nil {
		t.Fatal(err)
	}
	foreign, err := net.Listen("unix", a.P.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	time.Sleep(100 * time.Millisecond)
	current := readRestartControl(t, a)
	if current.Retries != 3 || current.Child == nil || current.Child.PID != c.Child.PID {
		t.Fatalf("foreign health reset budget or killed live child: %+v", current)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel blocked")
	}
	conn, err := net.Dial("unix", a.P.Socket)
	if err != nil {
		t.Fatalf("foreign listener removed after cancel: %v", err)
	}
	conn.Close()
}
