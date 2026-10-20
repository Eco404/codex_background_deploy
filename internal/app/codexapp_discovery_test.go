package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const codexAppTestHelp = "Usage: codexui [options]\n--port <port>\n--password <pass>\n--no-login\n--no-tunnel\n--no-open\n"

func codexAppNPMFixture(t *testing.T, home string) (link, entry, node, pkg string) {
	t.Helper()
	base := filepath.Join(home, "apps/codexapp/node_modules")
	entry = filepath.Join(base, "codexapp/dist-cli/index.js")
	pkg = filepath.Join(base, "codexapp/package.json")
	link = filepath.Join(base, ".bin/codexapp")
	node = filepath.Join(home, ".nvm/versions/node/v24.11.0/bin/node")
	discoveryFixture(t, entry, "#!/usr/bin/env node\n// fixture: deliberately does not support --version\n")
	discoveryFixture(t, pkg, `{"name":"codexapp","version":"0.1.90","bin":{"codexapp":"dist-cli/index.js","codexui":"dist-cli/index.js"}}`)
	discoveryFixture(t, node, "node runtime")
	if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../codexapp/dist-cli/index.js", link); err != nil {
		t.Fatal(err)
	}
	return
}

func TestCodexAppNPMDiscoveryAndReuseWithoutVersionOrNVMAlias(t *testing.T) {
	a := componentTestApp(t)
	link, entry, node, pkg := codexAppNPMFixture(t, a.P.Home)
	oldNode := filepath.Join(a.P.Home, "bin/node")
	discoveryFixture(t, oldNode, "node 18 fixture")
	t.Setenv("PATH", filepath.Dir(oldNode))
	t.Setenv("NVM_DIR", filepath.Join(a.P.Home, ".nvm"))
	discoveryFixture(t, filepath.Join(a.P.Home, ".nvm/alias/default"), "nonexistent-version")
	legacyHook := filepath.Join(a.P.Home, "entrypoint.d/10-start-codexapp.sh")
	discoveryFixture(t, legacyHook, "export CODEXUI_CODEX_COMMAND=\"$HOME/apps/codex-daemon-bridge/codex\"\n")
	beforeEntry, _ := os.ReadFile(entry)
	beforePkg, _ := os.ReadFile(pkg)
	helpCalls := 0
	a.RunCommand = func(ctx context.Context, name string, args, env []string) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 3100*time.Millisecond {
			t.Fatal("discovery command is not bounded")
		}
		if name == entry || name == link {
			t.Fatal("CodexApp invoked through its PATH-dependent shebang")
		}
		if name == oldNode {
			if strings.Join(args, " ") != "--version" {
				t.Fatal("incompatible PATH Node used for CodexApp")
			}
			return []byte("v18.20.0"), nil
		}
		if name == node && strings.Join(args, " ") == "--version" {
			return []byte("v24.11.0"), nil
		}
		if name == node && len(args) == 2 && args[0] == entry && args[1] == "--help" {
			helpCalls++
			if !strings.HasPrefix(testEnv(env, "PATH"), filepath.Dir(node)+":") {
				t.Fatal("help does not use the selected Node environment")
			}
			return []byte(codexAppTestHelp), nil
		}
		return nil, errors.New("unavailable unrelated capability")
	}
	inv, err := a.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	apps := inv.Candidates["codexapp"]
	if len(apps) != 1 || !apps[0].Compatible || apps[0].Version != "0.1.90" || apps[0].Path != entry {
		t.Fatalf("CodexApp not discovered: %+v", apps)
	}
	if strings.Count(strings.Join(inv.Legacy, "\n"), "CodexApp dependency: "+legacyHook) != 1 {
		t.Fatalf("legacy CodexApp dependency absent or duplicated: %v", inv.Legacy)
	}
	if _, err = os.Stat(a.P.Runtime); !os.IsNotExist(err) {
		t.Fatal("discovery created runtime data")
	}
	if _, err = os.Stat(a.P.Data); !os.IsNotExist(err) {
		t.Fatal("discovery created maintenance data")
	}
	if err = a.reuseComponent(context.Background(), "codexapp", ComponentOptions{Path: link}); err != nil {
		t.Fatal(err)
	}
	c := a.S.Components["codexapp"]
	if c.Ownership != "reused" || c.Version != "0.1.90" || c.Options["node"] != node || c.Status != "available" {
		t.Fatalf("bad reusable registration: %+v", c)
	}
	if err = a.removeComponent(context.Background(), "codexapp"); err != nil {
		t.Fatal(err)
	}
	afterEntry, _ := os.ReadFile(entry)
	afterPkg, _ := os.ReadFile(pkg)
	if string(beforeEntry) != string(afterEntry) || string(beforePkg) != string(afterPkg) || !executable(link) || helpCalls != 2 {
		t.Fatal("external CodexApp changed or help was not checked during both discovery and reuse")
	}
}

func TestCodexAppRejectsUnassociatedOrMalformedPackageWithoutExecution(t *testing.T) {
	for _, tc := range []struct{ name, metadata string }{
		{"unrelated", `{"name":"another-package","version":"0.1.90","bin":{"codexapp":"dist-cli/index.js"}}`},
		{"wrong-entry", `{"name":"codexapp","version":"0.1.90","bin":{"codexapp":"different.js"}}`},
		{"missing-entry", `{"name":"codexapp","version":"0.1.90"}`},
		{"outside-package", `{"name":"codexapp","version":"0.1.90","bin":{"codexapp":"../outside.js"}}`},
		{"invalid-version", `{"name":"codexapp","version":"garbage 0.1.90","bin":{"codexapp":"dist-cli/index.js"}}`},
		{"malformed", `{"name":"codexapp",`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := componentTestApp(t)
			link, _, _, pkg := codexAppNPMFixture(t, a.P.Home)
			discoveryFixture(t, pkg, tc.metadata)
			a.RunCommand = func(context.Context, string, []string, []string) ([]byte, error) {
				t.Fatal("executed entry whose package identity was not established")
				return nil, nil
			}
			if _, err := a.probeCodexApp(context.Background(), link); err == nil {
				t.Fatal("invalid CodexApp accepted")
			}
		})
	}
}

func TestCodexAppHelpRequiredAndRegisteredNodeAuthoritative(t *testing.T) {
	for _, tc := range []struct{ name, nodeVersion, help string }{
		{"unsupported-node", "v18.20.0", codexAppTestHelp},
		{"missing-capability", "v24.11.0", "--port --password --no-login --no-open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := componentTestApp(t)
			link, entry, node, _ := codexAppNPMFixture(t, a.P.Home)
			a.S.Components["node"] = &Component{Name: "node", Ownership: "reused", Path: node, Status: "installed"}
			a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
				if name != node {
					t.Fatal("registered Node bypassed")
				}
				if len(args) == 1 && args[0] == "--version" {
					return []byte(tc.nodeVersion), nil
				}
				if args[0] != entry || tc.name == "unsupported-node" {
					t.Fatal("ran CodexApp with incompatible Node")
				}
				return []byte(tc.help), nil
			}
			if _, err := a.probeCodexApp(context.Background(), link); err == nil {
				t.Fatal("incompatible CodexApp accepted")
			}
		})
	}
}

func TestCodexAppProbeHonorsCancellation(t *testing.T) {
	a := componentTestApp(t)
	link, _, node, _ := codexAppNPMFixture(t, a.P.Home)
	a.S.Components["node"] = &Component{Name: "node", Ownership: "reused", Path: node}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	a.RunCommand = func(ctx context.Context, _ string, _, _ []string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if _, err := a.probeCodexApp(ctx, link); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestCodexAppNativeVersionFallbackAndUnknownJSRejection(t *testing.T) {
	a := componentTestApp(t)
	entry := filepath.Join(a.P.Home, "bin/codexapp")
	discoveryFixture(t, entry, "native fixture")
	a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
		if name != entry || strings.Join(args, " ") != "--version" {
			t.Fatal("unexpected native probe")
		}
		return []byte("codexapp 0.1.90"), nil
	}
	if version, err := a.probeCodexApp(context.Background(), entry); err != nil || version != "0.1.90" {
		t.Fatalf("native fallback: %s %v", version, err)
	}
	discoveryFixture(t, entry, "#!/usr/bin/env node\n")
	if _, err := a.probeCodexApp(context.Background(), entry); err == nil {
		t.Fatal("unassociated JavaScript was run")
	}
}

func TestCodexAppInstalledNPMReadOnlyProbe(t *testing.T) {
	entry := os.Getenv("CB_TEST_CODEXAPP")
	node := os.Getenv("CB_TEST_CODEXAPP_NODE")
	if entry == "" || node == "" {
		t.Skip("set CB_TEST_CODEXAPP and CB_TEST_CODEXAPP_NODE for installed CLI help validation")
	}
	a := componentTestApp(t)
	a.S.Components["node"] = &Component{Name: "node", Path: node, Ownership: "reused"}
	probe, err := a.probeCodexAppDetails(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Version == "" || probe.Entry == "" || probe.Node == "" {
		t.Fatalf("incomplete probe: %+v", probe)
	}
	t.Logf("validated CodexApp %s with %s using --help", probe.Version, probe.Node)
}
