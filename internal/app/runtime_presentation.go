package app

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// RuntimeStatus is a read-only presentation snapshot. The control file remains
// the authoritative runtime state; inspecting it must never repair or reset it.
type RuntimeStatus struct {
	AutoRestart  bool         `json:"auto_restart"`
	Recorded     bool         `json:"recorded"`
	Desired      string       `json:"desired,omitempty"`
	Mode         string       `json:"mode,omitempty"`
	Operation    string       `json:"operation,omitempty"`
	Retries      int          `json:"retries"`
	MaxRetries   int          `json:"max_retries"`
	NextRetry    *time.Time   `json:"next_retry,omitempty"`
	LastExit     *runtimeExit `json:"last_exit,omitempty"`
	ControlError string       `json:"control_error,omitempty"`
}

func (a *App) readRuntimeStatus() *RuntimeStatus {
	s := &RuntimeStatus{AutoRestart: a.AutoRestartEnabled(), MaxRetries: runtimeMaxRetries}
	c, err := a.readRuntimeControl()
	if err != nil {
		s.ControlError = err.Error()
		return s
	}
	_, statErr := os.Stat(a.runtimeControlFile())
	s.Recorded = statErr == nil
	if !s.Recorded {
		return s
	}
	s.Desired, s.Mode, s.Operation, s.Retries = c.Desired, c.Mode, c.Operation, c.Retries
	if !c.NextRetry.IsZero() {
		next := c.NextRetry
		s.NextRetry = &next
	}
	if c.LastExit != nil {
		exit := *c.LastExit
		s.LastExit = &exit
	}
	return s
}

func (s *RuntimeStatus) Summary(state string) string {
	if s == nil {
		return ""
	}
	var text strings.Builder
	fmt.Fprintf(&text, "后台状态：%s\n", serviceStateLabel(state))
	policy := "开启"
	if !s.AutoRestart {
		policy = "关闭"
	}
	fmt.Fprintf(&text, "崩溃后自动重启：%s", policy)
	if s.ControlError != "" {
		fmt.Fprintf(&text, "\n控制状态异常：%s；先检查或恢复，未重置重试额度", s.ControlError)
		return text.String()
	}
	if !s.Recorded {
		text.WriteString("\n运行意图尚无记录，首次启动后登记")
		return text.String()
	}
	fmt.Fprintf(&text, " · 已用重试 %d/%d", s.Retries, s.MaxRetries)
	intent := "运行"
	if s.Desired == "stopped" {
		intent = "停止"
	}
	mode := map[string]string{"normal": "正常", "installing": "安装", "migrating": "迁移", "updating": "升级", "rolling_back": "回退", "recovering": "恢复", "uninstalling": "卸载"}[s.Mode]
	if mode == "" {
		mode = s.Mode
	}
	fmt.Fprintf(&text, "\n运行意图：%s · 当前模式：%s", intent, mode)
	if s.Operation != "" {
		fmt.Fprintf(&text, " · 操作 %s", s.Operation)
	}
	if s.NextRetry != nil {
		fmt.Fprintf(&text, "\n下次尝试：%s", displayRuntimeTime(*s.NextRetry))
	}
	if exit := s.LastExit; exit != nil {
		fmt.Fprintf(&text, "\n最近退出：%s · 退出码 %d", displayRuntimeTime(exit.Time), exit.Code)
		if exit.Signal != "" {
			fmt.Fprintf(&text, " · 信号 %s", exit.Signal)
		}
		if exit.CoreDump {
			text.WriteString(" · core dump")
		}
		fmt.Fprintf(&text, " · 运行 %d 秒 · Codex %s", exit.UptimeSeconds, exit.Version)
		if exit.Reason != "" {
			fmt.Fprintf(&text, "\n退出原因：%s", exit.Reason)
		}
	}
	if state == "exhausted" || state == "failed" {
		text.WriteString("\n自动尝试已停止；检查原因后明确选择启动")
	}
	if state == "stopping" || state == "stop_failed" {
		text.WriteString("\n停止尚未确认完成；可再次选择停止，确认进程退出后才可启动")
	}
	return text.String()
}

func displayRuntimeTime(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04:05 -07:00")
}
