package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestUIAutoRestartDefaultAndOptOutReachDeployment(t *testing.T) {
	for _, migrate := range []bool{false, true} {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("migrate=%t/disabled=%t", migrate, disabled), func(t *testing.T) {
				called := false
				m := &uiModel{networkReady: true, ctx: context.Background(), app: &App{}}
				execute := func(_ context.Context, opt InstallOptions) error {
					called = true
					if opt.NoAutoRestart != disabled || !opt.Yes {
						t.Fatalf("deployment lost restart choice: %+v", opt)
					}
					return nil
				}
				m.actions.Install, m.actions.Migrate = execute, execute
				m.installWizard(migrate)
				m.installComponents()
				if m.options.NoAutoRestart || uiFind(t, m, "[x] 崩溃后自动重启").disabled != "" {
					t.Fatal("automatic restart should be selected by default")
				}
				if disabled {
					uiChooseTest(t, m, "[x] 崩溃后自动重启")
					uiFind(t, m, "[ ] 崩溃后自动重启")
				}
				uiChooseTest(t, m, "预览并确认")
				want := "崩溃后自动重启：开启"
				if disabled {
					want = "崩溃后自动重启：关闭"
				}
				if m.screen != "confirm" || !strings.Contains(m.detail, want) || called {
					t.Fatal("restart policy absent from preview or applied prematurely")
				}
				uiKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
				if m.options.NoAutoRestart != disabled {
					t.Fatal("returning to choices reset restart selection")
				}
				uiChooseTest(t, m, "预览并确认")
				uiKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
				if !called {
					t.Fatal("confirmed deployment was not executed")
				}
			})
		}
	}
}

func TestUIRetryStopAndFailedStartUseExplicitNormalConfirmation(t *testing.T) {
	for _, state := range []string{"retrying", "exhausted", "failed"} {
		t.Run(state, func(t *testing.T) {
			called := false
			action, label := "start", "启动"
			if state == "retrying" {
				action, label = "stop", "停止"
			}
			m := &uiModel{ctx: context.Background(), app: &App{S: State{Installed: true}}, inventory: Inventory{ServiceState: state}}
			m.actions.Service = func(_ context.Context, got string, force bool) error {
				called = true
				if got != action || force {
					t.Fatalf("unexpected operation: %s force=%t", got, force)
				}
				return nil
			}
			m.serviceMenu()
			if !strings.Contains(m.note, serviceStateLabel(state)) {
				t.Fatal("service state missing from menu")
			}
			uiChooseTest(t, m, label)
			if m.screen != "confirm" || called {
				t.Fatal("expected a single explicit confirmation")
			}
			uiKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
			if called {
				t.Fatal("Enter confirmed service operation")
			}
			uiKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
			if !called {
				t.Fatal("explicit confirmation did not run operation")
			}
		})
	}
}

func TestUICodexAppRequiresReadyCoreDuringRecovery(t *testing.T) {
	m := &uiModel{app: &App{P: Paths{Data: t.TempDir()}, S: State{Installed: true}}, component: "codexapp"}
	for _, state := range []string{"starting", "retrying", "exhausted", "failed", "stopped", "stopping", "stop_failed", "maintenance"} {
		m.inventory.ServiceState = state
		if m.componentInstallBlock("codexapp", false) == "" || m.componentActionBlock("configure") == "" {
			t.Fatalf("CodexApp startup enabled while core is %s", state)
		}
	}
	m.inventory.ServiceState = "running"
	if m.componentInstallBlock("codexapp", false) != "" || m.componentActionBlock("configure") != "" {
		t.Fatal("ready core should permit CodexApp startup")
	}
}
