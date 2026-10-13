package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestComponentsHelperProcess(t *testing.T) {
	if os.Getenv("CODEX_BG_COMPONENT_HELPER") != "1" {
		return
	}
	var home, root string
	for i := 0; i < len(os.Args)-1; i++ {
		switch os.Args[i] {
		case "--home":
			home = os.Args[i+1]
		case "--runtime":
			root = os.Args[i+1]
		}
	}
	a, err := New(home, "", root)
	if err != nil {
		os.Exit(31)
	}
	a.P.Socket = os.Getenv("CODEX_BG_COMPONENT_TEST_SOCKET")
	if err = a.RunComponent(context.Background(), "codexapp"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(32)
	}
	os.Exit(0)
}

func TestComponentsDetachedCodexAppSurvivesCallerAndRestarts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node HTTP fixture unavailable")
	}
	a := componentTestApp(t)
	a.P.Socket = runtimeFixture(t, runtimeResponder("idle"))
	if err = a.componentDirs(); err != nil {
		t.Fatal(err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launcher := "#!/bin/sh\nexec " + shellQuote(testBinary) + " -test.run=^TestComponentsHelperProcess$ -- \"$@\"\n"
	if err = AtomicWrite(a.P.Binary(), []byte(launcher), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_BG_COMPONENT_HELPER", "1")
	t.Setenv("CODEX_BG_COMPONENT_TEST_SOCKET", a.P.Socket)
	root := a.componentRoot("codexapp")
	entry := filepath.Join(root, "node_modules", ".bin", "codexapp")
	fixture := `const http=require('node:http');const fs=require('node:fs');const cp=require('node:child_process');const a=process.argv;const child=cp.spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{stdio:'ignore'});fs.writeFileSync('grandchild.pid',String(child.pid));http.createServer((q,r)=>r.end('fixture')).listen(Number(a[a.indexOf('--port')+1]),'127.0.0.1');setInterval(()=>console.log('fixture-alive'),100);`
	if err = AtomicWrite(entry, []byte(fixture), 0700); err != nil {
		t.Fatal(err)
	}
	if err = AtomicWrite(filepath.Join(root, "password"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	listener.Close()
	c := &Component{Name: "codexapp", Path: root, Ownership: "managed", Status: "installed", Options: map[string]string{"node": node, "port": port}}
	a.S.Components["codexapp"] = c
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.StopComponent(context.Background(), "codexapp") })
	caller, cancel := context.WithCancel(context.Background())
	if err = a.StartComponent(caller, "codexapp"); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	var first componentProcess
	b, err := os.ReadFile(filepath.Join(root, "process.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &first); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	client := http.Client{Timeout: time.Second}
	response, err := client.Get("http://127.0.0.1:" + port)
	if err != nil {
		t.Fatalf("caller cancellation killed detached app: %v", err)
	}
	response.Body.Close()
	// Unexpected child death is restarted by the detached long-lived supervisor.
	if err = syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	var restarted componentProcess
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		b, _ = os.ReadFile(filepath.Join(root, "process.json"))
		_ = json.Unmarshal(b, &restarted)
		if restarted.PID != first.PID && componentProcessMatches(restarted) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if restarted.PID == first.PID || !componentOwnerMatches(restarted) {
		t.Fatal("detached supervisor did not restart child")
	}
	grandRaw, err := os.ReadFile(filepath.Join(root, "grandchild.pid"))
	if err != nil {
		t.Fatal(err)
	}
	grandPID, _ := strconv.Atoi(strings.TrimSpace(string(grandRaw)))
	if err = a.StopComponent(context.Background(), "codexapp"); err != nil {
		t.Fatal(err)
	}
	if componentOwnerMatches(restarted) || componentProcessMatches(restarted) {
		t.Fatal("managed processes remain after stop")
	}
	if _, _, err = componentIdentity(grandPID); err == nil {
		t.Fatal("child process group descendants remain after stop")
	}
	if _, err = os.Stat(filepath.Join(root, "process.json")); !os.IsNotExist(err) {
		t.Fatal("PID record not cleaned")
	}
	logs, err := os.ReadFile(filepath.Join(a.P.Data, "logs", "codexapp.log"))
	if err != nil || !strings.Contains(string(logs), "fixture-alive") {
		t.Fatal("detached logs missing")
	}
}
