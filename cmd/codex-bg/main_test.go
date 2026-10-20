package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemovedExtensionInstallerRejectsLegacyCommandsWithoutWrites(t *testing.T) {
	for _, args := range [][]string{
		{"extension", "install", "--yes"},
		{"install", "--extension-source", "marketplace", "--yes"},
		{"migrate", "--extension-source", "/old/plugin.vsix", "--yes"},
		{"extension", "install", "--source", "marketplace", "--yes"},
		{"extension", "install", "--code-server", "/existing/code-server", "--yes"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := t.TempDir()
			cli := []string{"--home", filepath.Join(root, "home"), "--installer-dir", filepath.Join(root, "installer"), "--runtime", filepath.Join(root, "runtime")}
			err := run(context.Background(), append(cli, args...))
			if err == nil || (!strings.Contains(err.Error(), "未知命令") && !strings.Contains(err.Error(), "未知选项")) {
				t.Fatalf("removed extension installer was not rejected: %v", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("unsupported command created files: %v %v", entries, err)
			}
		})
	}
}

func TestMigrationDependencyFlagCannotBeSilentlyIgnored(t *testing.T) {
	for _, command := range []string{"install", "inspect", "component"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			args := []string{"--home", filepath.Join(root, "h"), "--installer-dir", filepath.Join(root, "i"), "--runtime", filepath.Join(root, "r"), command, "--migrate-codexapp", "--yes"}
			err := run(context.Background(), args)
			if err == nil || !strings.Contains(err.Error(), "仅用于 migrate") {
				t.Fatalf("migration flag ignored: %v", err)
			}
			files, err := os.ReadDir(root)
			if err != nil || len(files) != 0 {
				t.Fatalf("invalid command wrote files: %v %v", files, err)
			}
		})
	}
}

func TestAutoRestartDeploymentFlagCannotBeSilentlyIgnored(t *testing.T) {
	for _, command := range []string{"inspect", "status", "component", "update", "start", "stop", "serve", "proxy", "component-run", "autostart"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			args := []string{"--home", filepath.Join(root, "h"), "--installer-dir", filepath.Join(root, "i"), "--runtime", filepath.Join(root, "r"), command, "--no-auto-restart"}
			err := run(context.Background(), args)
			if err == nil || !strings.Contains(err.Error(), "仅用于 install 或 migrate") {
				t.Fatalf("deployment flag ignored: %v", err)
			}
			files, err := os.ReadDir(root)
			if err != nil || len(files) != 0 {
				t.Fatalf("invalid command wrote files: %v %v", files, err)
			}
		})
	}
}

func TestWaitIdleRejectsUnsafeOrMisplacedFlagsWithoutWrites(t *testing.T) {
	for _, args := range [][]string{
		{"update", "--wait-idle", "1m", "--force", "--yes"},
		{"update", "--wait-idle", "0", "--yes"},
		{"update", "--wait-idle", "-1h", "--yes"},
		{"update", "--wait-idle"},
		{"start", "--wait-idle", "1m", "--yes"},
		{"serve", "--wait-idle", "1m"},
		{"update", "--dry-run", "--yes"},
		{"record-socket-owner", "--dry-run", "--yes"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := t.TempDir()
			cli := []string{"--home", filepath.Join(root, "h"), "--installer-dir", filepath.Join(root, "i"), "--runtime", filepath.Join(root, "r")}
			if err := run(context.Background(), append(cli, args...)); err == nil {
				t.Fatal("unsafe arguments accepted")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid command wrote files: %v %v", entries, err)
			}
		})
	}
}
