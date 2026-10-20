package app

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var errUpdateBusy = errors.New("更新等待空闲")

// UpdateWhenIdle waits without holding the management lock. Every probe reloads
// state, and Update performs its own lock, ownership, and final busy checks.
// The timeout bounds waiting only; an update already begun must be allowed to
// complete its normal transaction and recovery sequence.
func (a *App) UpdateWhenIdle(ctx context.Context, version string, timeout time.Duration) error {
	if timeout <= 0 {
		return errors.New("等待空闲的时限必须大于 0")
	}
	fmt.Fprintln(a.Out, "等待后台连续空闲 10 秒后更新；无法确认空闲时继续等待，超时不强制切换。")
	return waitUpdateIdle(ctx, timeout, 5*time.Second, 10*time.Second, func(ctx context.Context) (bool, error) {
		if err := a.Reload(); err != nil {
			return false, err
		}
		if !a.S.Installed {
			return false, errors.New("核心后台已卸载，取消等待更新")
		}
		if version != "" && a.S.Codex.Source != "managed" {
			return false, errors.New("外部 Codex 只复用，不自动升级")
		}
		if err := a.checkManaged(); err != nil {
			return false, err
		}
		if hasPending(a.P.Data) {
			return false, errors.New("存在未完成事务，取消等待更新；请先 recover")
		}
		control, err := a.readRuntimeControl()
		if err != nil {
			return false, err
		}
		if control.Mode != "normal" {
			return false, errors.New("后台进入维护状态，取消等待更新")
		}
		if a.runtimeKnownNotRunning() {
			return true, nil
		}
		busy, err := a.Busy(ctx)
		return err == nil && !busy, nil
	}, func() error {
		fmt.Fprintln(a.Out, "已观察到连续空闲，重新核验并执行正常更新。")
		return a.Update(ctx, version, false)
	})
}

func waitUpdateIdle(ctx context.Context, timeout, poll, stable time.Duration, idle func(context.Context) (bool, error), apply func() error) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var since time.Time
	for {
		if err := waitCtx.Err(); err != nil {
			return fmt.Errorf("等待空闲结束，更新未应用：%w", err)
		}
		ready, err := idle(waitCtx)
		if err != nil {
			return err
		}
		if ready {
			if since.IsZero() {
				since = time.Now()
			}
			if time.Since(since) >= stable {
				// Never use a timeout/cancellation as permission to switch.
				if err := waitCtx.Err(); err != nil {
					return fmt.Errorf("等待空闲结束，更新未应用：%w", err)
				}
				if err := apply(); !errors.Is(err, errUpdateBusy) {
					return err
				}
				since = time.Time{}
			}
		} else {
			since = time.Time{}
		}
		if err := runtimeWait(waitCtx, poll); err != nil {
			return fmt.Errorf("等待空闲结束，更新未应用：%w", err)
		}
	}
}
