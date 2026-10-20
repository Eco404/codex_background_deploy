package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const runtimeMaxRetries = 5

var errRuntimeSuperseded = errors.New("后台控制代际已变化")

type runtimeChild struct {
	PID    int    `json:"pid"`
	Start  string `json:"start"`
	BootID string `json:"boot_id,omitempty"`
}
type runtimeExit struct {
	Time          time.Time `json:"time"`
	Code          int       `json:"code"`
	Signal        string    `json:"signal,omitempty"`
	CoreDump      bool      `json:"core_dump,omitempty"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	Version       string    `json:"version"`
	Reason        string    `json:"reason"`
}

// This is runtime control, not disposable diagnostic data. It must survive
// manager/container restarts and unavailable installer maintenance directories.
type runtimeControl struct {
	Schema     int           `json:"schema"`
	Generation string        `json:"generation"`
	Desired    string        `json:"desired"`
	Mode       string        `json:"mode"`
	Operation  string        `json:"operation,omitempty"`
	Status     string        `json:"status"`
	Retries    int           `json:"retries"`
	NextRetry  time.Time     `json:"next_retry,omitempty"`
	Launch     bool          `json:"launch,omitempty"`
	Child      *runtimeChild `json:"child,omitempty"`
	LastExit   *runtimeExit  `json:"last_exit,omitempty"`
	Changed    time.Time     `json:"changed"`
}

func (a *App) AutoRestartEnabled() bool   { return a.S.AutoRestart == nil || *a.S.AutoRestart }
func (a *App) runtimeControlFile() string { return filepath.Join(a.P.Runtime, "run", "control.json") }

func (a *App) readRuntimeControl() (runtimeControl, error) {
	var c runtimeControl
	raw, err := os.ReadFile(a.runtimeControlFile())
	if errors.Is(err, os.ErrNotExist) {
		if a.S.Installed && a.S.AutoRestart != nil {
			return c, errors.New("后台控制状态缺失；保留停止状态，需恢复运行资料后再启动")
		}
		// Older Go deployments already restarted unconditionally. Missing state
		// is compatible only as the initial default, never as an implicit reset.
		return runtimeControl{Schema: 1, Generation: "legacy", Desired: "running", Mode: "normal", Status: "ready"}, nil
	}
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("后台控制状态损坏，未自动启动: %w", err)
	}
	if c.Schema != 1 || c.Generation == "" || (c.Desired != "running" && c.Desired != "stopped") || c.Retries < 0 || c.Retries > runtimeMaxRetries {
		return c, errors.New("后台控制状态无效，未自动启动")
	}
	switch c.Mode {
	case "normal", "installing", "migrating", "updating", "rolling_back", "recovering", "uninstalling":
	default:
		return c, errors.New("后台维护状态未知，未自动启动")
	}
	switch c.Status {
	case "ready", "starting", "running", "retrying", "exhausted", "failed", "stopped", "stopping", "stop_failed", "maintenance":
	default:
		return c, errors.New("后台运行状态未知，未自动启动")
	}
	return c, nil
}

// Separate from the long management lock: the supervisor must update status
// while installation holds that lock and waits for a healthy child. Generation
// checking prevents an older supervisor from overwriting a newer stop/upgrade.
func (a *App) updateRuntimeControl(generation string, change func(*runtimeControl) error) (runtimeControl, error) {
	var empty runtimeControl
	if _, err := os.Stat(a.P.Runtime); err != nil {
		return empty, err
	}
	dir := filepath.Join(a.P.Runtime, "run")
	if err := checkParents(dir); err != nil {
		return empty, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return empty, err
	}
	path := filepath.Join(dir, "control.lock")
	if err := checkParents(path); err != nil {
		return empty, err
	}
	deadline := time.Now().Add(3 * time.Second)
	var lock *os.File
	for {
		var err error
		lock, err = runtimeLock(path)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) || time.Now().After(deadline) {
			return empty, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer runtimeUnlock(lock)
	c, err := a.readRuntimeControl()
	if err != nil {
		return c, err
	}
	if generation != "" && c.Generation != generation {
		return c, errRuntimeSuperseded
	}
	if err := change(&c); err != nil {
		return c, err
	}
	c.Changed = time.Now().UTC()
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return c, err
	}
	return c, AtomicWrite(a.runtimeControlFile(), append(raw, '\n'), 0600)
}

func (a *App) setRuntimeIntent(desired, mode, operation string, reset bool) (runtimeControl, error) {
	return a.updateRuntimeControl("", func(c *runtimeControl) error {
		c.Generation, c.Desired, c.Mode, c.Operation = ID(), desired, mode, operation
		c.Launch = desired == "running"
		c.NextRetry = time.Time{}
		if reset {
			c.Retries = 0
		}
		if desired == "running" {
			c.Status = "starting"
		} else if mode != "normal" {
			c.Status = "maintenance"
		} else {
			c.Status = "stopping"
		}
		return nil
	})
}

func (a *App) finishRuntimeMaintenance(requireRunning bool) error {
	c, err := a.readRuntimeControl()
	if err != nil {
		return err
	}
	_, err = a.updateRuntimeControl(c.Generation, func(next *runtimeControl) error {
		if requireRunning && (next.Desired != "running" || next.Status == "failed" || next.Status == "exhausted") {
			return errors.New("维护期间后台已退出，不能将此次操作标记成功")
		}
		next.Mode, next.Operation, next.Launch = "normal", "", false
		if next.Desired == "stopped" {
			next.Status = "stopped"
		}
		return nil
	})
	return err
}

func (a *App) stopRuntimeFor(ctx context.Context, mode, operation string) error {
	c, err := a.setRuntimeIntent("stopped", mode, operation, false)
	if err != nil {
		return err
	}
	if err = a.Stop(ctx); err != nil {
		_, _ = a.updateRuntimeControl(c.Generation, func(next *runtimeControl) error { next.Status = "stop_failed"; return nil })
		return err
	}
	_, err = a.updateRuntimeControl(c.Generation, func(next *runtimeControl) error {
		next.Child = nil
		if next.Mode == "normal" {
			next.Status = "stopped"
		}
		return nil
	})
	return err
}

func (a *App) restoreRuntimeIntent(ctx context.Context, t *Transaction) error {
	previous := runtimeControl{Schema: 1, Desired: "running", Mode: "normal", Status: "ready"}
	if t.PreviousControl != nil {
		previous = *t.PreviousControl
	}
	if !t.Previous.Installed {
		previous.Desired, previous.Status = "stopped", "stopped"
	}
	return a.resumeRuntimeAfterMaintenance(ctx, &previous, "recovering", t.ID)
}

func (a *App) resumeRuntimeAfterMaintenance(ctx context.Context, prior *runtimeControl, mode, operation string) error {
	previous := runtimeControl{Schema: 1, Desired: "running", Mode: "normal", Status: "ready"}
	if prior != nil {
		previous = *prior
	}
	previous.Generation, previous.Child, previous.Launch = ID(), nil, false
	previous.Mode, previous.Operation = mode, operation
	previous.NextRetry = time.Time{}
	_, err := a.updateRuntimeControl("", func(c *runtimeControl) error { *c = previous; return nil })
	if err != nil {
		return err
	}
	if previous.Desired != "running" || previous.Status == "exhausted" || previous.Status == "failed" {
		return nil
	}
	// Recovery starts exactly the prior intended deployment. The retry budget
	// survives; a formerly stopped/exhausted deployment is never revived here.
	_, err = a.updateRuntimeControl(previous.Generation, func(c *runtimeControl) error {
		c.Status, c.Launch = "starting", true
		return nil
	})
	if err != nil {
		return err
	}
	return a.Start(ctx)
}

func (a *App) RuntimeServiceState(current string) string {
	if !a.S.Installed {
		return current
	}
	c, err := a.readRuntimeControl()
	if err != nil {
		return "unknown"
	}
	if c.Mode != "normal" {
		return "maintenance"
	}
	if c.Desired == "stopped" {
		if c.Status == "stopping" || c.Status == "stop_failed" {
			return c.Status
		}
		if current == "running" {
			return "stop_failed"
		}
		return "stopped"
	}
	switch c.Status {
	case "starting", "retrying", "exhausted", "failed":
		return c.Status
	default:
		return current
	}
}

func (a *App) runtimeKnownNotRunning() bool {
	c, err := a.readRuntimeControl()
	if err != nil {
		return false
	}
	if c.Desired != "stopped" && c.Status != "stopped" && c.Status != "failed" && c.Status != "exhausted" && c.Status != "retrying" {
		return false
	}
	if c.Child != nil {
		started, err := runtimeProcessStart(c.Child.PID)
		if err == nil && started == c.Child.Start {
			return false
		}
	}
	// Never bypass the busy check if there is an unknown listening socket.
	if _, err := os.Lstat(a.P.Socket); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	return true
}

// The image's startup hook preserves stopped, maintenance and exhausted states.
// Only an explicit Service(start/restart) resets a failed recovery budget.
func (a *App) AutoStart(ctx context.Context) error {
	unlock, err := a.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	if !a.S.Installed {
		return errors.New("后台尚未安装")
	}
	c, err := a.readRuntimeControl()
	if err != nil {
		return err
	}
	if hasPending(a.P.Data) || c.Desired != "running" || c.Mode != "normal" || c.Status == "exhausted" || c.Status == "failed" {
		fmt.Fprintf(a.Out, "保留后台状态 %s（%s），未自动启动；请检查状态后明确启动或恢复。\n", c.Status, c.Mode)
		return nil
	}
	return a.Start(ctx)
}
