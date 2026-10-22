package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationV3ControllerUsesEmbeddedSource(t *testing.T) {
	original := string(legacyV3Controller)
	changed := original + "\n# unreviewed controller change\n"
	for _, tc := range []struct {
		name, installed, installerReference string
		wantError                           bool
	}{
		{"no-reference-directory", original, "", false},
		{"external-reference-ignored", original, changed, false},
		{"matching-external-forgery-rejected", changed, changed, true},
		{"missing-controller-rejected", "", original, true},
	} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/force=%t", tc.name, force), func(t *testing.T) {
				a, opt, hook := migrationPreflightApp(t)
				control := filepath.Join(a.P.Home, ".local/share/codex-background/payload/control.sh")
				if tc.installed != "" {
					engineFile(t, control, tc.installed)
				}
				engineFile(t, filepath.Join(a.P.Home, ".local/share/codex-background/managed-v3"), "managed\n")
				if tc.installerReference != "" {
					// Even if an installer carries a matching forged reference,
					// the reviewed embedded original must remain authoritative.
					engineFile(t, filepath.Join(a.P.Installer, "legacy/v3/payload/control.sh"), tc.installerReference)
				}
				before := engineRead(t, hook)
				opt.DryRun, opt.Force = true, force
				plan, err := a.planMigration(context.Background(), opt)
				if tc.wantError {
					if err == nil || !strings.Contains(err.Error(), "v3控制器与内置原件不一致") {
						t.Fatalf("unverified controller was not rejected: %v", err)
					}
				} else if err != nil || plan.Control != control {
					t.Fatalf("migration without external source did not select installed controller: plan=%+v err=%v", plan, err)
				}
				if tc.installed != "" && engineRead(t, control) != tc.installed || engineRead(t, hook) != before {
					t.Fatal("read-only migration preflight changed old deployment")
				}
				for _, path := range []string{a.P.Data, a.P.Runtime} {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("preflight created runtime/maintenance data: %s: %v", path, err)
					}
				}
			})
		}
	}
}

func TestMigrationV3SupervisorWithoutSocketIsNotAssumedIdle(t *testing.T) {
	a, opt, _ := migrationPreflightApp(t)
	engineFile(t, filepath.Join(a.P.Home, ".local/share/codex-background/payload/control.sh"), string(legacyV3Controller))
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
