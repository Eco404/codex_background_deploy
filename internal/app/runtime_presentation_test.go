package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRuntimeStatusSnapshotAndDiagnosisRemainReadOnly(t *testing.T) {
	a := engineApp(t)
	a.S.Installed, a.S.Backend = true, "container"
	t.Setenv("PATH", filepath.Join(a.P.Home, "no-programs"))
	t.Setenv("NVM_DIR", filepath.Join(a.P.Home, "no-nvm"))
	a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		isProbe := name == "systemctl" && joined == "--user show-environment" || name == "sudo" && joined == "-n true" || joined == "--version" || joined == "app-server --help"
		if !isProbe {
			t.Fatalf("unexpected diagnostic subprocess: %s %v", name, args)
		}
		return nil, errors.New("not available")
	}
	now := time.Date(2026, 10, 20, 1, 44, 26, 0, time.FixedZone("CST", 8*60*60))
	c := runtimeControl{
		Schema: 1, Generation: "presentation-fixture", Desired: "running", Mode: "normal", Status: "retrying", Retries: 2,
		NextRetry: now.Add(4 * time.Second), LastExit: &runtimeExit{Time: now, Code: -1, Signal: "segmentation fault", CoreDump: true, UptimeSeconds: 37, Version: "fixture-v1", Reason: "unexpected exit"},
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	engineFile(t, a.runtimeControlFile(), string(raw))
	engineFile(t, filepath.Join(a.P.Runtime, "run/health.json"), `{"status":"stale duplicate"}`)
	before, err := os.ReadDir(filepath.Join(a.P.Runtime, "run"))
	if err != nil {
		t.Fatal(err)
	}
	inv, err := a.Discover(context.Background())
	if err != nil || inv.ServiceState != "retrying" || inv.RuntimeStatus == nil {
		t.Fatalf("recovery state absent from inventory: %+v %v", inv, err)
	}
	runtime := inv.RuntimeStatus
	if !runtime.AutoRestart || runtime.Retries != 2 || runtime.MaxRetries != 5 || runtime.NextRetry == nil || !runtime.NextRetry.Equal(c.NextRetry) || runtime.LastExit == nil || runtime.LastExit.Signal != c.LastExit.Signal {
		t.Fatalf("incomplete recovery evidence: %+v", runtime)
	}
	encoded, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"runtime_status":`, `"auto_restart":true`, `"retries":2`, `"next_retry":`, `"last_exit":`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("inspect JSON omitted %s: %s", want, encoded)
		}
	}
	diagnosis := a.Diagnose(context.Background())
	for _, want := range []string{"等待自动重试", "崩溃后自动重启：开启", "已用重试 2/5", "下次尝试：" + displayRuntimeTime(c.NextRetry), "segmentation fault", "运行 37 秒", "Codex fixture-v1", "模型请求: 未执行"} {
		if !strings.Contains(diagnosis, want) {
			t.Fatalf("diagnosis omitted %q: %s", want, diagnosis)
		}
	}
	if strings.Count(diagnosis, "崩溃后自动重启") != 1 || strings.Contains(diagnosis, "stale duplicate") || strings.Contains(diagnosis, "不健康") {
		t.Fatalf("retrying service was duplicated or incorrectly reported as unhealthy: %s", diagnosis)
	}
	after, err := os.ReadDir(filepath.Join(a.P.Runtime, "run"))
	if err != nil || len(before) != len(after) || engineRead(t, a.runtimeControlFile()) != string(raw) {
		t.Fatal("diagnosis modified runtime control")
	}
	if _, err := os.Stat(a.P.Data); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("diagnosis created maintenance data")
	}
}

func TestRuntimeStatusMissingAndCorruptControlNeverResetIt(t *testing.T) {
	a := engineApp(t)
	enabled := false
	a.S.AutoRestart = &enabled
	s := a.readRuntimeStatus()
	if s.Recorded || s.AutoRestart || s.ControlError != "" || !strings.Contains(s.Summary("stopped"), "运行意图尚无记录") {
		t.Fatalf("missing control misrepresented: %+v", s)
	}
	if _, err := os.Stat(a.P.Runtime); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read-only status created runtime")
	}
	engineFile(t, a.runtimeControlFile(), "broken control")
	s = a.readRuntimeStatus()
	if s.Recorded || s.ControlError == "" || !strings.Contains(s.Summary("unknown"), "控制状态异常") || strings.Contains(s.Summary("unknown"), "已用重试 0/5") {
		t.Fatal("corrupt control disguised as a fresh retry budget")
	}
	if engineRead(t, a.runtimeControlFile()) != "broken control" {
		t.Fatal("status repaired state implicitly")
	}
}

func TestUIHomeAndServiceExposeRecoveryEvidenceOnce(t *testing.T) {
	next := time.Date(2026, 10, 20, 1, 44, 30, 0, time.UTC)
	m := &uiModel{app: &App{S: State{Installed: true}}, width: 140, height: 45, inventory: Inventory{ServiceState: "retrying", RuntimeStatus: &RuntimeStatus{
		AutoRestart: true, Recorded: true, Desired: "running", Mode: "normal", Retries: 2, MaxRetries: 5, NextRetry: &next,
		LastExit: &runtimeExit{Time: next.Add(-4 * time.Second), Signal: "SIGSEGV", Reason: "unexpected exit", Version: "fixture"},
	}}}
	for _, show := range []func(){m.home, m.serviceMenu} {
		show()
		view := m.View()
		for _, want := range []string{"等待自动重试", "已用重试 2/5", "下次尝试", "SIGSEGV", "崩溃后自动重启：开启"} {
			if !strings.Contains(view, want) {
				t.Fatalf("%s omitted %s: %s", m.title, want, view)
			}
		}
		if strings.Count(view, "崩溃后自动重启：开启") != 1 {
			t.Fatal("policy displayed twice")
		}
	}
}

func TestRuntimeSummaryDoesNotClaimUnconfirmedStopSucceeded(t *testing.T) {
	s := &RuntimeStatus{AutoRestart: true, Recorded: true, Desired: "stopped", Mode: "normal", MaxRetries: 5}
	for _, state := range []string{"stopping", "stop_failed"} {
		text := s.Summary(state)
		if !strings.Contains(text, serviceStateLabel(state)) || !strings.Contains(text, "停止尚未确认完成") || strings.Contains(text, "后台状态：已停止") {
			t.Fatalf("unconfirmed stop misrepresented: %s", text)
		}
	}
}
