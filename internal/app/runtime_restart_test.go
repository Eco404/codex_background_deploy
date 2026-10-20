package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func restartFixture(t *testing.T, script string, enabled bool) (*App, string, runtimeRestartPolicy) {
	t.Helper()
	root := runtimeTestDir(t)
	a := &App{P: Paths{Home: root, Runtime: filepath.Join(root, "runtime"), Data: filepath.Join(root, "data"), Socket: filepath.Join(root, "codex.sock")}, S: State{AutoRestart: &enabled, Codex: Candidate{Version: "fixture-1"}}}
	for _, dir := range []string{filepath.Join(a.P.Runtime, "run"), filepath.Join(a.P.Data, "logs")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cli := filepath.Join(root, "codex")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := a.updateRuntimeControl("legacy", func(c *runtimeControl) error { c.Generation = "fixture"; c.Launch = true; return nil }); err != nil {
		t.Fatal(err)
	}
	p := runtimeRestartPolicy{poll: time.Millisecond, healthInterval: 5 * time.Millisecond, stable: 50 * time.Millisecond, backoff: func(int) time.Duration { return 5 * time.Millisecond }, health: func(context.Context) error { return errors.New("not ready") }}
	return a, cli, p
}

func readRestartControl(t *testing.T, a *App) runtimeControl {
	t.Helper()
	c, err := a.readRuntimeControl()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func waitRestartControl(t *testing.T, a *App, check func(runtimeControl) bool) runtimeControl {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c := readRestartControl(t, a); check(c) {
			return c
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("control condition not observed: %+v", readRestartControl(t, a))
	return runtimeControl{}
}

func TestRuntimeRecoveryBudgetPersistsAndCannotBeBypassed(t *testing.T) {
	a, cli, p := restartFixture(t, "exit 23", true)
	launches := filepath.Join(a.P.Home, "launches")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nprintf x >> "+shellQuote(launches)+"\nexit 23\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.superviseCodex(ctx, cli, "", a.P.Home, p); err != nil {
		t.Fatal(err)
	}
	c := readRestartControl(t, a)
	if c.Status != "exhausted" || c.Retries != 5 || c.Child != nil || c.LastExit == nil || c.LastExit.Code != 23 || c.LastExit.Version != "fixture-1" {
		t.Fatalf("wrong exhaustion evidence: %+v", c)
	}
	before, _ := os.ReadFile(a.runtimeControlFile())
	if err := a.superviseCodex(ctx, cli, "", a.P.Home, p); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(a.runtimeControlFile())
	if string(before) != string(after) {
		t.Fatal("outer supervisor restart bypassed exhaustion")
	}
	if !c.NextRetry.IsZero() {
		t.Fatal("exhaustion retained retry timer")
	}
	started, err := os.ReadFile(launches)
	if err != nil || string(started) != "xxxxxx" {
		t.Fatalf("expected initial launch plus exactly five recoveries: %q %v", started, err)
	}
}

func TestRuntimeRecoveryDisabledMakesOnlyInitialAttempt(t *testing.T) {
	a, cli, p := restartFixture(t, "exit 19", false)
	if err := a.superviseCodex(context.Background(), cli, "", a.P.Home, p); err != nil {
		t.Fatal(err)
	}
	c := readRestartControl(t, a)
	if c.Status != "failed" || c.Retries != 0 || c.LastExit == nil || c.LastExit.Code != 19 {
		t.Fatalf("disabled recovery: %+v", c)
	}
}

func TestRuntimeRecoveryLaunchFailureConsumesBudget(t *testing.T) {
	a, cli, p := restartFixture(t, "exit 0", true)
	if err := os.Remove(cli); err != nil {
		t.Fatal(err)
	}
	if err := a.superviseCodex(context.Background(), cli, "", a.P.Home, p); err != nil {
		t.Fatal(err)
	}
	c := readRestartControl(t, a)
	if c.Status != "exhausted" || c.Retries != 5 || c.LastExit.Reason != "start_failed" {
		t.Fatalf("launch failure: %+v", c)
	}
}

func TestRuntimeRecoveryMaintenanceAllowsOneExplicitLaunch(t *testing.T) {
	a, cli, p := restartFixture(t, "exit 17", true)
	_, err := a.updateRuntimeControl("fixture", func(c *runtimeControl) error { c.Mode = "updating"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := a.superviseCodex(context.Background(), cli, "", a.P.Home, p); err != nil {
		t.Fatal(err)
	}
	c := readRestartControl(t, a)
	if c.Status != "failed" || c.Retries != 0 || c.Launch || c.Mode != "updating" {
		t.Fatalf("maintenance retried: %+v", c)
	}
}

func TestRuntimeRecoveryStopInterruptsBackoffWithoutNewAttempt(t *testing.T) {
	a, cli, p := restartFixture(t, "exit 12", true)
	p.backoff = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.superviseCodex(ctx, cli, "", a.P.Home, p) }()
	waitRestartControl(t, a, func(c runtimeControl) bool { return c.Status == "retrying" })
	_, err := a.updateRuntimeControl("fixture", func(c *runtimeControl) error {
		c.Generation = "manual-stop"
		c.Desired = "stopped"
		c.Status = "stopped"
		c.NextRetry = time.Time{}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("backoff ignored stop")
	}
	c := readRestartControl(t, a)
	if c.Generation != "manual-stop" || c.Status != "stopped" || c.Retries != 0 {
		t.Fatalf("old supervisor overwrote stop: %+v", c)
	}
}

func TestRuntimeRecoveryCancellationDuringBackoffPreservesBudget(t *testing.T) {
	a, cli, p := restartFixture(t, "exit 12", true)
	p.backoff = func(int) time.Duration { return 50 * time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.superviseCodex(ctx, cli, "", a.P.Home, p) }()
	before := waitRestartControl(t, a, func(c runtimeControl) bool { return c.Status == "retrying" })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("backoff ignored cancellation")
	}
	c := readRestartControl(t, a)
	if c.Status != "retrying" || c.Child != nil || !c.NextRetry.Equal(before.NextRetry) || c.LastExit == nil {
		t.Fatalf("cancelled backoff changed recovery allowance: %+v", c)
	}
	p.backoff = func(int) time.Duration { return 5 * time.Millisecond }
	if err := a.superviseCodex(context.Background(), cli, "", a.P.Home, p); err != nil {
		t.Fatal(err)
	}
	c = readRestartControl(t, a)
	if c.Status != "exhausted" || c.Retries != 5 {
		t.Fatalf("restart after cancel bypassed budget: %+v", c)
	}
}

func TestRuntimeRecoveryCapturesSignalAndLastExitSummary(t *testing.T) {
	a, cli, p := restartFixture(t, "kill -TERM $$", false)
	if err := a.superviseCodex(context.Background(), cli, "", a.P.Home, p); err != nil {
		t.Fatal(err)
	}
	c := readRestartControl(t, a)
	if c.LastExit == nil || c.LastExit.Signal != "SIGTERM" || c.LastExit.Code != -1 {
		t.Fatalf("signal absent: %+v", c.LastExit)
	}
	data, err := os.ReadFile(filepath.Join(a.P.Runtime, "run", "health.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary struct {
		LastExit *runtimeExit `json:"last_exit"`
	}
	if err = json.Unmarshal(data, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.LastExit == nil || summary.LastExit.Signal != c.LastExit.Signal {
		t.Fatalf("summary lost exit evidence: %s", data)
	}
}

func TestRuntimeRecoverySupervisorRestartRetainsAllowance(t *testing.T) {
	a, cli, p := restartFixture(t, "exit 13", true)
	_, err := a.updateRuntimeControl("fixture", func(c *runtimeControl) error {
		c.Launch = false
		c.Status = "running"
		c.Retries = 5
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.superviseCodex(context.Background(), cli, "", a.P.Home, p); err != nil {
		t.Fatal(err)
	}
	c := readRestartControl(t, a)
	if c.Status != "exhausted" || c.Retries != 5 || c.LastExit == nil || c.LastExit.Reason != "supervisor_interrupted" {
		t.Fatalf("supervisor restart replenished allowance: %+v", c)
	}
}

func TestRuntimeRecoveryMaintenanceWithoutLaunchNeverStarts(t *testing.T) {
	a, cli, p := restartFixture(t, "exit 13", true)
	_, err := a.updateRuntimeControl("fixture", func(c *runtimeControl) error {
		c.Launch = false
		c.Status = "maintenance"
		c.Mode = "updating"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.superviseCodex(context.Background(), cli, "", a.P.Home, p); err != nil {
		t.Fatal(err)
	}
	c := readRestartControl(t, a)
	if c.Status != "maintenance" || c.LastExit != nil || c.Child != nil {
		t.Fatalf("maintenance launch was not explicitly requested: %+v", c)
	}
}

func TestRuntimeRecoveryHealthMustStayContinuousBeforeBudgetReset(t *testing.T) {
	a, cli, p := restartFixture(t, "while :; do sleep 1; done", true)
	p.stable = 80 * time.Millisecond
	var healthy atomic.Bool
	healthy.Store(true)
	p.health = func(context.Context) error {
		if healthy.Load() {
			return nil
		}
		return errors.New("probe failed")
	}
	_, err := a.updateRuntimeControl("fixture", func(c *runtimeControl) error { c.Retries = 3; return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.superviseCodex(ctx, cli, "", a.P.Home, p) }()
	initial := waitRestartControl(t, a, func(c runtimeControl) bool { return c.Status == "running" && c.Child != nil })
	time.Sleep(35 * time.Millisecond)
	healthy.Store(false)
	time.Sleep(20 * time.Millisecond)
	healthy.Store(true)
	time.Sleep(40 * time.Millisecond)
	c := readRestartControl(t, a)
	if c.Retries != 3 || c.Child == nil || c.Child.PID != initial.Child.PID {
		t.Fatalf("failed probe reset budget or restarted live child: %+v", c)
	}
	waitRestartControl(t, a, func(c runtimeControl) bool { return c.Retries == 0 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("child cleanup blocked")
	}
	c = readRestartControl(t, a)
	if c.Child != nil || c.Status != "running" {
		t.Fatalf("cancellation lost running intent or left child: %+v", c)
	}
	if _, err := runtimeProcessStart(initial.Child.PID); err == nil {
		t.Fatal("cancel left child process")
	}
}

func TestRuntimeRecoveryNewIntentStopsChildWithoutOverwritingControl(t *testing.T) {
	a, cli, p := restartFixture(t, "while :; do sleep 1; done", true)
	p.health = func(context.Context) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.superviseCodex(ctx, cli, "", a.P.Home, p) }()
	c := waitRestartControl(t, a, func(c runtimeControl) bool { return c.Status == "running" && c.Child != nil })
	_, err := a.updateRuntimeControl("fixture", func(c *runtimeControl) error {
		c.Generation = "upgrade"
		c.Mode = "updating"
		c.Desired = "stopped"
		c.Status = "maintenance"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("new intent did not stop old child")
	}
	current := readRestartControl(t, a)
	if current.Generation != "upgrade" || current.Status != "maintenance" || current.LastExit != nil {
		t.Fatalf("old child overwrote upgrade: %+v", current)
	}
	if _, err := runtimeProcessStart(c.Child.PID); err == nil {
		t.Fatal("old child survived")
	}
}

func TestRuntimeRecoveryRecordContainsNoCommandErrorDetails(t *testing.T) {
	a, cli, p := restartFixture(t, "exit 0", false)
	cli += "-private-secret"
	if err := a.superviseCodex(context.Background(), cli, "", a.P.Home, p); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(a.runtimeControlFile())
	if strings.Contains(string(raw), "private-secret") || strings.Contains(string(raw), a.P.Home) {
		t.Fatalf("raw process error persisted: %s", raw)
	}
}
