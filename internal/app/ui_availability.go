package app

import (
	"encoding/json"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
)

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func serviceStateLabel(state string) string {
	if label, ok := map[string]string{
		"running": "正常运行", "starting": "正在启动", "retrying": "等待自动重试",
		"exhausted": "重试额度已耗尽", "failed": "异常退出，等待手动启动",
		"stopped": "已停止", "stopping": "正在确认停止", "stop_failed": "停止失败，进程状态待核实",
		"maintenance": "维护中", "unknown": "无法确认",
	}[state]; ok {
		return label
	}
	return state
}

func coreUnavailable(state string) bool {
	switch state {
	case "stopped", "stopping", "stop_failed", "starting", "retrying", "exhausted", "failed", "maintenance":
		return true
	}
	return false
}

func (m *uiModel) mutationBlock() string {
	if hasPending(m.app.P.Data) {
		return "存在未完成事务，请先在运行与维护中恢复"
	}
	if m.app.S.Installed && !directoryExists(m.app.P.Data) {
		return "维护目录不可用，请先重新关联维护目录"
	}
	return ""
}

func (m *uiModel) coreBlock() string {
	if !m.app.S.Installed {
		return "核心后台尚未部署"
	}
	return m.mutationBlock()
}

func (m *uiModel) componentInstallBlock(name string, installingCore bool) string {
	if reason := m.mutationBlock(); reason != "" {
		return reason
	}
	if c := m.app.S.Components[name]; c != nil {
		return "已登记此组件，请使用组件维护操作"
	}
	switch name {
	case "codexapp":
		if !installingCore && !m.app.S.Installed {
			return "请先部署核心后台"
		}
		if !installingCore && coreUnavailable(m.inventory.ServiceState) {
			return "核心后台尚未就绪（" + serviceStateLabel(m.inventory.ServiceState) + "）"
		}
	case "apti":
		if !m.inventory.Container || !m.inventory.Apt {
			return "需要容器以及 apt-get、dpkg-query"
		}
		if !m.inventory.Privileged {
			return "需要 root 或可用的免密 sudo"
		}
		if _, err := os.Lstat(filepath.Join(m.app.P.Home, ".local/bin/apti")); err == nil {
			return "已有 apti 入口，保留原安装"
		}
	case "branding":
		if !m.inventory.Container || m.inventory.MediaDir == "" {
			return "未识别到容器中的 code-server 图标资源"
		}
		if unix.Access(m.inventory.MediaDir, unix.W_OK) != nil && !m.inventory.Privileged {
			return "图标目录不可写，且没有可用的免密 sudo"
		}
	case "marketplace":
		return marketplaceInstallBlock(m.inventory)
	}
	return ""
}

func (m *uiModel) componentNames() []string {
	names := []string{"miniconda", "node", "codexapp"}
	for _, name := range []string{"apti", "branding", "marketplace"} {
		if name == "marketplace" && (!m.inventory.Container || m.inventory.CodeServerProduct == "") {
			continue
		}
		if m.app.S.Components[name] != nil || m.componentInstallBlock(name, true) == "" {
			names = append(names, name)
		}
	}
	return names
}

func (m *uiModel) cleanupBlock() string {
	if reason := m.mutationBlock(); reason != "" {
		return reason
	}
	if !directoryExists(m.app.P.Data) {
		return "维护目录尚不存在"
	}
	return ""
}

func (m *uiModel) componentActionBlock(action string) string {
	if reason := m.mutationBlock(); reason != "" {
		return reason
	}
	if action == "install" {
		return m.componentInstallBlock(m.component, false)
	}
	if m.component == "marketplace" && action == "repair" && os.Getenv("EXTENSIONS_GALLERY") != "" {
		return "EXTENSIONS_GALLERY 正在覆盖插件源，请先处理容器环境配置"
	}
	if m.component == "codexapp" && action == "configure" {
		if reason := m.coreBlock(); reason != "" {
			return reason
		}
		if coreUnavailable(m.inventory.ServiceState) {
			return "核心后台尚未就绪（" + serviceStateLabel(m.inventory.ServiceState) + "）"
		}
	}
	if action == "uninstall" && m.component == "node" {
		c, web := m.app.S.Components["node"], m.app.S.Components["codexapp"]
		if c != nil && c.Ownership == "managed" && web != nil && Within(c.Path, web.Options["node"]) {
			return "CodexApp 正在引用此 Node，请先解除引用"
		}
	}
	return ""
}

func (m *uiModel) rollbackAvailable() bool {
	entries, _ := os.ReadDir(filepath.Join(m.app.P.Data, "history"))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(m.app.P.Data, "history", e.Name()))
		var tx Transaction
		if err == nil && json.Unmarshal(b, &tx) == nil && tx.Action == "update" {
			return true
		}
	}
	return false
}

func (m *uiModel) serviceBlock(action string) string {
	if !m.app.S.Installed {
		return "核心后台尚未部署"
	}
	if hasPending(m.app.P.Data) {
		return "先恢复未完成事务"
	}
	if m.app.S.Backend == "systemd" && !m.inventory.Systemd {
		return "无法连接 systemd 用户管理器"
	}
	state := m.inventory.ServiceState
	if state == "maintenance" {
		return "维护进行中，请先完成或恢复该操作"
	}
	if (state == "stopping" || state == "stop_failed") && action != "stop" {
		return "上次停止尚未完成，请先选择停止并核实进程已退出"
	}
	if action == "start" {
		switch state {
		case "running", "starting", "retrying":
			return "后台" + serviceStateLabel(state) + "，无需重复启动"
		}
	} else {
		switch state {
		case "stopped", "exhausted", "failed":
			return "后台" + serviceStateLabel(state) + "，请选择启动"
		}
	}
	return ""
}
