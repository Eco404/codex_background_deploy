package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func discoveryFixture(t *testing.T, file, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(contents), 0700); err != nil {
		t.Fatal(err)
	}
}
func TestDiscoverReadOnlyCandidatesAndPrivacy(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	t.Setenv("PATH", bin)
	t.Setenv("NVM_DIR", filepath.Join(home, ".nvm"))
	codex := filepath.Join(bin, "codex")
	node := filepath.Join(home, ".nvm/versions/node/v22.5.0/bin/node")
	wrapper := filepath.Join(home, ".local/bin/codex")
	discoveryFixture(t, codex, "standalone")
	discoveryFixture(t, node, "node")
	discoveryFixture(t, wrapper, "#!/bin/bash\n# "+Marker)
	entry := filepath.Join(home, ".vscode-server/extensions/openai.chatgpt-1.2.3/bin/linux-x64/codex")
	discoveryFixture(t, entry, "extension binary")
	settings := filepath.Join(home, ".vscode-server/data/Machine/settings.json")
	discoveryFixture(t, settings, "{\"auth\":\"TOP-SECRET\", // untouched\n\"chatgpt.cliExecutable\":\"/old/wrapper\",}")
	calls := []string{}
	app := &App{P: Paths{Home: home, Runtime: filepath.Join(home, "new-runtime")}, RunCommand: func(ctx context.Context, name string, args, env []string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if _, ok := ctx.Deadline(); !ok {
			t.Error("discovery command has no timeout")
		}
		if name == wrapper {
			t.Error("managed wrapper executed")
		}
		if name == "systemctl" {
			return nil, nil
		}
		if name == node {
			return []byte("v22.5.0"), nil
		}
		if strings.Join(args, " ") == "app-server --help" {
			return []byte("--listen unix://PATH"), nil
		}
		return []byte("codex-cli 0.123.0"), nil
	}}
	inv, err := app.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Systemd {
		t.Fatal("systemd capability not detected")
	}
	if len(inv.Extensions) != 1 || inv.Extensions[0].Entry != entry {
		t.Fatalf("wrong extensions: %+v", inv.Extensions)
	}
	if len(inv.Candidates["node"]) != 1 || !inv.Candidates["node"][0].Compatible {
		t.Fatalf("Node candidate missing: %+v", inv.Candidates)
	}
	foundWrapper := false
	for _, c := range inv.Candidates["codex"] {
		if c.Path == wrapper {
			foundWrapper = true
			if c.Compatible {
				t.Error("wrapper eligible for reuse")
			}
		}
	}
	if !foundWrapper {
		t.Fatal("wrapper rejection not reported")
	}
	legacy := strings.Join(inv.Legacy, "\n")
	if strings.Contains(legacy, "TOP-SECRET") {
		t.Fatal("unrelated secret leaked")
	}
	if !strings.Contains(legacy, "/old/wrapper") {
		t.Fatal("target setting not discovered")
	}
	if _, err := os.Stat(app.P.Runtime); !os.IsNotExist(err) {
		t.Fatal("discovery created install record")
	}
	for _, call := range calls {
		if strings.Contains(call, " start") || strings.Contains(call, " install") {
			t.Fatalf("mutation invoked: %s", call)
		}
	}
}
func TestDiscoverySettingJSONC(t *testing.T) {
	cases := []struct {
		input, want string
		ok          bool
	}{
		{`{"password":"secret","nested":{"chatgpt.cliExecutable":"bad"},"chatgpt.cliExecutable":"https://example/a,}?x=/*ok*/",}`, "https://example/a,}?x=/*ok*/", true},
		{"{ // comment\n\"chatgpt.cliExecutable\":\"/path\" /* comment */ ,}", "/path", true},
		{`{"nested":{"chatgpt.cliExecutable":"bad"}}`, "", false},
		{`{"chatgpt.cliExecutable":null}`, "", false},
		{`{"secret":"dont print",broken}`, "", false},
	}
	for _, c := range cases {
		got, ok := discoverySetting(c.input)
		if got != c.want || ok != c.ok {
			t.Fatalf("got %q,%v want %q,%v", got, ok, c.want, c.ok)
		}
	}
}
func TestDiscoverCancelledDoesNotRunCandidates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", filepath.Join(home, "bin"))
	discoveryFixture(t, filepath.Join(home, "bin/codex"), "binary")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app := &App{P: Paths{Home: home}, RunCommand: func(ctx context.Context, name string, args, env []string) ([]byte, error) { return nil, ctx.Err() }}
	_, err := app.Discover(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
func TestUIRequiresExplicitRootsAndConfirmation(t *testing.T) {
	called := 0
	m := &uiModel{ctx: context.Background(), app: &App{}, selected: map[string]bool{}, actions: UIActions{Install: func(context.Context, InstallOptions) error { called++; return nil }}}
	m.inventory.Extensions = []Extension{{Root: "/extensions"}}
	m.installWizard(false)
	if m.selected["/extensions"] {
		t.Fatal("extension root auto selected")
	}
	m.items[len(m.items)-1].action(m)
	if m.err == nil {
		t.Fatal("zero roots allowed")
	}
	m.confirm("install", false, func() tea.Msg { called++; return uiResult{} })
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || called != 0 {
		t.Fatal("Enter performed unconfirmed mutation")
	}
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd == nil || called != 0 {
		t.Fatal("mutation not scheduled asynchronously")
	}
	cmd()
	if called != 1 {
		t.Fatal("confirmed operation not invoked")
	}
}
func TestUIStrongDeleteRequiresTwoConfirmations(t *testing.T) {
	called := 0
	m := &uiModel{ctx: context.Background(), app: &App{}}
	m.confirm("delete", true, func() tea.Msg { called++; return uiResult{} })
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd != nil || called != 0 {
		t.Fatal("first confirmation deleted")
	}
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd == nil {
		t.Fatal("second confirmation not accepted")
	}
	cmd()
	if called != 1 {
		t.Fatal("operation not run")
	}
}

func TestUIOptionalComponentsStayDisabledAndCollectOptionsBeforeInstall(t *testing.T) {
	m := &uiModel{ctx: context.Background(), app: &App{}}
	m.installWizard(false)
	if len(m.options.Components) != 0 {
		t.Fatal("optional component enabled by default")
	}
	m.options.Backend = "systemd"
	m.installComponents()
	for _, item := range m.items {
		if item.label == "[ ] codexapp" {
			item.action(m)
			break
		}
	}
	if !m.componentForInstall || m.component != "codexapp" {
		t.Fatal("component options wizard not opened")
	}
	for _, item := range m.items {
		if item.label == "安装受管版本" {
			item.action(m)
			break
		}
	}
	m.onInput(m, "0.1.90")
	m.onInput(m, "/private/password.txt")
	m.onInput(m, "8214")
	uiChooseTest(t, m, "将受管 Node 依赖加入计划")
	opt, ok := m.options.ComponentOptions["codexapp"]
	if !ok || opt.Version != "0.1.90" || opt.PasswordFile != "/private/password.txt" || opt.Port != "8214" {
		t.Fatalf("lost component options: %+v", opt)
	}
	if m.screen != "menu" || m.pending != nil {
		t.Fatal("component was executed before installation preview")
	}
}

func TestUICodexAppNodeDependencyRequiresFinalConfirmation(t *testing.T) {
	called := false
	var received ComponentOptions
	m := &uiModel{ctx: context.Background(), app: &App{}, component: "codexapp", componentAction: "install", componentOptions: ComponentOptions{Yes: true, Path: "managed"}, actions: UIActions{Component: func(_ context.Context, _, _ string, opt ComponentOptions) error {
		called = true
		received = opt
		return nil
	}}}
	m.componentConfirm()
	if !strings.Contains(m.title, "缺少 Node >=20") {
		t.Fatal("missing prerequisite page")
	}
	cmd := uiChooseTest(t, m, "将受管 Node 依赖加入计划")
	if cmd != nil || called {
		t.Fatal("dependency selection executed mutation")
	}
	if m.screen != "confirm" || !strings.Contains(m.detail, "安装受管 Node 依赖：true") {
		t.Fatal("preview omitted dependency")
	}
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || called {
		t.Fatal("Enter bypassed explicit confirmation")
	}
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd == nil || called {
		t.Fatal("confirmed mutation not asynchronous")
	}
	cmd()
	if !called || !received.InstallNode {
		t.Fatal("dependency authorization lost")
	}
}
func TestUICodexAppDependencyDetectsNpmAndAvoidsDuplicatePlannedNode(t *testing.T) {
	dir := t.TempDir()
	node := filepath.Join(dir, "node")
	m := &uiModel{inventory: Inventory{Candidates: map[string][]Candidate{"node": {{Path: node, Version: "v22.1.0", Compatible: true}}}}}
	if m.codexAppNodeReady() {
		t.Fatal("Node without adjacent npm accepted")
	}
	discoveryFixture(t, filepath.Join(dir, "npm"), "npm")
	if !m.codexAppNodeReady() {
		t.Fatal("compatible Node and npm not accepted")
	}
	m.inventory.Candidates = nil
	m.componentForInstall = true
	m.options = InstallOptions{Components: []string{"node"}, ComponentOptions: map[string]ComponentOptions{"node": {Path: "managed"}}}
	if !m.codexAppNodeReady() {
		t.Fatal("already planned managed Node duplicated")
	}
}

func TestUIShellIntegrationDefaultsOnAndCanBeDisabled(t *testing.T) {
	m := &uiModel{ctx: context.Background(), app: &App{}}
	m.installWizard(false)
	m.options.Backend = "systemd"
	m.installComponents()
	for _, item := range m.items {
		if item.label == "[ ] node" {
			item.action(m)
			break
		}
	}
	if !m.componentOptions.ShellIntegration {
		t.Fatal("shell integration did not default on")
	}
	m.componentOptions.Path = "/existing/node"
	m.componentConfirm()
	if m.title != "组件 · shell 集成" {
		t.Fatal("integration toggle not presented")
	}
	m.items[0].action(m)
	if m.componentOptions.ShellIntegration {
		t.Fatal("toggle not applied")
	}
	m.items[1].action(m)
	if m.options.ComponentOptions["node"].ShellIntegration {
		t.Fatal("disabled integration lost in install plan")
	}
}

func uiChooseTest(t *testing.T, m *uiModel, label string) tea.Cmd {
	t.Helper()
	for _, item := range m.items {
		if strings.HasPrefix(item.label, label) {
			return item.action(m)
		}
	}
	t.Fatalf("menu item missing: %s", label)
	return nil
}
func TestUICoreActionsDefaultToSafeWithoutForce(t *testing.T) {
	for _, action := range []string{"update", "rollback", "uninstall", "stop", "restart"} {
		t.Run(action, func(t *testing.T) {
			called := false
			forced := true
			invoke := func(force bool) error { called = true; forced = force; return nil }
			m := &uiModel{ctx: context.Background(), app: &App{}, actions: UIActions{
				Update: func(_ context.Context, _ string, f bool) error { return invoke(f) }, Rollback: func(_ context.Context, _ string, f bool) error { return invoke(f) }, Uninstall: func(_ context.Context, f bool) error { return invoke(f) }, Service: func(_ context.Context, _ string, f bool) error { return invoke(f) },
			}}
			m.home()
			m.app.S.Installed = true
			uiChooseTest(t, m, "部署管理")
			switch action {
			case "update":
				uiChooseTest(t, m, "更新核心")
				m.onInput(m, "")
			case "rollback":
				uiChooseTest(t, m, "回滚核心")
				m.onInput(m, "abcdef")
			case "uninstall":
				uiChooseTest(t, m, "卸载核心")
			default:
				m.serviceMenu()
				uiChooseTest(t, m, map[string]string{"stop": "停止", "restart": "重启"}[action])
			}
			if m.cursor != 0 || !strings.HasPrefix(m.items[0].label, "安全执行") {
				t.Fatal("safe action is not default")
			}
			m.items[0].action(m)
			if called {
				t.Fatal("action ran before confirmation")
			}
			_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
			if cmd == nil {
				t.Fatal("confirmed action missing")
			}
			cmd()
			if !called || forced {
				t.Fatal("ordinary confirmation bypassed busy protection")
			}
		})
	}
}
func TestUIRepairUsesRepairCallbackAndManagedSelectionIsExplicit(t *testing.T) {
	called := false
	m := &uiModel{ctx: context.Background(), app: &App{}, actions: UIActions{Repair: func(context.Context) error { called = true; return nil }}}
	m.home()
	uiChooseTest(t, m, "运行与维护")
	uiChooseTest(t, m, "修复受管扩展入口")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd == nil {
		t.Fatal("repair command missing")
	}
	cmd()
	if !called {
		t.Fatal("repair callback not invoked")
	}
	m.busy = false
	m.component = "node"
	m.componentAction = "install"
	m.componentCandidates()
	uiChooseTest(t, m, "安装受管版本")
	m.onInput(m, "")
	if m.componentOptions.Path != "managed" {
		t.Fatal("managed install can fall back to implicit reuse")
	}
}
