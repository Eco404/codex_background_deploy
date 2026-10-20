package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func controlApp(t *testing.T, status string) *App {
	t.Helper()
	a := engineApp(t)
	a.S.Backend = "container"
	a.S.EntrypointDir = filepath.Join(a.P.Home, "entrypoint.d")
	if err := a.EnsureData(); err != nil {
		t.Fatal(err)
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	_, err := a.setRuntimeIntent("running", "normal", "", false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.updateRuntimeControl("", func(c *runtimeControl) error {
		c.Status, c.Retries, c.Launch = status, 4, false
		if status == "stopped" {
			c.Desired = "stopped"
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a.S.Installed = true
	enabled := true
	a.S.AutoRestart = &enabled
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRuntimeControlManualStopSurvivesAutostartAndMaintenanceDataLoss(t *testing.T) {
	for _, status := range []string{"retrying", "exhausted", "failed", "stopped"} {
		t.Run(status, func(t *testing.T) {
			a := controlApp(t, status)
			if err := a.Service(context.Background(), "stop", false); err != nil {
				t.Fatal(err)
			}
			before := readRestartControl(t, a)
			if before.Desired != "stopped" || before.Status != "stopped" || before.Retries != 4 {
				t.Fatalf("stop lost intent: %+v", before)
			}
			if err := os.RemoveAll(a.P.Data); err != nil {
				t.Fatal(err)
			}
			if err := a.AutoStart(context.Background()); err != nil {
				t.Fatal(err)
			}
			after := readRestartControl(t, a)
			if after.Generation != before.Generation || after.Retries != before.Retries {
				t.Fatal("boot reset manual intent")
			}
			if _, err := os.Stat(a.P.Data); !os.IsNotExist(err) {
				t.Fatal("boot recreated missing maintenance data")
			}
		})
	}
}

func TestRuntimeControlAutostartPreservesExhaustedAndMaintenance(t *testing.T) {
	for _, state := range []string{"exhausted", "failed", "updating"} {
		t.Run(state, func(t *testing.T) {
			a := controlApp(t, "exhausted")
			if state == "updating" {
				_, err := a.setRuntimeIntent("running", state, "operation", false)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				_, err := a.updateRuntimeControl("", func(c *runtimeControl) error { c.Status = state; return nil })
				if err != nil {
					t.Fatal(err)
				}
			}
			before := engineRead(t, a.runtimeControlFile())
			if err := a.AutoStart(context.Background()); err != nil {
				t.Fatal(err)
			}
			if engineRead(t, a.runtimeControlFile()) != before {
				t.Fatal("autostart reset blocked recovery")
			}
		})
	}
}

func TestRuntimeControlMissingOrMalformedFailsClosed(t *testing.T) {
	for _, content := range []string{"", "{", `{"schema":99}`} {
		t.Run(content, func(t *testing.T) {
			a := controlApp(t, "stopped")
			if content == "" {
				_ = os.Remove(a.runtimeControlFile())
			} else {
				engineFile(t, a.runtimeControlFile(), content)
			}
			if err := a.AutoStart(context.Background()); err == nil {
				t.Fatal("invalid control allowed automatic start")
			}
			if err := a.Service(context.Background(), "start", false); err == nil {
				t.Fatal("invalid control silently overwritten")
			}
		})
	}
}

func TestRuntimeControlUpdateRollbackPreservesStoppedAndExhausted(t *testing.T) {
	for _, status := range []string{"stopped", "exhausted"} {
		t.Run(status, func(t *testing.T) {
			a := controlApp(t, status)
			oldHook := filepath.Join(a.S.EntrypointDir, "codex-bg.sh")
			engineFile(t, a.P.Binary(), "original-manager")
			engineFile(t, oldHook, "old-start-hook")
			a.S.Files = []ManagedFile{{Path: a.P.Binary(), Hash: HashBytes([]byte("original-manager")), Kind: "binary"}, {Path: oldHook, Hash: HashBytes([]byte("old-start-hook")), Kind: "entrypoint"}}
			if err := a.Save(); err != nil {
				t.Fatal(err)
			}
			if err := a.Update(context.Background(), "", false); err != nil {
				t.Fatal(err)
			}
			c := readRestartControl(t, a)
			if c.Status != status || c.Mode != "normal" || c.Retries != 4 {
				t.Fatalf("update reset stopped/failed service: %+v", c)
			}
			if !strings.Contains(engineRead(t, oldHook), " autostart ") {
				t.Fatal("update retained unsafe old hook")
			}
			history, err := os.ReadDir(filepath.Join(a.P.Data, "history"))
			if err != nil || len(history) != 1 {
				t.Fatalf("history: %v %v", history, err)
			}
			id := strings.TrimSuffix(history[0].Name(), ".json")
			if err := a.Service(context.Background(), "stop", false); err != nil {
				t.Fatal(err)
			}
			if err := a.Rollback(context.Background(), id, false); err != nil {
				t.Fatal(err)
			}
			c = readRestartControl(t, a)
			if c.Desired != "stopped" || c.Status != "stopped" || c.Retries != 4 {
				t.Fatalf("rollback resurrected historical running intent: %+v", c)
			}
			if engineRead(t, a.P.Binary()) != "original-manager" {
				t.Fatal("rollback did not restore binary")
			}
		})
	}
}

// Exercise lifecycle against real subprocesses and local Unix protocol only.
// The helper is the test binary, with an isolated HOME and no model provider.
func TestRuntimeControlLiveLifecycle(t *testing.T) {
	a := controlApp(t, "exhausted")
	a.RunCommand = nil
	a.P.Socket = filepath.Join(a.P.Runtime, "run/app-server.sock")
	t.Setenv("CODEX_BG_MIGRATION_INTEGRATION_HELPER", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	a.S.Codex = Candidate{Path: filepath.Join(a.P.Home, "codex-fixture"), Version: "fixture", Source: "reused"}
	if err = AtomicWrite(a.S.Codex.Path, bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err = AtomicWrite(a.P.Binary(), bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = a.stopRuntimeFor(context.Background(), "normal", "") })
	if err = a.Service(ctx, "start", false); err != nil {
		t.Fatal(err)
	}
	initial := waitRestartControl(t, a, func(c runtimeControl) bool { return c.Status == "running" && c.Child != nil })
	if initial.Retries != 0 {
		t.Fatal("explicit start did not reset retry limit")
	}
	if err = a.Service(ctx, "start", false); err != nil {
		t.Fatal(err)
	}
	if c := readRestartControl(t, a); c.Generation != initial.Generation || c.Child.PID != initial.Child.PID {
		t.Fatal("idempotent start interrupted child")
	}
	// Failed preparation recovery must not stop or replace a live supervisor.
	tx, err := a.begin("update")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.recoverLocked(ctx); err != nil {
		t.Fatal(err)
	}
	if c := readRestartControl(t, a); c.Generation != initial.Generation || c.Child.PID != initial.Child.PID {
		t.Fatal("preparation rollback interrupted live child")
	}
	if hasPending(a.P.Data) {
		t.Fatalf("preparation transaction %s remains", tx.ID)
	}
	if err = syscall.Kill(initial.Child.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err = a.Service(ctx, "stop", true); err != nil {
		t.Fatal(err)
	}
	if err = a.AutoStart(ctx); err != nil {
		t.Fatal(err)
	}
	if c := readRestartControl(t, a); c.Desired != "stopped" || c.Child != nil {
		t.Fatalf("manual stop resurrected child: %+v", c)
	}
	if err = a.Service(ctx, "start", false); err != nil {
		t.Fatal(err)
	}
	if err = a.Update(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	if c := readRestartControl(t, a); c.Mode != "normal" || c.Desired != "running" {
		t.Fatalf("update left maintenance: %+v", c)
	}
	if err = a.Health(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeControlStopIntentPrecedesSystemdSignal(t *testing.T) {
	a := controlApp(t, "running")
	a.S.Backend = "systemd"
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	var calls []string
	a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "systemctl" && strings.Contains(strings.Join(args, " "), " stop ") {
			c := readRestartControl(t, a)
			if c.Desired != "stopped" {
				t.Fatal("stop signal before persistent intent")
			}
		}
		return nil, nil
	}
	if err := a.Service(context.Background(), "stop", true); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "stop codex-bg.service") {
		t.Fatalf("calls: %v", calls)
	}
	if err := a.AutoStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatal("boot started manually stopped systemd service")
	}
}

func TestRuntimeControlRejectsStaleWriter(t *testing.T) {
	a := controlApp(t, "retrying")
	old := readRestartControl(t, a)
	if _, err := a.setRuntimeIntent("stopped", "normal", "", false); err != nil {
		t.Fatal(err)
	}
	_, err := a.updateRuntimeControl(old.Generation, func(c *runtimeControl) error { c.Desired = "running"; return nil })
	if err != errRuntimeSuperseded {
		t.Fatalf("stale generation accepted: %v", err)
	}
	raw, _ := json.Marshal(readRestartControl(t, a))
	if !strings.Contains(string(raw), `"desired":"stopped"`) {
		t.Fatal("stale writer undid manual stop")
	}
}

func TestRuntimeControlFailedStopDoesNotClaimStopped(t *testing.T) {
	a := controlApp(t, "running")
	a.S.Backend = "systemd"
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	a.RunCommand = func(context.Context, string, []string, []string) ([]byte, error) {
		return nil, errors.New("fixture stop failed")
	}
	if err := a.Service(context.Background(), "stop", true); err == nil {
		t.Fatal("stop failure hidden")
	}
	c := readRestartControl(t, a)
	if c.Desired != "stopped" || c.Status != "stop_failed" || a.RuntimeServiceState("running") != "stop_failed" {
		t.Fatalf("stop failed but reported stopped: %+v", c)
	}
	if err := a.Service(context.Background(), "start", false); err == nil {
		t.Fatal("start allowed before stop confirmed")
	}
	a.RunCommand = func(context.Context, string, []string, []string) ([]byte, error) { return nil, nil }
	if err := a.Service(context.Background(), "stop", true); err != nil {
		t.Fatal(err)
	}
	if c = readRestartControl(t, a); c.Status != "stopped" {
		t.Fatal("successful stop retry not reflected")
	}
}

func TestRuntimeControlStartRejectsForeignHealthyListener(t *testing.T) {
	a := controlApp(t, "starting")
	a.S.Backend = "systemd"
	a.P.Socket = runtimeFixture(t, runtimeResponder("idle"))
	a.RunCommand = func(context.Context, string, []string, []string) ([]byte, error) { return nil, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := a.Start(ctx); err == nil {
		t.Fatal("foreign healthy protocol response accepted as our child")
	}
	if err := a.Health(context.Background()); err != nil {
		t.Fatal("foreign listener was disturbed")
	}
}
