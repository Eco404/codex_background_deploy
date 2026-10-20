package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Exit status is part of systemctl's state protocol: inactive/failed and
// disabled intentionally return nonzero. Transport, permission and unexpected
// execution errors must never be recorded as a known false state.
func migrationCommandExit(err error) int {
	if err == nil {
		return 0
	}
	var status interface{ ExitCode() int }
	if errors.As(err, &status) {
		return status.ExitCode()
	}
	return -1
}

func (a *App) inspectLegacyUnitState(ctx context.Context, name string) (LegacyUnit, error) {
	unit := LegacyUnit{Name: name}
	active, err := a.probe(ctx, "systemctl", "--user", "is-active", name)
	switch strings.TrimSpace(string(active)) {
	case "active":
		if migrationCommandExit(err) != 0 {
			return unit, fmt.Errorf("无法可靠读取旧服务 %s 的运行状态，无法保证恢复；未停止服务", name)
		}
		unit.Active = true
	case "inactive", "failed":
		if migrationCommandExit(err) != 3 {
			return unit, fmt.Errorf("旧服务 %s 的运行状态与 systemctl 退出状态不一致，无法保证恢复；未停止服务", name)
		}
	default:
		return unit, fmt.Errorf("旧服务 %s 的运行状态未知，无法保证恢复；未停止服务", name)
	}
	enabled, err := a.probe(ctx, "systemctl", "--user", "is-enabled", name)
	switch strings.TrimSpace(string(enabled)) {
	case "enabled":
		if migrationCommandExit(err) != 0 {
			return unit, fmt.Errorf("无法可靠读取旧服务 %s 的启用状态，无法保证恢复；未停止服务", name)
		}
		unit.Enabled = true
	case "disabled":
		if migrationCommandExit(err) != 1 {
			return unit, fmt.Errorf("旧服务 %s 的启用状态与 systemctl 退出状态不一致，无法保证恢复；未停止服务", name)
		}
	case "static":
		if migrationCommandExit(err) != 0 {
			return unit, fmt.Errorf("无法可靠读取旧服务 %s 的静态启用状态，无法保证恢复；未停止服务", name)
		}
	default:
		// States such as enabled-runtime, linked and masked require preserving
		// different link metadata. The current transaction cannot restore those.
		return unit, fmt.Errorf("旧服务 %s 的启用状态未知或无法完整恢复；未停止服务", name)
	}
	return unit, nil
}
