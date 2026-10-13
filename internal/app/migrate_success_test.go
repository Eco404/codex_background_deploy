package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationSettingsKeepsUnrelatedStrings(t *testing.T) {
	for _, raw := range []string{`{"chatgpt.cliExecutable":"/old","url":"https://x/a,}?q=/*raw*/",}`, "{\n // header\n \"keep\": 1,\n \"chatgpt.cliExecutable\": \"/old\", // tail\n}\n", `{"keep":1,"chatgpt.cliExecutable":"/old","last":"x"}`} {
		out, e := editConfigKey([]byte(raw), "chatgpt.cliExecutable", nil)
		if e != nil {
			t.Fatal(e)
		}
		clean, e := cleanJSONC(out)
		if e != nil {
			t.Fatalf("%s: %v", out, e)
		}
		var obj map[string]any
		if e = json.Unmarshal(clean, &obj); e != nil {
			t.Fatal(e)
		}
		if _, ok := obj["chatgpt.cliExecutable"]; ok {
			t.Fatal("key remains")
		}
		if strings.Contains(raw, "https://") && !strings.Contains(string(out), "https://x/a,}?q=/*raw*/") {
			t.Fatal("unrelated string modified")
		}
	}
}
func TestDataDirectoryIdentityRejectsEmptyReplacement(t *testing.T) {
	a := engineApp(t)
	if e := a.EnsureData(); e != nil {
		t.Fatal(e)
	}
	if e := a.Save(); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(a.P.Installer, "new")
	if e := os.MkdirAll(filepath.Join(path, "installer-data"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := a.DataAction(context.Background(), "relink", path); e == nil {
		t.Fatal("empty maintenance directory replaced original identity")
	}
}
func TestCoreRefusesExtensionBinaryAsBackend(t *testing.T) {
	a := engineApp(t)
	ex := engineExtension(t, a)
	a.RunCommand = func(_ context.Context, n string, args, env []string) ([]byte, error) {
		if n == "systemctl" {
			return []byte("ok"), nil
		}
		if len(args) > 0 && args[0] == "--version" {
			return []byte("codex-cli 0.153.4"), nil
		}
		if len(args) > 0 && args[0] == "app-server" {
			return []byte("unix://"), nil
		}
		return nil, errors.New("unavailable")
	}
	_, _, e := a.PlanInstall(context.Background(), InstallOptions{Backend: "systemd", CodexPath: ex.Entry, Roots: []string{ex.Root}})
	if e == nil {
		t.Fatal("recursive hooked backend allowed")
	}
}
