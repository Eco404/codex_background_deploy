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
