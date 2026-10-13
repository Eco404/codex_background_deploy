package app

import (
	"fmt"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
)

func (m *uiModel) codeServerPath() string {
	for _, c := range m.inventory.Candidates["code-server"] {
		if c.Compatible {
			return c.Path
		}
	}
	return ""
}

func (m *uiModel) extensionWizard(forInstall bool) tea.Cmd {
	m.extensionForInstall = forInstall
	m.extensionDraft = ExtensionInstallOptions{CodeServer: m.codeServerPath(), Yes: true}
	parent := uiShow((*uiModel).deploymentMenu)
	if forInstall {
		parent = uiShow((*uiModel).rootMenu)
	}
	m.menu("code-server · 添加 Codex 扩展", parent,
		m.item("从官方 Marketplace 下载", func(m *uiModel) tea.Cmd { m.extensionDraft.Source = "marketplace"; return m.extensionTarget() }),
		m.item("使用本地 VSIX 或已安装扩展目录", func(m *uiModel) tea.Cmd {
			return m.ask("Codex VSIX 或原扩展目录绝对路径", func(m *uiModel, s string) tea.Cmd {
				if !filepath.IsAbs(s) {
					m.err = fmt.Errorf("请输入绝对路径")
					return nil
				}
				m.extensionDraft.Source = s
				return m.extensionTarget()
			})
		}),
	)
	m.note = "仅安装 openai.chatgpt 到指定 code-server 扩展目录；不改变插件库。已有扩展由 IDE 更新，若版本不兼容会停止并报告。"
	return nil
}

func (m *uiModel) extensionTarget() tea.Cmd {
	m.ask("code-server 扩展根目录（预填常用路径；Ctrl+U 清空）", func(m *uiModel, s string) tea.Cmd {
		if !filepath.IsAbs(s) {
			m.err = fmt.Errorf("请输入扩展根目录绝对路径")
			return nil
		}
		m.extensionDraft.Root = s
		if m.extensionForInstall {
			opt := m.extensionDraft
			m.options.Extension = &opt
			if !hasString(m.roots, s) {
				m.roots = append(m.roots, s)
			}
			m.selected[s] = true
			m.rootMenu()
			return nil
		}
		opt := m.extensionDraft
		return m.withNetwork(func(m *uiModel) tea.Cmd {
			return m.confirm("安装 Codex 扩展\n来源："+opt.Source+"\n目标："+opt.Root+"\n"+m.app.Network.Summary(), false, func() tea.Msg {
				if m.actions.Extension == nil {
					return unavailable()
				}
				return uiResult{err: m.actions.Extension(m.ctx, opt), text: "Codex 扩展安装完成。重新加载 code-server 窗口；后台接入只作用于明确选择的扩展根目录。"}
			})
		})
	})
	m.input = filepath.Join(m.app.P.Home, ".local/share/code-server/extensions")
	return nil
}
