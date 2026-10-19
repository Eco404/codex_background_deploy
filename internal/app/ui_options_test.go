package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func uiFind(t *testing.T, m *uiModel, label string) uiItem {
	t.Helper()
	for _, item := range m.items {
		if strings.HasPrefix(item.label, label) {
			return item
		}
	}
	t.Fatalf("missing item %s in %s", label, m.title)
	return uiItem{}
}

func TestUIMarketplaceConfirmationHasNoDownloadOrComponentInputs(t *testing.T) {
	called := false
	m := &uiModel{
		ctx: context.Background(), app: &App{}, component: "marketplace", componentAction: "install",
		actions: UIActions{Component: func(_ context.Context, action, name string, opts ComponentOptions) error {
			called = true
			if action != "install" || name != "marketplace" || opts.Path != "" || opts.Version != "" {
				t.Fatalf("unexpected marketplace operation: %s %s %+v", action, name, opts)
			}
			return nil
		}},
	}
	m.componentCandidates()
	if called || m.screen != "confirm" || m.pending == nil || m.networkReady {
		t.Fatalf("marketplace should directly preview a local operation before confirmation: screen=%s called=%v network=%v", m.screen, called, m.networkReady)
	}
	if !strings.Contains(m.detail, "重启 code-server") || !strings.Contains(m.detail, "备份") {
		t.Fatal("marketplace preview omitted restart or recovery information")
	}
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if called {
		t.Fatal("Enter must not apply marketplace change")
	}
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if !called {
		t.Fatal("explicit confirmation did not apply marketplace change")
	}
}

func TestUIUnavailableCoreActionsCannotBeSelected(t *testing.T) {
	called := false
	m := &uiModel{app: &App{}, actions: UIActions{Repair: func(context.Context) error { called = true; return nil }}}
	m.maintenanceMenu()
	for _, label := range []string{"后台服务", "修复受管扩展入口"} {
		item := uiFind(t, m, label)
		if item.disabled == "" {
			t.Fatalf("uninstalled action enabled: %s", label)
		}
		if item.action(m) != nil || m.screen != "menu" || called {
			t.Fatal("disabled operation started")
		}
	}
	if uiFind(t, m, "只读诊断").disabled != "" {
		t.Fatal("diagnosis should work before install")
	}
}

func TestUIServiceActionsFollowDetectedState(t *testing.T) {
	for _, state := range []string{"running", "stopped", "unknown"} {
		t.Run(state, func(t *testing.T) {
			m := &uiModel{app: &App{}, inventory: Inventory{ServiceState: state}}
			m.app.S.Installed = true
			m.serviceMenu()
			if (uiFind(t, m, "启动").disabled != "") != (state == "running") {
				t.Fatal("start availability incorrect")
			}
			for _, label := range []string{"停止", "重启"} {
				if (uiFind(t, m, label).disabled != "") != (state == "stopped") {
					t.Fatal("stop availability incorrect")
				}
			}
		})
	}
}

func TestUIComponentsRespectRegistrationDependenciesAndCapabilities(t *testing.T) {
	m := &uiModel{app: &App{P: Paths{Data: t.TempDir()}}, component: "codexapp"}
	m.componentMenu()
	if uiFind(t, m, "安装 / 复用").disabled == "" {
		t.Fatal("CodexApp install before core enabled")
	}
	m.app.S.Installed = true
	m.app.S.Components = map[string]*Component{"node": {Status: "installed", Ownership: "managed", Path: "/node"}, "codexapp": {Status: "available", Options: map[string]string{"node": "/node/bin/node"}}}
	m.component = "node"
	m.componentMenu()
	if len(m.items) != 2 || uiFind(t, m, "卸载").disabled == "" {
		t.Fatal("registered Node should validate and protect CodexApp reference")
	}
	m.component = "codexapp"
	m.componentMenu()
	if len(m.items) != 2 || uiFind(t, m, "修改配置").disabled != "" {
		t.Fatal("registered available app should configure or remove")
	}
	m.app.S.Components["codexapp"].Status = "failed"
	m.componentMenu()
	if len(m.items) != 1 || !strings.HasPrefix(m.items[0].label, "卸载") {
		t.Fatal("failed app must not show no-op install/invalid repair")
	}
	m.inventory = Inventory{Container: true, Apt: true}
	if hasString(m.componentNames(), "apti") || hasString(m.componentNames(), "branding") {
		t.Fatal("incapable addons offered")
	}
	m.inventory.Privileged = true
	m.inventory.MediaDir = t.TempDir()
	if !hasString(m.componentNames(), "apti") || !hasString(m.componentNames(), "branding") {
		t.Fatal("available addons missing")
	}
	m.app.S.Components["branding"] = &Component{Status: "installed"}
	m.inventory = Inventory{}
	if !hasString(m.componentNames(), "branding") {
		t.Fatal("registered addon maintenance disappeared")
	}
}

func TestUIPendingTransactionAndMissingDataDisableMutations(t *testing.T) {
	m := &uiModel{app: &App{P: Paths{Data: filepath.Join(t.TempDir(), "missing")}}}
	m.app.S.Installed = true
	m.deploymentMenu()
	if uiFind(t, m, "更新核心").disabled == "" || uiFind(t, m, "回滚核心").disabled == "" {
		t.Fatal("unavailable data enabled mutation")
	}
	discoveryFixture(t, filepath.Join(m.app.P.Data, "transactions/active.json"), `{}`)
	m.maintenanceMenu()
	if uiFind(t, m, "修复受管").disabled == "" || uiFind(t, m, "恢复未完成事务").disabled != "" {
		t.Fatal("pending transaction not reflected")
	}
}

func TestUIBackupAvailabilityReflectsRecoveryState(t *testing.T) {
	m := &uiModel{app: &App{}}
	b := Backup{ID: "abc", Deleted: true}
	m.backupDetails(b)
	for _, item := range m.items {
		if item.disabled == "" {
			t.Fatal("deleted backup exposes action")
		}
	}
	b.Deleted = false
	b.Ref = "binding"
	b.DataPath = filepath.Join(t.TempDir(), "content")
	discoveryFixture(t, b.DataPath, "original")
	m.backupDetails(b)
	if uiFind(t, m, "删除").disabled == "" || uiFind(t, m, "强制删除").disabled != "" {
		t.Fatal("referenced backup deletion not distinguished")
	}
}

func TestUINetworkChoicePrecedesInstallAndCanBeEdited(t *testing.T) {
	called := false
	m := &uiModel{ctx: context.Background(), app: &App{}, actions: UIActions{Install: func(context.Context, InstallOptions) error { called = true; return nil }}}
	m.installWizard(false)
	m.installComponents()
	uiChooseTest(t, m, "预览并确认")
	if !strings.Contains(m.title, "镜像源") || m.pending != nil {
		t.Fatal("network not selected before preview")
	}
	uiChooseTest(t, m, "使用国内镜像")
	uiChooseTest(t, m, "npm（")
	if m.input != "https://repo.huaweicloud.com/repository/npm" {
		t.Fatal("no prefilled mirror")
	}
	m.onInput(m, "https://mirror.example/npm")
	uiChooseTest(t, m, "下一步")
	uiChooseTest(t, m, "使用代理：")
	m.onInput(m, "http://user:private-password@127.0.0.1:7890")
	if m.screen != "confirm" || called || !strings.Contains(m.detail, "mirror.example") || strings.Contains(m.detail, "private-password") {
		t.Fatal("network preview/confirmation unsafe")
	}
	uiKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if !strings.Contains(m.title, "可选组件") {
		t.Fatal("cancel preview lost installation selections")
	}
	if m.app.Network.ProxyMode != "custom" || !m.app.Network.UseMirrors {
		t.Fatal("network choice lost")
	}
}

func TestUIBlankCodexAppPasswordAcceptedWithoutMutation(t *testing.T) {
	m := &uiModel{app: &App{}, component: "codexapp", componentAction: "install", networkReady: true}
	m.codexAppOptions()
	if cmd := m.onInput(m, ""); cmd != nil || m.componentOptions.PasswordFile != "" || !m.componentOptions.Configure || !strings.Contains(m.inputLabel, "端口") {
		t.Fatal("blank password did not use default")
	}
	m.onInput(m, "")
	uiChooseTest(t, m, "将受管 Node 依赖加入计划")
	if !strings.Contains(m.detail, "密码文件：") || m.screen != "confirm" {
		t.Fatal("default credential path missing from preview")
	}
}

func TestUIProgressIsBoundedAndFitsNarrowTerminal(t *testing.T) {
	p := &uiProgress{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				fmt.Fprintln(p, "下载中 "+strings.Repeat("路径", 50))
				_ = p.snapshot()
			}
		}()
	}
	wg.Wait()
	if len(p.snapshot()) > 32*1024 {
		t.Fatal("progress memory unbounded")
	}
	m := &uiModel{app: &App{}, progress: p, width: 48, height: 20, busy: true, started: time.Now()}
	m.title = "安装 · 可选组件"
	view := m.View()
	if !strings.Contains(view, "已用时") || !strings.Contains(view, "下载中") {
		t.Fatal("no live progress")
	}
	for _, line := range strings.Split(view, "\n") {
		if ansi.StringWidth(line) > 48 {
			t.Fatalf("overflow: %q", line)
		}
	}
	if strings.Count(view, "\n") >= 20 {
		t.Fatal("progress exceeds terminal height")
	}
	if strings.Contains(uiPlain("abc\x1b[2J\r\x07"), "\x1b") {
		t.Fatal("terminal escape leaked")
	}
}

func TestUIBusyCancellationWaitsForWorker(t *testing.T) {
	m := &uiModel{app: &App{}, parentCtx: context.Background()}
	m.startBusy("work")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd != nil || !m.busy || !m.cancelling || m.ctx.Err() == nil {
		t.Fatal("Ctrl+C exited before rollback")
	}
	m.Update(uiResult{err: context.Canceled})
	if m.busy || m.screen != "result" {
		t.Fatal("worker completion not shown")
	}
}

func TestUIRegisteredNodeOverridesOtherDetectedCandidates(t *testing.T) {
	bin := t.TempDir()
	discoveryFixture(t, filepath.Join(bin, "npm"), "npm")
	m := &uiModel{app: &App{}, inventory: Inventory{Candidates: map[string][]Candidate{"node": {{Path: filepath.Join(bin, "node"), Version: "v22.1.0", Compatible: true}}}}, component: "codexapp", componentAction: "install"}
	m.app.S.Components = map[string]*Component{"node": {Path: "/old/node", Ownership: "reused", Version: "v18.1.0", Status: "installed"}}
	if m.codexAppNodeReady() {
		t.Fatal("other detected Node bypassed registered incompatible runtime")
	}
	m.componentConfirm()
	if len(m.items) != 1 || !strings.Contains(m.title, "已登记") {
		t.Fatal("no-op dependency installation still offered")
	}
	delete(m.app.S.Components, "node")
	m.componentForInstall = true
	m.options = InstallOptions{Components: []string{"node", "codexapp"}, ComponentOptions: map[string]ComponentOptions{"node": {Path: "managed", Version: "18.0.0"}}}
	if m.codexAppNodeReady() {
		t.Fatal("other detected Node bypassed planned incompatible runtime")
	}
	m.componentConfirm()
	uiChooseTest(t, m, "将受管 Node 依赖")
	if hasString(m.options.Components, "node") || !m.options.ComponentOptions["codexapp"].InstallNode {
		t.Fatal("incompatible plan not replaced")
	}
}

func TestUILayoutBoundsAndManualPaging(t *testing.T) {
	for _, width := range []int{32, 48, 80, 120} {
		m := &uiModel{app: &App{}, width: width, height: 20}
		m.menu("一个需要换行的安装菜单标题", nil, m.item("最后的选项", func(*uiModel) tea.Cmd { return nil }))
		m.note = strings.Repeat("很长的预览路径和说明\n", 30)
		view := m.View()
		if strings.Count(view, "\n") >= m.height {
			t.Fatalf("menu overflows %d columns", width)
		}
		previous := m.scroll
		m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
		m.View()
		if m.scroll >= previous {
			t.Fatal("PgUp undone by focused item")
		}
		m.busy = true
		m.progress = &uiProgress{text: strings.Repeat("download\n", 100)}
		if strings.Count(m.View(), "\n") >= m.height {
			t.Fatalf("progress overflows %d columns", width)
		}
	}
}

func TestUIParentCancellationWaitsForResultBeforeQuit(t *testing.T) {
	m := &uiModel{app: &App{}, parentCtx: context.Background()}
	m.startBusy("operation")
	_, cmd := m.Update(uiCancelMsg{})
	if cmd != nil || !m.quitAfterResult || m.ctx.Err() == nil {
		t.Fatal("parent cancellation quit before cleanup")
	}
	_, cmd = m.Update(uiResult{err: context.Canceled})
	if cmd == nil {
		t.Fatal("completion did not release cancelled UI")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("expected quit after worker completion")
	}
}

func TestUIFailureDoesNotDisplaySuccessText(t *testing.T) {
	m := &uiModel{app: &App{}, progress: &uiProgress{text: "正在下载\n"}}
	m.Update(uiResult{err: fmt.Errorf("download failed"), text: "安装操作完成"})
	if strings.Contains(m.View(), "安装操作完成") || !strings.Contains(m.View(), "正在下载") {
		t.Fatal("failure presentation misleading or missing progress")
	}
}

// Launched only by the PTY smoke runner; never executes actual deployment.
func TestUIProgressPTYHelper(t *testing.T) {
	root := os.Getenv("CODEX_BG_UI_TEST_ROOT")
	if root == "" {
		t.Skip("PTY helper")
	}
	a := &App{P: Paths{Home: root, Runtime: filepath.Join(root, "runtime"), Data: filepath.Join(root, "installer-data")}}
	if err := os.MkdirAll(filepath.Join(a.P.Data, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	actions := UIActions{
		Inventory: func(context.Context) (Inventory, error) {
			return Inventory{Systemd: true, Extensions: []Extension{{Root: "/fixture/extensions"}}, Candidates: map[string][]Candidate{"codex": {{Path: "/fixture/codex", Compatible: true}}}}, nil
		},
		Install: func(ctx context.Context, opt InstallOptions) error {
			defer func() { _ = os.WriteFile(filepath.Join(root, "worker-finished"), []byte("finished"), 0600) }()
			for i := 1; i <= 4; i++ {
				fmt.Fprintf(a.Out, "步骤 %d/4 · 验证安装输出 %s\n", i, strings.Repeat("长路径/", 10))
				select {
				case <-ctx.Done():
					time.Sleep(100 * time.Millisecond)
					fmt.Fprintln(a.Out, "取消清理已完成")
					return ctx.Err()
				case <-time.After(300 * time.Millisecond):
				}
			}
			return nil
		},
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	if err := RunUI(ctx, a, actions); err != nil {
		t.Fatal(err)
	}
}
