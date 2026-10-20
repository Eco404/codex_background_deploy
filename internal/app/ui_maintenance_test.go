package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestUIWaitIdleRequiresConfirmationAndUsesSafeOperation(t *testing.T) {
	called := false
	m := &uiModel{ctx: context.Background(), app: &App{}}
	m.actions.Update = func(context.Context, string, bool) error { t.Fatal("selected immediate update"); return nil }
	m.actions.UpdateWhenIdle = func(_ context.Context, version string, timeout time.Duration) error {
		called = true
		if version != "" || timeout != 30*time.Minute {
			t.Fatal("lost selected update constraints")
		}
		return nil
	}
	m.updateChoice("更新工具", "")
	uiChooseTest(t, m, "等待空闲后更新")
	if called || m.screen != "confirm" {
		t.Fatal("missing confirmation")
	}
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if called {
		t.Fatal("Enter implicitly agreed to update")
	}
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if !called {
		t.Fatal("confirmed waiting update was not invoked")
	}
}

func TestUILegacyUpdaterOnlyOffersDetectedRecordAndBlocksFailedPreview(t *testing.T) {
	a := engineApp(t)
	a.S.Installed = true
	m := &uiModel{ctx: context.Background(), app: a}
	m.maintenanceMenu()
	for _, item := range m.items {
		if strings.Contains(item.label, "updater") {
			t.Fatal("absent updater offered")
		}
	}
	engineFile(t, filepath.Join(a.P.Data, ".marker"), "test")
	engineFile(t, filepath.Join(a.S.CodexHome, "app-server-daemon", "app-server-updater.pid"), "unverified")
	m.actions.PreviewLegacyUpdater = func(context.Context) (string, error) { return "身份冲突", errors.New("unknown owner") }
	m.actions.CleanupLegacyUpdater = func(context.Context, bool) error { t.Fatal("failed preview was applied"); return nil }
	m.maintenanceMenu()
	uiDrain(t, m, uiChooseTest(t, m, "检查并清理旧 updater"))
	if m.screen != "result" || m.err == nil || !strings.Contains(m.err.Error(), "身份冲突") {
		t.Fatal("unknown updater did not remain inspection-only")
	}
}
