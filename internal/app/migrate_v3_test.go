package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationV3SupervisorWithoutSocketIsNotAssumedIdle(t *testing.T) {
	a, opt, _ := migrationPreflightApp(t)
	source, err := os.ReadFile("../../legacy/v3/payload/control.sh")
	if err != nil {
		t.Fatal(err)
	}
	engineFile(t, filepath.Join(a.P.Installer, "legacy/v3/payload/control.sh"), string(source))
	engineFile(t, filepath.Join(a.P.Home, ".local/share/codex-background/payload/control.sh"), string(source))
	engineFile(t, filepath.Join(a.P.Home, ".local/share/codex-background/managed-v3"), "managed\n")
	engineFile(t, filepath.Join(a.P.Home, ".local/state/codex-background/supervisor.pid"), "12345 6789\n")
	opt.DryRun = true
	if err := a.Migrate(context.Background(), opt); err == nil || !strings.Contains(err.Error(), "不能确认旧后台空闲") {
		t.Fatalf("supervisor without socket treated as idle: %v", err)
	}
	opt.Force = true
	if err := a.Migrate(context.Background(), opt); err != nil {
		t.Fatal("explicit forced preview failed", err)
	}
	for _, path := range []string{a.P.Data, a.P.Runtime} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("preview wrote runtime data", path, err)
		}
	}
}
