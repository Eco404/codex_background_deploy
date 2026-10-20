package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var errRuntimeIntentStopped = errors.New("runtime no longer permits launch")

// Durations are injectable only for isolated tests. Deployment has one bounded
// policy; restarting the supervisor cannot replenish its persisted allowance.
type runtimeRestartPolicy struct {
	poll, healthInterval, stable time.Duration
	backoff                      func(int) time.Duration
	health                       func(context.Context) error
}

func defaultRuntimeRestartPolicy() runtimeRestartPolicy {
	return runtimeRestartPolicy{poll: 250 * time.Millisecond, healthInterval: 5 * time.Second, stable: 2 * time.Minute, backoff: runtimeBackoff}
}

func runtimeMayLaunch(c runtimeControl) bool {
	return c.Desired == "running" && (c.Launch || (c.Mode == "normal" && c.Status != "exhausted" && c.Status != "failed"))
}

func runtimeRecordFailure(c *runtimeControl, exit runtimeExit, enabled bool, now time.Time, delay func(int) time.Duration) {
	c.Child = nil
	c.LastExit = &exit
	c.NextRetry = time.Time{}
	if c.Desired != "running" {
		return
	}
	if c.Mode != "normal" || !enabled {
		c.Status = "failed"
	} else if c.Retries >= runtimeMaxRetries {
		c.Status = "exhausted"
	} else {
		c.Status = "retrying"
		c.NextRetry = now.Add(delay(c.Retries + 1))
	}
}

func (a *App) runtimeControlStatus(c runtimeControl, pid int, health string) {
	current, err := a.readRuntimeControl()
	if err != nil || current.Generation != c.Generation {
		return
	}
	c = current
	fields := map[string]any{"generation": c.Generation, "status": c.Status, "retries": c.Retries, "max_retries": runtimeMaxRetries, "auto_restart": a.AutoRestartEnabled(), "desired": c.Desired, "mode": c.Mode, "operation": c.Operation, "last_exit": c.LastExit}
	if !c.NextRetry.IsZero() {
		fields["next_retry"] = c.NextRetry
	}
	if pid > 0 {
		fields["pid"] = pid
	}
	if health != "" {
		fields["health"] = health
	}
	a.runtimeStatus("health", fields)
}

func (a *App) runtimeExitEvent(exit runtimeExit) {
	// Do not persist raw exec/protocol error text: arguments and output can carry
	// credentials. Exit code, signal, version and timing are sufficient evidence.
	b, _ := json.Marshal(exit)
	a.event("codex_exit", string(b))
}

func (a *App) runtimeClassifyExit(exit runtimeExit, owned *runtimeSocketIdentity) runtimeExit {
	if owned == nil {
		if _, err := os.Lstat(a.P.Socket); err == nil {
			exit.Reason = "socket_conflict"
		}
	}
	return exit
}

func runtimeProcessExit(cmd *exec.Cmd, launched time.Time, version, reason string) runtimeExit {
	exit := runtimeExit{Time: time.Now().UTC(), Code: -1, Version: version, Reason: reason, UptimeSeconds: int64(time.Since(launched) / time.Second)}
	if cmd.ProcessState != nil {
		exit.Code = cmd.ProcessState.ExitCode()
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			exit.Signal = unix.SignalName(status.Signal())
			exit.CoreDump = status.CoreDump()
		}
	}
	return exit
}

func (a *App) superviseCodex(ctx context.Context, codexPath, codexHome, runtimeHome string, policy runtimeRestartPolicy) error {
	if policy.health == nil {
		policy.health = a.Health
	}
	control, err := a.readRuntimeControl()
	if err != nil {
		return err
	}
	if !runtimeMayLaunch(control) {
		a.runtimeControlStatus(control, 0, "")
		return nil
	}
	generation := control.Generation
	defer func() {
		if ctx.Err() == nil {
			return
		}
		// Cancellation of this supervisor is not a managed stop. Preserve the
		// running/retrying intent and its timer: an outer restart must consume
		// the same recovery budget. Managed stop already changed generation.
		current, writeErr := a.updateRuntimeControl(generation, func(c *runtimeControl) error {
			c.Child = nil
			return nil
		})
		if writeErr == nil {
			a.runtimeControlStatus(current, 0, "")
		}
	}()
	if !control.Launch && control.Status == "retrying" && !a.AutoRestartEnabled() {
		control, err = a.updateRuntimeControl(generation, func(c *runtimeControl) error {
			c.Status, c.NextRetry = "failed", time.Time{}
			return nil
		})
		if err != nil {
			return runtimeSupervisorError(err)
		}
		a.runtimeControlStatus(control, 0, "")
		return nil
	}
	// An outer supervisor restarting us is not a new user start. Preserve and
	// charge the same allowance even if the previous process died before Wait.
	if !control.Launch && (control.Status == "running" || control.Status == "starting") {
		exit := runtimeExit{Time: time.Now().UTC(), Code: -1, Version: a.S.Codex.Version, Reason: "supervisor_interrupted"}
		control, err = a.updateRuntimeControl(generation, func(c *runtimeControl) error {
			runtimeRecordFailure(c, exit, a.AutoRestartEnabled(), time.Now(), policy.backoff)
			return nil
		})
		if err != nil {
			return runtimeSupervisorError(err)
		}
		a.runtimeExitEvent(exit)
		a.runtimeControlStatus(control, 0, "")
	}
	for {
		control, err = a.waitRuntimeLaunch(ctx, generation, policy.poll)
		if err != nil {
			return runtimeSupervisorError(err)
		}
		control, err = a.updateRuntimeControl(generation, func(c *runtimeControl) error {
			if !runtimeMayLaunch(*c) {
				return errRuntimeIntentStopped
			}
			if !c.Launch && c.Status == "retrying" {
				if !a.AutoRestartEnabled() || c.Retries >= runtimeMaxRetries {
					return errRuntimeIntentStopped
				}
				// Reserve before exec, so a crash between reservation and launch
				// cannot create an uncounted automatic attempt.
				c.Retries++
			}
			c.Launch = false
			c.Status = "starting"
			c.Child = nil
			c.NextRetry = time.Time{}
			return nil
		})
		if err != nil {
			return runtimeSupervisorError(err)
		}
		a.runtimeControlStatus(control, 0, "starting")
		exit, interrupted, err := a.runSupervisedCodex(ctx, generation, codexPath, codexHome, runtimeHome, policy)
		if err != nil {
			return runtimeSupervisorError(err)
		}
		if interrupted {
			return nil
		}
		a.runtimeExitEvent(exit)
		control, err = a.updateRuntimeControl(generation, func(c *runtimeControl) error {
			runtimeRecordFailure(c, exit, a.AutoRestartEnabled() && exit.Reason != "socket_conflict", time.Now(), policy.backoff)
			return nil
		})
		if err != nil {
			return runtimeSupervisorError(err)
		}
		a.runtimeControlStatus(control, 0, "")
		if control.Status == "exhausted" || control.Status == "failed" {
			a.event("codex_recovery_stopped", control.Status)
			return nil // A systemd Restart=on-failure must not bypass the limit.
		}
	}
}

func runtimeSupervisorError(err error) error {
	if errors.Is(err, errRuntimeSuperseded) || errors.Is(err, errRuntimeIntentStopped) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

func (a *App) waitRuntimeLaunch(ctx context.Context, generation string, poll time.Duration) (runtimeControl, error) {
	for {
		if err := ctx.Err(); err != nil {
			return runtimeControl{}, err
		}
		c, err := a.readRuntimeControl()
		if err != nil {
			return c, err
		}
		if c.Generation != generation {
			return c, errRuntimeSuperseded
		}
		if !runtimeMayLaunch(c) {
			return c, errRuntimeIntentStopped
		}
		if c.Launch || c.NextRetry.IsZero() || !time.Now().Before(c.NextRetry) {
			return c, nil
		}
		delay := time.Until(c.NextRetry)
		if delay > poll {
			delay = poll
		}
		if err := runtimeWait(ctx, delay); err != nil {
			return c, err
		}
	}
}

type runtimeHealthResult struct {
	ok   bool
	time time.Time
}

func (a *App) runSupervisedCodex(ctx context.Context, generation, codexPath, codexHome, runtimeHome string, policy runtimeRestartPolicy) (runtimeExit, bool, error) {
	// Linux ties Pdeathsig to the launching thread. Keep that thread alive until
	// Wait, so a Go thread retirement cannot accidentally kill a healthy Codex.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	log := a.RuntimeLog("app-server")
	defer log.Close()
	cmd := exec.Command(codexPath, "app-server", "--listen", "unix://"+a.P.Socket)
	cmd.Env = runtimeEnv(runtimeHome, codexHome)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Pdeathsig: syscall.SIGKILL}
	cmd.WaitDelay = time.Second
	launched := time.Now()
	if _, err := os.Lstat(a.P.Socket); !errors.Is(err, os.ErrNotExist) {
		return runtimeProcessExit(cmd, launched, a.S.Codex.Version, "socket_conflict"), false, nil
	}
	if err := cmd.Start(); err != nil {
		return runtimeProcessExit(cmd, launched, a.S.Codex.Version, "start_failed"), false, nil
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var ownedSocket *runtimeSocketIdentity
	defer func() { a.runtimeRemoveChildSocket(ownedSocket) }()
	started, err := runtimeProcessStart(cmd.Process.Pid)
	if err != nil {
		// The child may legitimately exit before /proc is inspected. Wait below
		// still records its actual signal and exit code.
		select {
		case <-done:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			exit := runtimeProcessExit(cmd, launched, a.S.Codex.Version, "unexpected_exit")
			return a.runtimeClassifyExit(exit, nil), false, nil
		default:
			runtimeStopChild(cmd, done)
			return runtimeProcessExit(cmd, launched, a.S.Codex.Version, "identity_unavailable"), false, nil
		}
	}
	bootID, err := runtimeBootID()
	if err != nil {
		runtimeStopChild(cmd, done)
		return runtimeProcessExit(cmd, launched, a.S.Codex.Version, "identity_unavailable"), false, nil
	}
	control, err := a.updateRuntimeControl(generation, func(c *runtimeControl) error {
		if c.Desired != "running" {
			return errRuntimeIntentStopped
		}
		c.Child = &runtimeChild{PID: cmd.Process.Pid, Start: started, BootID: bootID}
		return nil
	})
	if err != nil {
		runtimeStopChild(cmd, done)
		return runtimeExit{}, true, err
	}
	child := runtimeChild{PID: cmd.Process.Pid, Start: started, BootID: bootID}
	proofWriteFailed := false
	persistSocketOwnership := func() {
		if ownedSocket == nil {
			return
		}
		proofErr := a.recordRuntimeSocketOwnership(generation, child, ownedSocket)
		if proofErr != nil && !proofWriteFailed {
			a.event("socket_ownership_unavailable", "Persistent socket ownership could not be refreshed; preserve the live child")
		}
		proofWriteFailed = proofErr != nil
	}
	ownedSocket = runtimeChildSocket(a.P.Socket, child)
	persistSocketOwnership()
	a.runtimeControlStatus(control, cmd.Process.Pid, "starting")
	probeCtx, cancel := context.WithCancel(ctx)
	healthDone := make(chan struct{})
	healthResults := make(chan runtimeHealthResult, 1)
	go func() {
		defer close(healthDone)
		for {
			ok := policy.health(probeCtx) == nil
			select {
			case healthResults <- runtimeHealthResult{ok: ok, time: time.Now()}:
			case <-probeCtx.Done():
				return
			}
			if runtimeWait(probeCtx, policy.healthInterval) != nil {
				return
			}
		}
	}()
	defer func() { cancel(); <-healthDone }()
	tick := time.NewTicker(policy.poll)
	defer tick.Stop()
	var healthySince time.Time
	for {
		select {
		case <-ctx.Done():
			if ownedSocket == nil {
				ownedSocket = runtimeChildSocket(a.P.Socket, child)
			}
			runtimeStopChild(cmd, done)
			return runtimeExit{}, true, nil
		case <-done:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			exit := runtimeProcessExit(cmd, launched, a.S.Codex.Version, "unexpected_exit")
			return a.runtimeClassifyExit(exit, ownedSocket), false, nil
		case <-tick.C:
			if ownedSocket == nil {
				ownedSocket = runtimeChildSocket(a.P.Socket, child)
				// Initial protocol setup can be slow or fail. Kernel ownership is
				// already sufficient to preserve recovery evidence for this child.
				persistSocketOwnership()
			}
			current, readErr := a.readRuntimeControl()
			if readErr != nil || current.Generation != generation || current.Desired != "running" {
				runtimeStopChild(cmd, done)
				if readErr == nil {
					readErr = errRuntimeSuperseded
				}
				return runtimeExit{}, true, readErr
			}
			control = current
		case result := <-healthResults:
			if ownedSocket == nil {
				ownedSocket = runtimeChildSocket(a.P.Socket, child)
			}
			if result.ok {
				if _, socketErr := os.Lstat(a.P.Socket); ownedSocket != nil && !runtimeSocketMatches(a.P.Socket, ownedSocket) || socketErr == nil && ownedSocket == nil {
					healthySince = time.Time{}
					a.runtimeControlStatus(control, cmd.Process.Pid, "ownership_unavailable")
					continue
				}
			}
			persistSocketOwnership()
			if !result.ok {
				healthySince = time.Time{}
				a.runtimeControlStatus(control, cmd.Process.Pid, "unavailable")
				continue // An unresponsive live process is never killed by health.
			}
			if healthySince.IsZero() {
				healthySince = result.time
			}
			stable := result.time.Sub(healthySince) >= policy.stable
			if control.Status != "running" || stable && control.Retries > 0 {
				control, err = a.updateRuntimeControl(generation, func(c *runtimeControl) error {
					if c.Desired != "running" {
						return errRuntimeIntentStopped
					}
					c.Status = "running"
					if stable {
						c.Retries = 0
					}
					return nil
				})
				if err != nil {
					runtimeStopChild(cmd, done)
					return runtimeExit{}, true, err
				}
			}
			a.runtimeControlStatus(control, cmd.Process.Pid, "ready")
		}
	}
}
