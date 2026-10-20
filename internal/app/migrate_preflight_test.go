package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/coder/websocket"
)

func migrationPreflightApp(t *testing.T) (*App, InstallOptions, string) {
	t.Helper()
	a := engineApp(t)
	t.Setenv("PATH", filepath.Join(a.P.Home, "empty-bin"))
	t.Setenv("NVM_DIR", filepath.Join(a.P.Home, ".nvm"))
	t.Setenv("ENTRYPOINTD", filepath.Join(a.P.Home, "entrypoint.d"))
	root := filepath.Join(a.P.Home, ".local/share/code-server/extensions")
	entry := filepath.Join(root, "openai.chatgpt-1/bin/linux/codex")
	engineFile(t, entry, "original-codex")
	cli := filepath.Join(a.P.Home, "standalone-codex")
	engineFile(t, cli, "standalone")
	hook := filepath.Join(a.P.Home, "entrypoint.d/05-start-codex-daemon.sh")
	engineFile(t, hook, "#!/bin/sh\n\"$HOME/.codex/packages/standalone/current/codex\" app-server daemon start\n")
	a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
		if name == cli {
			if len(args) == 1 && args[0] == "--version" {
				return []byte("codex-cli 0.163.0"), nil
			}
			if strings.Join(args, " ") == "app-server --help" {
				return []byte("--listen unix://PATH"), nil
			}
		}
		if name == "systemctl" && strings.Join(args, " ") == "--user show-environment" {
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected external command %s %v", name, args)
	}
	return a, InstallOptions{Backend: "systemd", CodexPath: cli, Roots: []string{root}, EntrypointDir: filepath.Dir(hook), Yes: true}, hook
}

// Keep the Unix socket path short without placing any test files outside the
// installer maintenance directory, whose absolute name can already be long.
func migrationOldDaemonFixture(t *testing.T, a *App, status string) {
	t.Helper()
	dir := filepath.Join(a.P.Home, "old-codex-home")
	if err := os.MkdirAll(filepath.Join(dir, "app-server-control"), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	a.S.CodexHome = fmt.Sprintf("/proc/self/fd/%d", f.Fd())
	socket := filepath.Join(a.S.CodexHome, "app-server-control/app-server-control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		runtimeResponder(status)(r.Context(), c)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close(); listener.Close() })
}

func TestMigrationPreflightDryRunChecksBusyWithoutWriting(t *testing.T) {
	for _, status := range []string{"active", "idle", "unknown"} {
		t.Run(status, func(t *testing.T) {
			a, opt, hook := migrationPreflightApp(t)
			migrationOldDaemonFixture(t, a, status)
			original := engineRead(t, hook)
			opt.DryRun = true
			err := a.Migrate(context.Background(), opt)
			if (err == nil) != (status == "idle") {
				t.Fatalf("dry-run busy=%s: %v", status, err)
			}
			if status == "active" && !strings.Contains(err.Error(), "活跃任务") {
				t.Fatalf("wrong busy reason: %v", err)
			}
			if status == "unknown" && !strings.Contains(err.Error(), "不能确认") {
				t.Fatalf("unknown status was not rejected conservatively: %v", err)
			}
			if engineRead(t, hook) != original {
				t.Fatal("preview altered old startup hook")
			}
			for _, p := range []string{a.P.Runtime, a.P.Data} {
				if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("dry-run created %s: %v", p, err)
				}
			}
		})
	}
}

func TestMigrationPreflightProtectsUnselectedIDE(t *testing.T) {
	for _, kind := range []string{"legacy-setting", "external-setting", "legacy-hook"} {
		t.Run(kind, func(t *testing.T) {
			a, opt, _ := migrationPreflightApp(t)
			otherRoot := filepath.Join(a.P.Home, ".vscode-server/extensions")
			path := filepath.Join(a.P.Home, ".vscode-server/data/Machine/settings.json")
			original := `{"editor.fontSize":17,"chatgpt.cliExecutable":"/external/codex"}`
			if kind == "legacy-setting" {
				raw, _ := json.Marshal(map[string]any{"editor.fontSize": 17, "chatgpt.cliExecutable": filepath.Join(a.P.Home, "apps/codex-daemon-bridge/codex")})
				original = string(raw)
			}
			if kind == "legacy-hook" {
				path = filepath.Join(otherRoot, "openai.chatgpt-1/bin/linux/codex")
				original = "#!/bin/sh\n# vscode-codex-background-deploy extension hook\n"
			}
			engineFile(t, path, original)
			opt.DryRun = true
			err := a.Migrate(context.Background(), opt)
			if kind == "external-setting" {
				if err != nil {
					t.Fatalf("unrelated IDE prevents selected migration: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "未选择") {
				t.Fatalf("unselected legacy consumer not protected: %v", err)
			}
			if engineRead(t, path) != original {
				t.Fatal("unselected IDE was modified")
			}
		})
	}
}

func TestMigrationPreflightChecksActiveSystemdSocket(t *testing.T) {
	for _, socketState := range []string{"active", "idle", "missing"} {
		t.Run(socketState, func(t *testing.T) {
			a, opt, hook := migrationPreflightApp(t)
			socket := ""
			if socketState != "missing" {
				socket = runtimeFixture(t, runtimeResponder(socketState))
			}
			unit := filepath.Join(a.P.Home, ".config/systemd/user/codex-app-server.service")
			body := "Description=Codex app-server background runtime\n"
			if socket != "" {
				body += "ExecStart=" + opt.CodexPath + " app-server --listen \"unix://" + socket + "\"\n"
			}
			engineFile(t, unit, body)
			base := a.RunCommand
			a.RunCommand = func(ctx context.Context, name string, args, env []string) ([]byte, error) {
				if name == "systemctl" && len(args) > 1 && args[1] == "is-active" {
					return []byte("active\n"), nil
				}
				if name == "systemctl" && len(args) > 1 && args[1] == "is-enabled" {
					return []byte("enabled\n"), nil
				}
				return base(ctx, name, args, env)
			}
			opt.DryRun = true
			err := a.Migrate(context.Background(), opt)
			if (err == nil) != (socketState == "idle") {
				t.Fatalf("systemd %s socket accepted/rejected incorrectly: %v", socketState, err)
			}
			if engineRead(t, unit) != body {
				t.Fatal("preflight changed legacy unit")
			}
			if _, err := os.Stat(hook); err != nil {
				t.Fatal("preflight removed legacy hook")
			}
			for _, p := range []string{a.P.Runtime, a.P.Data} {
				if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("systemd preview created %s: %v", p, err)
				}
			}
		})
	}
}

func migrationAssertLockHeld(t *testing.T, a *App, stage string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(a.P.Runtime, "operation.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("%s: operation lock missing: %v", stage, err)
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		t.Fatalf("%s: concurrent management can enter during migration", stage)
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("%s: unexpected lock error: %v", stage, err)
	}
}

// These unit-lifecycle fixtures serve the protocol in the test process. Publish
// the same readiness and ownership record a real supervisor would produce.
func migrationFixtureReady(t *testing.T, a *App) {
	t.Helper()
	start, err := runtimeProcessStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.updateRuntimeControl("", func(c *runtimeControl) error {
		c.Child = &runtimeChild{PID: os.Getpid(), Start: start}
		c.Status, c.Launch = "running", false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigrationKeepsLockThroughNewCoreAndRecovery(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail-new-start=%v", fail), func(t *testing.T) {
			a, opt, hook := migrationPreflightApp(t)
			migrationOldDaemonFixture(t, a, "idle")
			unit := filepath.Join(a.P.Home, ".config/systemd/user/codex-app-server.service")
			unitBody := "Description=Codex app-server background runtime\nExecStart=" + opt.CodexPath + " app-server --listen unix://\n"
			engineFile(t, unit, unitBody)
			settings := filepath.Join(a.P.Home, ".local/share/code-server/User/settings.json")
			legacyCLI := filepath.Join(a.P.Home, "apps/codex-daemon-bridge/codex")
			settingsRaw, _ := json.Marshal(map[string]any{"chatgpt.cliExecutable": legacyCLI, "editor.fontSize": 21})
			engineFile(t, settings, string(settingsRaw))
			a.P.Socket = runtimeFixture(t, runtimeResponder("idle"))
			base := a.RunCommand
			stopped, coreChecked, startedNew, restored := false, false, false, false
			a.RunCommand = func(ctx context.Context, name string, args, env []string) ([]byte, error) {
				if stopped {
					migrationAssertLockHeld(t, a, name+" "+strings.Join(args, " "))
					if name == opt.CodexPath {
						coreChecked = true
					}
				}
				if name != "systemctl" {
					return base(ctx, name, args, env)
				}
				if len(args) > 1 {
					switch args[1] {
					case "is-active":
						return []byte("active\n"), nil
					case "is-enabled":
						return []byte("enabled\n"), nil
					case "disable":
						migrationAssertLockHeld(t, a, "old-service-stop")
						stopped = true
					case "start":
						if args[len(args)-1] == "codex-bg.service" {
							startedNew = true
							if !hasPending(a.P.Data) {
								t.Fatal("core committed before migration finished")
							}
							if fail {
								return nil, errors.New("injected new-core startup failure")
							}
							migrationFixtureReady(t, a)
						} else if args[len(args)-1] == filepath.Base(unit) {
							restored = true
						}
					}
				}
				return nil, nil
			}
			err := a.Migrate(context.Background(), opt)
			if fail {
				if err == nil || !strings.Contains(err.Error(), "injected new-core") {
					t.Fatalf("failed core not reported: %v", err)
				}
				if !restored || a.S.Installed || engineRead(t, unit) != unitBody {
					t.Fatal("legacy service was not fully restored")
				}
				if _, err := os.Stat(hook); err != nil {
					t.Fatalf("legacy hook not restored: %v", err)
				}
			} else if err != nil || !a.S.Installed || len(a.S.Bindings) != 1 {
				t.Fatalf("migration did not install and bind: %v", err)
			}
			var settingsAfter map[string]any
			if err := json.Unmarshal([]byte(engineRead(t, settings)), &settingsAfter); err != nil {
				t.Fatal(err)
			}
			value, present := settingsAfter["chatgpt.cliExecutable"]
			if present != fail || fail && value != legacyCLI || settingsAfter["editor.fontSize"] != float64(21) {
				t.Fatalf("setting was not removed on commit/restored on rollback without losing unrelated values: %v", settingsAfter)
			}
			if !stopped || !coreChecked || !startedNew || hasPending(a.P.Data) {
				t.Fatalf("missing lifecycle coverage: stopped=%v core=%v start=%v pending=%v", stopped, coreChecked, startedNew, hasPending(a.P.Data))
			}
			unlock, err := a.Lock()
			if err != nil {
				t.Fatalf("finished migration retained lock: %v", err)
			}
			unlock()
		})
	}
}

func TestMigrationChangedSettingPreservedAndOldServiceRestored(t *testing.T) {
	a, opt, _ := migrationPreflightApp(t)
	migrationOldDaemonFixture(t, a, "idle")
	settings := filepath.Join(a.P.Home, ".local/share/code-server/User/settings.json")
	raw, _ := json.Marshal(map[string]string{"chatgpt.cliExecutable": filepath.Join(a.P.Home, "apps/codex-daemon-bridge/codex")})
	engineFile(t, settings, string(raw))
	unit := filepath.Join(a.P.Home, ".config/systemd/user/codex-app-server.service")
	unitBody := "Description=Codex app-server background runtime\nExecStart=" + opt.CodexPath + " app-server --listen unix://\n"
	engineFile(t, unit, unitBody)
	changed := `{"chatgpt.cliExecutable":"/user/new/codex","editor.fontSize":21}`
	base := a.RunCommand
	stopped, restored := false, false
	a.RunCommand = func(ctx context.Context, name string, args, env []string) ([]byte, error) {
		if name != "systemctl" {
			return base(ctx, name, args, env)
		}
		if len(args) > 1 {
			switch args[1] {
			case "is-active":
				return []byte("active\n"), nil
			case "is-enabled":
				return []byte("enabled\n"), nil
			case "disable":
				stopped = true
				engineFile(t, settings, changed)
			case "start":
				if args[len(args)-1] == filepath.Base(unit) {
					restored = true
				}
			}
		}
		return nil, nil
	}
	err := a.Migrate(context.Background(), opt)
	if err == nil || !strings.Contains(err.Error(), "预检查后变化") {
		t.Fatalf("setting change was not rejected: %v", err)
	}
	if engineRead(t, settings) != changed {
		t.Fatal("concurrent user setting overwritten")
	}
	if !stopped || !restored || engineRead(t, unit) != unitBody || hasPending(a.P.Data) {
		t.Fatalf("untouched setting conflict prevented old service recovery: stopped=%v restored=%v pending=%v err=%v", stopped, restored, hasPending(a.P.Data), err)
	}
}

func TestMigrationChangedLegacyEntryDoesNotOverwriteExternalChanges(t *testing.T) {
	for _, stage := range []string{"before-stop", "after-stop"} {
		for _, target := range []string{"wrapper", "original"} {
			t.Run(stage+"/"+target, func(t *testing.T) {
				a, opt, _ := migrationPreflightApp(t)
				entry := filepath.Join(opt.Roots[0], "openai.chatgpt-1/bin/linux/codex")
				wrapper := "#!/bin/sh\n# vscode-codex-background-deploy extension hook\nexit 0\n"
				original := "native-original"
				engineFile(t, entry, wrapper)
				engineFile(t, entry+".real", original)
				unit := filepath.Join(a.P.Home, ".config/systemd/user/codex-extension-hook.timer")
				unitBody := "# vscode-codex-background-deploy extension-hook timer v2\n"
				engineFile(t, unit, unitBody)
				path := entry
				if target == "original" {
					path += ".real"
				}
				changed := "external-new-" + target
				base := a.RunCommand
				validations, stopped, restored, mutated := 0, false, false, false
				a.RunCommand = func(ctx context.Context, name string, args, env []string) ([]byte, error) {
					if name == opt.CodexPath && strings.Join(args, " ") == "app-server --help" {
						validations++
						if validations == 2 && stage == "before-stop" {
							// This second capability check follows entry hashing in
							// the preflight repeated under the management lock.
							engineFile(t, path, changed)
							mutated = true
						}
					}
					if name != "systemctl" {
						return base(ctx, name, args, env)
					}
					if len(args) > 1 {
						switch args[1] {
						case "is-active":
							return []byte("active\n"), nil
						case "is-enabled":
							return []byte("enabled\n"), nil
						case "disable":
							stopped = true
							if stage == "after-stop" {
								engineFile(t, path, changed)
								mutated = true
							}
						case "start":
							if args[len(args)-1] == filepath.Base(unit) {
								restored = true
							}
						}
					}
					return nil, nil
				}
				err := a.Migrate(context.Background(), opt)
				if err == nil || !strings.Contains(err.Error(), "变化") {
					t.Fatalf("changed legacy source was not rejected: %v", err)
				}
				if !mutated || engineRead(t, path) != changed {
					t.Fatal("external change was not preserved")
				}
				if target == "wrapper" && engineRead(t, entry+".real") != original || target == "original" && engineRead(t, entry) != wrapper {
					t.Fatal("unmodified legacy source was changed")
				}
				wantStopped := stage == "after-stop"
				if stopped != wantStopped || restored != wantStopped || engineRead(t, unit) != unitBody || hasPending(a.P.Data) || a.S.Installed || len(a.S.Bindings) != 0 {
					t.Fatalf("wrong recovery state: stopped=%v restored=%v installed=%v pending=%v err=%v", stopped, restored, a.S.Installed, hasPending(a.P.Data), err)
				}
			})
		}
	}
}

func TestMigrationV3SystemdUsesOnlyUnitLifecycle(t *testing.T) {
	controller, err := os.ReadFile("../../legacy/v3/payload/control.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail-new-start=%v", fail), func(t *testing.T) {
			a, opt, _ := migrationPreflightApp(t)
			a.P.Socket = runtimeFixture(t, runtimeResponder("idle"))
			control := filepath.Join(a.P.Home, ".local/share/codex-background/payload/control.sh")
			engineFile(t, control, string(controller))
			engineFile(t, filepath.Join(a.P.Installer, "legacy/v3/payload/control.sh"), string(controller))
			engineFile(t, filepath.Join(a.P.Home, ".local/share/codex-background/managed-v3"), "managed\n")
			v3Hook := filepath.Join(a.P.Home, "entrypoint.d/20-codex-background.sh")
			engineFile(t, v3Hook, "#!/bin/sh\n# codex-background managed startup hook\n/bin/bash "+control+" start\n")
			unit := filepath.Join(a.P.Home, ".config/systemd/user/codex-background.service")
			unitBody := "Description=Codex background runtime (managed)\nExecStart=" + opt.CodexPath + " app-server --listen \"unix://" + a.P.Socket + "\"\n"
			engineFile(t, unit, unitBody)
			base := a.RunCommand
			stops, restarts, controllerCalls := 0, 0, 0
			a.RunCommand = func(ctx context.Context, name string, args, env []string) ([]byte, error) {
				if name == "/bin/bash" {
					controllerCalls++
					return nil, errors.New("controller duplicates the systemd lifecycle")
				}
				if name != "systemctl" {
					return base(ctx, name, args, env)
				}
				if len(args) > 1 {
					switch args[1] {
					case "is-active":
						return []byte("active\n"), nil
					case "is-enabled":
						return []byte("enabled\n"), nil
					case "disable":
						if args[len(args)-1] == filepath.Base(unit) {
							stops++
						}
					case "start":
						if args[len(args)-1] == filepath.Base(unit) {
							restarts++
						} else if fail && args[len(args)-1] == "codex-bg.service" {
							return nil, errors.New("injected core failure")
						} else if args[len(args)-1] == "codex-bg.service" {
							migrationFixtureReady(t, a)
						}
					}
				}
				return nil, nil
			}
			err := a.Migrate(context.Background(), opt)
			if fail {
				if err == nil || !strings.Contains(err.Error(), "injected core failure") || restarts != 1 || engineRead(t, unit) != unitBody {
					t.Fatalf("v3 rollback failed: restarts=%d err=%v", restarts, err)
				}
				if _, err := os.Stat(v3Hook); err != nil {
					t.Fatalf("v3 startup hook not restored: %v", err)
				}
			} else if err != nil || restarts != 0 || !a.S.Installed {
				t.Fatalf("v3 migration failed: restarts=%d err=%v", restarts, err)
			}
			if controllerCalls != 0 || stops != 1 || hasPending(a.P.Data) {
				t.Fatalf("duplicate/incomplete v3 lifecycle: controller=%d stops=%d pending=%v", controllerCalls, stops, hasPending(a.P.Data))
			}
		})
	}
}

type migrationExitStatus int

func (s migrationExitStatus) Error() string { return fmt.Sprintf("exit status %d", s) }
func (s migrationExitStatus) ExitCode() int { return int(s) }

func TestMigrationUnitStateRequiresKnownOutputAndExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name, active, enabled             string
		activeErr, enableErr              error
		wantActive, wantEnable, wantError bool
	}{
		{name: "enabled active", active: "active", enabled: "enabled", wantActive: true, wantEnable: true},
		{name: "disabled inactive", active: "inactive", activeErr: migrationExitStatus(3), enabled: "disabled", enableErr: migrationExitStatus(1)},
		{name: "static failed", active: "failed", activeErr: migrationExitStatus(3), enabled: "static"},
		{name: "enabled inactive", active: "inactive", activeErr: migrationExitStatus(3), enabled: "enabled", wantEnable: true},
		{name: "active transport failure", active: "active", activeErr: errors.New("manager unavailable"), enabled: "enabled", wantError: true},
		{name: "inactive unexpected error", active: "inactive", activeErr: migrationExitStatus(1), enabled: "enabled", wantError: true},
		{name: "unknown unit", active: "unknown", activeErr: migrationExitStatus(4), enabled: "enabled", wantError: true},
		{name: "enabled unreadable", active: "active", enabled: "enabled", enableErr: errors.New("manager unavailable"), wantError: true},
		{name: "disabled mismatched status", active: "active", enabled: "disabled", enableErr: migrationExitStatus(4), wantError: true},
		{name: "unknown enabled", active: "active", enabled: "", wantError: true},
		{name: "runtime links unsupported", active: "active", enabled: "enabled-runtime", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := engineApp(t)
			a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
				if name != "systemctl" || len(args) != 3 {
					t.Fatalf("unexpected state query %s %v", name, args)
				}
				if args[1] == "is-active" {
					return []byte(tc.active + "\n"), tc.activeErr
				}
				if args[1] == "is-enabled" {
					return []byte(tc.enabled + "\n"), tc.enableErr
				}
				t.Fatalf("unexpected systemctl operation %v", args)
				return nil, nil
			}
			state, err := a.inspectLegacyUnitState(context.Background(), "codex-extension-hook.timer")
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected state acceptance: %+v, %v", state, err)
			}
			if !tc.wantError && (state.Active != tc.wantActive || state.Enabled != tc.wantEnable) {
				t.Fatalf("incorrect recovery state: %+v", state)
			}
		})
	}
}

func TestMigrationUnknownTimerStateNeverStopsEvenWhenForced(t *testing.T) {
	for _, when := range []string{"preflight", "before-stop"} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/force=%v", when, force), func(t *testing.T) {
				a, opt, hook := migrationPreflightApp(t)
				opt.Force = force
				unit := filepath.Join(a.P.Home, ".config/systemd/user/codex-extension-hook.timer")
				unitBody := "# vscode-codex-background-deploy extension-hook timer v2\n"
				engineFile(t, unit, unitBody)
				base := a.RunCommand
				stops, pendingReads := 0, 0
				a.RunCommand = func(ctx context.Context, name string, args, env []string) ([]byte, error) {
					if name != "systemctl" || len(args) < 2 {
						return base(ctx, name, args, env)
					}
					switch args[1] {
					case "is-active":
						return []byte("active\n"), nil
					case "is-enabled":
						if hasPending(a.P.Data) {
							pendingReads++
						}
						if when == "preflight" || pendingReads > 1 {
							return []byte("enabled\n"), errors.New("injected manager read failure")
						}
						return []byte("enabled\n"), nil
					case "disable", "stop":
						stops++
						return nil, nil
					}
					return base(ctx, name, args, env)
				}
				err := a.Migrate(context.Background(), opt)
				if err == nil || !strings.Contains(err.Error(), "无法保证恢复") {
					t.Fatalf("unreadable timer state was accepted: %v", err)
				}
				if stops != 0 || engineRead(t, unit) != unitBody || hasPending(a.P.Data) || a.S.Installed {
					t.Fatalf("unknown timer state mutated deployment: stops=%d pending=%v installed=%v err=%v", stops, hasPending(a.P.Data), a.S.Installed, err)
				}
				if _, err := os.Stat(hook); err != nil {
					t.Fatalf("legacy startup hook changed: %v", err)
				}
				if when == "preflight" {
					for _, path := range []string{a.P.Runtime, a.P.Data} {
						if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("failed preflight created %s: %v", path, err)
						}
					}
				}
			})
		}
	}
}
