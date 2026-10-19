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
		if !installingCore && m.inventory.ServiceState == "stopped" {
			return "请先启动核心后台"
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
		if m.inventory.ServiceState == "stopped" {
			return "请先启动核心后台"
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
	if action == "start" && m.inventory.ServiceState == "running" {
		return "后台已在运行"
	}
	if action != "start" && m.inventory.ServiceState == "stopped" {
		return "后台已停止，请选择启动"
	}
	return ""
}
