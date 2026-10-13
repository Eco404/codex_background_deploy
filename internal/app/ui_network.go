package app

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func (m *uiModel) withNetwork(next uiRoute) tea.Cmd {
	if m.networkReady {
		return next(m)
	}
	back := m.returnHere()
	return m.networkWizard(func(m *uiModel) tea.Cmd { back(m); return next(m) }, back)
}

func (m *uiModel) networkWizard(next, back uiRoute) tea.Cmd {
	m.networkNext, m.networkBack = next, back
	m.networkDraft = m.app.Network
	if m.networkDraft.NPMRegistry == "" {
		m.networkDraft = DefaultNetworkOptions()
	}
	m.networkMenu()
	return nil
}

func (m *uiModel) networkMenu() {
	m.menu("下载设置 · 镜像源", m.networkBack,
		m.item("使用官方源", func(m *uiModel) tea.Cmd { m.networkDraft.UseMirrors = false; m.proxyMenu(); return nil }),
		m.item("使用国内镜像（可修改预填地址）", func(m *uiModel) tea.Cmd { m.networkDraft.UseMirrors = true; m.mirrorMenu(); return nil }),
	)
	m.note = "仅用于本工具本次安装和下载；不会改写系统、npm 或 Conda 的全局配置。"
}

func (m *uiModel) mirrorMenu() {
	items := []uiItem{}
	for _, source := range []struct {
		label string
		value *string
	}{
		{"npm（Codex / CodexApp）", &m.networkDraft.NPMRegistry},
		{"Node 安装包", &m.networkDraft.NodeMirror},
		{"Miniconda 安装包", &m.networkDraft.CondaMirror},
	} {
		label, value := source.label, source.value
		items = append(items, m.item(label+"："+*value, func(m *uiModel) tea.Cmd {
			m.ask(label+" URL（已预填，Ctrl+U 清空）", func(m *uiModel, s string) tea.Cmd {
				previous := *value
				*value = s
				validation := m.networkDraft
				validation.ProxyMode = "inherit"
				if err := validation.Validate(); err != nil {
					*value = previous
					m.err = err
					return nil
				}
				m.networkDraft = m.networkDraft.normalized()
				m.mirrorMenu()
				return nil
			})
			m.input = *value
			return nil
		}))
	}
	items = append(items, m.item("下一步：代理", func(m *uiModel) tea.Cmd { m.proxyMenu(); return nil }))
	m.menu("下载设置 · 国内镜像地址", uiShow((*uiModel).networkMenu), items...)
	m.note = "预填 npm / Node 为华为云，Miniconda 为中科大镜像。可按网络环境修改。Codex 扩展通过官方 Marketplace 下载，可单独使用代理。"
}

func (m *uiModel) proxyMenu() {
	back := uiShow((*uiModel).networkMenu)
	if m.networkDraft.UseMirrors {
		back = uiShow((*uiModel).mirrorMenu)
	}
	m.menu("下载设置 · 代理", back,
		m.item("不使用代理（忽略当前代理环境变量）", func(m *uiModel) tea.Cmd {
			m.networkDraft.ProxyMode = "none"
			m.networkDraft.Proxy = ""
			return m.finishNetwork()
		}),
		m.item("使用代理：输入地址", func(m *uiModel) tea.Cmd {
			m.ask("代理地址（http://、https:// 或 socks5://；Ctrl+U 清空）", func(m *uiModel, s string) tea.Cmd {
				if strings.TrimSpace(s) == "" {
					m.err = fmt.Errorf("请输入代理地址，或 Esc 返回选择不使用代理")
					return nil
				}
				m.networkDraft.ProxyMode = "custom"
				m.networkDraft.Proxy = s
				return m.finishNetwork()
			})
			m.input = m.networkDraft.Proxy
			return nil
		}),
		m.item("沿用当前进程代理环境", func(m *uiModel) tea.Cmd {
			m.networkDraft.ProxyMode = "inherit"
			m.networkDraft.Proxy = ""
			return m.finishNetwork()
		}),
	)
}

func (m *uiModel) finishNetwork() tea.Cmd {
	if err := m.networkDraft.Validate(); err != nil {
		m.err = err
		return nil
	}
	m.app.Network = m.networkDraft
	m.networkReady = true
	return m.networkNext(m)
}
