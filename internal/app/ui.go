package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type uiResult struct {
	text      string
	err       error
	inventory *Inventory
	backups   []Backup
	resume    uiRoute
}
type uiCancelMsg struct{}
type uiRoute func(*uiModel) tea.Cmd

func uiShow(show func(*uiModel)) uiRoute {
	return func(m *uiModel) tea.Cmd { show(m); return nil }
}

type uiItem struct {
	label    string
	action   func(*uiModel) tea.Cmd
	disabled string
}

// Only presentation state is restored when leaving an input or confirmation.
// Menus render again from current selections and refreshed deployment state.
type uiPage struct {
	screen, title, detail, note, input, inputLabel string
	items                                          []uiItem
	cursor                                         int
	err                                            error
	onInput                                        func(*uiModel, string) tea.Cmd
	backTo, refreshTo                              uiRoute
}
type uiModel struct {
	uiPage
	ctx                        context.Context
	app                        *App
	actions                    UIActions
	inventory                  Inventory
	busy                       bool
	resultTo                   uiRoute
	refreshNeeded              bool
	pending                    func() tea.Msg
	confirmStage               int
	destructive                bool
	options                    InstallOptions
	migrate                    bool
	roots                      []string
	selected                   map[string]bool
	component, componentAction string
	componentOptions           ComponentOptions
	componentForInstall        bool
	pendingCodexApp            *ComponentOptions
	backups                    []Backup
	width, height, scroll      int
	manualScroll               bool
	progress                   *uiProgress
	started                    time.Time
	frame                      int
	networkReady               bool
	networkDraft               NetworkOptions
	networkNext, networkBack   uiRoute
	parentCtx                  context.Context
	cancel                     context.CancelFunc
	cancelling                 bool
	quitAfterResult            bool
}

var uiTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
var uiSelected = lipgloss.NewStyle().Foreground(lipgloss.Color("120"))
var uiError = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))

func RunUI(ctx context.Context, a *App, actions UIActions) error {
	progress := &uiProgress{app: a}
	out, stderr := a.Out, a.Err
	a.Out, a.Err = progress, progress
	defer func() { a.Out, a.Err = out, stderr }()
	m := &uiModel{ctx: ctx, parentCtx: ctx, app: a, actions: actions, selected: map[string]bool{}, progress: progress}
	m.home()
	program := tea.NewProgram(m, tea.WithAltScreen(), tea.WithoutSignalHandler())
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			program.Send(uiCancelMsg{})
		case <-done:
		}
	}()
	_, err := program.Run()
	return err
}
func (m *uiModel) Init() tea.Cmd { return tea.Batch(m.discover(uiShow((*uiModel).home)), uiTick()) }
func (m *uiModel) discover(resume uiRoute) tea.Cmd {
	m.startBusy("正在扫描环境")
	return func() tea.Msg {
		var inv Inventory
		var err error
		if m.actions.Inventory != nil {
			inv, err = m.actions.Inventory(m.ctx)
		} else {
			inv, err = m.app.Discover(m.ctx)
		}
		return uiResult{inventory: &inv, err: err, resume: resume}
	}
}
func (m *uiModel) menu(title string, parent uiRoute, items ...uiItem) {
	m.uiPage = uiPage{screen: "menu", title: title, items: items, backTo: parent}
	m.scroll = 0
	m.manualScroll = false
}
func (m *uiModel) returnHere() uiRoute {
	page := m.uiPage
	page.err = nil
	return func(m *uiModel) tea.Cmd {
		m.uiPage = page
		m.pending = nil
		m.confirmStage = 0
		return nil
	}
}
func (m *uiModel) back() tea.Cmd {
	if m.backTo != nil {
		return m.backTo(m)
	}
	return nil
}
func (m *uiModel) leaveResult() tea.Cmd {
	resume := m.resultTo
	if resume == nil {
		resume = uiShow((*uiModel).home)
	}
	if m.refreshNeeded {
		m.refreshNeeded = false
		return m.discover(resume)
	}
	return resume(m)
}
func (m *uiModel) item(label string, fn func(*uiModel) tea.Cmd) uiItem {
	return uiItem{label: label, action: fn}
}
func (m *uiModel) choice(label, reason string, fn func(*uiModel) tea.Cmd) uiItem {
	if reason == "" {
		return m.item(label, fn)
	}
	return uiItem{label: label, disabled: reason, action: func(m *uiModel) tea.Cmd { m.err = fmt.Errorf("%s", reason); return nil }}
}
func (m *uiModel) home() {
	m.menu("Codex Background · "+Version, nil,
		m.item("部署管理", uiShow((*uiModel).deploymentMenu)),
		m.item("运行与维护", uiShow((*uiModel).maintenanceMenu)),
		m.item("备份与文件", uiShow((*uiModel).filesMenu)),
		m.item("退出", func(*uiModel) tea.Cmd { return tea.Quit }),
	)
	m.resultTo = uiShow((*uiModel).home)
	m.refreshTo = m.resultTo
}
func (m *uiModel) deploymentMenu() {
	items := []uiItem{}
	note := "首次部署：选择扩展目录、Codex 环境和后台方式，预览后确认安装。"
	if !m.app.S.Installed {
		label := "安装后台"
		if len(m.inventory.Legacy) > 0 {
			label = "迁移现有部署"
			note = m.migrationSummary()
		}
		items = append(items, m.choice(label, m.mutationBlock(), func(m *uiModel) tea.Cmd {
			m.installWizard(len(m.inventory.Legacy) > 0)
			return nil
		}))
	} else {
		note = "核心已安装。可在此管理组件和版本；检查服务或修复扩展入口请进入「运行与维护」。"
	}
	items = append(items, m.item("可选组件", uiShow((*uiModel).components)))
	items = append(items, m.item("下载设置 · 镜像源与代理", func(m *uiModel) tea.Cmd {
		return m.networkWizard(uiShow((*uiModel).deploymentMenu), uiShow((*uiModel).deploymentMenu))
	}))
	if m.app.S.Installed {
		rollbackReason := m.coreBlock()
		if rollbackReason == "" && !m.rollbackAvailable() {
			rollbackReason = "没有可回滚的更新记录"
		}
		uninstallReason := m.coreBlock()
		if m.app.S.Components["codexapp"] != nil {
			uninstallReason = "先卸载或解除 CodexApp 对核心的引用"
		}
		items = append(items,
			m.choice("更新核心", m.coreBlock(), func(m *uiModel) tea.Cmd {
				if m.app.S.Codex.Source != "managed" {
					return m.forceChoice("更新工具到当前安装包；继续复用现有 Codex", func(force bool) tea.Msg {
						if m.actions.Update == nil {
							return unavailable()
						}
						return uiResult{err: m.actions.Update(m.ctx, "", force), text: "工具更新完成，外部 Codex 保持原版本"}
					})
				}
				return m.ask("受管 Codex 目标版本（留空仅更新工具到当前安装包）", func(m *uiModel, s string) tea.Cmd {
					description := "更新工具到当前安装包"
					if s != "" {
						description += "，并更新受管 Codex 至 " + s
					}
					return m.withNetwork(func(m *uiModel) tea.Cmd {
						return m.forceChoice(description+"\n"+m.app.Network.Summary(), func(force bool) tea.Msg {
							if m.actions.Update == nil {
								return unavailable()
							}
							return uiResult{err: m.actions.Update(m.ctx, s, force), text: "更新操作完成"}
						})
					})
				})
			}),
			m.item("更新历史 · 查看回滚事务 ID", func(m *uiModel) tea.Cmd { return m.history() }),
			m.choice("回滚核心", rollbackReason, func(m *uiModel) tea.Cmd {
				return m.ask("更新历史中的事务 ID（部署管理 → 更新历史可查看）", func(m *uiModel, s string) tea.Cmd {
					return m.forceChoice("回滚核心："+s, func(force bool) tea.Msg {
						if m.actions.Rollback == nil {
							return unavailable()
						}
						return uiResult{err: m.actions.Rollback(m.ctx, s, force), text: "回滚操作完成"}
					})
				})
			}),
			m.choice("卸载核心", uninstallReason, func(m *uiModel) tea.Cmd {
				return m.forceChoice("卸载核心并恢复受管入口", func(force bool) tea.Msg {
					if m.actions.Uninstall == nil {
						return unavailable()
					}
					return uiResult{err: m.actions.Uninstall(m.ctx, force), text: "卸载操作完成"}
				})
			}),
		)
	}
	m.menu("部署管理", uiShow((*uiModel).home), items...)
	m.note = note
	m.resultTo = uiShow((*uiModel).deploymentMenu)
	m.refreshTo = m.resultTo
}
func (m *uiModel) migrationSummary() string {
	return "迁移会先检查旧入口原件和恢复条件，再切换已确认的旧服务、入口及 CLI 配置覆盖。\n检测结果：\n  " + strings.Join(m.inventory.Legacy, "\n  ")
}
func (m *uiModel) maintenanceMenu() {
	items := []uiItem{
		m.choice("后台服务", func() string {
			if !m.app.S.Installed {
				return "核心后台尚未部署"
			}
			return ""
		}(), uiShow((*uiModel).serviceMenu)),
		m.item("只读诊断", func(m *uiModel) tea.Cmd { return m.readText(m.actions.Diagnose) }),
		m.choice("修复受管扩展入口", m.coreBlock(), func(m *uiModel) tea.Cmd {
			return m.confirm("重新检查并修复已选择扩展目录中的受管入口", false, func() tea.Msg {
				if m.actions.Repair == nil {
					return unavailable()
				}
				return uiResult{err: m.actions.Repair(m.ctx), text: "入口修复完成"}
			})
		}),
	}
	if hasPending(m.app.P.Data) {
		items = append(items, m.item("恢复未完成事务", func(m *uiModel) tea.Cmd {
			return m.confirm("按事务记录恢复中断前的受管资源", false, func() tea.Msg {
				if m.actions.Recover == nil {
					return unavailable()
				}
				return uiResult{err: m.actions.Recover(m.ctx), text: "事务恢复完成"}
			})
		}))
	}
	m.menu("运行与维护", uiShow((*uiModel).home), items...)
	m.resultTo = uiShow((*uiModel).maintenanceMenu)
	m.refreshTo = m.resultTo
}
func (m *uiModel) filesMenu() {
	relinkReason := ""
	if hasPending(m.app.P.Data) {
		relinkReason = "先恢复未完成事务，再重新关联目录"
	}
	m.menu("备份与文件", uiShow((*uiModel).home),
		m.item("备份管理", (*uiModel).loadBackups),
		m.item("查看日志", func(m *uiModel) tea.Cmd {
			m.startBusy("正在读取日志")
			return func() tea.Msg {
				if m.actions.Logs == nil {
					return uiResult{err: fmt.Errorf("日志功能不可用")}
				}
				return uiResult{text: m.actions.Logs()}
			}
		}),
		m.choice("清理日志、缓存和临时文件", m.cleanupBlock(), uiShow((*uiModel).dataMenu)),
		m.choice("重新关联维护目录", relinkReason, func(m *uiModel) tea.Cmd {
			return m.ask("新安装器目录绝对路径（需已完整迁入 installer-data）", func(m *uiModel, target string) tea.Cmd {
				if !filepath.IsAbs(target) {
					m.err = fmt.Errorf("请输入安装器目录的绝对路径")
					return nil
				}
				return m.dataConfirm("relink", target)
			})
		}),
	)
	m.note = "维护资料路径：" + m.app.P.Data
	m.resultTo = uiShow((*uiModel).filesMenu)
	m.refreshTo = m.resultTo
}
func unavailable() tea.Msg { return uiResult{err: fmt.Errorf("此操作未配置")} }
func (m *uiModel) forceChoice(description string, fn func(bool) tea.Msg) tea.Cmd {
	m.menu(description, m.returnHere(),
		m.item("安全执行：仅后台空闲时继续（默认）", func(m *uiModel) tea.Cmd {
			return m.confirm(description+"\n检查活跃任务；忙碌或无法确认空闲时拒绝执行。", false, func() tea.Msg { return fn(false) })
		}),
		m.item("强制执行：明确中断当前后台任务", func(m *uiModel) tea.Cmd {
			return m.confirm(description+"\n将绕过空闲检查，明确允许中断正在执行的任务。", true, func() tea.Msg { return fn(true) })
		}))
	return nil
}
func (m *uiModel) readText(fn func(context.Context) string) tea.Cmd {
	m.startBusy("正在读取状态")
	return func() tea.Msg {
		if fn == nil {
			return unavailable()
		}
		return uiResult{text: fn(m.ctx)}
	}
}
func (m *uiModel) ask(label string, fn func(*uiModel, string) tea.Cmd) tea.Cmd {
	m.backTo = m.returnHere()
	m.screen = "input"
	m.inputLabel = label
	m.input = ""
	m.onInput = fn
	m.err = nil
	return nil
}
func (m *uiModel) confirm(detail string, strong bool, fn func() tea.Msg) tea.Cmd {
	m.backTo = m.returnHere()
	m.screen = "confirm"
	m.detail = detail
	m.pending = fn
	m.confirmStage = 0
	m.destructive = strong
	m.err = nil
	return nil
}
func (m *uiModel) installWizard(migrate bool) {
	m.migrate = migrate
	m.resultTo = uiShow((*uiModel).deploymentMenu)
	m.componentForInstall = false
	m.pendingCodexApp = nil
	m.options = InstallOptions{Yes: true, ComponentOptions: map[string]ComponentOptions{}}
	m.selected = map[string]bool{}
	m.roots = nil
	seen := map[string]bool{}
	for _, ext := range m.inventory.Extensions {
		if !seen[ext.Root] {
			seen[ext.Root] = true
			m.roots = append(m.roots, ext.Root)
		}
	}
	sort.Strings(m.roots)
	m.rootMenu()
}
func (m *uiModel) deploymentKind() string {
	if m.migrate {
		return "迁移"
	}
	return "安装"
}
func (m *uiModel) rootMenu() {
	items := []uiItem{}
	for _, root := range m.roots {
		root := root
		mark := "[ ] "
		if m.selected[root] {
			mark = "[x] "
		}
		items = append(items, m.item(mark+root, func(m *uiModel) tea.Cmd { m.selected[root] = !m.selected[root]; m.rootMenu(); return nil }))
	}
	items = append(items, m.item("添加扩展根目录", func(m *uiModel) tea.Cmd {
		return m.ask("扩展根目录绝对路径", func(m *uiModel, s string) tea.Cmd {
			if !strings.HasPrefix(s, "/") {
				m.err = fmt.Errorf("请输入绝对路径")
				return nil
			}
			m.roots = append(m.roots, s)
			m.selected[s] = true
			m.rootMenu()
			return nil
		})
	}), m.item("下一步：选择 Codex", func(m *uiModel) tea.Cmd {
		m.options.Roots = nil
		for _, root := range m.roots {
			if m.selected[root] {
				m.options.Roots = append(m.options.Roots, root)
			}
		}
		if len(m.options.Roots) == 0 {
			m.err = fmt.Errorf("请明确选择至少一个扩展根目录")
			return nil
		}
		m.codexMenu()
		return nil
	}))
	m.menu(m.deploymentKind()+" · 明确选择扩展目录（默认全不选）", uiShow((*uiModel).deploymentMenu), items...)
	if m.migrate {
		m.note = m.migrationSummary()
	}
}
func (m *uiModel) codexMenu() {
	items := []uiItem{}
	for _, candidate := range m.inventory.Candidates["codex"] {
		c := candidate
		if !c.Compatible {
			continue
		}
		items = append(items, m.item("复用 "+c.Version+" · "+c.Path, func(m *uiModel) tea.Cmd {
			m.options.CodexPath = c.Path
			m.options.CodexVersion = ""
			m.backendMenu()
			return nil
		}))
	}
	items = append(items, m.item("指定现有 Codex 路径", func(m *uiModel) tea.Cmd {
		return m.ask("Codex 绝对路径", func(m *uiModel, s string) tea.Cmd {
			if !strings.HasPrefix(s, "/") {
				m.err = fmt.Errorf("请输入绝对路径")
				return nil
			}
			m.options.CodexPath = s
			m.options.CodexVersion = ""
			m.backendMenu()
			return nil
		})
	}), m.item("安装受管 Codex", func(m *uiModel) tea.Cmd {
		return m.ask("Codex 版本（空白为默认版本）", func(m *uiModel, s string) tea.Cmd {
			m.options.CodexPath = "managed"
			m.options.CodexVersion = s
			m.backendMenu()
			return nil
		})
	}))
	m.menu(m.deploymentKind()+" · Codex 运行时（优先复用兼容候选）", uiShow((*uiModel).rootMenu), items...)
}
func (m *uiModel) backendMenu() {
	items := []uiItem{}
	if m.inventory.Systemd {
		items = append(items, m.item("systemd 用户服务", func(m *uiModel) tea.Cmd {
			m.options.Backend = "systemd"
			m.options.EntrypointDir = ""
			m.installComponents()
			return nil
		}))
	}
	if m.inventory.Container {
		items = append(items, m.item("容器启动入口", func(m *uiModel) tea.Cmd {
			m.options.Backend = "container"
			m.options.Linger = false
			if m.inventory.EntrypointDir != "" {
				m.options.EntrypointDir = m.inventory.EntrypointDir
				m.installComponents()
				return nil
			}
			return m.ask("镜像实际执行的 entrypoint.d 绝对目录", func(m *uiModel, s string) tea.Cmd {
				if !strings.HasPrefix(s, "/") {
					m.err = fmt.Errorf("请输入绝对路径")
					return nil
				}
				m.options.EntrypointDir = s
				m.installComponents()
				return nil
			})
		}))
	}
	m.menu(m.deploymentKind()+" · 后台管理方式", uiShow((*uiModel).codexMenu), items...)
	if len(items) == 0 {
		m.err = fmt.Errorf("未发现可用后台；先检查 systemd 用户管理器或容器启动契约")
	}
}
func hasString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
func (m *uiModel) installComponents() {
	// Returning from an unfinished component form must not enable that component.
	configured := []string{}
	for _, name := range m.options.Components {
		if _, ok := m.options.ComponentOptions[name]; ok {
			configured = append(configured, name)
		}
	}
	m.options.Components = configured
	m.componentForInstall = false
	m.pendingCodexApp = nil
	names := m.componentNames()
	items := []uiItem{}
	for _, name := range names {
		name := name
		mark := "[ ] "
		if hasString(m.options.Components, name) {
			mark = "[x] "
		}
		items = append(items, m.choice(mark+componentLabel(name), m.componentInstallBlock(name, true), func(m *uiModel) tea.Cmd {
			next := []string{}
			found := false
			for _, n := range m.options.Components {
				if n == name {
					found = true
				} else {
					next = append(next, n)
				}
			}
			if !found {
				next = append(next, name)
			}
			m.options.Components = next
			if !found {
				m.component = name
				m.componentAction = "install"
				m.componentOptions = ComponentOptions{Yes: true, ShellIntegration: true}
				m.componentForInstall = true
				m.componentCandidates()
				return nil
			}
			delete(m.options.ComponentOptions, name)
			m.installComponents()
			return nil
		}))
	}
	if m.options.Backend == "systemd" {
		mark := "[ ] "
		if m.options.Linger {
			mark = "[x] "
		}
		items = append(items, m.item(mark+"启用 linger（注销后继续）", func(m *uiModel) tea.Cmd { m.options.Linger = !m.options.Linger; m.installComponents(); return nil }))
	}
	items = append(items, m.item("预览并确认", func(m *uiModel) tea.Cmd {
		return m.withNetwork(func(m *uiModel) tea.Cmd {
			description := fmt.Sprintf("操作：%s\n方式：%s\nCodex：%s 版本：%s\n扩展目录：\n  %s\n可选组件：%v\nlinger：%v", m.deploymentKind(), m.options.Backend, m.options.CodexPath, m.options.CodexVersion, strings.Join(m.options.Roots, "\n  "), m.options.Components, m.options.Linger)
			description += "\n" + m.app.Network.Summary()
			if m.migrate {
				description += "\n\n" + m.migrationSummary()
			}
			for _, name := range m.options.Components {
				opt := m.options.ComponentOptions[name]
				if name == "marketplace" {
					description += "\n微软官方插件源：" + m.inventory.CodeServerProduct + "\n替换整个插件库来源；已是微软源则复用。需要手动重启 code-server 后生效，不安装任何插件。"
					continue
				}
				description += fmt.Sprintf("\n%s：path=%s version=%s port=%s password-file=%s 安装Node依赖=%v", name, opt.Path, opt.Version, opt.Port, opt.PasswordFile, opt.InstallNode)
				if name == "codexapp" && opt.PasswordFile == "" {
					description += "\n密码默认保存位置：" + m.app.CodexAppPasswordPath() + "（已有则保留，否则自动生成）"
				}
			}
			opts := m.options
			return m.confirm(description, false, func() tea.Msg {
				fn := m.actions.Install
				if m.migrate {
					fn = m.actions.Migrate
				}
				if fn == nil {
					return unavailable()
				}
				return uiResult{err: fn(m.ctx, opts), text: m.deploymentKind() + "操作完成；请检查扩展连接"}
			})
		})
	}))
	m.menu(m.deploymentKind()+" · 可选组件（核心不依赖 Node）", uiShow((*uiModel).backendMenu), items...)
}
func componentLabel(name string) string {
	if name == "marketplace" {
		return "微软官方插件源 (marketplace)"
	}
	return name
}

func (m *uiModel) components() {
	items := []uiItem{}
	names := m.componentNames()
	for _, name := range names {
		name := name
		items = append(items, m.item(componentLabel(name), func(m *uiModel) tea.Cmd {
			m.component = name
			m.componentForInstall = false
			m.componentOptions = ComponentOptions{Yes: true, ShellIntegration: true}
			m.componentMenu()
			return nil
		}))
	}
	m.menu("可选组件 · 默认不安装", uiShow((*uiModel).deploymentMenu), items...)
	m.resultTo = uiShow((*uiModel).components)
	m.refreshTo = m.resultTo
}
func (m *uiModel) componentMenu() {
	items := []uiItem{}
	actions := []string{"install"}
	c := m.app.S.Components[m.component]
	if c != nil {
		actions = []string{"uninstall"}
		if c.Status == "installed" {
			actions = append([]string{"repair"}, actions...)
		}
		if m.component == "codexapp" && (c.Status == "installed" || c.Status == "available") {
			actions = append([]string{"configure"}, actions...)
		}
	}
	for _, action := range actions {
		action := action
		labels := map[string]string{"install": "安装 / 复用", "repair": "校验组件", "uninstall": "卸载 / 解除引用", "configure": "修改配置"}
		if m.component == "marketplace" {
			labels["install"] = "启用 / 复用微软官方插件源"
			labels["repair"] = "校验并重新应用插件源"
			labels["uninstall"] = "恢复原插件源 / 解除引用"
			if c != nil && c.Ownership == "reused" {
				labels["repair"] = "校验已有插件源"
			}
		}
		reason := m.componentActionBlock(action)
		items = append(items, m.choice(labels[action], reason, func(m *uiModel) tea.Cmd {
			m.componentAction = action
			m.componentForInstall = false
			m.componentOptions = ComponentOptions{Yes: true, ShellIntegration: true}
			m.pendingCodexApp = nil
			if action == "configure" {
				return m.codexAppOptions()
			}
			if action == "uninstall" || action == "repair" {
				return m.componentConfirm()
			}
			m.componentCandidates()
			return nil
		}))
	}
	m.menu("组件 · "+componentLabel(m.component), uiShow((*uiModel).components), items...)
	if c != nil {
		m.note = "状态：" + c.Status + " · " + c.Ownership + "\n路径：" + c.Path
	}
	m.resultTo = uiShow((*uiModel).componentMenu)
}
func (m *uiModel) componentParent() uiRoute {
	if m.componentForInstall {
		return uiShow((*uiModel).installComponents)
	}
	return uiShow((*uiModel).componentMenu)
}
func (m *uiModel) componentCandidates() {
	if m.component == "apti" || m.component == "marketplace" {
		m.componentConfirm()
		return
	}
	if m.component == "branding" {
		m.ask("图标目标 media 绝对目录（留空采用检测结果）", func(m *uiModel, s string) tea.Cmd {
			if s == "" {
				s = m.inventory.MediaDir
			}
			if !strings.HasPrefix(s, "/") {
				m.err = fmt.Errorf("请输入绝对路径")
				return nil
			}
			m.componentOptions.MediaDir = s
			return m.componentConfirm()
		})
		m.backTo = m.componentParent()
		return
	}
	items := []uiItem{}
	kind := m.component
	if kind == "miniconda" {
		kind = "conda"
	}
	for _, candidate := range m.inventory.Candidates[kind] {
		c := candidate
		if !c.Compatible {
			continue
		}
		items = append(items, m.item("复用 "+c.Version+" · "+c.Path, func(m *uiModel) tea.Cmd {
			m.componentOptions.Path = c.Path
			m.componentOptions.ShellIntegration = false
			if m.component == "codexapp" {
				return m.codexAppOptions()
			}
			return m.componentConfirm()
		}))
	}
	items = append(items, m.item("指定复用路径", func(m *uiModel) tea.Cmd {
		return m.ask("组件绝对路径", func(m *uiModel, s string) tea.Cmd {
			if !strings.HasPrefix(s, "/") {
				m.err = fmt.Errorf("请输入绝对路径")
				return nil
			}
			m.componentOptions.Path = s
			m.componentOptions.ShellIntegration = false
			if m.component == "codexapp" {
				return m.codexAppOptions()
			}
			return m.componentConfirm()
		})
	}), m.item("安装受管版本", func(m *uiModel) tea.Cmd {
		return m.ask("版本（留空使用默认）", func(m *uiModel, s string) tea.Cmd {
			m.componentOptions.Version = s
			m.componentOptions.Path = "managed"
			m.componentOptions.ShellIntegration = true
			if m.component == "codexapp" {
				return m.codexAppOptions()
			}
			return m.componentConfirm()
		})
	}))
	m.menu("组件 · "+m.component+" · 优先复用", m.componentParent(), items...)
}
func (m *uiModel) codexAppOptions() tea.Cmd {
	return m.ask("WebUI 密码文件绝对路径（留空保留现有密码，首次自动生成）", func(m *uiModel, s string) tea.Cmd {
		if s != "" && !filepath.IsAbs(s) {
			m.err = fmt.Errorf("请输入密码文件绝对路径")
			return nil
		}
		m.componentOptions.PasswordFile = s
		m.componentOptions.Configure = true
		return m.ask("WebUI 端口（留空使用默认）", func(m *uiModel, s string) tea.Cmd { m.componentOptions.Port = s; return m.componentConfirm() })
	})
}
func (m *uiModel) componentConfirm() tea.Cmd {
	if m.component == "codexapp" && (m.componentAction == "install" || m.componentAction == "configure") && !m.componentOptions.InstallNode && !m.codexAppNodeReady() {
		if m.app.S.Components["node"] != nil {
			m.menu("CodexApp · 已登记的 Node 不兼容", m.componentParent(), m.item("前往 Node 组件管理", func(m *uiModel) tea.Cmd {
				m.component = "node"
				m.componentForInstall = false
				m.componentMenu()
				return nil
			}))
			m.note = "已登记的 Node 版本低于 20 或缺少 npm。请先解除该引用，再选择兼容 Node；重复安装不会替换已有环境。"
			return nil
		}
		m.menu("CodexApp · 缺少 Node >=20 与同目录 npm", uiShow((*uiModel).componentCandidates),
			m.item("将受管 Node 依赖加入计划（替换不兼容的 Node 选择，确认后安装）", func(m *uiModel) tea.Cmd {
				if m.componentForInstall {
					kept := []string{}
					for _, name := range m.options.Components {
						if name != "node" {
							kept = append(kept, name)
						}
					}
					m.options.Components = kept
					delete(m.options.ComponentOptions, "node")
				}
				m.componentOptions.InstallNode = true
				return m.finishComponentOptions()
			}),
			m.item("返回选择已有 Node", func(m *uiModel) tea.Cmd {
				if m.componentForInstall {
					saved := m.componentOptions
					m.pendingCodexApp = &saved
					if !hasString(m.options.Components, "node") {
						m.options.Components = append(m.options.Components, "node")
					}
				}
				m.component = "node"
				m.componentAction = "install"
				m.componentOptions = ComponentOptions{Yes: true, ShellIntegration: true}
				m.componentCandidates()
				return nil
			}))
		return nil
	}
	if (m.component == "node" || m.component == "miniconda") && m.componentAction == "install" && m.componentOptions.Path == "managed" {
		mark := "[ ] "
		if m.componentOptions.ShellIntegration {
			mark = "[x] "
		}
		m.menu("组件 · shell 集成", uiShow((*uiModel).componentCandidates), m.item(mark+"为交互 shell 添加受管初始化（默认开启）", func(m *uiModel) tea.Cmd {
			m.componentOptions.ShellIntegration = !m.componentOptions.ShellIntegration
			return m.componentConfirm()
		}), m.item("继续预览", func(m *uiModel) tea.Cmd { return m.finishComponentOptions() }))
		return nil
	}
	return m.finishComponentOptions()
}
func (m *uiModel) codexAppNodeReady() bool {
	if m.app != nil && m.app.S.Components["node"] != nil {
		c := m.app.S.Components["node"]
		var major int
		fmt.Sscanf(strings.TrimPrefix(c.Version, "v"), "%d", &major)
		return c.Status == "installed" && major >= 20 && executable(filepath.Join(filepath.Dir(m.app.componentBinary("node")), "npm"))
	}
	if m.componentForInstall && hasString(m.options.Components, "node") {
		opt := m.options.ComponentOptions["node"]
		if opt.Path == "managed" {
			if opt.Version == "" {
				return true
			}
			var major int
			fmt.Sscanf(strings.TrimPrefix(opt.Version, "v"), "%d", &major)
			return major >= 20
		}
		for _, candidate := range m.inventory.Candidates["node"] {
			var major int
			fmt.Sscanf(strings.TrimPrefix(candidate.Version, "v"), "%d", &major)
			if candidate.Path == opt.Path && candidate.Compatible && major >= 20 && executable(filepath.Join(filepath.Dir(candidate.Path), "npm")) {
				return true
			}
		}
		return false
	}
	for _, candidate := range m.inventory.Candidates["node"] {
		var major int
		fmt.Sscanf(strings.TrimPrefix(candidate.Version, "v"), "%d", &major)
		if candidate.Compatible && major >= 20 && executable(filepath.Join(filepath.Dir(candidate.Path), "npm")) {
			return true
		}
	}
	return false
}
func (m *uiModel) finishComponentOptions() tea.Cmd {
	if m.componentForInstall {
		m.options.ComponentOptions[m.component] = m.componentOptions
		if m.component == "node" && m.pendingCodexApp != nil {
			ordered := []string{"node"}
			for _, name := range m.options.Components {
				if name != "node" {
					ordered = append(ordered, name)
				}
			}
			m.options.Components = ordered
			m.component = "codexapp"
			m.componentOptions = *m.pendingCodexApp
			m.pendingCodexApp = nil
			return m.componentConfirm()
		}
		m.componentForInstall = false
		m.installComponents()
		return nil
	}
	name, action, opts := m.component, m.componentAction, m.componentOptions
	next := func(m *uiModel) tea.Cmd {
		detail := fmt.Sprintf("组件 %s · %s\n路径：%s\n版本：%s\n端口：%s\nshell 集成：%v\n安装受管 Node 依赖：%v", name, action, opts.Path, opts.Version, opts.Port, opts.ShellIntegration, opts.InstallNode)
		if name == "marketplace" {
			detail = "组件 微软官方插件源 (marketplace) · " + action
			target := m.inventory.CodeServerProduct
			if c := m.app.S.Components[name]; c != nil {
				target = c.Options["product"]
			}
			detail += "\n目标：" + target
			switch action {
			case "install":
				detail += "\n替换此 code-server 的整个插件库；原配置备份到维护目录，已是微软源则复用。\n需要手动重启 code-server 后生效，不安装任何插件。卸载时恢复原插件源。"
			case "uninstall":
				detail += "\n恢复备份中的原插件源；需要重启 code-server 后生效。"
				if c := m.app.S.Components[name]; c != nil && c.Ownership == "reused" {
					detail = "解除已有微软插件源的引用，保留插件库配置。\n目标：" + target
				}
			case "repair":
				detail += "\n校验配置与恢复备份；仅当恢复为已备份的原插件源时重新应用微软源。\n未知外部改动将保留。重新应用后需要手动重启 code-server。"
				if c := m.app.S.Components[name]; c != nil && c.Ownership == "reused" {
					detail = "校验已复用的微软插件源；保留外部配置，不重新应用。\n目标：" + target
				}
			}
		}
		if name == "codexapp" && opts.PasswordFile == "" {
			detail += "\n密码：保留现有密码；首次自动生成\n密码文件：" + m.app.CodexAppPasswordPath()
		}
		if (action == "install" && name != "marketplace") || opts.InstallNode {
			detail += "\n" + m.app.Network.Summary()
		}
		return m.confirm(detail, action == "uninstall", func() tea.Msg {
			if m.actions.Component == nil {
				return unavailable()
			}
			return uiResult{err: m.actions.Component(m.ctx, action, name, opts), text: "组件操作完成"}
		})
	}
	if (action == "install" && name != "marketplace") || opts.InstallNode {
		return m.withNetwork(next)
	}
	return next(m)
}
func (m *uiModel) serviceMenu() {
	items := []uiItem{m.item("只读状态", func(m *uiModel) tea.Cmd { return m.readText(m.actions.Status) })}
	for _, name := range []string{"start", "stop", "restart"} {
		name := name
		label := map[string]string{"start": "启动", "stop": "停止", "restart": "重启"}[name]
		items = append(items, m.choice(label, m.serviceBlock(name), func(m *uiModel) tea.Cmd {
			operation := func(force bool) tea.Msg {
				if m.actions.Service == nil {
					return unavailable()
				}
				return uiResult{err: m.actions.Service(m.ctx, name, force), text: "后台操作完成"}
			}
			if name == "start" {
				return m.confirm("启动后台服务", false, func() tea.Msg { return operation(false) })
			}
			return m.forceChoice("后台服务："+label, operation)
		}))
	}
	m.menu("后台服务", uiShow((*uiModel).maintenanceMenu), items...)
	m.resultTo = uiShow((*uiModel).serviceMenu)
	m.refreshTo = m.resultTo
}
func (m *uiModel) loadBackups() tea.Cmd {
	m.startBusy("正在读取备份")
	return func() tea.Msg {
		if m.actions.Backups == nil {
			return uiResult{err: fmt.Errorf("备份功能不可用"), resume: uiShow((*uiModel).filesMenu)}
		}
		backups, err := m.actions.Backups()
		return uiResult{backups: append([]Backup{}, backups...), err: err, resume: uiShow((*uiModel).filesMenu)}
	}
}
func (m *uiModel) backupMenu() {
	items := []uiItem{}
	prunable := false
	for _, backup := range m.backups {
		b := backup
		if !b.Deleted && !m.app.backupReferenced(b) {
			prunable = true
		}
		items = append(items, m.item(b.ID+" · "+b.Kind+" · "+b.Original, func(m *uiModel) tea.Cmd { m.backupDetails(b); return nil }))
	}
	reason := m.mutationBlock()
	if reason == "" && !prunable {
		reason = "没有无引用备份可清理"
	}
	items = append(items, m.choice("清理无引用备份", reason, func(m *uiModel) tea.Cmd { return m.backupConfirm("prune", "", false) }))
	m.menu("备份管理", uiShow((*uiModel).filesMenu), items...)
	if len(m.backups) == 0 {
		m.note = "暂无登记的备份。"
	}
	m.resultTo = (*uiModel).loadBackups
	m.refreshTo = m.resultTo
}
func (m *uiModel) history() tea.Cmd {
	m.startBusy("正在读取更新历史")
	data := m.app.P.Data
	return func() tea.Msg {
		entries, e := os.ReadDir(filepath.Join(data, "history"))
		if e != nil {
			return uiResult{err: e}
		}
		var text strings.Builder
		text.WriteString("更新历史（回滚使用事务 ID）\n")
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			raw, e := os.ReadFile(filepath.Join(data, "history", entry.Name()))
			if e != nil {
				return uiResult{err: e}
			}
			var record Transaction
			if e = json.Unmarshal(raw, &record); e != nil {
				return uiResult{err: fmt.Errorf("历史记录无法读取：%s", entry.Name())}
			}
			if record.Action == "update" {
				fmt.Fprintf(&text, "ID: %s\n时间: %s\n之前 Codex: %s\n\n", record.ID, record.Started.Format("2006-01-02 15:04:05"), record.Previous.Codex.Version)
			}
		}
		return uiResult{text: text.String()}
	}
}
func (m *uiModel) backupDetails(b Backup) {
	detail := fmt.Sprintf("ID：%s\n原路径：%s\n备份：%s\nSHA256：%s\n字节：%d\n引用：%s\n创建：%s", b.ID, b.Original, b.DataPath, b.Hash, b.Size, b.Ref, b.Created.Format("2006-01-02 15:04:05"))
	items := []uiItem{}
	for _, action := range []string{"verify", "restore", "delete"} {
		action := action
		label := map[string]string{"verify": "校验", "restore": "恢复", "delete": "删除"}[action]
		reason := m.mutationBlock()
		if action == "verify" {
			reason = ""
		}
		if b.Deleted {
			reason = "备份已删除，仅保留历史记录"
		} else if _, e := os.Stat(b.DataPath); e != nil && action != "delete" {
			reason = "备份内容缺失或不可读"
		}
		if action == "delete" && m.app.backupReferenced(b) {
			reason = "仍用于恢复；如需放弃恢复，请使用强制删除"
		}
		if action == "restore" && m.app.backupReferenced(b) {
			bound := false
			for _, binding := range m.app.S.Bindings {
				if binding.BackupID == b.ID {
					bound = true
				}
			}
			if !bound {
				reason = "备份仍被组件或回滚记录引用，请通过对应维护操作恢复"
			}
		}
		items = append(items, m.choice(label, reason, func(m *uiModel) tea.Cmd { return m.backupConfirm(action, b.ID, false) }))
	}
	if !b.Deleted && m.app.backupReferenced(b) {
		items = append(items, m.choice("强制删除（双确认）", m.mutationBlock(), func(m *uiModel) tea.Cmd { return m.backupConfirm("delete", b.ID, true) }))
	}
	m.menu("备份详情", (*uiModel).loadBackups, items...)
	m.note = detail
}
func (m *uiModel) backupConfirm(action, id string, force bool) tea.Cmd {
	return m.confirm(fmt.Sprintf("备份 %s：%s\nforce=%v", action, id, force), force, func() tea.Msg {
		if m.actions.BackupAction == nil {
			return unavailable()
		}
		return uiResult{err: m.actions.BackupAction(m.ctx, action, id, force), text: "备份操作完成"}
	})
}
func (m *uiModel) dataMenu() {
	items := []uiItem{}
	for _, target := range []string{"logs", "cache", "tmp"} {
		target := target
		label := map[string]string{"logs": "清理日志", "cache": "清理无引用缓存", "tmp": "清理已结束操作的临时文件"}[target]
		items = append(items, m.item(label, func(m *uiModel) tea.Cmd {
			return m.dataConfirm("clean", target)
		}))
	}
	m.menu("文件清理", uiShow((*uiModel).filesMenu), items...)
	m.note = "只处理维护目录内可清理的文件；备份通过「备份管理」单独处理。"
	m.resultTo = uiShow((*uiModel).dataMenu)
}
func (m *uiModel) dataConfirm(action, target string) tea.Cmd {
	detail := "重新关联维护目录\n新安装器目录：" + target
	if action == "clean" {
		detail = "清理维护文件\n目标：" + filepath.Join(m.app.P.Data, target)
	}
	return m.confirm(detail, true, func() tea.Msg {
		if m.actions.DataAction == nil {
			return unavailable()
		}
		return uiResult{err: m.actions.DataAction(m.ctx, action, target), text: "数据操作完成"}
	})
}

func (m *uiModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case uiCancelMsg:
		if !m.busy {
			return m, tea.Quit
		}
		m.quitAfterResult = true
		m.cancelling = true
		if m.cancel != nil {
			m.cancel()
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case uiTickMsg:
		m.frame++
		return m, uiTick()
	case uiResult:
		m.busy = false
		if m.cancel != nil {
			m.cancel()
			m.cancel = nil
		}
		if m.quitAfterResult {
			return m, tea.Quit
		}
		m.err = msg.err
		if msg.inventory != nil && msg.err == nil {
			m.inventory = *msg.inventory
			if msg.resume != nil {
				return m, msg.resume(m)
			}
			return m, nil
		}
		if msg.backups != nil && msg.err == nil {
			m.backups = msg.backups
			m.backupMenu()
			return m, nil
		}
		m.screen = "result"
		m.backTo = (*uiModel).leaveResult
		m.detail = msg.text
		output := ""
		if m.progress != nil && strings.TrimSpace(m.progress.snapshot()) != "" {
			output = "\n\n操作输出：\n" + m.progress.snapshot()
			m.detail += output
		}
		m.pending = nil
		if msg.err != nil {
			m.detail = "操作未完成；请根据上方错误修复后重试。" + output
			if msg.resume != nil {
				m.resultTo = msg.resume
			}
		}
	case tea.KeyMsg:
		key := msg.String()
		if key == "ctrl+c" {
			if m.busy {
				if m.cancel != nil {
					m.cancelling = true
					m.cancel()
				}
				return m, nil
			}
			return m, tea.Quit
		}
		if m.busy {
			return m, nil
		}
		if key == "esc" {
			return m, m.back()
		}
		if key == "pgup" || key == "pgdown" {
			m.manualScroll = true
			_, h := m.dimensions()
			if key == "pgup" {
				m.scroll = max(0, m.scroll-max(1, h/2))
			} else {
				m.scroll += max(1, h/2)
			}
			return m, nil
		}
		switch m.screen {
		case "input":
			switch key {
			case "enter":
				m.err = nil
				return m, m.onInput(m, strings.TrimSpace(m.input))
			case "backspace", "ctrl+h":
				r := []rune(m.input)
				if len(r) > 0 {
					m.input = string(r[:len(r)-1])
				}
			case "ctrl+u":
				m.input = ""
			default:
				if msg.Type == tea.KeyRunes {
					m.input += string(msg.Runes)
				}
			}
		case "confirm":
			if key == "n" {
				return m, m.back()
			}
			if key == "y" {
				if m.destructive && m.confirmStage == 0 {
					m.confirmStage = 1
					return m, nil
				}
				m.startBusy("开始执行已确认的操作")
				m.refreshNeeded = true
				fn := m.pending
				return m, func() tea.Msg { return fn() }
			}
		case "result":
			if key == "enter" {
				return m, m.leaveResult()
			}
			if key == "q" {
				return m, tea.Quit
			}
		default:
			switch key {
			case "q":
				return m, tea.Quit
			case "r":
				if m.refreshTo != nil {
					return m, m.discover(m.refreshTo)
				}
			case "up", "k":
				m.manualScroll = false
				if m.cursor > 0 {
					m.cursor--
				}
			case "down", "j":
				m.manualScroll = false
				if m.cursor+1 < len(m.items) {
					m.cursor++
				}
			case "enter", " ":
				if len(m.items) > 0 {
					return m, m.items[m.cursor].action(m)
				}
			}
		}
	}
	return m, nil
}
func (m *uiModel) View() string {
	var b strings.Builder
	b.WriteString(uiTitle.Render(uiPlain(m.title)) + "\n\n")
	if m.busy {
		return m.progressView()
	}
	if m.err != nil {
		b.WriteString(uiError.Render(uiPlain(m.err.Error())) + "\n\n")
	}
	footer := ""
	focus := -1
	switch m.screen {
	case "input":
		b.WriteString(uiPlain(m.inputLabel) + "\n> " + uiPlain(m.input) + "▏")
		footer = "Enter 继续 · Ctrl+U 清空 · Esc 返回上一步"
	case "confirm":
		b.WriteString(uiPlain(m.detail))
		if m.confirmStage == 1 {
			footer = "再次确认不可逆/中断操作：按 y 执行，n 取消"
		} else {
			footer = "确认继续？y / n（默认不执行） · Esc 返回"
		}
	case "result":
		b.WriteString(uiPlain(m.detail))
		footer = "Enter / Esc 返回所属菜单 · q 退出"
	default:
		if strings.HasPrefix(m.title, "Codex Background") {
			status := "未安装"
			if m.app.S.Installed {
				status = "已安装 · " + m.app.S.Backend
			} else if len(m.inventory.Legacy) > 0 {
				status = "检测到旧部署，尚无新版安装记录"
			}
			b.WriteString(status + "\n" + fmt.Sprintf("主机 %s · systemd=%v · container=%v\n", m.inventory.Host, m.inventory.Systemd, m.inventory.Container))
			b.WriteByte('\n')
		}
		if m.note != "" {
			b.WriteString(uiPlain(m.note) + "\n\n")
		}
		for i, item := range m.items {
			line := "  " + uiPlain(item.label)
			if item.disabled != "" {
				line += " [不可用：" + uiPlain(item.disabled) + "]"
			}
			if i == m.cursor {
				w, _ := m.dimensions()
				focus = strings.Count(ansi.Hardwrap(b.String(), max(1, w-2), true), "\n")
				line = uiSelected.Render("› " + strings.TrimPrefix(line, "  "))
			}
			b.WriteString(line + "\n")
		}
		footer = "↑↓ 选择 · Enter/空格 操作"
		if m.backTo != nil {
			footer += " · Esc 返回上一级"
		}
		if m.refreshTo != nil {
			footer += " · r 刷新"
		}
		footer += " · q 退出"
	}
	return m.frameView(b.String(), footer, focus)
}
