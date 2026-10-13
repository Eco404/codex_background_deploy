package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func (a *App) codexappUnit() string {
	return "[Unit]\nDescription=CodexApp optional Web UI\nAfter=codex-bg.service\nRequires=codex-bg.service\n[Service]\nType=simple\nExecStart=" + systemdComponentQuote(a.P.Binary()) + " --home " + systemdComponentQuote(a.P.Home) + " --runtime " + systemdComponentQuote(a.P.Runtime) + " component-run codexapp\nStandardOutput=null\nStandardError=null\nRestart=on-failure\n[Install]\nWantedBy=default.target\n"
}
func componentOwnerMatches(p componentProcess) bool {
	if p.OwnerPID <= 1 || p.OwnerStart == "" || p.OwnerExecutable == "" {
		return false
	}
	return componentProcessMatches(componentProcess{PID: p.OwnerPID, Start: p.OwnerStart, Executable: p.OwnerExecutable})
}
func (a *App) waitCodexAppReady(ctx context.Context, c *Component, port int) error {
	client := http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var p componentProcess
		b, e := os.ReadFile(filepath.Join(a.codexappRuntime(c), "process.json"))
		if e == nil && json.Unmarshal(b, &p) == nil && componentOwnerMatches(p) && componentProcessMatches(p) {
			r, e := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
			if e == nil {
				r.Body.Close()
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("CodexApp HTTP readiness timeout; inspect installer-data/logs/codexapp.log")
}
func (a *App) launchCodexAppDetached(ctx context.Context, c *Component, port int) error {
	cmd := exec.Command(a.P.Binary(), "--home", a.P.Home, "--runtime", a.P.Runtime, "component-run", "codexapp")
	cmd.Env = a.componentEnv(nil)
	cmd.Dir = a.codexappRuntime(c)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// nil stdio maps to /dev/null. No parent's pipe or cancellation context escapes.
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := cmd.Process.Release(); err != nil {
		return err
	}
	return a.waitCodexAppReady(ctx, c, port)
}
func (a *App) superviseCodexApp(parent context.Context, c *Component, node string, args []string, env map[string]string) error {
	ctx, cancel := signal.NotifyContext(parent, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	root := a.codexappRuntime(c)
	lock, err := os.OpenFile(filepath.Join(root, "supervisor.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("CodexApp supervisor already running")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ownerStart, _, err := componentIdentity(os.Getpid())
	if err != nil {
		return err
	}
	ownerExe, err := os.Executable()
	if err != nil {
		return err
	}
	recordPath := filepath.Join(root, "process.json")
	defer func() {
		var p componentProcess
		if b, e := os.ReadFile(recordPath); e == nil && json.Unmarshal(b, &p) == nil && p.OwnerPID == os.Getpid() && p.OwnerStart == ownerStart {
			_ = os.Remove(recordPath)
		}
	}()
	log := a.RuntimeLog("codexapp")
	defer log.Close()
	backoff := time.Second
	for {
		if err = ctx.Err(); err != nil {
			return nil
		}
		cmd := exec.Command(node, args...)
		cmd.Env = a.componentEnv(env)
		cmd.Dir = root
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdout = log
		cmd.Stderr = log
		if err = cmd.Start(); err != nil {
			fmt.Fprintf(log, "CodexApp child start failed: %v\n", err)
		} else {
			start, _, e := componentIdentity(cmd.Process.Pid)
			if e != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				_ = cmd.Wait()
				return e
			}
			p := componentProcess{PID: cmd.Process.Pid, Start: start, Executable: args[0], OwnerPID: os.Getpid(), OwnerStart: ownerStart, OwnerExecutable: ownerExe}
			if e = writeJSON(recordPath, p); e != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				_ = cmd.Wait()
				return e
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-ctx.Done():
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					<-done
				}
				// Also terminate remaining descendants in the child's owned process group.
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				return nil
			case e = <-done:
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				fmt.Fprintf(log, "CodexApp exited (%v); restart in %s\n", e, backoff)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff += time.Second
		}
	}
}
