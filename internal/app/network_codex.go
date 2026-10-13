package app

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Mirrored installations use the official npm native architecture package. Its
// contents are executed directly; neither Node nor npm is required by the core.
func (a *App) installCodexFromRegistry(ctx context.Context, c *Component, version string) error {
	arch, err := platformArch()
	if err != nil {
		return err
	}
	if runtime.GOOS != "linux" {
		return errors.New("mirrored native Codex installation currently supports Linux")
	}
	nativeVersion := version + "-linux-" + arch
	cache := filepath.Join(a.P.Data, "cache", "codex-"+nativeVersion)
	if err = os.MkdirAll(cache, 0700); err != nil {
		return err
	}
	registry := strings.TrimRight(a.Network.normalized().NPMRegistry, "/")
	metadataPath := filepath.Join(cache, "metadata.json")
	if err = a.componentDownload(ctx, registry+"/@openai%2fcodex/"+nativeVersion, metadataPath); err != nil {
		return err
	}
	b, err := os.ReadFile(metadataPath)
	if err != nil {
		return err
	}
	if len(b) > 4<<20 {
		return errors.New("Codex package metadata too large")
	}
	var metadata struct {
		Name, Version string
		Dist          struct{ Tarball, Integrity string }
	}
	if err = json.Unmarshal(b, &metadata); err != nil {
		return err
	}
	if metadata.Name != "@openai/codex" || metadata.Version != nativeVersion {
		return errors.New("native Codex package identity/version mismatch")
	}
	archiveURL, err := registryTarballURL(registry, metadata.Dist.Tarball)
	if err != nil {
		return err
	}
	archive := filepath.Join(cache, "native.tgz")
	if err = a.componentDownload(ctx, archiveURL, archive); err != nil {
		return err
	}
	a.progress("校验 Codex %s 原生安装包…", version)
	if err = verifySHA512Archive(archive, metadata.Dist.Integrity); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Join(a.P.Data, "tmp"), "codex-release-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err = extractCodexPackage(archive, stage); err != nil {
		return err
	}
	triple := "x86_64-unknown-linux-musl"
	if arch == "arm64" {
		triple = "aarch64-unknown-linux-musl"
	}
	prepared, binaryRelative, err := prepareNativeCodexRelease(stage, triple)
	if err != nil {
		return err
	}
	binary := filepath.Join(prepared, binaryRelative)
	privateHome := filepath.Join(stage, "validation-home")
	if err = os.MkdirAll(privateHome, 0700); err != nil {
		return err
	}
	env := map[string]string{"HOME": privateHome, "CODEX_HOME": privateHome, "BASH_ENV": "", "ENV": ""}
	output, err := a.componentRun(ctx, env, binary, "--version")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(output)) != "codex-cli "+version {
		return errors.New("installed Codex version mismatch")
	}
	if err = os.RemoveAll(privateHome); err != nil {
		return err
	}
	hasHost := executable(filepath.Join(prepared, "bin", "codex-code-mode-host"))
	standalone := filepath.Join(c.Path, "packages-home", "packages", "standalone")
	releases := filepath.Join(standalone, "releases")
	if err = os.MkdirAll(releases, 0700); err != nil {
		return err
	}
	release := filepath.Join(releases, version+"-"+triple)
	if _, err = os.Lstat(release); err == nil {
		return fmt.Errorf("release %s already exists; use recorded rollback instead of overwriting it", version)
	} else if !os.IsNotExist(err) {
		return err
	}
	// Cross-device staging keeps the large temporary package in maintenance data;
	// the destination is the new immutable, registered runtime release itself.
	if err = copyComponentTree(prepared, release); err != nil {
		return err
	}
	if err = selectCodexLink(filepath.Join(standalone, "current"), release); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Join(c.Path, "bin"), 0700); err != nil {
		return err
	}
	if err = selectCodexLink(filepath.Join(c.Path, "bin", "codex"), filepath.Join(standalone, "current", binaryRelative)); err != nil {
		return err
	}
	if hasHost {
		if err = selectCodexLink(filepath.Join(c.Path, "bin", "codex-code-mode-host"), filepath.Join(standalone, "current", "bin", "codex-code-mode-host")); err != nil {
			return err
		}
	} else {
		visibleHost := filepath.Join(c.Path, "bin", "codex-code-mode-host")
		if target, e := os.Readlink(visibleHost); e == nil && target == filepath.Join(standalone, "current", "bin", "codex-code-mode-host") {
			if err = os.Remove(visibleHost); err != nil {
				return err
			}
		}
	}
	c.Version = version
	a.progress("Codex %s 原生版本验证完成", version)
	return nil
}

// Registry metadata may publish npm-relative tarballs such as /@openai/...
// (Huawei). These are relative to the registry namespace, not the host root;
// RFC URL.ResolveReference alone would incorrectly discard /repository/npm.
func registryTarballURL(registry, tarball string) (string, error) {
	if err := validNetworkURL(registry, false); err != nil {
		return "", fmt.Errorf("npm registry URL: %w", err)
	}
	base, _ := url.Parse(registry)
	archive, err := url.Parse(tarball)
	if err != nil || tarball == "" || archive.User != nil || archive.Fragment != "" || archive.Opaque != "" || strings.ContainsAny(tarball, "\\\r\n\t") {
		return "", errors.New("invalid npm tarball URL: credentials, fragments and control characters are not allowed")
	}
	for _, segment := range strings.Split(archive.Path, "/") {
		if segment == "." || segment == ".." {
			return "", errors.New("npm tarball path must not contain traversal segments")
		}
	}
	if strings.Contains(archive.Path, "\\") || archive.Path == "" || archive.Path == "/" {
		return "", errors.New("invalid npm tarball path")
	}
	if archive.IsAbs() {
		if (archive.Scheme != "https" && archive.Scheme != "http") || archive.Hostname() == "" {
			return "", errors.New("npm tarball requires HTTP(S)")
		}
		if archive.Host != "registry.npmjs.org" {
			return archive.String(), nil
		}
	} else if archive.Host != "" {
		return "", errors.New("npm tarball must not use a scheme-relative host")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(archive.Path, "/")
	base.RawPath = ""
	base.RawQuery = archive.RawQuery
	return base.String(), nil
}

// Preserve the current native package's complete release layout, including
// rg, bwrap and bundled shell resources. Older packages used codex/ and path/
// directories; normalize those the same way as the upstream standalone installer.
func prepareNativeCodexRelease(stage, triple string) (string, string, error) {
	vendor := filepath.Join(stage, "package", "vendor", triple)
	if executable(filepath.Join(vendor, "bin", "codex")) {
		for _, rel := range []string{"bin/codex-code-mode-host", "codex-path/rg", "codex-resources/bwrap"} {
			if !executable(filepath.Join(vendor, rel)) {
				return "", "", fmt.Errorf("incomplete native Codex package: %s", rel)
			}
		}
		if _, err := os.Stat(filepath.Join(vendor, "codex-package.json")); err != nil {
			return "", "", fmt.Errorf("missing native Codex package manifest: %w", err)
		}
		if err := os.Symlink("bin/codex", filepath.Join(vendor, "codex")); err != nil {
			return "", "", err
		}
		return vendor, "bin/codex", nil
	}
	for _, rel := range []string{"codex/codex", "path/rg", "codex-resources/bwrap"} {
		if !executable(filepath.Join(vendor, rel)) {
			return "", "", fmt.Errorf("incomplete legacy native Codex package: %s", rel)
		}
	}
	prepared := filepath.Join(stage, "legacy-release")
	if err := os.MkdirAll(prepared, 0700); err != nil {
		return "", "", err
	}
	if err := copyComponentTree(filepath.Join(vendor, "codex-resources"), filepath.Join(prepared, "codex-resources")); err != nil {
		return "", "", err
	}
	for source, destination := range map[string]string{"codex/codex": "codex", "path/rg": "codex-resources/rg"} {
		if err := copyFile(filepath.Join(vendor, source), filepath.Join(prepared, destination), 0700); err != nil {
			return "", "", err
		}
	}
	return prepared, "codex", nil
}
func selectCodexLink(path, target string) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("refusing to overwrite non-symlink %s", path)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	tmp := path + ".select-" + ID()
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, path)
}
func verifySHA512Archive(path, integrity string) error {
	digest := ""
	for _, part := range strings.Fields(integrity) {
		if strings.HasPrefix(part, "sha512-") {
			digest = strings.TrimPrefix(part, "sha512-")
			break
		}
	}
	expected, err := base64.StdEncoding.DecodeString(digest)
	if err != nil || len(expected) != sha512.Size {
		return errors.New("native Codex package requires valid SHA512 integrity")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha512.New()
	if _, err = io.Copy(h, f); err != nil {
		return err
	}
	if !strings.EqualFold(fmt.Sprintf("%x", expected), fmt.Sprintf("%x", h.Sum(nil))) {
		return errors.New("native Codex package integrity mismatch")
	}
	return nil
}
func extractCodexPackage(path, destination string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(header.Name)
		if clean == "." || clean == "package" {
			continue
		}
		if strings.Contains(header.Name, "\\") || !strings.HasPrefix(clean, "package"+string(os.PathSeparator)) || filepath.IsAbs(clean) {
			return errors.New("unsafe path in Codex archive")
		}
		target := filepath.Join(destination, clean)
		if header.Typeflag == tar.TypeDir {
			if err = os.MkdirAll(target, 0700); err != nil {
				return err
			}
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return errors.New("unsupported link or special file in Codex archive")
		}
		total += header.Size
		if header.Size < 0 || total > 2<<30 {
			return errors.New("Codex archive exceeds extraction limit")
		}
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		mode := os.FileMode(0600)
		if header.Mode&0111 != 0 {
			mode = 0700
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, reader)
		closeErr := out.Close()
		if err = errors.Join(copyErr, closeErr); err != nil {
			return err
		}
	}
}
