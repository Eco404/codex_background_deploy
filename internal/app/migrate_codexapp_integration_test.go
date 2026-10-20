package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The copied test executable acts as the real manager entry for this isolated
// integration only. No other test or normal invocation enters this dispatch.
func TestMain(m *testing.M) {
	if os.Getenv("CODEX_BG_UPDATER_INTEGRATION_HELPER") == "1" {
		os.Exit(legacyUpdaterFixtureMain())
	}
	if os.Getenv("CODEX_BG_MIGRATION_INTEGRATION_HELPER") == "1" {
		if filepath.Base(os.Args[0]) == "codex-fixture" {
			os.Exit(migrationFakeCodexMain())
		}
		if len(os.Args) > 1 && os.Args[1] == "--home" {
			os.Exit(migrationManagerMain())
		}
	}
	os.Exit(m.Run())
}
func migrationManagerMain() int {
	var home, root string
	for i := 1; i+1 < len(os.Args); i++ {
		switch os.Args[i] {
		case "--home":
			home = os.Args[i+1]
		case "--runtime":
			root = os.Args[i+1]
		}
	}
	a, err := New(home, "", root)
	if err != nil {
		return 41
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch os.Args[len(os.Args)-1] {
	case "serve":
		err = a.Serve(ctx)
	case "codexapp":
		err = a.RunComponent(ctx, "codexapp")
	default:
		return 42
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 43
	}
	return 0
}
func migrationFakeCodexMain() int {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("codex-cli 0.163.0")
		return 0
	}
	if len(args) == 2 && args[0] == "app-server" && args[1] == "--help" {
		fmt.Println("--listen unix://socket")
		return 0
	}
	if len(args) != 3 || args[0] != "app-server" || args[1] != "--listen" || !strings.HasPrefix(args[2], "unix://") {
		return 44
	}
	socket := strings.TrimPrefix(args[2], "unix://")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return 45
	}
	defer listener.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		runtimeResponder("idle")(r.Context(), conn)
	})}
	go func() { <-ctx.Done(); _ = server.Close() }()
	_ = server.Serve(listener)
	return 0
}
func TestMigrationCodexAppTransaction(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("isolated HTTP integration requires existing Node")
	}
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "component_start_failure"
		}
		t.Run(name, func(t *testing.T) {
			a, opt := legacyCodexAppFixtureAt(t, engineApp(t))
			a.P.Socket = filepath.Join(a.P.Runtime, "run/app-server.sock")
			a.RunCommand = nil
			t.Setenv("CODEX_BG_MIGRATION_INTEGRATION_HELPER", "1")
			t.Setenv("NVM_DIR", filepath.Join(a.P.Home, ".nvm"))
			old, node := startLegacyCodexAppFixture(t, a)
			t.Setenv("PATH", filepath.Dir(node)+":/usr/bin:/bin")
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
				defer cancel()
				if a.S.Installed {
					_ = a.Stop(ctx)
				}
				candidates, _ := a.legacyCodexAppProcesses(filepath.Join(a.P.Home, "apps/codexapp/node_modules/.bin/codexapp"))
				for _, p := range candidates {
					if p.PID != old.Process.Pid && legacyCodexAppSameProcess(p) {
						process, _ := os.FindProcess(p.PID)
						_ = process.Kill()
					}
				}
			})
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			bin, err := os.ReadFile(exe)
			if err != nil {
				t.Fatal(err)
			}
			opt.CodexPath = filepath.Join(a.P.Home, "fixture/codex-fixture")
			if err = AtomicWrite(opt.CodexPath, bin, 0700); err != nil {
				t.Fatal(err)
			}
			ex := engineExtension(t, a)
			opt.Roots = []string{ex.Root}
			opt.EntrypointDir = filepath.Join(a.P.Home, "entrypoint.d")
			oldHook := filepath.Join(opt.EntrypointDir, "10-start-codexapp.sh")
			oldPassword := filepath.Join(a.P.Home, ".config/codexapp/webui-password")
			// Modify the inert application before planning, while preserving the old
			// live instance. The failure only applies to its new managed bridge.
			if fail {
				entry := filepath.Join(a.P.Home, "apps/codexapp/node_modules/codexapp/cli.js")
				script, err := os.ReadFile(entry)
				if err != nil {
					t.Fatal(err)
				}
				needle := "const http=require('http');"
				replacement := "if(process.env.CODEXUI_CODEX_COMMAND===process.env.CODEX_BG_TEST_NEW_BRIDGE) process.exit(7);\n" + needle
				script = []byte(strings.Replace(string(script), needle, replacement, 1))
				if err = os.WriteFile(entry, script, 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CODEX_BG_TEST_NEW_BRIDGE", filepath.Join(a.P.Runtime, "codex"))
			}
			var output bytes.Buffer
			a.Out = &output
			a.Err = &output
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
			defer cancel()
			err = a.Migrate(ctx, opt)
			if fail {
				if err == nil || !strings.Contains(err.Error(), "readiness timeout") {
					t.Fatalf("missing startup failure: %v\n%s", err, output.String())
				}
				if strings.Contains(err.Error(), "恢复未完成") {
					t.Fatalf("rollback incomplete: %v\n%s", err, output.String())
				}
				if a.S.Installed || a.S.Components["codexapp"] != nil {
					t.Fatal("failed migration retained core/component state")
				}
				if _, e := os.Stat(a.componentRoot("codexapp")); !os.IsNotExist(e) {
					t.Fatal("failed component runtime not removed")
				}
				if b, e := os.ReadFile(oldHook); e != nil || !bytes.Equal(b, legacyCodexAppHook) {
					t.Fatal("old startup hook not restored")
				}
				if engineRead(t, ex.Entry) != "original-v1" {
					t.Fatal("extension not restored after failed CodexApp handoff")
				}
				if _, e := os.Stat(a.P.Binary()); !os.IsNotExist(e) {
					t.Fatal("failed core binary retained")
				}
				candidates, e := a.legacyCodexAppProcesses(filepath.Join(a.P.Home, "apps/codexapp/node_modules/.bin/codexapp"))
				if e != nil || len(candidates) != 1 || candidates[0].PID == old.Process.Pid {
					t.Fatalf("old CodexApp not restored: %v %+v", e, candidates)
				}
			} else {
				if err != nil {
					t.Fatalf("migration failed: %v\n%s", err, output.String())
				}
				if !a.S.Installed || a.S.Components["codexapp"] == nil {
					t.Fatal("new deployment not committed")
				}
				if _, e := os.Stat(oldHook); !os.IsNotExist(e) {
					t.Fatal("old hook retained after commit")
				}
				if !strings.Contains(engineRead(t, ex.Entry), Marker) {
					t.Fatal("selected IDE not attached")
				}
				password, e := os.ReadFile(a.CodexAppPasswordPath())
				if e != nil || string(password) != "test-password-kept\n" {
					t.Fatal("managed credential rotated")
				}
				if e := a.Health(ctx); e != nil {
					t.Fatal("new core unhealthy", e)
				}
				if e := a.removeComponent(ctx, "codexapp"); e != nil {
					t.Fatal(e)
				}
				if e := a.Uninstall(ctx, true); e != nil {
					t.Fatal(e)
				}
				if engineRead(t, ex.Entry) != "original-v1" {
					t.Fatal("uninstall did not restore selected extension")
				}
				if _, e := os.Stat(oldHook); !os.IsNotExist(e) {
					t.Fatal("uninstall resurrected old hook without backend")
				}
			}
			// All processes associated with the newly created manager and fake core
			// must be gone after failed handoff, or after successful uninstall.
			dirs, e := os.ReadDir("/proc")
			if e != nil {
				t.Fatal(e)
			}
			for _, dir := range dirs {
				pid, e := strconv.Atoi(dir.Name())
				if e != nil || pid == os.Getpid() {
					continue
				}
				raw, e := os.ReadFile(filepath.Join("/proc", dir.Name(), "cmdline"))
				if e != nil || len(raw) == 0 {
					continue
				}
				args := strings.Split(string(raw), "\x00")
				if args[0] == a.P.Binary() || args[0] == opt.CodexPath {
					t.Fatalf("migration left new runtime process %d", pid)
				}
			}
			if hasPending(a.P.Data) {
				t.Fatal("completed migration or rollback left pending transaction")
			}
			if engineRead(t, oldPassword) != "test-password-kept\n" {
				t.Fatal("external credential changed")
			}
			if a.S.Components["node"] == nil || a.S.Components["node"].Path != node {
				t.Fatal("external Node registration not preserved")
			}
			if !executable(filepath.Join(a.P.Home, "apps/codexapp/node_modules/.bin/codexapp")) {
				t.Fatal("external CodexApp installation removed")
			}
			history, err := os.ReadDir(filepath.Join(a.P.Data, "history"))
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range history {
				if !strings.HasSuffix(item.Name(), ".json") {
					continue
				}
				raw, e := os.ReadFile(filepath.Join(a.P.Data, "history", item.Name()))
				if e != nil {
					t.Fatal(e)
				}
				if bytes.Contains(raw, []byte("test-password-kept")) {
					t.Fatal("credential leaked in history")
				}
				var tx Transaction
				if json.Unmarshal(raw, &tx) == nil && tx.LegacyCodexApp != nil && tx.LegacyCodexApp.HookBackupID == "" {
					t.Fatal("handoff history lost original hook backup")
				}
			}
		})
	}
}
