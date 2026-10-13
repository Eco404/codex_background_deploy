package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNetworkValidationAndCredentialRedaction(t *testing.T) {
	for _, raw := range []string{"file:///tmp/mirror", "https://user:password@example.com", "https://example.com/?token=secret", "https://example.com/#fragment"} {
		n := DefaultNetworkOptions()
		n.UseMirrors = true
		n.NPMRegistry = raw
		if n.Validate() == nil {
			t.Fatalf("accepted mirror %q", raw)
		}
	}
	n := DefaultNetworkOptions()
	n.ProxyMode = "custom"
	n.Proxy = "socks5://user:private-secret@127.0.0.1:7890"
	if err := n.Validate(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(n.Summary(), "private-secret") || strings.Contains(n.redact("error "+n.Proxy), "private-secret") {
		t.Fatal("proxy credential leaked")
	}
	n.Proxy = "http://host/path"
	if n.Validate() == nil {
		t.Fatal("proxy path accepted")
	}
}

func TestCondaMirrorHTMLIsRejectedBeforeExecution(t *testing.T) {
	a := componentTestApp(t)
	a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
		if name != "curl" {
			t.Fatalf("executed downloaded webpage with %s", name)
		}
		return nil, os.WriteFile(args[len(args)-1], []byte("<!DOCTYPE html><html>mirror portal</html>"), 0600)
	}
	err := a.ComponentAction(context.Background(), "install", "miniconda", ComponentOptions{Path: "managed", Yes: true})
	if err == nil || !strings.Contains(err.Error(), "镜像可能返回网页") {
		t.Fatalf("unexpected error: %v", err)
	}
}
func TestNetworkOnlyChangesControlledInstallEnvironment(t *testing.T) {
	a := componentTestApp(t)
	a.Network = DefaultNetworkOptions()
	a.Network.UseMirrors = true
	a.Network.ProxyMode = "none"
	t.Setenv("HTTPS_PROXY", "http://existing:8080")
	runtimeEnv := a.componentEnv(nil)
	if testEnv(runtimeEnv, "HTTPS_PROXY") != "http://existing:8080" {
		t.Fatal("runtime environment changed")
	}
	installEnv := a.componentEnv(a.networkEnv(nil))
	if testEnv(installEnv, "HTTPS_PROXY") != "" || testEnv(installEnv, "npm_config_registry") != a.Network.NPMRegistry {
		t.Fatal("installation network options not applied")
	}
	if testEnv(installEnv, "npm_config_cache") != filepath.Join(a.P.Data, "cache", "npm") {
		t.Fatal("npm cache escaped maintenance")
	}
	a.Network.ProxyMode = "custom"
	a.Network.Proxy = "http://proxy:7890"
	if testEnv(a.componentEnv(a.networkEnv(nil)), "https_proxy") != a.Network.Proxy {
		t.Fatal("configured proxy ignored")
	}
	a.Network.UseMirrors = false
	t.Setenv("NPM_CONFIG_REGISTRY", "https://stale.invalid")
	if testEnv(a.componentEnv(a.networkEnv(nil)), "NPM_CONFIG_REGISTRY") != "https://registry.npmjs.org" {
		t.Fatal("official source choice did not override inherited registry")
	}
}
func TestDownloadUsesHTTPAndContainsTemporaryFiles(t *testing.T) {
	a := componentTestApp(t)
	a.Network.ProxyMode = "none"
	var output bytes.Buffer
	a.Out = &output
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "download fixture") }))
	defer server.Close()
	if err := a.componentDirs(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(a.P.Data, "cache", "fixture")
	if err := a.componentDownload(context.Background(), server.URL+"/fixture", destination); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(destination)
	if err != nil || string(b) != "download fixture" {
		t.Fatalf("download: %s %v", b, err)
	}
	entries, _ := os.ReadDir(filepath.Join(a.P.Data, "tmp"))
	if len(entries) != 0 {
		t.Fatal("download temporary file left behind")
	}
	if !strings.Contains(output.String(), "下载完成") {
		t.Fatal("no download progress")
	}
}
func TestDownloadFailurePreservesPreviousCacheAndRemovesTemporary(t *testing.T) {
	a := componentTestApp(t)
	a.Network.ProxyMode = "none"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		fmt.Fprint(w, "partial")
	}))
	defer server.Close()
	if err := a.componentDirs(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(a.P.Data, "cache", "fixture")
	if err := os.WriteFile(destination, []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.componentDownload(context.Background(), server.URL, destination); err == nil {
		t.Fatal("partial download accepted")
	}
	b, _ := os.ReadFile(destination)
	if string(b) != "previous" {
		t.Fatal("previous cache replaced by truncated download")
	}
	entries, _ := os.ReadDir(filepath.Join(a.P.Data, "tmp"))
	if len(entries) != 0 {
		t.Fatal("failed download left temporary file")
	}
}
func nativePackageFixture(t *testing.T, unsafe bool) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	triple := "x86_64-unknown-linux-musl"
	if runtime.GOARCH == "arm64" {
		triple = "aarch64-unknown-linux-musl"
	}
	names := []string{"bin/codex", "bin/codex-code-mode-host", "codex-path/rg", "codex-resources/bwrap", "codex-resources/zsh/bin/zsh", "codex-package.json"}
	if unsafe {
		names = []string{"../../escaped"}
	}
	content := []byte("native fixture")
	for _, relative := range names {
		name := "package/vendor/" + triple + "/" + relative
		if unsafe {
			name = relative
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0700, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}
func TestMirroredNativeCodexUsesPinnedPackageWithoutNode(t *testing.T) {
	for _, badDigest := range []bool{false, true} {
		t.Run(fmt.Sprintf("bad_digest_%t", badDigest), func(t *testing.T) {
			a := componentTestApp(t)
			a.Network = DefaultNetworkOptions()
			a.Network.UseMirrors = true
			a.Network.NPMRegistry = "https://mirror.test/npm"
			payload := nativePackageFixture(t, false)
			sum := sha512.Sum512(payload)
			integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
			if badDigest {
				integrity = "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
			}
			arch, _ := platformArch()
			version := "0.153.4-linux-" + arch
			metadata, _ := json.Marshal(map[string]any{"name": "@openai/codex", "version": version, "dist": map[string]string{"tarball": "https://registry.npmjs.org/@openai/codex/-/codex-" + version + ".tgz", "integrity": integrity}})
			verified := false
			a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
				if name == "curl" {
					raw := args[len(args)-3]
					if !strings.HasPrefix(raw, "https://mirror.test/npm/") {
						t.Fatalf("mirror bypassed: %s", raw)
					}
					data := payload
					if strings.HasSuffix(raw, "/"+version) {
						data = metadata
					}
					return nil, os.WriteFile(args[len(args)-1], data, 0600)
				}
				if filepath.Base(name) == "codex" && len(args) == 1 && args[0] == "--version" {
					verified = true
					if !strings.HasPrefix(testEnv(env, "HOME"), filepath.Join(a.P.Data, "tmp")) {
						t.Fatal("validation used real HOME")
					}
					return []byte("codex-cli 0.153.4"), nil
				}
				t.Fatalf("unnecessary dependency executed: %s", name)
				return nil, nil
			}
			candidate, err := a.InstallManagedCodex(context.Background(), "0.153.4")
			if badDigest {
				if err == nil || verified {
					t.Fatal("invalid package executed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !verified || !executable(candidate.Path) || !strings.Contains(candidate.Path, "/releases/0.153.4-") {
				t.Fatalf("invalid candidate %#v", candidate)
			}
			release := filepath.Dir(filepath.Dir(candidate.Path))
			for _, resource := range []string{"codex-path/rg", "codex-resources/bwrap", "codex-resources/zsh/bin/zsh"} {
				if !executable(filepath.Join(release, resource)) {
					t.Fatalf("runtime resource missing: %s", resource)
				}
			}
			if _, err = os.Stat(a.S.CodexHome); !os.IsNotExist(err) {
				t.Fatal("user authentication touched")
			}
		})
	}
}
func TestNativeCodexArchiveRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "unsafe.tgz")
	os.WriteFile(archive, nativePackageFixture(t, true), 0600)
	if err := extractCodexPackage(archive, filepath.Join(root, "out")); err == nil {
		t.Fatal("unsafe archive accepted")
	}
}

func TestDownloadUsesExplicitProxyWithoutLoggingCredentials(t *testing.T) {
	a := componentTestApp(t)
	var output bytes.Buffer
	a.Out = &output
	observed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = true
		if r.URL.Host != "download.invalid" || r.URL.RawQuery != "targetPlatform=linux-x64" {
			t.Errorf("unexpected proxied URL %s", r.URL)
		}
		fmt.Fprint(w, "through proxy")
	}))
	defer server.Close()
	a.Network = DefaultNetworkOptions()
	a.Network.ProxyMode = "custom"
	a.Network.Proxy = strings.Replace(server.URL, "http://", "http://user:proxy-secret@", 1)
	if err := a.componentDirs(); err != nil {
		t.Fatal(err)
	}
	if err := a.componentDownload(context.Background(), "http://download.invalid/vsix?targetPlatform=linux-x64", filepath.Join(a.P.Data, "cache", "fixture")); err != nil {
		t.Fatal(err)
	}
	if !observed {
		t.Fatal("selected proxy bypassed")
	}
	if strings.Contains(output.String(), "proxy-secret") {
		t.Fatal("proxy credentials logged")
	}
}
func TestInstallSubprocessStreamsRedactedOutput(t *testing.T) {
	a := componentTestApp(t)
	var output bytes.Buffer
	a.Out = &output
	a.Network.ProxyMode = "custom"
	a.Network.Proxy = "http://user:proxy-secret@127.0.0.1:7890"
	if _, err := a.installRun(context.Background(), nil, "/bin/sh", "-c", "printf 'downloading via %s\\n' \"$https_proxy\""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "downloading via http://") || strings.Contains(output.String(), "proxy-secret") {
		t.Fatalf("unexpected output %s", output.String())
	}
	output.Reset()
	writer := &installOutput{a: a}
	writer.Write([]byte("split proxy-sec"))
	writer.Write([]byte("ret\n"))
	if strings.Contains(output.String(), "proxy-secret") {
		t.Fatal("split credential leaked")
	}
}

func TestLegacyNativeCodexReleaseNormalizesResources(t *testing.T) {
	stage := t.TempDir()
	triple := "x86_64-unknown-linux-musl"
	vendor := filepath.Join(stage, "package", "vendor", triple)
	for _, name := range []string{"codex/codex", "path/rg", "codex-resources/bwrap"} {
		if err := AtomicWrite(filepath.Join(vendor, name), []byte(name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	prepared, relative, err := prepareNativeCodexRelease(stage, triple)
	if err != nil {
		t.Fatal(err)
	}
	if relative != "codex" {
		t.Fatal("legacy executable path wrong")
	}
	for _, name := range []string{"codex", "codex-resources/rg", "codex-resources/bwrap"} {
		if !executable(filepath.Join(prepared, name)) {
			t.Fatalf("missing %s", name)
		}
	}
}
func TestInstallCancellationStopsChildProcesses(t *testing.T) {
	a := componentTestApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	began := time.Now()
	output, err := a.installRun(ctx, nil, "/bin/sh", "-c", "sleep 30 & printf '%s\\n' \"$!\"; wait")
	if err == nil {
		t.Fatal("cancelled installation succeeded")
	}
	if time.Since(began) > 3*time.Second {
		t.Fatal("installer cancellation waited for surviving child pipes")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		t.Fatalf("child pid unavailable: %s", output)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	end := strings.LastIndexByte(string(stat), ')')
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) == 0 || fields[0] != "Z" {
		t.Fatalf("installer child remains running: %s", stat)
	}
}

func TestManagedCodexRecordedSelectionMismatchPreservesReleases(t *testing.T) {
	for _, requested := range []string{"", "1.0.0", "2.0.0"} {
		t.Run("requested_"+requested, func(t *testing.T) {
			a := componentTestApp(t)
			if err := a.componentDirs(); err != nil {
				t.Fatal(err)
			}
			root := a.componentRoot("codex-cli")
			standalone := filepath.Join(root, "packages-home", "packages", "standalone")
			old := filepath.Join(standalone, "releases", "1.0.0")
			newer := filepath.Join(standalone, "releases", "2.0.0")
			for _, p := range []string{old, newer} {
				if err := AtomicWrite(filepath.Join(p, "codex"), []byte(p), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(newer, filepath.Join(standalone, "current")); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "bin"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(standalone, "current", "codex"), filepath.Join(root, "bin", "codex")); err != nil {
				t.Fatal(err)
			}
			a.S.Components["codex-cli"] = &Component{Name: "codex-cli", Path: root, Ownership: "managed", Status: "installed", Version: "1.0.0"}
			a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
				if name != filepath.Join(root, "bin", "codex") || len(args) != 1 || args[0] != "--version" {
					t.Fatalf("mutation before selection verified: %s %v", name, args)
				}
				return []byte("codex-cli 2.0.0"), nil
			}
			if _, err := a.InstallManagedCodex(context.Background(), requested); err == nil || !strings.Contains(err.Error(), "selection differs") {
				t.Fatalf("selection mismatch accepted: %v", err)
			}
			for _, p := range []string{old, newer} {
				info, err := os.Lstat(p)
				if err != nil || !info.IsDir() {
					t.Fatalf("release archived despite mismatch: %s %v", p, err)
				}
			}
			if entries, _ := os.ReadDir(filepath.Join(a.P.Data, "cache", "codex-rollback")); len(entries) > 0 {
				t.Fatal("created rollback archive from inconsistent selection")
			}
		})
	}
}

func TestRegistryTarballResolutionPreservesHuaweiNamespace(t *testing.T) {
	registry := "https://repo.huaweicloud.com/repository/npm"
	for _, tarball := range []string{"/@openai/codex/-/codex-0.153.4-linux-x64.tgz", "@openai/codex/-/codex-0.153.4-linux-x64.tgz", "https://registry.npmjs.org/@openai/codex/-/codex-0.153.4-linux-x64.tgz"} {
		got, err := registryTarballURL(registry, tarball)
		if err != nil {
			t.Fatal(err)
		}
		expected := registry + "/@openai/codex/-/codex-0.153.4-linux-x64.tgz"
		if got != expected {
			t.Fatalf("registry path lost: got %s want %s", got, expected)
		}
	}
	for _, tarball := range []string{"", "../archive.tgz", "/pkg/../archive.tgz", "/%2e%2e/archive.tgz", "/pkg/%2e%2e/archive.tgz", "//external.test/archive.tgz", "https://user:secret@example.com/archive.tgz", "file:///tmp/archive.tgz", "/pkg%5c..%5carchive.tgz"} {
		if _, err := registryTarballURL(registry, tarball); err == nil {
			t.Fatalf("unsafe tarball accepted: %s", tarball)
		}
	}
	direct := "https://cdn.example.com/native.tgz"
	if got, err := registryTarballURL(registry, direct); err != nil || got != direct {
		t.Fatalf("valid CDN URL rejected: %s %v", got, err)
	}
}
