package app

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func extensionFixture(t *testing.T, a *App) string {
	t.Helper()
	dir := filepath.Join(a.P.Home, "source-extension")
	engineFile(t, filepath.Join(dir, "package.json"), `{"publisher":"openai","name":"chatgpt","version":"1.2.3"}`)
	engineFile(t, filepath.Join(dir, ".vsixmanifest"), fmt.Sprintf(`<PackageManifest><Metadata><Identity Id="chatgpt" Publisher="openai" Version="1.2.3" TargetPlatform="%s"/></Metadata></PackageManifest>`, nativeExtensionPlatform()))
	engineFile(t, filepath.Join(dir, strings.TrimPrefix(nativeExtensionEntries()[0], "extension/")), "native codex executable")
	return dir
}

func extensionVSIXFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "test.vsix")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	for name, body := range files {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0755)
		w, err := z.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err = errors.Join(z.Close(), f.Close()); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestExtensionRejectsUntrustedArchivesAndHookedSources(t *testing.T) {
	base := map[string]string{
		"extension/package.json":    `{"publisher":"openai","name":"chatgpt","version":"1.2.3"}`,
		"extension.vsixmanifest":    fmt.Sprintf(`<PackageManifest><Metadata><Identity Id="chatgpt" Publisher="openai" Version="1.2.3" TargetPlatform="%s"/></Metadata></PackageManifest>`, nativeExtensionPlatform()),
		nativeExtensionEntries()[0]: "original codex",
	}
	for _, tc := range []struct{ name, file, content string }{
		{"traversal", "../outside", "bad"},
		{"windows path", "extension/..\\outside", "bad"},
		{"other publisher", "extension/package.json", `{"publisher":"someone","name":"chatgpt","version":"1.2.3"}`},
		{"wrong platform", "extension.vsixmanifest", `<PackageManifest><Metadata><Identity Id="chatgpt" Publisher="openai" Version="1.2.3" TargetPlatform="win32-x64"/></Metadata></PackageManifest>`},
		{"hooked", nativeExtensionEntries()[0], "#!/bin/sh\n# " + Marker},
		{"legacy", nativeExtensionEntries()[0], "#!/bin/sh\n# codex-daemon-bridge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			for k, v := range base {
				files[k] = v
			}
			files[tc.file] = tc.content
			if _, err := validateExtensionVSIX(extensionVSIXFixture(t, files)); err == nil {
				t.Fatal("invalid VSIX accepted")
			}
		})
	}
	if _, err := validateExtensionVSIX(extensionVSIXFixture(t, base)); err != nil {
		t.Fatal(err)
	}
	a := engineApp(t)
	source := extensionFixture(t, a)
	engineFile(t, filepath.Join(source, "symlink-target"), "source")
	if err := os.Symlink("symlink-target", filepath.Join(source, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := packExtensionDirectory(context.Background(), source, filepath.Join(a.P.Home, "bad.vsix")); err == nil {
		t.Fatal("source symlink copied")
	}
}

func TestMarketplaceSelectsStablePlatformAndTrustedPublisher(t *testing.T) {
	response := `{"results":[{"extensions":[{"publisher":{"publisherName":"openai"},"extensionName":"chatgpt","versions":[
{"version":"99.0.0","targetPlatform":"linux-x64","properties":[{"key":"Microsoft.VisualStudio.Code.PreRelease","value":"true"}],"files":[{"assetType":"Microsoft.VisualStudio.Services.VSIXPackage","source":"https://openai.gallerycdn.vsassets.io/extensions/openai/chatgpt/99.0.0/123/Microsoft.VisualStudio.Services.VSIXPackage"}]},
{"version":"1.0.0","targetPlatform":"universal","files":[{"assetType":"Microsoft.VisualStudio.Services.VSIXPackage","source":"https://openai.gallerycdn.vsassets.io/extensions/openai/chatgpt/1.0.0/123/Microsoft.VisualStudio.Services.VSIXPackage"}]},
{"version":"2.0.0","targetPlatform":"linux-x64","files":[{"assetType":"Microsoft.VisualStudio.Services.VSIXPackage","source":"https://openai.gallerycdn.vsassets.io/extensions/openai/chatgpt/2.0.0/123/Microsoft.VisualStudio.Services.VSIXPackage"}]}
]}]}]}`
	selected, err := selectMarketplacePackage([]byte(response), "linux-x64")
	if err != nil || !strings.Contains(selected, "/2.0.0/") {
		t.Fatalf("wrong stable native package: %s %v", selected, err)
	}
	selected, err = selectMarketplacePackage([]byte(response), "linux-arm64")
	if err != nil || !strings.Contains(selected, "/1.0.0/") {
		t.Fatalf("universal fallback unavailable: %s %v", selected, err)
	}
	for _, raw := range []string{
		strings.ReplaceAll(response, `"publisherName":"openai"`, `"publisherName":"other"`),
		strings.ReplaceAll(response, "openai.gallerycdn.vsassets.io", "untrusted.example"),
		`{"results":[{"extensions":[{"publisher":{"publisherName":"openai"},"extensionName":"chatgpt","versions":[{"version":"99.0.0","targetPlatform":"linux-x64","properties":[{"key":"Microsoft.VisualStudio.Code.PreRelease","value":"true"}],"files":[{"assetType":"Microsoft.VisualStudio.Services.VSIXPackage","source":"https://openai.gallerycdn.vsassets.io/extensions/openai/chatgpt/99.0.0/123/Microsoft.VisualStudio.Services.VSIXPackage"}]}]}]}]}`,
	} {
		if _, err := selectMarketplacePackage([]byte(raw), "linux-x64"); err == nil {
			t.Fatal("untrusted or prerelease-only marketplace response accepted")
		}
	}
}

func extensionInstallerFixture(t *testing.T, a *App, fail, empty bool) (string, *int) {
	t.Helper()
	codeServer := filepath.Join(a.P.Home, "bin/code-server")
	engineFile(t, codeServer, "code-server fixture")
	calls := new(int)
	a.RunCommand = func(ctx context.Context, name string, args, env []string) ([]byte, error) {
		if name != codeServer {
			return nil, errors.New("unexpected command: " + name)
		}
		if len(args) == 1 && args[0] == "--version" {
			return []byte("4.100.0 fixture"), nil
		}
		*calls++
		if len(args) != 6 || args[0] != "--user-data-dir" || args[2] != "--extensions-dir" || args[4] != "--install-extension" {
			t.Fatalf("unsafe install arguments: %v", args)
		}
		if !Within(a.P.Data, args[1]) || !Within(a.P.Data, args[5]) {
			t.Fatalf("staging leaked outside maintenance data: %v", args)
		}
		for _, key := range []string{"TMPDIR", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
			found := false
			for _, kv := range env {
				if strings.HasPrefix(kv, key+"=") {
					found = Within(a.P.Data, strings.TrimPrefix(kv, key+"="))
				}
			}
			if !found {
				t.Fatalf("%s is not contained in maintenance data", key)
			}
		}
		if empty {
			return []byte("success without installing"), nil
		}
		z, err := zip.OpenReader(args[5])
		if err != nil {
			return nil, err
		}
		defer z.Close()
		dir := filepath.Join(args[3], "openai.chatgpt-1.2.3-"+nativeExtensionPlatform())
		for _, f := range z.File {
			if f.Name == "[Content_Types].xml" {
				continue
			}
			name := strings.TrimPrefix(f.Name, "extension/")
			if name == "extension.vsixmanifest" {
				name = ".vsixmanifest"
			}
			r, err := f.Open()
			if err != nil {
				return nil, err
			}
			b, err := io.ReadAll(r)
			r.Close()
			if err != nil {
				return nil, err
			}
			engineFile(t, filepath.Join(dir, name), string(b))
		}
		if fail {
			return []byte("installation interrupted"), errors.New("fixture failure")
		}
		return []byte("extension installed"), nil
	}
	return codeServer, calls
}

func TestExtensionInstallConfirmationContainmentAndExistingPreservation(t *testing.T) {
	a := engineApp(t)
	source := extensionFixture(t, a)
	codeServer, calls := extensionInstallerFixture(t, a, false, false)
	root := filepath.Join(a.P.Home, ".local/share/code-server/extensions")
	opt := ExtensionInstallOptions{Root: root, Source: source, CodeServer: codeServer, DryRun: true}
	if err := a.InstallExtension(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.P.Data); !os.IsNotExist(err) || *calls != 0 {
		t.Fatal("extension preview mutated files")
	}
	opt.DryRun = false
	if err := a.InstallExtension(context.Background(), opt); err == nil || *calls != 0 {
		t.Fatal("unconfirmed extension installed")
	}
	opt.Yes = true
	if err := a.InstallExtension(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || a.S.Installed {
		t.Fatal("independent extension install unexpectedly installed core")
	}
	if files, _ := os.ReadDir(filepath.Join(a.P.Data, "tmp")); len(files) != 0 {
		t.Fatal("extension staging retained")
	}
	receipts, _ := filepath.Glob(filepath.Join(a.P.Data, "history", "extension-*.json"))
	var receipt extensionReceipt
	if len(receipts) != 1 || json.Unmarshal([]byte(engineRead(t, receipts[0])), &receipt) != nil || receipt.Status != "installed-ide-owned" || len(receipt.PackageHash) != 64 {
		t.Fatalf("invalid provenance receipt: %+v", receipt)
	}
	if err := a.InstallExtension(context.Background(), opt); err == nil || *calls != 1 {
		t.Fatal("existing extension overwritten")
	}
	if got := engineRead(t, filepath.Join(source, strings.TrimPrefix(nativeExtensionEntries()[0], "extension/"))); got != "native codex executable" {
		t.Fatal("source extension modified")
	}
}

func TestExtensionFailurePreservesIDEFilesAndDoesNotClaimSuccess(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint("empty=", empty), func(t *testing.T) {
			a := engineApp(t)
			source := extensionFixture(t, a)
			codeServer, _ := extensionInstallerFixture(t, a, !empty, empty)
			root := filepath.Join(a.P.Home, "target-extensions")
			if err := a.InstallExtension(context.Background(), ExtensionInstallOptions{Root: root, Source: source, CodeServer: codeServer, Yes: true}); err == nil {
				t.Fatal("failed installation reported success")
			}
			if !empty {
				if matches, _ := filepath.Glob(filepath.Join(root, "openai.chatgpt-*")); len(matches) != 1 {
					t.Fatal("failure erased IDE-owned partial files")
				}
			}
			if files, _ := os.ReadDir(filepath.Join(a.P.Data, "tmp")); len(files) != 0 {
				t.Fatal("failed extension staging retained")
			}
			receipts, _ := filepath.Glob(filepath.Join(a.P.Data, "history", "extension-*.json"))
			if len(receipts) != 1 || !strings.Contains(engineRead(t, receipts[0]), "failed-preserved") {
				t.Fatal("failure provenance missing")
			}
		})
	}
}

func TestExtensionRootMustBeExplicitlySelectedForCore(t *testing.T) {
	a := engineApp(t)
	_, _, err := a.PlanInstall(context.Background(), InstallOptions{Backend: "container", Roots: []string{filepath.Join(a.P.Home, "selected")}, Extension: &ExtensionInstallOptions{Root: filepath.Join(a.P.Home, "different")}})
	if err == nil || !strings.Contains(err.Error(), "明确选择") {
		t.Fatalf("extension root scope not checked: %v", err)
	}
}

func TestExtensionSurvivesCoreRollbackAsIDEInstallation(t *testing.T) {
	a := engineApp(t)
	source := extensionFixture(t, a)
	codeServer, _ := extensionInstallerFixture(t, a, false, false)
	root := filepath.Join(a.P.Home, "target-extensions")
	err := a.installCore(context.Background(), InstallOptions{
		Backend: "container", CodexPath: filepath.Join(a.P.Home, "missing-cli"), Roots: []string{root}, Yes: true,
		Extension: &ExtensionInstallOptions{Root: root, Source: source, CodeServer: codeServer, Yes: true},
	})
	if err == nil || a.S.Installed || hasPending(a.P.Data) {
		t.Fatalf("core failure did not roll back: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(root, "openai.chatgpt-*"))
	if len(matches) != 1 {
		t.Fatal("core rollback removed the explicitly installed IDE extension")
	}
	if _, err = validateExtensionDirectory(matches[0]); err != nil {
		t.Fatalf("retained extension is damaged: %v", err)
	}
	receipts, _ := filepath.Glob(filepath.Join(a.P.Data, "history", "extension-*.json"))
	if len(receipts) != 1 || !strings.Contains(engineRead(t, receipts[0]), "installed-ide-owned") {
		t.Fatal("core rollback lost extension installation provenance")
	}
}

func TestDiscoverCodeServerAndServiceAvailability(t *testing.T) {
	a := engineApp(t)
	codeServer, _ := extensionInstallerFixture(t, a, false, false)
	t.Setenv("PATH", filepath.Dir(codeServer))
	inv, err := a.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Candidates["code-server"]) == 0 || inv.Candidates["code-server"][0].Path != codeServer || !inv.Candidates["code-server"][0].Compatible || inv.ServiceState != "" {
		t.Fatalf("bad discovery: %+v", inv)
	}
	a.S.Installed, a.S.Backend = true, "container"
	inv, err = a.Discover(context.Background())
	if err != nil || inv.ServiceState != "stopped" {
		t.Fatalf("missing supervisor should be stopped: %s %v", inv.ServiceState, err)
	}
	engineFile(t, a.runtimePIDFile(), "broken json")
	inv, err = a.Discover(context.Background())
	if err != nil || inv.ServiceState != "unknown" {
		t.Fatalf("corrupt supervisor should remain unknown: %s %v", inv.ServiceState, err)
	}
	for _, tc := range []struct{ state, want string }{{"active", "running"}, {"inactive", "stopped"}, {"failed", "stopped"}, {"activating", "unknown"}} {
		a.S.Backend = "systemd"
		a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
			if name == "systemctl" {
				return []byte(tc.state), nil
			}
			return []byte("4.100.0"), nil
		}
		inv, err = a.Discover(context.Background())
		if err != nil || inv.ServiceState != tc.want {
			t.Fatalf("%s -> %s, want %s (%v)", tc.state, inv.ServiceState, tc.want, err)
		}
	}
}

// Opt-in integration check: installs only a synthetic, inert extension into an
// isolated HOME and extensions root, never the user's actual editor profile.
func TestExtensionWithRealIsolatedCodeServer(t *testing.T) {
	codeServer := os.Getenv("CODEX_BG_TEST_CODE_SERVER")
	if codeServer == "" {
		t.Skip("set CODEX_BG_TEST_CODE_SERVER to a real code-server executable")
	}
	a := engineApp(t)
	a.RunCommand = nil
	source := extensionFixture(t, a)
	engineFile(t, filepath.Join(source, "package.json"), `{"publisher":"openai","name":"chatgpt","version":"1.2.3","displayName":"Inert installer fixture","engines":{"vscode":"^1.96.0"}}`)
	for _, key := range []string{"XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, filepath.Join(a.P.Home, strings.ToLower(key)))
	}
	t.Setenv("DO_NOT_TRACK", "1")
	t.Setenv("VSCODE_TELEMETRY_LEVEL", "off")
	root := filepath.Join(a.P.Home, "isolated-extensions")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := a.InstallExtension(ctx, ExtensionInstallOptions{Root: root, Source: source, CodeServer: codeServer, Yes: true}); err != nil {
		t.Fatal(err)
	}
	installed, err := scanRoot(root)
	if err != nil || len(installed) == 0 || !executable(installed[0].Entry) {
		t.Fatalf("real code-server did not preserve executable native entry: %v %+v", err, installed)
	}
	if _, err = os.Stat(filepath.Join(root, "extensions.json")); err != nil {
		t.Fatalf("real code-server did not register extension: %v", err)
	}
}

// This checks an actual vendor package without invoking its code or installing
// it. Optional download uses the installer's normal HTTP/proxy/staging path.
func TestExtensionOfficialVSIXReadOnly(t *testing.T) {
	file := os.Getenv("CODEX_BG_TEST_VSIX")
	if file == "" {
		t.Skip("set CODEX_BG_TEST_VSIX to an official Codex VSIX file")
	}
	if _, err := os.Stat(file); os.IsNotExist(err) && os.Getenv("CODEX_BG_TEST_VSIX_DOWNLOAD") == "1" {
		a := engineApp(t)
		a.RunCommand, a.Out = nil, os.Stdout
		if proxy := os.Getenv("CODEX_BG_TEST_VSIX_PROXY"); proxy != "" {
			if proxy == "none" {
				a.Network.ProxyMode = "none"
			} else {
				a.Network.ProxyMode, a.Network.Proxy = "custom", proxy
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		sourceURL, err := a.marketplaceExtensionURL(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("official package URL: %s", sourceURL)
		if err = a.componentDownload(ctx, sourceURL, file); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	manifest, err := validateExtensionVSIX(file)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := Hash(file)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("validated %s.%s %s (%s), %d bytes, SHA256 %s", manifest.Publisher, manifest.Name, manifest.Version, nativeExtensionPlatform(), info.Size(), hash)
}
