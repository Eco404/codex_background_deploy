package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestUIOutputFollowsTailThroughCompletion(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%v", fail), func(t *testing.T) {
			m := &uiModel{app: &App{}, progress: &uiProgress{}, width: 48, height: 20}
			m.title = "迁移 · 后台保护与可选组件"
			m.scroll, m.manualScroll = 30, true // A scrolled confirmation must not leak into output.
			m.startBusy("开始执行")
			for i := 0; i < 60; i++ {
				fmt.Fprintf(m.progress, "阶段 %d %s\nLATEST-%02d\n", i, strings.Repeat("较长的输出路径/", 8), i)
				if i == 20 {
					m.Update(tea.WindowSizeMsg{Width: 32, Height: 16})
				} else if i == 40 {
					m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
				}
				view := m.View()
				if !strings.Contains(view, fmt.Sprintf("LATEST-%02d", i)) {
					t.Fatalf("new output hidden at step %d: %s", i, view)
				}
				if strings.Count(view, "\n") >= m.height {
					t.Fatal("progress exceeds terminal height")
				}
				for _, line := range strings.Split(view, "\n") {
					if ansi.StringWidth(line) > m.width {
						t.Fatal("progress exceeds terminal width")
					}
				}
				m.Update(uiTickMsg{})
			}
			result := uiResult{text: "迁移操作完成"}
			if fail {
				result.err = fmt.Errorf("测试错误：最后阶段失败")
			}
			m.Update(result)
			view := m.View()
			if !strings.Contains(view, "LATEST-59") {
				t.Fatalf("completion jumped away from latest output: %s", view)
			}
			if fail {
				if !strings.Contains(view, result.err.Error()) || strings.Contains(view, result.text) {
					t.Fatal("failure not visible or misreported as success")
				}
			} else if !strings.Contains(view, result.text) {
				t.Fatal("completion summary not visible")
			}
			m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
			if strings.Contains(m.View(), "LATEST-59") {
				t.Fatal("manual paging overridden by automatic tail focus")
			}
			m.Update(tea.KeyMsg{Type: tea.KeyEnd})
			if !strings.Contains(m.View(), "LATEST-59") {
				t.Fatal("End did not return to latest output")
			}
		})
	}
}

func TestUIDeploymentCompletionRefreshesHome(t *testing.T) {
	for _, migrate := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			for _, key := range []tea.KeyType{tea.KeyEnter, tea.KeyEsc} {
				t.Run(fmt.Sprintf("migration=%v/failure=%v/key=%v", migrate, fail, key), func(t *testing.T) {
					m := &uiModel{networkReady: true, ctx: context.Background(), app: &App{}}
					inventory := Inventory{Host: "before", Systemd: true, Extensions: []Extension{{Root: "/extensions"}}, Candidates: map[string][]Candidate{"codex": {{Path: "/codex", Compatible: true}}}}
					m.inventory = inventory
					refreshes := 0
					deploy := func(context.Context, InstallOptions) error {
						if fail {
							return fmt.Errorf("deployment failed")
						}
						m.app.S.Installed, m.app.S.Backend = true, "systemd"
						return nil
					}
					m.actions = UIActions{Install: deploy, Migrate: deploy, Inventory: func(context.Context) (Inventory, error) {
						refreshes++
						inventory.Host = "after-refresh"
						return inventory, nil
					}}
					m.installWizard(migrate)
					uiChooseTest(t, m, "[ ] /extensions")
					uiChooseTest(t, m, "下一步")
					uiChooseTest(t, m, "复用")
					uiChooseTest(t, m, "systemd")
					uiChooseTest(t, m, "预览并确认")
					uiKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
					if m.screen != "result" || refreshes != 0 {
						t.Fatal("completion skipped its result page")
					}
					if !fail && !strings.Contains(m.View(), "刷新并返回主界面") {
						t.Fatal("completion footer does not describe destination")
					}
					uiKey(t, m, tea.KeyMsg{Type: key})
					if refreshes != 1 || m.inventory.Host != "after-refresh" {
						t.Fatal("continue did not refresh exactly once")
					}
					if fail {
						if m.title != "部署管理" {
							t.Fatal("failure lost its retry route")
						}
					} else if !strings.HasPrefix(m.title, "Codex Background") || !strings.Contains(m.View(), "已安装 · systemd") {
						t.Fatalf("success returned to a stale deployment page: %s", m.View())
					}
				})
			}
		}
	}
}
