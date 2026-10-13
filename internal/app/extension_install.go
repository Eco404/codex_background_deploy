package app

import (
	"archive/zip"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// An explicitly selected extension becomes an IDE-managed installation. Core
// rollback/uninstall restores our entry hook but never removes the IDE package.
type ExtensionInstallOptions struct {
	Root, Source, CodeServer string
	Yes, DryRun              bool
}

type extensionManifest struct {
	Publisher string `json:"publisher"`
	Name      string `json:"name"`
	Version   string `json:"version"`
}

type vsixIdentity struct {
	Metadata struct {
		Identity struct {
			ID             string `xml:"Id,attr"`
			Publisher      string `xml:"Publisher,attr"`
			Version        string `xml:"Version,attr"`
			TargetPlatform string `xml:"TargetPlatform,attr"`
		} `xml:"Identity"`
	} `xml:"Metadata"`
}

type extensionReceipt struct {
	ID, Action, Status, Root, Source, CodeServer, Version, PackageHash, Error string
	Started                                                                   time.Time
	Directories                                                               []string
}

func nativeExtensionPlatform() string {
	arch, _ := platformArch()
	return "linux-" + arch
}

func nativeExtensionEntries() []string {
	if nativeExtensionPlatform() == "linux-arm64" {
		return []string{"extension/bin/linux-arm64/codex", "extension/bin/linux-aarch64/codex"}
	}
	return []string{"extension/bin/linux-x64/codex", "extension/bin/linux-x86_64/codex"}
}

func (a *App) marketplaceExtensionURL(ctx context.Context) (string, error) {
	if err := a.Network.Validate(); err != nil {
		return "", err
	}
	// Ask for both latest stable and prerelease per platform. The /latest/
	// package alias currently returns 404 for this platform-specific extension.
	// 65683 = versions + files + properties + asset URI + latest stable/prerelease.
	body := `{"filters":[{"criteria":[{"filterType":7,"value":"openai.chatgpt"}]}],"flags":65683}`
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://marketplace.visualstudio.com/_apis/public/gallery/extensionquery", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json;api-version=7.2-preview.1")
	a.progress("查询官方 Marketplace 中适用于 %s 的稳定版 Codex 扩展…", nativeExtensionPlatform())
	response, err := a.networkClient().Do(request)
	if err != nil {
		return "", fmt.Errorf("Marketplace 查询失败: %s", a.Network.redact(err.Error()))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Marketplace 查询 HTTP %d；也可选择本地 VSIX", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if err != nil {
		return "", err
	}
	if len(raw) > 16<<20 {
		return "", errors.New("Marketplace 元数据超出大小限制")
	}
	return selectMarketplacePackage(raw, nativeExtensionPlatform())
}

func selectMarketplacePackage(raw []byte, platform string) (string, error) {
	var response struct {
		Results []struct {
			Extensions []struct {
				ExtensionName string
				Publisher     struct{ PublisherName string }
				Versions      []struct {
					Version, TargetPlatform string
					Properties              []struct{ Key, Value string }
					Files                   []struct{ AssetType, Source string }
				}
			}
		}
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", err
	}
	// Prefer the published native platform. A universal archive is accepted only
	// if subsequent archive validation finds a current-architecture Codex entry.
	for _, target := range []string{platform, "universal", ""} {
		for _, result := range response.Results {
			for _, extension := range result.Extensions {
				if extension.Publisher.PublisherName != "openai" || extension.ExtensionName != "chatgpt" {
					continue
				}
				for _, version := range extension.Versions {
					if version.TargetPlatform != target || !componentVersion.MatchString(version.Version) || strings.ContainsAny(version.Version, "-+") {
						continue
					}
					prerelease := false
					for _, property := range version.Properties {
						if property.Key == "Microsoft.VisualStudio.Code.PreRelease" && strings.EqualFold(property.Value, "true") {
							prerelease = true
						}
					}
					if prerelease {
						continue
					}
					for _, file := range version.Files {
						if file.AssetType != "Microsoft.VisualStudio.Services.VSIXPackage" {
							continue
						}
						u, err := url.Parse(file.Source)
						if err != nil || u.Scheme != "https" || u.Host != "openai.gallerycdn.vsassets.io" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/extensions/openai/chatgpt/"+version.Version+"/") || !strings.HasSuffix(u.Path, "/Microsoft.VisualStudio.Services.VSIXPackage") {
							return "", errors.New("Marketplace 返回了非预期扩展下载地址")
						}
						return file.Source, nil
					}
				}
			}
		}
	}
	return "", fmt.Errorf("Marketplace 未提供 %s 的稳定版 Codex 扩展；可选择本地 VSIX", platform)
}

// PlanExtension is read-only. Local metadata is checked before confirmation;
// downloaded archive validation is repeated before code-server can install it.
func (a *App) PlanExtension(ctx context.Context, opt ExtensionInstallOptions) (ExtensionInstallOptions, error) {
	if opt.Root == "" || !filepath.IsAbs(opt.Root) {
		return opt, errors.New("请指定 code-server 扩展根目录的绝对路径")
	}
	opt.Root = filepath.Clean(opt.Root)
	if opt.Root == "/" || opt.Root == a.P.Home || Within(opt.Root, a.P.Runtime) || Within(a.P.Data, opt.Root) {
		return opt, errors.New("扩展根目录范围过大或与运行/维护目录冲突")
	}
	if err := checkParents(opt.Root); err != nil {
		return opt, err
	}
	if matches, _ := filepath.Glob(filepath.Join(opt.Root, "openai.chatgpt-*")); len(matches) > 0 {
		return opt, errors.New("所选目录已有 Codex 扩展，保留现有安装；请通过 IDE 更新，或直接选择接管现有入口")
	}
	if opt.CodeServer == "" {
		inv, err := a.Discover(ctx)
		if err != nil {
			return opt, err
		}
		for _, c := range inv.Candidates["code-server"] {
			if c.Compatible {
				opt.CodeServer = c.Path
				break
			}
		}
	}
	if !filepath.IsAbs(opt.CodeServer) || !executable(opt.CodeServer) {
		return opt, errors.New("未发现可用 code-server；请指定其可执行文件绝对路径")
	}
	if output, err := a.probe(ctx, opt.CodeServer, "--version"); err != nil || discoveryVersion.FindString(string(output)) == "" {
		return opt, errors.New("code-server 版本检查失败")
	}
	if opt.Source == "" {
		return opt, errors.New("请选择来源：marketplace、本地 VSIX 或已解压的原始 Codex 扩展目录")
	}
	if opt.Source != "marketplace" {
		if !filepath.IsAbs(opt.Source) {
			return opt, errors.New("本地扩展来源必须是绝对路径")
		}
		info, err := os.Stat(opt.Source)
		if err != nil {
			return opt, err
		}
		if info.IsDir() {
			if _, err = validateExtensionDirectory(opt.Source); err != nil {
				return opt, err
			}
		} else if info.Mode().IsRegular() {
			if _, err = validateExtensionVSIX(opt.Source); err != nil {
				return opt, err
			}
		} else {
			return opt, errors.New("扩展来源必须是目录或普通 VSIX 文件")
		}
	}
	return opt, nil
}

func (a *App) InstallExtension(ctx context.Context, opt ExtensionInstallOptions) error {
	plan, err := a.PlanExtension(ctx, opt)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "Codex 扩展: %s → %s\n使用 code-server 安装；保留现有插件库配置，扩展交由 IDE 管理。\n", plan.Source, plan.Root)
	if opt.DryRun {
		return nil
	}
	if !opt.Yes {
		return errors.New("扩展安装需要明确确认 (--yes)")
	}
	unlock, err := a.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	if hasPending(a.P.Data) {
		return errors.New("请先恢复未完成的操作")
	}
	return a.installExtensionLocked(ctx, plan)
}

func (a *App) installExtensionLocked(ctx context.Context, opt ExtensionInstallOptions) (retErr error) {
	// Recheck the destination after taking the mutation lock. Never request
	// --force: the IDE installer also checks engine and platform compatibility.
	var err error
	opt, err = a.PlanExtension(ctx, opt)
	if err != nil {
		return err
	}
	if err = a.EnsureData(); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Join(a.P.Data, "tmp"), "codex-extension-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	vsix := filepath.Join(stage, "openai.chatgpt.vsix")
	officialVersion := ""
	fmt.Fprintln(a.Out, "准备 Codex 扩展安装包…")
	if opt.Source == "marketplace" {
		var sourceURL string
		sourceURL, err = a.marketplaceExtensionURL(ctx)
		if err == nil {
			u, _ := url.Parse(sourceURL)
			officialVersion = strings.Split(strings.TrimPrefix(u.Path, "/extensions/openai/chatgpt/"), "/")[0]
			err = a.componentDownload(ctx, sourceURL, vsix)
		}
	} else if info, e := os.Stat(opt.Source); e != nil {
		err = e
	} else if info.IsDir() {
		err = packExtensionDirectory(ctx, opt.Source, vsix)
	} else {
		err = copyFile(opt.Source, vsix, 0600)
	}
	if err != nil {
		return err
	}
	manifest, err := validateExtensionVSIX(vsix)
	if err != nil {
		return err
	}
	if officialVersion != "" && manifest.Version != officialVersion {
		return errors.New("下载包版本与 Marketplace 所选稳定版不一致")
	}
	hash, err := Hash(vsix)
	if err != nil {
		return err
	}
	receipt := extensionReceipt{ID: ID(), Action: "extension-install", Status: "installing", Root: opt.Root, Source: opt.Source, CodeServer: opt.CodeServer, Version: manifest.Version, PackageHash: hash, Started: time.Now().UTC()}
	receiptPath := filepath.Join(a.P.Data, "history", "extension-"+receipt.ID+".json")
	saveReceipt := func() error {
		b, e := json.MarshalIndent(receipt, "", "  ")
		if e != nil {
			return e
		}
		return AtomicWrite(receiptPath, b, 0600)
	}
	if err = saveReceipt(); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			receipt.Status, receipt.Error = "failed-preserved", retErr.Error()
			retErr = errors.Join(retErr, saveReceipt())
		}
	}()
	fmt.Fprintf(a.Out, "安装 Codex 扩展 %s（%s）…\n", manifest.Version, nativeExtensionPlatform())
	_, err = a.installRun(ctx, map[string]string{
		"XDG_DATA_HOME": filepath.Join(stage, "data"), "XDG_CONFIG_HOME": filepath.Join(stage, "config"), "XDG_CACHE_HOME": filepath.Join(stage, "cache"),
	}, opt.CodeServer, "--user-data-dir", filepath.Join(stage, "user-data"), "--extensions-dir", opt.Root, "--install-extension", vsix)
	if err != nil {
		return fmt.Errorf("code-server 扩展安装失败；保留目标目录供 IDE 检查: %w", err)
	}
	matches, err := filepath.Glob(filepath.Join(opt.Root, "openai.chatgpt-*"))
	if err != nil {
		return err
	}
	for _, dir := range matches {
		installed, e := validateExtensionDirectory(dir)
		if e == nil && installed.Version == manifest.Version {
			receipt.Directories = append(receipt.Directories, dir)
		}
	}
	if len(receipt.Directories) == 0 {
		return errors.New("code-server 返回成功但未发现完整且版本匹配的 Codex 扩展；保留目标目录并报告失败")
	}
	receipt.Status = "installed-ide-owned"
	if err = saveReceipt(); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "Codex 扩展已安装到 %s。请重新加载 IDE；核心卸载时保留此扩展。\n", opt.Root)
	if a.S.Installed && !hasString(a.S.SelectedRoots, opt.Root) {
		fmt.Fprintln(a.Out, "此目录尚未加入后台接管范围；本次仅安装 IDE 扩展。")
	}
	return nil
}

func validateManifest(pkg, manifest []byte) (extensionManifest, error) {
	var p extensionManifest
	if err := json.Unmarshal(pkg, &p); err != nil {
		return p, err
	}
	if p.Publisher != "openai" || p.Name != "chatgpt" || !componentVersion.MatchString(p.Version) {
		return p, errors.New("仅接受 publisher=openai、name=chatgpt 且版本有效的 Codex 扩展")
	}
	var identity vsixIdentity
	if err := xml.Unmarshal(manifest, &identity); err != nil {
		return p, fmt.Errorf("无效 VSIX 清单: %w", err)
	}
	x := identity.Metadata.Identity
	if x.ID != p.Name || x.Publisher != p.Publisher || x.Version != p.Version {
		return p, errors.New("VSIX 清单与 package.json 的扩展身份不一致")
	}
	if x.TargetPlatform != "" && x.TargetPlatform != "universal" && x.TargetPlatform != nativeExtensionPlatform() {
		return p, fmt.Errorf("扩展平台 %s 不适用于 %s", x.TargetPlatform, nativeExtensionPlatform())
	}
	return p, nil
}

func checkExtensionOriginal(prefix string) error {
	if strings.Contains(prefix, Marker) || legacyEntry(prefix) || strings.Contains(prefix, "payload/bridge/wrapper.sh") {
		return errors.New("来源扩展的 Codex 入口已经被接管；请提供原始 VSIX 或未接管的扩展目录")
	}
	return nil
}

func readSmallFile(file string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if len(b) > 4<<20 {
		return nil, errors.New("扩展元数据过大")
	}
	return b, err
}

func validateExtensionDirectory(dir string) (extensionManifest, error) {
	var empty extensionManifest
	if err := checkParents(dir); err != nil {
		return empty, err
	}
	pkg, err := readSmallFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return empty, err
	}
	manifest, err := readSmallFile(filepath.Join(dir, ".vsixmanifest"))
	if err != nil {
		return empty, fmt.Errorf("已安装扩展缺少原始 .vsixmanifest；请改用 VSIX: %w", err)
	}
	p, err := validateManifest(pkg, manifest)
	if err != nil {
		return p, err
	}
	for _, file := range nativeExtensionEntries() {
		entry := filepath.Join(dir, strings.TrimPrefix(file, "extension/"))
		if executable(entry) {
			if _, backupErr := os.Lstat(entry + ".real"); backupErr == nil {
				return p, errors.New("来源扩展含旧接管备份 codex.real；请使用原始 VSIX 或未接管的扩展目录")
			}
			if err = checkParents(entry); err != nil {
				return p, err
			}
			return p, checkExtensionOriginal(readPrefix(entry, 8192))
		}
	}
	return p, errors.New("扩展不包含当前 Linux 架构可执行的 Codex 入口")
}

func validateExtensionVSIX(file string) (extensionManifest, error) {
	var empty extensionManifest
	z, err := zip.OpenReader(file)
	if err != nil {
		return empty, err
	}
	defer z.Close()
	if len(z.File) > 50000 {
		return empty, errors.New("VSIX 文件数超过限制")
	}
	files := map[string]*zip.File{}
	var total uint64
	for _, f := range z.File {
		n := strings.TrimSuffix(f.Name, "/")
		if n == "" || path.Clean(n) != n || strings.ContainsAny(n, "\\\x00\r\n:") || strings.HasPrefix(n, "/") || n == ".." || strings.HasPrefix(n, "../") {
			return empty, fmt.Errorf("VSIX 包含不安全路径: %q", f.Name)
		}
		if files[n] != nil {
			return empty, fmt.Errorf("VSIX 包含重复路径: %s", n)
		}
		if !f.Mode().IsRegular() && !f.Mode().IsDir() {
			return empty, errors.New("VSIX 不允许符号链接或特殊文件")
		}
		if f.UncompressedSize64 > 3<<30 || total > (3<<30)-f.UncompressedSize64 {
			return empty, errors.New("VSIX 解压大小超过 3 GiB 限制")
		}
		total += f.UncompressedSize64
		files[n] = f
	}
	for n := range files {
		for parent := path.Dir(n); parent != "."; parent = path.Dir(parent) {
			if f := files[parent]; f != nil && !f.Mode().IsDir() {
				return empty, fmt.Errorf("VSIX 文件与目录路径冲突: %s", parent)
			}
		}
	}
	read := func(name string, limit int64) ([]byte, error) {
		f := files[name]
		if f == nil || f.Mode().IsDir() {
			return nil, fmt.Errorf("VSIX 缺少文件: %s", name)
		}
		r, e := f.Open()
		if e != nil {
			return nil, e
		}
		defer r.Close()
		return io.ReadAll(io.LimitReader(r, limit))
	}
	pkg, err := read("extension/package.json", (4<<20)+1)
	if err != nil {
		return empty, err
	}
	manifest, err := read("extension.vsixmanifest", (4<<20)+1)
	if err != nil {
		return empty, err
	}
	if len(pkg) > 4<<20 || len(manifest) > 4<<20 {
		return empty, errors.New("VSIX 元数据过大")
	}
	p, err := validateManifest(pkg, manifest)
	if err != nil {
		return p, err
	}
	for _, file := range nativeExtensionEntries() {
		if f := files[file]; f != nil && !f.Mode().IsDir() && f.UncompressedSize64 > 0 {
			prefix, err := read(file, 8192)
			if err != nil {
				return p, err
			}
			return p, checkExtensionOriginal(string(prefix))
		}
	}
	return p, errors.New("VSIX 不包含当前 Linux 架构的 Codex 入口")
}

func packExtensionDirectory(ctx context.Context, source, target string) (retErr error) {
	if _, err := validateExtensionDirectory(source); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	z := zip.NewWriter(f)
	defer func() { retErr = errors.Join(retErr, z.Close(), f.Close()) }()
	write := func(name string, data []byte) error {
		w, err := z.Create(name)
		if err == nil {
			_, err = w.Write(data)
		}
		return err
	}
	manifest, err := readSmallFile(filepath.Join(source, ".vsixmanifest"))
	if err != nil {
		return err
	}
	if err = write("extension.vsixmanifest", manifest); err != nil {
		return err
	}
	if err = write("[Content_Types].xml", []byte(`<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="json" ContentType="application/json"/><Default Extension="vsixmanifest" ContentType="text/xml"/></Types>`)); err != nil {
		return err
	}
	var total int64
	count := 0
	return filepath.WalkDir(source, func(file string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(source, file)
		if err != nil {
			return err
		}
		if rel == ".vsixmanifest" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("不复制扩展中的符号链接或特殊文件: %s", rel)
		}
		total += info.Size()
		count++
		if count > 50000 || total > 3<<30 {
			return errors.New("扩展大小或文件数超出限制")
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name, header.Method = "extension/"+filepath.ToSlash(rel), zip.Deflate
		w, err := z.CreateHeader(header)
		if err != nil {
			return err
		}
		r, err := os.Open(file)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, r)
		return errors.Join(err, r.Close())
	})
}
