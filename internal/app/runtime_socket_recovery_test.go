package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func recoverySocketFixture(t *testing.T, symlink bool) (*App, *net.UnixListener, runtimeSocketOwnership) {
	t.Helper()
	a, _, _ := restartFixture(t, "exit 0", true)
	target := a.P.Socket
	if symlink {
		target = filepath.Join(a.P.Home, "target.sock")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: target, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	t.Cleanup(func() { listener.Close() })
	if symlink {
		if err = os.Symlink(target, a.P.Socket); err != nil {
			t.Fatal(err)
		}
	}
	start, err := runtimeProcessStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	identity := runtimeChildSocket(a.P.Socket, runtimeChild{PID: os.Getpid(), Start: start})
	if identity == nil {
		t.Fatal("kernel proof unavailable")
	}
	boot, err := runtimeBootID()
	if err != nil {
		t.Fatal(err)
	}
	proof := runtimeSocketOwnership{Schema: 1, Generation: "fixture", Path: a.P.Socket, Entry: runtimeSocketFileFrom(identity), Supervisor: runtimePID{PID: 2147483646, Start: "1", UID: os.Getuid(), Binary: a.P.Binary(), Runtime: a.P.Runtime, BootID: boot}, Child: runtimeChild{PID: 2147483645, Start: "2", BootID: boot}, Recorded: time.Now()}
	if identity.target != nil {
		targetIdentity := runtimeSocketFileFrom(identity.target)
		proof.Target, proof.TargetPath, proof.TargetLink = &targetIdentity, identity.targetPath, identity.targetLink
	}
	writeRecoveryProof(t, a, proof)
	return a, listener, proof
}

func writeRecoveryProof(t *testing.T, a *App, proof runtimeSocketOwnership) {
	t.Helper()
	raw, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	if err = AtomicWrite(a.runtimeSocketOwnershipFile(), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func recoverFixtureSocket(t *testing.T, a *App) error {
	t.Helper()
	lock, err := runtimeLock(filepath.Join(a.P.Runtime, "run", "supervisor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeUnlock(lock)
	c, err := a.readRuntimeControl()
	if err != nil {
		t.Fatal(err)
	}
	return a.recoverRuntimeSocketLocked(c.Generation)
}

func TestRuntimeSocketRecoveryStaleDirectAndSymlink(t *testing.T) {
	for _, mode := range []string{"direct", "symlink", "lost_target"} {
		t.Run(mode, func(t *testing.T) {
			a, listener, proof := recoverySocketFixture(t, mode != "direct")
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			if mode == "lost_target" {
				if err := os.Remove(proof.TargetPath); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := a.updateRuntimeControl("fixture", func(c *runtimeControl) error {
				c.Retries = 4
				c.Status = "retrying"
				c.NextRetry = time.Now().Add(time.Hour)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := readRestartControl(t, a)
			if err := recoverFixtureSocket(t, a); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(a.P.Socket); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale entry remains: %v", err)
			}
			if proof.Target != nil {
				if _, err := os.Lstat(proof.TargetPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stale target remains: %v", err)
				}
			}
			after := readRestartControl(t, a)
			if after.Retries != 4 || after.Status != "retrying" || after.Desired != before.Desired || after.Generation != before.Generation || !after.NextRetry.Equal(before.NextRetry) {
				t.Fatalf("socket recovery altered restart policy: %+v", after)
			}
		})
	}
}

func TestRuntimeSocketRecoveryPreservesUnknownOrLiveEntries(t *testing.T) {
	for _, mode := range []string{"live_listener", "live_supervisor", "live_child", "replaced_entry", "replaced_target", "changed_link", "missing_proof", "invalid_proof", "different_child"} {
		t.Run(mode, func(t *testing.T) {
			a, listener, proof := recoverySocketFixture(t, true)
			if mode != "live_listener" {
				listener.Close()
			}
			switch mode {
			case "live_supervisor", "live_child":
				start, _ := runtimeProcessStart(os.Getpid())
				if mode == "live_supervisor" {
					proof.Supervisor.PID, proof.Supervisor.Start = os.Getpid(), start
				} else {
					proof.Child.PID, proof.Child.Start = os.Getpid(), start
				}
				writeRecoveryProof(t, a, proof)
			case "replaced_entry", "changed_link":
				os.Remove(a.P.Socket)
				if mode == "replaced_entry" {
					os.WriteFile(a.P.Socket, []byte("external"), 0600)
				} else {
					os.Symlink(filepath.Join(a.P.Home, "other.sock"), a.P.Socket)
				}
			case "replaced_target":
				os.Remove(proof.TargetPath)
				os.WriteFile(proof.TargetPath, []byte("external"), 0600)
			case "missing_proof":
				os.Remove(a.runtimeSocketOwnershipFile())
			case "invalid_proof":
				os.WriteFile(a.runtimeSocketOwnershipFile(), []byte("broken"), 0600)
			case "different_child":
				_, err := a.updateRuntimeControl("fixture", func(c *runtimeControl) error {
					c.Child = &runtimeChild{PID: os.Getpid(), Start: "other", BootID: proof.Child.BootID}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := recoverFixtureSocket(t, a); err == nil {
				t.Fatal("unsafe socket recovery accepted")
			}
			if _, err := os.Lstat(a.P.Socket); err != nil {
				t.Fatalf("unknown/live entry removed: %v", err)
			}
			if _, err := os.Lstat(proof.TargetPath); err != nil {
				t.Fatalf("unknown/live target removed: %v", err)
			}
		})
	}
}

func TestRuntimeProcessOwnershipBootAndPIDReuse(t *testing.T) {
	boot, err := runtimeBootID()
	if err != nil {
		t.Fatal(err)
	}
	start, err := runtimeProcessStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if gone, err := runtimeRecordedProcessGone(os.Getpid(), start, boot); err != nil || gone {
		t.Fatalf("live process absent: %v %v", gone, err)
	}
	if gone, err := runtimeRecordedProcessGone(os.Getpid(), start, "previous-boot"); err != nil || !gone {
		t.Fatalf("old boot treated as live: %v %v", gone, err)
	}
	if gone, err := runtimeRecordedProcessGone(os.Getpid(), start+"0", boot); err != nil || !gone {
		t.Fatalf("reused PID treated as original: %v %v", gone, err)
	}
	if gone, err := runtimeRecordedProcessGone(os.Getpid(), start, ""); err == nil || gone {
		t.Fatal("missing boot identity accepted")
	}
	if runtimeValidPID(runtimePID{PID: os.Getpid(), Start: start, UID: os.Getuid(), Binary: os.Args[0], Runtime: "/unused", BootID: "previous-boot"}) {
		t.Fatal("PID from earlier boot accepted")
	}
}

func TestRuntimeSocketRecoveryOldBootAndGeneration(t *testing.T) {
	a, listener, proof := recoverySocketFixture(t, true)
	listener.Close()
	start, err := runtimeProcessStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	// A host reboot can reuse both PID and start ticks. Boot identity prevents
	// confusing the current unrelated process with the recorded former owner.
	proof.Supervisor.PID, proof.Supervisor.Start, proof.Supervisor.BootID = os.Getpid(), start, "previous-boot"
	proof.Child = runtimeChild{PID: os.Getpid(), Start: start, BootID: "previous-boot"}
	writeRecoveryProof(t, a, proof)
	before, _ := os.ReadFile(a.runtimeSocketOwnershipFile())
	if err := a.recordRuntimeSocketOwnership("old-generation", proof.Child, nil); !errors.Is(err, errRuntimeSuperseded) {
		t.Fatalf("old generation not rejected: %v", err)
	}
	after, _ := os.ReadFile(a.runtimeSocketOwnershipFile())
	if string(before) != string(after) {
		t.Fatal("old generation rewrote ownership proof")
	}
	if err := recoverFixtureSocket(t, a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(a.P.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous boot socket remains: %v", err)
	}
}

func TestRuntimeSocketRecoveryServePreservesStoppedAndExhausted(t *testing.T) {
	for _, mode := range []string{"stopped", "exhausted", "maintenance"} {
		t.Run(mode, func(t *testing.T) {
			a, listener, _ := recoverySocketFixture(t, true)
			listener.Close()
			if err := a.Save(); err != nil {
				t.Fatal(err)
			}
			_, err := a.updateRuntimeControl("fixture", func(c *runtimeControl) error {
				c.Launch, c.Retries = false, 5
				switch mode {
				case "stopped":
					c.Desired, c.Status = "stopped", "stopped"
				case "exhausted":
					c.Status = "exhausted"
				case "maintenance":
					c.Mode, c.Status = "updating", "maintenance"
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(a.runtimeControlFile())
			if err := a.Serve(context.Background()); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(a.runtimeControlFile())
			if string(before) != string(after) {
				t.Fatal("Serve changed stopped/exhausted control")
			}
			if _, err := os.Lstat(a.P.Socket); err != nil {
				t.Fatal("inactive service unexpectedly cleaned socket")
			}
		})
	}
}

// Exercise the actual detached manager, kernel ownership registration, abrupt
// supervisor death, inherited child death and automatic budget-preserving boot.
func TestRuntimeSocketRecoveryAfterSupervisorSIGKILL(t *testing.T) {
	a, _, _ := restartFixture(t, "exit 0", true)
	t.Setenv("CODEX_BG_MIGRATION_INTEGRATION_HELPER", "1")
	a.P.Socket = filepath.Join(a.P.Runtime, "run", "app-server.sock")
	a.S.Backend, a.S.Installed = "container", true
	a.S.Schema, a.S.Installer, a.S.CodexHome = 1, a.P.Home, filepath.Join(a.P.Home, ".codex")
	a.P.Data = filepath.Join(a.S.Installer, "installer-data")
	a.Out, a.Err = io.Discard, io.Discard
	a.S.Codex.Path = filepath.Join(a.P.Home, "codex-fixture")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{a.P.Binary(), a.S.Codex.Path} {
		if err := AtomicWrite(path, bin, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = a.stopRuntimeFor(stopCtx, "normal", "")
	})
	if err = a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	control := readRestartControl(t, a)
	if control.Child == nil || control.Child.BootID == "" {
		t.Fatal("new child lacks boot ownership")
	}
	if err = a.RecordRuntimeSocketOwnership(); err != nil {
		t.Fatal(err)
	}
	proof, err := a.readRuntimeSocketOwnership()
	if err != nil {
		t.Fatal(err)
	}
	if proof.Child.PID != control.Child.PID || proof.Supervisor.PID == os.Getpid() {
		t.Fatal("wrong socket ownership process recorded")
	}
	// Upgrade an old Go record in place: missing boot IDs are filled from live
	// kernel proof, while no task is stopped and control generation is retained.
	_, err = a.updateRuntimeControl(control.Generation, func(c *runtimeControl) error { c.Child.BootID = ""; c.Retries = 3; return nil })
	if err != nil {
		t.Fatal(err)
	}
	pid := proof.Supervisor
	pid.BootID = ""
	rawPID, _ := json.Marshal(pid)
	if err = AtomicWrite(a.runtimePIDFile(), rawPID, 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.RecordRuntimeSocketOwnership(); err != nil {
		t.Fatal(err)
	}
	proof, err = a.readRuntimeSocketOwnership()
	if err != nil || proof.Child.BootID == "" || proof.Supervisor.BootID == "" {
		t.Fatalf("legacy live proof incomplete: %+v %v", proof, err)
	}
	if err = syscall.Kill(pid.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		gone, err := runtimeRecordedProcessGone(proof.Child.PID, proof.Child.Start, proof.Child.BootID)
		if err == nil && gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated child survived supervisor SIGKILL")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The process died before graceful unlink, leaving the exact registered inode.
	if _, err = os.Lstat(a.P.Socket); err != nil {
		t.Fatal("fixture did not leave stale socket")
	}
	// Give this integration a short persisted delay; production backoff remains
	// 2/4/8/16/30 seconds and has separate policy tests.
	_, err = a.updateRuntimeControl(control.Generation, func(c *runtimeControl) error { c.Status, c.NextRetry = "retrying", time.Now(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = a.AutoStart(ctx); err != nil {
		t.Fatal(err)
	}
	after := readRestartControl(t, a)
	if after.Child == nil || after.Child.PID == proof.Child.PID || after.Retries != 4 || after.Generation != control.Generation {
		t.Fatalf("unexpected recovered state: %+v", after)
	}
	if err = a.Health(ctx); err != nil {
		t.Fatal(err)
	}
	refreshed, err := a.readRuntimeSocketOwnership()
	if err != nil || refreshed.Child.PID != after.Child.PID || refreshed.Child.BootID == "" {
		t.Fatalf("new child did not refresh durable ownership: %+v %v", refreshed, err)
	}
}

func TestRuntimeSocketOwnershipPersistsBeforeProtocolReady(t *testing.T) {
	a, cli, _ := restartFixture(t, "exit 0", true)
	a.P.Socket = filepath.Join(a.P.Runtime, "run", "app-server.sock")
	a.S.Backend, a.S.Installed = "container", true
	a.S.Schema, a.S.Installer, a.S.CodexHome = 1, a.P.Home, filepath.Join(a.P.Home, ".codex")
	a.P.Data, a.Out, a.Err = filepath.Join(a.P.Home, "installer-data"), io.Discard, io.Discard
	a.S.Codex.Path = cli
	t.Setenv("CODEX_BG_MIGRATION_INTEGRATION_HELPER", "1")
	t.Setenv("CODEX_BG_SOCKET_HELPER", "1")
	t.Setenv("CODEX_BG_SOCKET_PATH", a.P.Socket)
	t.Setenv("CODEX_BG_SOCKET_GATE", "")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err = AtomicWrite(a.P.Binary(), bin, 0700); err != nil {
		t.Fatal(err)
	}
	// This child binds a real socket but never responds to protocol requests.
	// A recovery record must still exist before the first health probe finishes.
	if err = os.WriteFile(cli, []byte("#!/bin/sh\nexec "+shellQuote(a.P.Binary())+" -test.run=^TestRuntimeSocketProcessHelper$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(a.P.Binary(), "--home", a.P.Home, "--runtime", a.P.Runtime, "serve")
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.stopRuntimeFor(ctx, "normal", ""); err != nil {
			_ = cmd.Process.Kill()
			t.Errorf("fixture cleanup: %v", err)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("fixture supervisor did not exit")
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		proof, err := a.readRuntimeSocketOwnership()
		if err == nil {
			control := readRestartControl(t, a)
			if control.Status != "starting" || control.Child == nil || proof.Child.PID != control.Child.PID {
				t.Fatalf("proof is not for the initializing child: %+v", control)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket evidence waited for protocol readiness: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
