package app

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Opt-in, real CLI control-plane validation. The only requests are initialize,
// loaded/list and thread/read. Git is stubbed to prevent background registry I/O.
func TestRuntimeRealCodex(t *testing.T) {
	codex := os.Getenv("CB_TEST_CODEX")
	if codex == "" {
		t.Skip("set CB_TEST_CODEX to an existing real standalone executable")
	}
	root := runtimeTestDir(t)
	home := filepath.Join(root, "home")
	codexHome := filepath.Join(home, ".codex")
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{codexHome, bin} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("[analytics]\nenabled = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "s.sock")
	cmd := exec.Command(codex, "app-server", "--listen", "unix://"+socket)
	cmd.Env = []string{"HOME=" + home, "CODEX_HOME=" + codexHome, "PATH=" + bin + ":/usr/bin:/bin", "LANG=C.UTF-8"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { runtimeStopChild(cmd, done) })
	a := &App{P: Paths{Home: home, Socket: socket}, S: State{CodexHome: codexHome}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		if err := a.Health(ctx); err == nil {
			break
		}
		if runtimeWait(ctx, 50*time.Millisecond) != nil {
			t.Fatal("real CLI did not become healthy")
		}
	}
	if busy, err := a.Busy(ctx); err != nil || busy {
		t.Fatalf("empty isolated CLI: busy=%v err=%v", busy, err)
	}
	start, startErr := runtimeProcessStart(cmd.Process.Pid)
	if startErr != nil {
		t.Fatal(startErr)
	}
	owned := runtimeChildSocket(socket, runtimeChild{PID: cmd.Process.Pid, Start: start})
	if owned == nil {

		t.Fatal("real CLI socket ownership unavailable")
	}
	// A client disconnect must not own or stop the app-server process.
	c, err := runtimeConnect(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runtimeInitialize(ctx, c); err != nil {
		t.Fatal(err)
	}
	c.CloseNow()
	if err = a.Health(ctx); err != nil {
		t.Fatalf("reconnect failed after client disconnect: %v", err)
	}
	// The real CLI publishes a symlink on recent releases. A crash leaves its
	// entry behind; ownership proof must permit bounded cleanup of both entries.
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("isolated real CLI did not stop")
	}
	a.runtimeRemoveChildSocket(owned)
	if _, err = os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatalf("real CLI crash entry remains: %v", err)
	}
	if owned.targetPath != "" {
		if _, err = os.Lstat(owned.targetPath); !os.IsNotExist(err) {
			t.Fatalf("real CLI crash target remains: %v", err)
		}
	}
	// Cleanup's existing process-group helper expects to observe Wait completion.
	done <- nil
}
