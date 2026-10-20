package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func engineApp(t *testing.T) *App {
	t.Helper()
	dir, e := os.MkdirTemp(os.TempDir(), "engine-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	a, e := New(filepath.Join(dir, "h"), filepath.Join(dir, "i"), filepath.Join(dir, "r"))
	if e != nil {
		t.Fatal(e)
	}
	a.P.Socket = filepath.Join(dir, "s")
	a.Out = io.Discard
	a.Err = io.Discard
	a.RunCommand = func(context.Context, string, []string, []string) ([]byte, error) {
		return nil, errors.New("unexpected external command")
	}
	return a
}
func engineFile(t *testing.T, p, contents string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(contents), 0700); e != nil {
		t.Fatal(e)
	}
}
func engineRead(t *testing.T, p string) string {
	t.Helper()
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func engineExtension(t *testing.T, a *App) Extension {
	root := filepath.Join(a.P.Home, "extensions")
	entry := filepath.Join(root, "openai.chatgpt-1/bin/linux/codex")
	engineFile(t, entry, "original-v1")
	return Extension{Root: root, Directory: filepath.Dir(filepath.Dir(filepath.Dir(entry))), Entry: entry, Version: "1"}
}

func TestEngineBackupOwnershipAndForcedDeletionDegradesRecovery(t *testing.T) {
	a := engineApp(t)
	ex := engineExtension(t, a)
	if e := a.Attach(context.Background(), ex); e != nil {
		t.Fatal(e)
	}
	bind := a.S.Bindings[0]
	if e := a.DeleteBackup(bind.BackupID, false); e == nil {
		t.Fatal("referenced backup deleted without force")
	}
	if e := a.DeleteBackup(bind.BackupID, true); e != nil {
		t.Fatal(e)
	}
	if !hasString(a.S.RecoveryMissing, bind.BackupID) {
		t.Fatal("missing recovery not recorded")
	}
	b, e := a.ReadBackup(bind.BackupID)
	if e != nil || !b.Deleted {
		t.Fatalf("missing tombstone: %+v %v", b, e)
	}
	if e := a.VerifyBackup(bind.BackupID); e == nil {
		t.Fatal("deleted backup claims recoverable")
	}
	if e := a.DetachRoot(context.Background(), ex.Root); e == nil {
		t.Fatal("detach deleted runtime dependency with no original")
	}
	if !strings.Contains(engineRead(t, ex.Entry), Marker) {
		t.Fatal("hook changed despite missing restore material")
	}
}
func TestEngineAtomicReplacementFailureLeavesOriginal(t *testing.T) {
	a := engineApp(t)
	original := filepath.Join(a.P.Home, "original")
	engineFile(t, original, "keep")
	link := filepath.Join(a.P.Home, "link")
	if e := os.Symlink(original, link); e != nil {
		t.Fatal(e)
	}
	if e := AtomicWrite(link, []byte("replace"), 0600); e == nil {
		t.Fatal("symlink overwrite accepted")
	}
	if engineRead(t, original) != "keep" {
		t.Fatal("failed replace changed original")
	}
	target := filepath.Join(a.P.Home, "directory")
	if e := os.MkdirAll(target, 0700); e != nil {
		t.Fatal(e)
	}
	if e := AtomicWrite(target, []byte("replace"), 0600); e == nil {
		t.Fatal("directory replace accepted")
	}
	entries, _ := os.ReadDir(a.P.Home)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".codex-bg-replace-") {
			t.Fatal("atomic temp leaked")
		}
	}
}
func TestEngineJSONCMigrationPreservesOtherBytes(t *testing.T) {
	original := []byte("{\n  \"token\": \"private-secret\", // preserve\n  \"url\": \"https://example/a,}?comment=/*x*/\",\n  \"chatgpt.cliExecutable\": \"/old/wrapper\", // old path\n}\n")
	result, e := editConfigKey(original, "chatgpt.cliExecutable", nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, present, e := configValue(result, "chatgpt.cliExecutable"); e != nil || present {
		t.Fatalf("key not removed: %s %v", result, e)
	}
	for _, keep := range []string{"\"token\": \"private-secret\", // preserve", "\"url\": \"https://example/a,}?comment=/*x*/\"", "// old path"} {
		if !bytes.Contains(result, []byte(keep)) {
			t.Fatalf("other bytes changed: %q", keep)
		}
	}
}
func TestEngineKnownHookUpgradeKeepsBothOriginals(t *testing.T) {
	a := engineApp(t)
	ex := engineExtension(t, a)
	if e := a.Attach(context.Background(), ex); e != nil {
		t.Fatal(e)
	}
	first := a.S.Bindings[0]
	engineFile(t, ex.Entry, "original-v2")
	if e := a.Attach(context.Background(), ex); e != nil {
		t.Fatal(e)
	}
	if len(a.S.Bindings) != 1 || a.S.Bindings[0].BackupID == first.BackupID {
		t.Fatal("upgrade did not acquire a new original")
	}
	old, e := a.ReadBackup(first.BackupID)
	if e != nil {
		t.Fatal(e)
	}
	if old.Ref != "" || engineRead(t, old.DataPath) != "original-v1" {
		t.Fatal("historical backup lost")
	}
	current, e := a.ReadBackup(a.S.Bindings[0].BackupID)
	if e != nil {
		t.Fatal(e)
	}
	if engineRead(t, current.DataPath) != "original-v2" {
		t.Fatal("wrong new original")
	}
	if e := a.DetachRoot(context.Background(), ex.Root); e != nil {
		t.Fatal(e)
	}
	if engineRead(t, ex.Entry) != "original-v2" {
		t.Fatal("restored older extension binary")
	}
}
func TestEngineUnknownManagedMarkerRefused(t *testing.T) {
	a := engineApp(t)
	ex := engineExtension(t, a)
	engineFile(t, ex.Entry, "#!/bin/sh\n# "+Marker+"\nexit 0")
	scanned, e := scanRoot(ex.Root)
	if e != nil || len(scanned) != 1 {
		t.Fatal(e)
	}
	if e := a.Attach(context.Background(), scanned[0]); e == nil {
		t.Fatal("unowned hook adopted")
	}
	if len(a.S.Bindings) != 0 {
		t.Fatal("unknown hook recorded as owned")
	}
}
func TestEngineMissingStateInspectDoesNotCreateRecords(t *testing.T) {
	a := engineApp(t)
	t.Setenv("PATH", filepath.Join(a.P.Home, "empty-bin"))
	t.Setenv("NVM_DIR", filepath.Join(a.P.Home, ".nvm"))
	a.RunCommand = func(context.Context, string, []string, []string) ([]byte, error) {
		return nil, errors.New("not available")
	}
	_ = a.Status(context.Background())
	if _, e := os.Stat(a.P.Runtime); !os.IsNotExist(e) {
		t.Fatal("status created runtime")
	}
	if _, e := os.Stat(a.P.Data); !os.IsNotExist(e) {
		t.Fatal("status created installer-data")
	}
}
func TestEngineUninstallConflictKeepsDependencies(t *testing.T) {
	a := engineApp(t)
	ex := engineExtension(t, a)
	if e := a.Attach(context.Background(), ex); e != nil {
		t.Fatal(e)
	}
	engineFile(t, a.P.Binary(), "runtime")
	a.S.Files = []ManagedFile{{Path: a.P.Binary(), Hash: HashBytes([]byte("runtime")), Kind: "binary"}}
	a.S.SelectedRoots = []string{ex.Root}
	a.S.Installed = true
	a.S.Backend = "systemd"
	if e := a.Save(); e != nil {
		t.Fatal(e)
	}
	engineFile(t, ex.Entry, engineRead(t, ex.Entry)+"\n# user edit")
	calls := 0
	a.RunCommand = func(context.Context, string, []string, []string) ([]byte, error) { calls++; return nil, nil }
	if e := a.Uninstall(context.Background(), true); e == nil {
		t.Fatal("uninstall accepted restore conflict")
	}
	if calls != 0 {
		t.Fatal("conflict stopped service")
	}
	if engineRead(t, a.P.Binary()) != "runtime" {
		t.Fatal("runtime dependency deleted")
	}
}
func TestEngineRelinkRejectsEmptyReplacementWhenBackupsExist(t *testing.T) {
	a := engineApp(t)
	ex := engineExtension(t, a)
	if e := a.Attach(context.Background(), ex); e != nil {
		t.Fatal(e)
	}
	oldData := a.P.Data
	target := filepath.Join(a.P.Home, "replacement")
	if e := os.MkdirAll(filepath.Join(target, "installer-data"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := a.DataAction(context.Background(), "relink", target); e == nil {
		t.Fatal("relink accepted empty directory and discarded existing backup index")
	}
	if a.P.Data != oldData {
		t.Fatal("failed relink changed active data root")
	}
	if e := a.VerifyBackup(a.S.Bindings[0].BackupID); e != nil {
		t.Fatal(e)
	}
}
func TestEngineServiceFailureRollsBackPreparedFiles(t *testing.T) {
	for _, failure := range []string{"enable", "start"} {
		t.Run(failure, func(t *testing.T) {
			a := engineApp(t)
			ex := engineExtension(t, a)
			cli := filepath.Join(a.P.Home, "codex")
			engineFile(t, cli, "standalone")
			calls := []string{}
			a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
				calls = append(calls, name+" "+strings.Join(args, " "))
				if name == cli {
					if len(args) == 1 {
						return []byte("codex-cli 1.0.0"), nil
					}
					return []byte("--listen unix://"), nil
				}
				if name == "systemctl" {
					if len(args) > 1 && args[1] == failure {
						return nil, errors.New("injected service failure")
					}
					return nil, nil
				}
				return nil, errors.New("unexpected command")
			}
			e := a.installCore(context.Background(), InstallOptions{Backend: "systemd", CodexPath: cli, Roots: []string{ex.Root}, Yes: true})
			if e == nil || !strings.Contains(e.Error(), "injected service failure") {
				t.Fatalf("failure not propagated: %v; calls=%v", e, calls)
			}
			if a.S.Installed {
				t.Fatal("failed install marked installed")
			}
			if hasPending(a.P.Data) {
				t.Fatal("successful rollback left active transaction")
			}
			if engineRead(t, ex.Entry) != "original-v1" {
				t.Fatal("failed install altered original")
			}
			for _, p := range []string{a.P.Binary(), filepath.Join(a.P.Runtime, "codex"), filepath.Join(a.P.Home, ".config/systemd/user/codex-bg.service")} {
				if _, e := os.Stat(p); !os.IsNotExist(e) {
					t.Fatalf("rollback retained %s", p)
				}
			}
		})
	}
}

func TestEngineMigrationUnknownServiceRejectedBeforeStoppingKnownService(t *testing.T) {
	a := engineApp(t)
	ex := engineExtension(t, a)
	cli := filepath.Join(a.P.Home, "codex")
	engineFile(t, cli, "standalone")
	t.Setenv("PATH", filepath.Join(a.P.Home, "empty-bin"))
	t.Setenv("NVM_DIR", filepath.Join(a.P.Home, ".nvm"))
	timer := filepath.Join(a.P.Home, ".config/systemd/user/codex-extension-hook.timer")
	unknown := filepath.Join(a.P.Home, ".config/systemd/user/codex-extension-hook.service")
	engineFile(t, timer, "# vscode-codex-background-deploy extension-hook timer v2\noriginal timer")
	engineFile(t, unknown, "unrelated external service")
	mutations := []string{}
	a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
		if name == cli {
			if len(args) == 1 {
				return []byte("codex-cli 1.0.0"), nil
			}
			return []byte("unix://"), nil
		}
		if name != "systemctl" {
			return nil, errors.New("unexpected external command")
		}
		if len(args) > 1 {
			switch args[1] {
			case "is-active":
				return []byte("active\n"), nil
			case "is-enabled":
				return []byte("enabled\n"), nil
			case "enable", "disable", "start", "stop", "daemon-reload":
				mutations = append(mutations, strings.Join(args, " "))
			}
		}
		return nil, nil
	}
	e := a.Migrate(context.Background(), InstallOptions{Backend: "systemd", CodexPath: cli, Roots: []string{ex.Root}, Yes: true, Force: true})
	if e == nil || !strings.Contains(e.Error(), "来源未知") {
		t.Fatalf("unknown source not rejected: %v", e)
	}
	if len(mutations) != 0 {
		t.Fatalf("unknown service found after mutating known services: %v", mutations)
	}
	if engineRead(t, timer) != "# vscode-codex-background-deploy extension-hook timer v2\noriginal timer" {
		t.Fatal("legacy timer content lost")
	}
	if engineRead(t, unknown) != "unrelated external service" {
		t.Fatal("unknown service changed")
	}
	if hasPending(a.P.Data) {
		t.Fatal("preflight created a pending transaction")
	}
	for _, p := range []string{a.P.Runtime, a.P.Data} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed preflight created %s: %v", p, err)
		}
	}
}

func TestEngineInterruptedAdjacentReplacementCleanup(t *testing.T) {
	a := engineApp(t)
	if e := a.EnsureData(); e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(a.P.Home, "extensions", "codex")
	engineFile(t, target, "keep")
	id := ID()
	tmp := filepath.Join(filepath.Dir(target), ".codex-bg-replace-"+id)
	engineFile(t, tmp, "partial")
	b, _ := json.Marshal(replacementRecord{Target: target, Temporary: tmp})
	path := filepath.Join(a.P.Data, "transactions", "replacements", id+".json")
	if e := AtomicWrite(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	release, e := a.Lock()
	if e != nil {
		t.Fatal(e)
	}
	release()
	if _, e = os.Stat(tmp); !os.IsNotExist(e) {
		t.Fatal("interrupted temporary file remains")
	}
	if engineRead(t, target) != "keep" {
		t.Fatal("cleanup altered original")
	}
}
