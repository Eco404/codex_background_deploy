package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func passwordTestPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	_ = listener.Close()
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestCodexAppPasswordDefaultIsRandomAndReadOnlyUntilCommitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "password")
	first, err := codexAppPassword("", path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := codexAppPassword("", path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(first)))
	if err != nil || len(raw) != 32 || bytes.Equal(first, second) {
		t.Fatal("defaults must be independent 256-bit random credentials")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("preparing default password wrote runtime files")
	}
	if err := AtomicWrite(path, first, 0600); err != nil {
		t.Fatal(err)
	}
	reused, err := codexAppPassword("", path)
	if err != nil || !bytes.Equal(first, reused) {
		t.Fatal("default choice rotated existing credential")
	}
}

func TestCodexAppPasswordRejectsInvalidSourceWithoutRotation(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "password")
	if _, err := codexAppPassword("relative-password", destination); err == nil {
		t.Fatal("relative password source accepted")
	}
	if err := os.WriteFile(destination, []byte(" \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := codexAppPassword("", destination); err == nil {
		t.Fatal("invalid existing password silently rotated")
	}
	if _, err := codexAppPassword(root, destination); err == nil {
		t.Fatal("password source directory accepted")
	}
	symlink := filepath.Join(root, "password-link")
	if err := os.Symlink(destination, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := codexAppPassword("", symlink); err == nil {
		t.Fatal("unowned symlink accepted as managed password")
	}
}

func TestCodexAppManagedInstallUsesPrivateDefaultPassword(t *testing.T) {
	a := componentTestApp(t)
	if err := a.EnsureData(); err != nil {
		t.Fatal(err)
	}
	a.S.Installed = true
	a.P.Socket = runtimeFixture(t, runtimeResponder("idle"))
	output := &bytes.Buffer{}
	a.Out = output
	a.Err = output
	node := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(filepath.Join(filepath.Dir(node), "npm"), nil, 0700); err != nil {
		t.Fatal(err)
	}
	a.S.Components["node"] = &Component{Name: "node", Path: node, Ownership: "reused", Status: "installed"}
	var passedPassword string
	a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
		if name == node {
			if len(args) == 1 && args[0] == "--version" {
				return []byte("v22.20.0"), nil
			}
			if len(args) >= 2 && args[len(args)-2] == "--password" {
				passedPassword = args[len(args)-1]
				return nil, nil
			}
		}
		if name == filepath.Join(filepath.Dir(node), "npm") {
			return nil, nil
		}
		return nil, errors.New("unexpected command")
	}
	if err := a.ComponentAction(context.Background(), "install", "codexapp", ComponentOptions{Path: "managed", Port: passwordTestPort(t), Yes: true}); err != nil {
		t.Fatal(err)
	}
	path := a.CodexAppPasswordPath()
	password, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("default credential is not private")
	}
	if passedPassword == "" || passedPassword != strings.TrimSpace(string(password)) {
		t.Fatal("configured credential not supplied to application")
	}
	state, err := os.ReadFile(a.P.StateFile())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(state, []byte(passedPassword)) || strings.Contains(output.String(), passedPassword) {
		t.Fatal("credential escaped its runtime file")
	}
}

func TestCodexAppConfigureDefaultsPreservePasswordAndExternalTree(t *testing.T) {
	a := componentTestApp(t)
	a.P.Socket = runtimeFixture(t, runtimeResponder("idle"))
	externalRoot := t.TempDir()
	external := filepath.Join(externalRoot, "codexapp")
	if err := os.WriteFile(external, []byte("external installation"), 0700); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(t.TempDir(), "node")
	a.S.Components["node"] = &Component{Name: "node", Path: node, Ownership: "reused", Status: "installed"}
	a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
		if name == external {
			return []byte("codexapp 0.1.90"), nil
		}
		if name == node {
			if len(args) == 1 && args[0] == "--version" {
				return []byte("v22.20.0"), nil
			}
			return nil, nil
		}
		return nil, errors.New("unexpected command")
	}
	if err := a.ComponentAction(context.Background(), "install", "codexapp", ComponentOptions{Path: external, Yes: true}); err != nil {
		t.Fatal(err)
	}
	opt := ComponentOptions{Port: passwordTestPort(t), Yes: true}
	if err := a.ComponentAction(context.Background(), "configure", "codexapp", opt); err != nil {
		t.Fatal(err)
	}
	path := a.CodexAppPasswordPath()
	if path != filepath.Join(a.componentRoot("codexapp"), "password") {
		t.Fatal("reused executable credentials stored outside managed runtime")
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := a.ComponentAction(context.Background(), "configure", "codexapp", opt); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("blank reconfigure rotated existing password")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("reconfigured password permissions not private")
	}
	source := filepath.Join(t.TempDir(), "source-password")
	if err := os.WriteFile(source, []byte("explicit-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opt.PasswordFile = source
	if err := a.ComponentAction(context.Background(), "configure", "codexapp", opt); err != nil {
		t.Fatal(err)
	}
	replaced, err := os.ReadFile(path)
	if err != nil || string(replaced) != "explicit-secret\n" {
		t.Fatal("explicit credential file not copied")
	}
	entries, err := os.ReadDir(externalRoot)
	if err != nil || len(entries) != 1 || entries[0].Name() != "codexapp" {
		t.Fatal("external installation modified")
	}
	if err := a.ComponentAction(context.Background(), "uninstall", "codexapp", ComponentOptions{Yes: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("managed credentials left after unregistering runtime")
	}
	if sourcePassword, err := os.ReadFile(source); err != nil || string(sourcePassword) != "explicit-secret\n" {
		t.Fatal("external password source modified")
	}
}

func TestCodexAppDefaultPasswordDryRunNeverCreatesCredential(t *testing.T) {
	for _, action := range []string{"install", "configure"} {
		t.Run(action, func(t *testing.T) {
			a := componentTestApp(t)
			if err := a.ComponentAction(context.Background(), action, "codexapp", ComponentOptions{DryRun: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(a.P.Runtime); !os.IsNotExist(err) {
				t.Fatal("password dry run created runtime directory")
			}
		})
	}
}

func TestCodexAppPreflightProtectsAbsentAndStoppedCore(t *testing.T) {
	a := componentTestApp(t)
	for _, action := range []string{"install", "configure", "start"} {
		if err := a.CheckCodexAppReady(context.Background(), action, ComponentOptions{Configure: true}); err == nil {
			t.Fatalf("%s accepted missing core", action)
		}
	}
	if err := a.CheckCodexAppReady(context.Background(), "install", ComponentOptions{Path: "/external/codexapp"}); err != nil {
		t.Fatal("external registration unnecessarily requires active core")
	}
	a.S.Installed = true
	if err := a.CheckCodexAppReady(context.Background(), "install", ComponentOptions{Path: "managed"}); err == nil {
		t.Fatal("deployment accepted stopped core")
	}
	a.P.Socket = runtimeFixture(t, runtimeResponder("idle"))
	if err := a.CheckCodexAppReady(context.Background(), "configure", ComponentOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.P.Runtime); !os.IsNotExist(err) {
		t.Fatal("preflight changed deployment files")
	}
}
