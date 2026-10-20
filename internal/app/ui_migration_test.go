package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestUIMigrationDependencyIsExplicitAndPreflightPrecedesConfirmation(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "busy"}[blocked], func(t *testing.T) {
			ctx := context.Background()
			m := &uiModel{networkReady: true, ctx: ctx, parentCtx: ctx, app: engineApp(t)}
			m.inventory = Inventory{Container: true, Legacy: []string{"CodexApp dependency: /old/10-start-codexapp.sh"}}
			m.installWizard(true)
			m.options.Backend = "container"
			m.options.Roots = []string{"/extensions"}
			m.installComponents()
			if m.options.MigrateCodexApp {
				t.Fatal("existing service migration was selected implicitly")
			}
			for _, item := range m.items {
				if item.label == "[ ] codexapp" && item.disabled == "" {
					t.Fatal("ordinary install exposed alongside dependent service migration")
				}
			}
			uiChooseTest(t, m, "[ ] 迁移现有 CodexApp")
			if !m.options.MigrateCodexApp || hasString(m.options.Components, "codexapp") {
				t.Fatal("migration choice did not stay separate from ordinary install")
			}
			previews, changes := 0, 0
			m.actions.PreviewMigration = func(ctx context.Context, opt InstallOptions) (string, error) {
				previews++
				if ctx.Err() != nil || !opt.MigrateCodexApp {
					t.Fatal("preview lost context or selected handoff")
				}
				if blocked {
					return "", errors.New("旧后台存在活跃任务")
				}
				return "旧后台当前空闲；所有依赖已检查", nil
			}
			m.actions.Migrate = func(ctx context.Context, opt InstallOptions) error {
				if ctx.Err() != nil || !opt.Yes || !opt.MigrateCodexApp {
					t.Fatal("execution inherited cancelled preview context or lost confirmation")
				}
				changes++
				return nil
			}
			cmd := uiChooseTest(t, m, "预览并确认")
			if !m.busy || m.screen == "confirm" || previews != 0 || changes != 0 {
				t.Fatal("preflight did not run asynchronously before confirmation")
			}
			uiDrain(t, m, cmd)
			if previews != 1 || changes != 0 {
				t.Fatal("preview performed migration")
			}
			if blocked {
				if m.screen != "result" || m.err == nil || m.pending != nil {
					t.Fatal("failed preflight offered execution")
				}
				uiKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
				if m.screen != "menu" || !m.options.MigrateCodexApp || !strings.Contains(m.title, "可选组件") {
					t.Fatal("failed preview lost the user's choices")
				}
				return
			}
			if m.screen != "confirm" || !strings.Contains(m.detail, "所有依赖已检查") {
				t.Fatal("full preview absent from confirmation")
			}
			uiKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
			if m.screen != "menu" || !m.options.MigrateCodexApp {
				t.Fatal("preview cancellation lost migration choice")
			}
			uiDrain(t, m, uiChooseTest(t, m, "预览并确认"))
			uiKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
			if changes != 0 {
				t.Fatal("Enter implicitly confirmed migration")
			}
			uiKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
			if changes != 1 {
				t.Fatal("confirmed migration did not run")
			}
		})
	}
}
