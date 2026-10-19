package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureOpenGallery = `{
    "serviceUrl": "https://open-vsx.org/vscode/gallery",
    "itemUrl": "https://open-vsx.org/vscode/item",
    "cacheUrl": "https://open-vsx.org/vscode/gallery/cache",
    "resourceUrlTemplate": "https://open-vsx.org/vscode/unpkg/{publisher}/{name}/{version}/{path}"
  }`

func marketplaceProductFixture(gallery, version string) string {
	return `{
  // Preserve this product comment and all unrelated fields byte-for-byte.
  "nameShort": "Code - OSS",
  "version": "` + version + `",
  "extensionsGallery": ` + gallery + `,
  "other": {"extensionsGallery": "nested unrelated field", "feature": true},
}
`
}

func marketplaceFixture(t *testing.T, gallery string) (*App, Inventory, string) {
	t.Helper()
	t.Setenv("EXTENSIONS_GALLERY", "")
	a := engineApp(t)
	a.RunCommand = func(_ context.Context, name string, args, env []string) ([]byte, error) {
		t.Errorf("marketplace lifecycle unexpectedly executed an external command: %s %v", name, args)
		return nil, errors.New("external commands disabled in isolated marketplace test")
	}
	root := filepath.Join(a.P.Home, "code-server")
	product := filepath.Join(root, "lib/vscode/product.json")
	codeServer := filepath.Join(root, "bin/code-server")
	engineFile(t, filepath.Join(root, "package.json"), `{"name":"code-server","version":"4.100.0"}`)
	engineFile(t, codeServer, "#!/bin/sh\nexit 1\n")
	original := marketplaceProductFixture(gallery, "1.90.0")
	engineFile(t, product, original)
	if err := os.Chmod(product, 0640); err != nil {
		t.Fatal(err)
	}
	inv := Inventory{Container: true, CodeServerProduct: product, Candidates: map[string][]Candidate{
		"code-server": {{Kind: "code-server", Path: codeServer, Compatible: true}},
	}}
	if got := discoverCodeServerProduct(inv.Candidates["code-server"]); got != product {
		t.Fatalf("fixture code-server discovery returned %s, want %s", got, product)
	}
	return a, inv, original
}

func marketplaceInstallFixture(t *testing.T, a *App, inv Inventory) *Component {
	t.Helper()
	if err := a.installMarketplaceWithInventory(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	c := a.S.Components["marketplace"]
	if c == nil || c.Status != "installed" {
		t.Fatalf("marketplace component did not finish installation: %+v", c)
	}
	return c
}

func TestMarketplaceLifecyclePreservesOtherProductFieldsAndCentralizesBackups(t *testing.T) {
	a, inv, original := marketplaceFixture(t, fixtureOpenGallery)
	settings := filepath.Join(a.P.Home, ".vscode-server/data/Machine/settings.json")
	otherIDE := filepath.Join(a.P.Home, "other-ide/product.json")
	engineFile(t, settings, `{"chatgpt.cliExecutable":"/external/cli"}`)
	engineFile(t, otherIDE, original)
	c := marketplaceInstallFixture(t, a, inv)
	if c.Ownership != "managed" || c.Options["product"] != inv.CodeServerProduct || len(c.Backups) != 1 {
		t.Fatalf("missing target/ownership/recovery registration: %+v", c)
	}
	want := marketplaceProductFixture(microsoftGallery, "1.90.0")
	if got := engineRead(t, inv.CodeServerProduct); got != want {
		t.Fatalf("installation changed fields outside extensionsGallery:\n%s", got)
	}
	info, err := os.Stat(inv.CodeServerProduct)
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("product permissions changed: %v %v", info, err)
	}
	backup, err := a.ReadBackup(c.Backups[0])
	if err != nil || backup.Ref != "marketplace" || backup.Kind != "marketplace" || backup.Original != inv.CodeServerProduct || !Within(filepath.Join(a.P.Data, "backups"), backup.DataPath) {
		t.Fatalf("backup is not centrally registered: %+v %v", backup, err)
	}
	if engineRead(t, backup.DataPath) != original || a.VerifyBackup(backup.ID) != nil {
		t.Fatal("original product backup was not preserved and verified")
	}
	if err := a.DeleteBackup(backup.ID, false); err == nil {
		t.Fatal("ordinary deletion removed a referenced marketplace backup")
	}
	entries, err := os.ReadDir(filepath.Dir(inv.CodeServerProduct))
	if err != nil || len(entries) != 1 || entries[0].Name() != "product.json" {
		t.Fatalf("temporary or backup files leaked into code-server installation: %v %v", entries, err)
	}
	if engineRead(t, settings) != `{"chatgpt.cliExecutable":"/external/cli"}` || engineRead(t, otherIDE) != original {
		t.Fatal("marketplace install changed another IDE or client settings")
	}
	// A code-server update changes unrelated fields while retaining our gallery.
	updated := marketplaceProductFixture(microsoftGallery, "1.100.0")
	if err := os.WriteFile(inv.CodeServerProduct, []byte(updated), 0640); err != nil {
		t.Fatal(err)
	}
	if err := a.ComponentAction(context.Background(), "uninstall", "marketplace", ComponentOptions{Yes: true}); err != nil {
		t.Fatal(err)
	}
	if got := engineRead(t, inv.CodeServerProduct); got != marketplaceProductFixture(fixtureOpenGallery, "1.100.0") {
		t.Fatalf("uninstall overwrote the current product version or other fields:\n%s", got)
	}
	if a.S.Components["marketplace"] != nil {
		t.Fatal("uninstall retained component registration")
	}
	if _, err := os.Stat(c.Path); !os.IsNotExist(err) {
		t.Fatalf("uninstall left managed runtime resources: %v", err)
	}
	backup, err = a.ReadBackup(backup.ID)
	if err != nil || backup.Ref != "" || engineRead(t, backup.DataPath) != original {
		t.Fatalf("uninstall deleted or retained a reference to recovery materials: %+v %v", backup, err)
	}
}

func TestMarketplaceReappliesAfterRebuildWithoutRevertingNewProduct(t *testing.T) {
	a, inv, original := marketplaceFixture(t, fixtureOpenGallery)
	c := marketplaceInstallFixture(t, a, inv)
	rebuilt := marketplaceProductFixture(fixtureOpenGallery, "1.101.0")
	if err := os.WriteFile(inv.CodeServerProduct, []byte(rebuilt), 0640); err != nil {
		t.Fatal(err)
	}
	if err := a.checkMarketplace(c); err == nil {
		t.Fatal("rebuild reset was incorrectly reported as already applied")
	}
	if err := a.applyMarketplace(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := engineRead(t, inv.CodeServerProduct); got != marketplaceProductFixture(microsoftGallery, "1.101.0") {
		t.Fatalf("reapply reverted updated product metadata:\n%s", got)
	}
	if err := a.checkMarketplace(c); err != nil {
		t.Fatal(err)
	}
	backups, err := a.Backups()
	if err != nil || len(backups) != 1 || engineRead(t, backups[0].DataPath) != original {
		t.Fatalf("reapply replaced or duplicated the original recovery backup: %+v %v", backups, err)
	}
}

func TestMarketplaceExternalGalleryChangeBlocksApplyAndUninstall(t *testing.T) {
	a, inv, _ := marketplaceFixture(t, fixtureOpenGallery)
	c := marketplaceInstallFixture(t, a, inv)
	external := marketplaceProductFixture(`{"serviceUrl":"https://custom.example.invalid/gallery","itemUrl":"https://custom.example.invalid/items"}`, "1.102.0")
	if err := os.WriteFile(inv.CodeServerProduct, []byte(external), 0640); err != nil {
		t.Fatal(err)
	}
	if err := a.applyMarketplace(context.Background(), c); err == nil {
		t.Fatal("reapply overwrote an unknown external gallery")
	}
	if err := a.ComponentAction(context.Background(), "uninstall", "marketplace", ComponentOptions{Yes: true}); err == nil {
		t.Fatal("uninstall overwrote an unknown external gallery")
	}
	if engineRead(t, inv.CodeServerProduct) != external || a.S.Components["marketplace"] != c {
		t.Fatal("external change was overwritten or ownership metadata was lost")
	}
	backup, err := a.ReadBackup(c.Backups[0])
	if err != nil || backup.Ref != "marketplace" || a.VerifyBackup(backup.ID) != nil {
		t.Fatalf("failed uninstall lost recovery material: %+v %v", backup, err)
	}
}

func TestMarketplaceDeletedBackupPreventsFalseRecovery(t *testing.T) {
	a, inv, _ := marketplaceFixture(t, fixtureOpenGallery)
	c := marketplaceInstallFixture(t, a, inv)
	before := engineRead(t, inv.CodeServerProduct)
	backupID := c.Backups[0]
	if err := a.DeleteBackup(backupID, true); err != nil {
		t.Fatal(err)
	}
	if !hasString(a.S.RecoveryMissing, backupID) || a.checkMarketplace(c) == nil {
		t.Fatal("missing backup was not reflected in recovery capability")
	}
	if err := a.ComponentAction(context.Background(), "uninstall", "marketplace", ComponentOptions{Yes: true}); err == nil {
		t.Fatal("uninstall claimed recovery without the original gallery backup")
	}
	if engineRead(t, inv.CodeServerProduct) != before || a.S.Components["marketplace"] == nil {
		t.Fatal("failed recovery changed product or discarded component registration")
	}
}

func TestMarketplaceExistingOfficialSourceIsReusedAndLeftUntouched(t *testing.T) {
	a, inv, original := marketplaceFixture(t, microsoftGallery)
	c := marketplaceInstallFixture(t, a, inv)
	if c.Ownership != "reused" || len(c.Backups) != 0 || len(c.Files) != 0 || engineRead(t, inv.CodeServerProduct) != original {
		t.Fatalf("existing Microsoft gallery was unnecessarily taken over: %+v", c)
	}
	backups, err := a.Backups()
	if err != nil || len(backups) != 0 {
		t.Fatalf("reused gallery created recovery artifacts: %+v %v", backups, err)
	}
	if err := a.ComponentAction(context.Background(), "uninstall", "marketplace", ComponentOptions{Yes: true}); err != nil {
		t.Fatal(err)
	}
	if a.S.Components["marketplace"] != nil || engineRead(t, inv.CodeServerProduct) != original {
		t.Fatal("reused gallery was changed or deleted during uninstall")
	}
}

func TestMarketplaceScopeIsEnforcedAndOptionalByDefault(t *testing.T) {
	a, inv, original := marketplaceFixture(t, fixtureOpenGallery)
	for _, tc := range []struct {
		name string
		inv  Inventory
	}{
		{"non-container code-server", Inventory{CodeServerProduct: inv.CodeServerProduct}},
		{"container without code-server", Inventory{Container: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := a.planMarketplace(tc.inv); err == nil {
				t.Fatal("unsupported environment accepted by preview")
			}
			if err := a.installMarketplaceWithInventory(context.Background(), tc.inv); err == nil {
				t.Fatal("unsupported environment accepted by installation")
			}
			m := &uiModel{app: a, inventory: tc.inv}
			if hasString(m.componentNames(), "marketplace") {
				t.Fatal("unsupported environment exposed marketplace option")
			}
		})
	}
	if engineRead(t, inv.CodeServerProduct) != original {
		t.Fatal("unsupported plan changed the gallery")
	}
	if _, err := os.Stat(a.P.Data); !os.IsNotExist(err) {
		t.Fatalf("unsupported plan created maintenance artifacts: %v", err)
	}
	m := &uiModel{app: a, inventory: inv, selected: map[string]bool{}}
	m.installWizard(false)
	m.options.Backend = "container"
	m.installComponents()
	item := uiFind(t, m, "[ ] 微软官方插件源 (marketplace)")
	if item.disabled != "" || hasString(m.options.Components, "marketplace") {
		t.Fatal("eligible marketplace must be available and unselected by default")
	}
	item.action(m)
	if !hasString(m.options.Components, "marketplace") || m.screen != "menu" || len(a.S.Components) != 0 {
		t.Fatal("selecting marketplace should only add the optional plan without deploying")
	}
	uiFind(t, m, "[x] 微软官方插件源 (marketplace)").action(m)
	if hasString(m.options.Components, "marketplace") {
		t.Fatal("deselecting marketplace left it in the deployment plan")
	}
}

func TestMarketplaceEnvironmentOverrideBlocksPlanAndApply(t *testing.T) {
	a, inv, original := marketplaceFixture(t, fixtureOpenGallery)
	t.Setenv("EXTENSIONS_GALLERY", `{"serviceUrl":"https://override.example.invalid/gallery"}`)
	if err := a.planMarketplace(inv); err == nil || !strings.Contains(err.Error(), "EXTENSIONS_GALLERY") {
		t.Fatalf("effective gallery override was ignored by preview: %v", err)
	}
	if err := a.installMarketplaceWithInventory(context.Background(), inv); err == nil {
		t.Fatal("installation proceeded despite environment gallery override")
	}
	if engineRead(t, inv.CodeServerProduct) != original || len(a.S.Components) != 0 {
		t.Fatal("blocked installation still changed state")
	}
	m := &uiModel{app: a, inventory: inv}
	if reason := m.componentInstallBlock("marketplace", true); !strings.Contains(reason, "EXTENSIONS_GALLERY") {
		t.Fatalf("override is not represented in option availability: %s", reason)
	}
	t.Setenv("EXTENSIONS_GALLERY", "")
	c := marketplaceInstallFixture(t, a, inv)
	t.Setenv("EXTENSIONS_GALLERY", `{"serviceUrl":"https://override.example.invalid/gallery"}`)
	before := engineRead(t, inv.CodeServerProduct)
	if err := a.applyMarketplace(context.Background(), c); err == nil {
		t.Fatal("reapply ignored gallery override")
	}
	if err := a.checkMarketplace(c); err == nil {
		t.Fatal("diagnosis ignored effective environment override")
	}
	if engineRead(t, inv.CodeServerProduct) != before {
		t.Fatal("blocked reapply changed product")
	}
}
