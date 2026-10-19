package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Replace the complete gallery object so Open VSX cache/resource endpoints do
// not survive a switch of serviceUrl. No user/workspace settings are changed.
const microsoftGallery = `{
    "serviceUrl": "https://marketplace.visualstudio.com/_apis/public/gallery",
    "itemUrl": "https://marketplace.visualstudio.com/items",
    "resourceUrlTemplate": "https://{publisher}.vscode-unpkg.net/{publisher}/{name}/{version}/{path}"
  }`

func codeServerProductValid(path string) bool {
	if !filepath.IsAbs(path) || filepath.Base(path) != "product.json" || filepath.Base(filepath.Dir(path)) != "vscode" || filepath.Base(filepath.Dir(filepath.Dir(path))) != "lib" {
		return false
	}
	if checkParents(path) != nil {
		return false
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	raw, err := os.ReadFile(filepath.Join(root, "package.json"))
	var pkg struct{ Name string }
	if err != nil || json.Unmarshal(raw, &pkg) != nil || pkg.Name != "code-server" {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func discoverCodeServerProduct(candidates []Candidate) string {
	for _, c := range candidates {
		if !c.Compatible {
			continue
		}
		path, err := filepath.EvalSymlinks(c.Path)
		if err != nil {
			continue
		}
		// Handles standalone distributions and npm layouts without modifying
		// other VS Code installations found on the same host.
		for dir, depth := filepath.Dir(path), 0; depth < 4 && dir != "/"; dir, depth = filepath.Dir(dir), depth+1 {
			product := filepath.Join(dir, "lib/vscode/product.json")
			if codeServerProductValid(product) {
				return product
			}
		}
		// Distro launchers may be regular scripts rather than symlinks.
		for _, root := range []string{"/usr/lib/code-server", "/usr/local/lib/code-server"} {
			if path != "/usr/bin/code-server" && path != "/usr/local/bin/code-server" {
				continue
			}
			product := filepath.Join(root, "lib/vscode/product.json")
			if codeServerProductValid(product) {
				return product
			}
		}
	}
	return ""
}

func marketplaceInstallBlock(inv Inventory) string {
	if !inv.Container || inv.CodeServerProduct == "" {
		return "微软官方插件源仅适用于已识别到 code-server 的容器"
	}
	if os.Getenv("EXTENSIONS_GALLERY") != "" {
		return "EXTENSIONS_GALLERY 正在覆盖插件源，请先处理容器环境配置"
	}
	if raw, err := os.ReadFile(inv.CodeServerProduct); err == nil {
		if gallery, err := productGallery(raw); err == nil && sameGallery(gallery, []byte(microsoftGallery)) {
			return "" // Registering an existing gallery needs no system write permission.
		}
	}
	if unix.Access(filepath.Dir(inv.CodeServerProduct), unix.W_OK) != nil && !inv.Privileged {
		return "code-server 配置目录不可写，且没有可用的免密 sudo"
	}
	if info, err := os.Lstat(inv.CodeServerProduct); err == nil {
		if st, ok := info.Sys().(*syscall.Stat_t); ok && !marketplaceLocalOwnership(inv.CodeServerProduct, st) && !inv.Privileged {
			return "保留 code-server 原文件所有者需要 root 或免密 sudo"
		}
	}
	return ""
}

// Gallery edits retain all other product bytes, including comments and the
// newer product version that may have arrived with a rebuilt container.
func productGallery(raw []byte) ([]byte, error) {
	members, _, err := jsonMembers(raw)
	if err != nil {
		return nil, err
	}
	for _, member := range members {
		if member.key == "extensionsGallery" {
			value := raw[member.valueStart:member.end]
			clean, err := cleanJSONC(value)
			if err != nil || len(bytes.TrimSpace(clean)) == 0 || bytes.TrimSpace(clean)[0] != '{' {
				return nil, errors.New("extensionsGallery 不是有效对象；保留原配置")
			}
			return value, nil
		}
	}
	return nil, errors.New("code-server product.json 缺少 extensionsGallery；保留原配置")
}

func replaceProductGallery(raw, gallery []byte) ([]byte, error) {
	if _, err := productGallery(raw); err != nil {
		return nil, err
	}
	members, _, _ := jsonMembers(raw)
	for _, member := range members {
		if member.key == "extensionsGallery" {
			out := append([]byte{}, raw[:member.valueStart]...)
			out = append(out, gallery...)
			return append(out, raw[member.end:]...), nil
		}
	}
	return nil, errors.New("missing gallery")
}

func sameGallery(a, b []byte) bool {
	canonical := func(raw []byte) string {
		clean, err := cleanJSONC(raw)
		if err != nil {
			return ""
		}
		// Map ordering/indentation are immaterial, nested JSON is normalized too.
		var obj any
		decoder := json.NewDecoder(bytes.NewReader(clean))
		decoder.UseNumber()
		if decoder.Decode(&obj) != nil {
			return ""
		}
		encoded, _ := json.Marshal(obj)
		return string(encoded)
	}
	x, y := canonical(a), canonical(b)
	return x != "" && x == y
}

func (a *App) planMarketplace(inv Inventory) error {
	if reason := marketplaceInstallBlock(inv); reason != "" {
		return errors.New(reason)
	}
	if !codeServerProductValid(inv.CodeServerProduct) {
		return errors.New("无法验证 code-server product.json 归属")
	}
	raw, err := os.ReadFile(inv.CodeServerProduct)
	if err != nil {
		return err
	}
	gallery, err := productGallery(raw)
	if err != nil {
		return err
	}
	if a.Out != nil {
		if sameGallery(gallery, []byte(microsoftGallery)) {
			fmt.Fprintf(a.Out, "插件源: 已是微软官方源，复用现状，不创建恢复备份\n目标: %s\n", inv.CodeServerProduct)
		} else {
			fmt.Fprintf(a.Out, "插件源: %s 的 extensionsGallery → 微软官方 Marketplace\n备份: %s/backups；关闭选项时恢复原插件源\n", inv.CodeServerProduct, a.P.Data)
		}
		fmt.Fprintln(a.Out, "影响此 code-server 的整个插件库。需手动重启 code-server 后生效；不安装任何插件。")
	}
	return nil
}

func (a *App) installMarketplace(ctx context.Context) error {
	inv, err := a.Discover(ctx)
	if err != nil {
		return err
	}
	return a.installMarketplaceWithInventory(ctx, inv)
}

func (a *App) installMarketplaceWithInventory(ctx context.Context, inv Inventory) error {
	if err := a.planMarketplace(inv); err != nil {
		return err
	}
	if err := a.componentDirs(); err != nil {
		return err
	}
	raw, err := os.ReadFile(inv.CodeServerProduct)
	if err != nil {
		return err
	}
	gallery, err := productGallery(raw)
	if err != nil {
		return err
	}
	c := &Component{Name: "marketplace", Path: inv.CodeServerProduct, Ownership: "reused", Status: "installed", Options: map[string]string{"product": inv.CodeServerProduct}}
	if sameGallery(gallery, []byte(microsoftGallery)) {
		a.S.Components[c.Name] = c
		return a.Save()
	}
	c.Path, c.Ownership, c.Status = a.componentRoot(c.Name), "managed", "installing"
	if _, err = os.Lstat(c.Path); err == nil {
		return fmt.Errorf("拒绝接管已有目录: %s", c.Path)
	} else if !os.IsNotExist(err) {
		return err
	}
	backup, err := a.BackupFile(inv.CodeServerProduct, c.Name, c.Name)
	if err != nil {
		return err
	}
	if backup.Hash != HashBytes(raw) {
		return errors.New("备份期间 product.json 发生变化；未替换插件源")
	}
	c.Backups = []string{backup.ID}
	a.S.Components[c.Name] = c
	if err = a.Save(); err != nil {
		return err
	}
	if err = os.MkdirAll(c.Path, 0700); err == nil {
		err = a.applyMarketplace(ctx, c)
	}
	if err != nil {
		c.Status = "failed"
		return errors.Join(err, a.Save())
	}
	c.Status = "installed"
	return a.Save()
}

func (a *App) marketplaceOriginal(c *Component) ([]byte, error) {
	if len(c.Backups) != 1 {
		return nil, errors.New("插件源恢复材料缺失；保留现有配置")
	}
	if err := a.VerifyBackup(c.Backups[0]); err != nil {
		return nil, err
	}
	b, err := a.ReadBackup(c.Backups[0])
	if err != nil {
		return nil, err
	}
	if b.Original != c.Options["product"] || b.Kind != "marketplace" {
		return nil, errors.New("插件源备份关联不匹配")
	}
	raw, err := os.ReadFile(b.DataPath)
	if err != nil {
		return nil, err
	}
	return productGallery(raw)
}

func (a *App) marketplaceCurrent(c *Component) ([]byte, []byte, error) {
	target := c.Options["product"]
	if !codeServerProductValid(target) {
		return nil, nil, errors.New("code-server product.json 缺失或归属变化；未替换")
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return nil, nil, err
	}
	gallery, err := productGallery(raw)
	return raw, gallery, err
}

func (a *App) checkMarketplace(c *Component) error {
	if os.Getenv("EXTENSIONS_GALLERY") != "" {
		return errors.New("EXTENSIONS_GALLERY 覆盖了 product.json 插件源")
	}
	_, gallery, err := a.marketplaceCurrent(c)
	if err != nil {
		return err
	}
	if !sameGallery(gallery, []byte(microsoftGallery)) {
		return errors.New("插件源与已登记的微软源不一致；需要重新应用或检查外部改动")
	}
	if c.Ownership == "managed" {
		_, err = a.marketplaceOriginal(c)
	}
	return err
}

func (a *App) applyMarketplace(ctx context.Context, c *Component) error {
	if os.Getenv("EXTENSIONS_GALLERY") != "" {
		return errors.New("EXTENSIONS_GALLERY 正在覆盖插件源，未应用")
	}
	raw, gallery, err := a.marketplaceCurrent(c)
	if err != nil {
		return err
	}
	original, err := a.marketplaceOriginal(c)
	if err != nil {
		return err
	}
	if sameGallery(gallery, []byte(microsoftGallery)) {
		return nil
	}
	if !sameGallery(gallery, original) {
		return errors.New("插件源已被外部修改，保留现状及备份；请先人工确认")
	}
	updated, err := replaceProductGallery(raw, []byte(microsoftGallery))
	if err != nil {
		return err
	}
	target := c.Options["product"]
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	// Persist intent first: failed writes and interrupted installs can be safely
	// uninstalled even if the process exited between rename and Save.
	c.Files = []ManagedFile{{Path: target, Hash: HashBytes(updated), Kind: "marketplace"}}
	if err = a.Save(); err != nil {
		return err
	}
	a.progress("替换 code-server 插件源；需要重启 code-server 后生效")
	return a.writeMarketplaceProduct(ctx, c, target, updated, info.Mode().Perm(), HashBytes(raw))
}

func (a *App) restoreMarketplace(ctx context.Context, c *Component) error {
	raw, gallery, err := a.marketplaceCurrent(c)
	if err != nil {
		return err
	}
	original, err := a.marketplaceOriginal(c)
	if err != nil {
		return err
	}
	if !sameGallery(gallery, original) {
		if !sameGallery(gallery, []byte(microsoftGallery)) {
			return errors.New("插件源被外部修改；保留配置和备份，未卸载")
		}
		updated, err := replaceProductGallery(raw, original)
		if err != nil {
			return err
		}
		info, err := os.Stat(c.Options["product"])
		if err != nil {
			return err
		}
		if err = a.writeMarketplaceProduct(ctx, c, c.Options["product"], updated, info.Mode().Perm(), HashBytes(raw)); err != nil {
			return err
		}
	}
	for _, id := range c.Backups {
		b, err := a.ReadBackup(id)
		if err != nil {
			return err
		}
		b.Ref = ""
		if err = a.SaveBackup(b); err != nil {
			return err
		}
	}
	a.progress("已恢复原插件源；请手动重启 code-server 后使用")
	return nil
}

func marketplaceSummary(c *Component) string {
	if c == nil {
		return "未启用"
	}
	return strings.Join([]string{c.Status, c.Ownership, c.Options["product"]}, " · ")
}
