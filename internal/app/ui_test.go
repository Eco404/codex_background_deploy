package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func uiKey(t *testing.T, m *uiModel, key tea.KeyMsg) {
	t.Helper()
	_, cmd := m.Update(key)
	uiDrain(t, m, cmd)
}

func uiDrain(t *testing.T, m *uiModel, cmd tea.Cmd) {
	t.Helper()
	for n := 0; cmd != nil; n++ {
		if n > 10 {
			t.Fatal("UI commands did not settle")
		}
		_, cmd = m.Update(cmd())
	}
}

func TestUIDeploymentRoutesByDetectedState(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			installed, migrated := 0, 0
			m := &uiModel{networkReady: true, ctx: context.Background(), app: &App{}, actions: UIActions{
				Install: func(context.Context, InstallOptions) error { installed++; return nil },
				Migrate: func(context.Context, InstallOptions) error { migrated++; return nil },
			}}
			m.inventory = Inventory{Systemd: true, Extensions: []Extension{{Root: "/extensions"}}, Candidates: map[string][]Candidate{"codex": {{Path: "/existing/codex", Compatible: true}}}}
			if legacy {
				m.inventory.Legacy = []string{"/legacy/service"}
			}
			m.home()
			if len(m.items) != 4 {
				t.Fatalf("home not grouped: %d", len(m.items))
			}
			uiChooseTest(t, m, "部署管理")
			label := "安装后台"
			if legacy {
				label = "迁移现有部署"
			}
			if len(m.items) != 3 {
				t.Fatal("uninstalled state exposes duplicate or unavailable operations")
			}
			uiChooseTest(t, m, label)
			if m.migrate != legacy || !strings.HasPrefix(m.title, m.deploymentKind()) {
				t.Fatal("deployment route not labelled correctly")
			}
			uiChooseTest(t, m, "[ ] /extensions")
			uiChooseTest(t, m, "下一步")
			uiChooseTest(t, m, "复用")
			uiChooseTest(t, m, "systemd")
			uiChooseTest(t, m, "预览并确认")
			if legacy && (!strings.Contains(m.detail, "旧服务") || !strings.Contains(m.detail, "/legacy/service")) {
				t.Fatal("migration preview omitted legacy effects")
			}
			uiKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
			if installed+migrated != 0 {
				t.Fatal("Enter confirmed deployment")
			}
			uiKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
			if legacy && (migrated != 1 || installed != 0) || !legacy && (installed != 1 || migrated != 0) {
				t.Fatalf("wrong operation: installed=%d migrated=%d", installed, migrated)
			}
		})
	}
	m := &uiModel{networkReady: true, app: &App{}}
	m.app.S.Installed = true
	m.app.P.Data = t.TempDir()
	m.inventory.Legacy = []string{"remaining legacy files"}
	m.deploymentMenu()
	for _, item := range m.items {
		if strings.Contains(item.label, "安装") || strings.Contains(item.label, "迁移") || strings.Contains(item.label, "重新配置") {
			t.Fatalf("installed deployment offers unusable operation: %s", item.label)
		}
	}
	uiChooseTest(t, m, "更新核心")
}

func TestUIBackAndReadResultsStayInSection(t *testing.T) {
	for _, returnKey := range []tea.KeyMsg{{Type: tea.KeyEsc}, {Type: tea.KeyEnter}} {
		m := &uiModel{networkReady: true, ctx: context.Background(), app: &App{}, actions: UIActions{Status: func(context.Context) string { return "stopped" }}}
		m.app.S.Installed = true
		m.home()
		uiChooseTest(t, m, "运行与维护")
		uiChooseTest(t, m, "后台服务")
		uiDrain(t, m, uiChooseTest(t, m, "只读状态"))
		if m.screen != "result" || m.detail != "stopped" {
			t.Fatal("missing status output")
		}
		uiKey(t, m, returnKey)
		if m.title != "后台服务" {
			t.Fatal("result jumped out of service menu")
		}
		uiKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
		if m.title != "运行与维护" {
			t.Fatal("Esc skipped parent menu")
		}
		uiKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
		if !strings.HasPrefix(m.title, "Codex Background") {
			t.Fatal("Esc did not reach home")
		}
	}
}

func TestUIWizardBackPreservesSelectionAndDropsUnfinishedComponent(t *testing.T) {
	m := &uiModel{networkReady: true, app: &App{}, inventory: Inventory{Systemd: true, Extensions: []Extension{{Root: "/extensions"}}, Candidates: map[string][]Candidate{"codex": {{Path: "/codex", Compatible: true}}}}}
	m.installWizard(false)
	uiChooseTest(t, m, "[ ] /extensions")
	uiChooseTest(t, m, "下一步")
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if !m.selected["/extensions"] || !strings.HasPrefix(m.items[0].label, "[x]") {
		t.Fatal("returning lost explicit extension selection")
	}
	uiChooseTest(t, m, "下一步")
	uiChooseTest(t, m, "复用")
	uiChooseTest(t, m, "systemd")
	uiChooseTest(t, m, "[ ] node")
	uiChooseTest(t, m, "单独安装一份")
	m.input = "22.1.0"
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyEsc}) // shell integration -> candidate
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyEsc}) // candidate -> component list
	if len(m.options.Components) != 0 || m.componentForInstall {
		t.Fatal("cancelled component left in installation plan")
	}
	uiChooseTest(t, m, "预览并确认")
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if m.screen != "menu" || !strings.Contains(m.title, "可选组件") || m.pending != nil {
		t.Fatal("preview cancellation did not preserve wizard")
	}
}

func TestUICancelResetsStrongConfirmation(t *testing.T) {
	called := false
	m := &uiModel{networkReady: true, app: &App{}, actions: UIActions{Service: func(context.Context, string, bool) error { called = true; return nil }}}
	m.app.S.Installed = true
	m.serviceMenu()
	uiChooseTest(t, m, "停止")
	uiChooseTest(t, m, "强制执行")
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if called || m.screen != "menu" || m.pending != nil {
		t.Fatal("Esc executed action or left confirmation pending")
	}
	uiChooseTest(t, m, "强制执行")
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if called || m.confirmStage != 1 {
		t.Fatal("returning bypassed second confirmation")
	}
}

func TestUICancellingBrandingInputDoesNotEnableAddon(t *testing.T) {
	m := &uiModel{networkReady: true, app: &App{}, inventory: Inventory{Container: true, MediaDir: t.TempDir()}}
	m.installWizard(false)
	m.installComponents()
	uiChooseTest(t, m, "[ ] branding")
	if m.screen != "input" {
		t.Fatal("expected branding target prompt")
	}
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	uiChooseTest(t, m, "预览并确认")
	if len(m.options.Components) != 0 || strings.Contains(m.detail, "branding") {
		t.Fatal("cancelled branding still included in plan")
	}
}

func TestUIMutationRefreshesStateBeforeReturning(t *testing.T) {
	for _, returnKey := range []tea.KeyMsg{{Type: tea.KeyEsc}, {Type: tea.KeyEnter}} {
		m := &uiModel{networkReady: true, ctx: context.Background(), app: &App{}}
		m.app.S.Installed = true
		m.app.P.Data = t.TempDir()
		refreshed := false
		m.actions = UIActions{
			Uninstall: func(context.Context, bool) error { return nil },
			Inventory: func(context.Context) (Inventory, error) {
				refreshed = true
				m.app.S.Installed = false
				return Inventory{}, nil
			},
		}
		m.deploymentMenu()
		uiChooseTest(t, m, "卸载核心")
		uiChooseTest(t, m, "安全执行")
		uiKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
		uiKey(t, m, returnKey)
		if !refreshed || m.title != "部署管理" || m.items[0].label != "安装后台" {
			t.Fatal("returning left stale installation menu")
		}
	}
}

func TestUICleanupUsesSelectedTargetAndRefreshStaysInSection(t *testing.T) {
	var action, target string
	m := &uiModel{networkReady: true, ctx: context.Background(), app: &App{P: Paths{Data: "/installer/installer-data"}}, actions: UIActions{
		DataAction: func(_ context.Context, a, p string) error { action, target = a, p; return nil },
		Inventory:  func(context.Context) (Inventory, error) { return Inventory{}, nil },
	}}
	m.app.P.Data = t.TempDir()
	m.filesMenu()
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if m.title != "备份与文件" {
		t.Fatal("refresh jumped home")
	}
	uiChooseTest(t, m, "清理日志、缓存")
	uiChooseTest(t, m, "清理无引用缓存")
	if m.screen != "confirm" || !strings.Contains(m.detail, filepath.Join(m.app.P.Data, "cache")) {
		t.Fatal("cleanup omitted precise target")
	}
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if action != "" {
		t.Fatal("cleanup bypassed second confirmation")
	}
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if action != "clean" || target != "cache" {
		t.Fatal("cleanup selected wrong action/target")
	}
}

func TestUIBackupLoadFailureCanReturnToFiles(t *testing.T) {
	calls := 0
	m := &uiModel{networkReady: true, app: &App{}, actions: UIActions{Backups: func() ([]Backup, error) {
		calls++
		return nil, fmt.Errorf("backup metadata damaged")
	}}}
	m.backupMenu()
	m.backupDetails(Backup{ID: "test-backup"})
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.screen != "result" || m.err == nil {
		t.Fatal("backup load error not reported")
	}
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.title != "备份与文件" || calls != 1 {
		t.Fatal("backup error trapped navigation in a retry loop")
	}
}

func TestUICancelledComponentDraftDoesNotLeakIntoNextAction(t *testing.T) {
	m := &uiModel{networkReady: true, app: &App{}}
	m.app.P.Data = t.TempDir()
	m.app.S.Installed = true
	m.components()
	uiChooseTest(t, m, "codexapp")
	uiChooseTest(t, m, "安装 / 复用")
	uiChooseTest(t, m, "单独安装一份")
	m.onInput(m, "0.1.90")
	m.onInput(m, "/password")
	m.onInput(m, "8214")
	uiChooseTest(t, m, "同时安装 Node")
	if !m.componentOptions.InstallNode {
		t.Fatal("dependency not in draft")
	}
	for n := 0; m.title != "组件 · codexapp"; n++ {
		if n > 5 {
			t.Fatal("cannot return to component actions")
		}
		uiKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	}
	m.app.S.Components = map[string]*Component{"codexapp": {Status: "available", Ownership: "reused"}}
	m.componentMenu()
	uiChooseTest(t, m, "修改配置")
	if m.componentOptions.InstallNode || m.componentOptions.Path != "" || m.componentOptions.Version != "" {
		t.Fatal("cancelled install options leaked into configure")
	}
	m.onInput(m, "/new-password")
	m.onInput(m, "8215")
	if !strings.Contains(m.title, "缺少 Node") {
		t.Fatal("cancelled authorization bypassed dependency selection")
	}
}
