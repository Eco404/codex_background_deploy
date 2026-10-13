package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (a *App) installApti(ctx context.Context, c *Component) error {
	inv, err := a.Discover(ctx)
	if err != nil {
		return err
	}
	if !inv.Container || !inv.Apt {
		return errors.New("apti requires a container with apt-get/dpkg-query")
	}
	script := `#!/usr/bin/env bash
set -uo pipefail
ROOT=__ROOT__
LIST="$ROOT/packages.txt"
TMP="$ROOT/.packages.tmp"
LOCK="$ROOT/.apti-lock"
valid(){ [[ "$1" =~ ^[a-z0-9][a-z0-9+.-]*(:[a-z0-9-]+)?$ ]]; }
root(){ if [ "$(id -u)" = 0 ]; then "$@"; else sudo -n "$@"; fi; }
action="${1:-list}"
case "$action" in list) cat "$LIST"; exit $?;; install|forget|restore) shift;; *) action=install;; esac
packages=()
if [ "$action" = restore ]; then
 while IFS= read -r p; do
  [ -n "$p" ] || continue
  valid "$p" || exit 2
  dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -Fq 'install ok installed' || packages+=("$p")
 done < "$LIST"
 [ "${#packages[@]}" != 0 ] || exit 0
else
 [ "$#" != 0 ] || exit 2
 for p in "$@"; do valid "$p" || exit 2; packages+=("$p"); done
fi
mkdir "$LOCK" 2>/dev/null || { echo 'apti operation already active' >&2; exit 1; }
trap 'rm -f "$TMP"; rmdir "$LOCK" 2>/dev/null || true' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if [ "$action" = forget ]; then
 cp "$LIST" "$TMP" || exit 1
 for p in "${packages[@]}"; do
  content="$(awk -v p="$p" '$0 != p' "$TMP")" || exit 1
  printf '%s\n' "$content" > "$TMP" || exit 1
 done
else
 root apt-get -o DPkg::Lock::Timeout=60 -o Acquire::Retries=2 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 update || exit 1
 root /usr/bin/env DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=60 install -y -- "${packages[@]}" || exit 1
 [ "$action" != restore ] || exit 0
 { cat "$LIST"; printf '%s\n' "${packages[@]}"; } | LC_ALL=C sort -u > "$TMP" || exit 1
fi
mv "$TMP" "$LIST"
`
	script = strings.ReplaceAll(script, "__ROOT__", shellQuote(c.Path))
	if err = AtomicWrite(filepath.Join(c.Path, "packages.txt"), nil, 0600); err != nil {
		return err
	}
	if err = a.componentFile(c, filepath.Join(c.Path, "apti"), []byte(script), 0700); err != nil {
		return err
	}
	entry := filepath.Join(a.P.Home, ".local", "bin", "apti")
	if _, e := os.Lstat(entry); e == nil {
		return fmt.Errorf("existing apti entry preserved: %s", entry)
	} else if !os.IsNotExist(e) {
		return e
	}
	wrapper := "#!/bin/sh\nexec " + shellQuote(filepath.Join(c.Path, "apti")) + " \"$@\"\n"
	if err = a.componentFile(c, entry, []byte(wrapper), 0700); err != nil {
		return err
	}
	c.Options["command"] = filepath.Join(c.Path, "apti")
	return nil
}
func (a *App) installBranding(ctx context.Context, c *Component, opt ComponentOptions) error {
	inv, err := a.Discover(ctx)
	if err != nil {
		return err
	}
	if !inv.Container {
		return errors.New("branding is supported only in code-server containers")
	}
	media := opt.MediaDir
	if media == "" {
		media = inv.MediaDir
	}
	if media == "" {
		return errors.New("code-server media directory not found")
	}
	media, err = filepath.Abs(media)
	if err != nil {
		return err
	}
	if s, e := os.Stat(media); e != nil || !s.IsDir() {
		return errors.New("invalid branding directory")
	}
	c.Options["media"] = media
	entries, err := componentAssets.ReadDir("branding")
	if err != nil {
		return err
	}
	// Save every original before touching any target. Restoration is never based
	// on an assumed image version or another deployment's copied resources.
	for _, entry := range entries {
		target := filepath.Join(media, entry.Name())
		backup, e := a.BackupFile(target, "branding", c.Name)
		if e != nil {
			return e
		}
		c.Backups = append(c.Backups, backup.ID)
		if e = a.Save(); e != nil {
			return e
		}
	}
	return a.applyBranding(ctx, c)
}
func (a *App) applyBranding(ctx context.Context, c *Component) error {
	entries, err := componentAssets.ReadDir("branding")
	if err != nil {
		return err
	}
	originals := map[string]string{}
	for _, id := range c.Backups {
		backup, e := a.ReadBackup(id)
		if e != nil {
			return e
		}
		if e = a.VerifyBackup(id); e != nil {
			return e
		}
		originals[backup.Original] = backup.Hash
	}
	// A recreated container may expose the exact original image assets again.
	// Reapply those safely; a newer image or external edit requires confirmation.
	for _, f := range c.Files {
		if f.Kind != "branding" {
			continue
		}
		b, e := os.ReadFile(f.Path)
		if e != nil || (componentHash(b) != f.Hash && componentHash(b) != originals[f.Path]) {
			return fmt.Errorf("branding target differs from both managed and backed-up original; confirm new image assets before reapplying: %s", f.Path)
		}
	}
	for _, entry := range entries {
		b, e := componentAssets.ReadFile("branding/" + entry.Name())
		if e != nil {
			return e
		}
		target := filepath.Join(c.Options["media"], entry.Name())
		if e = a.componentBrandingWrite(ctx, c, target, b, 0644); e != nil {
			return fmt.Errorf("branding write %s (requires target write permission): %w", target, e)
		}
		found := false
		for _, f := range c.Files {
			if f.Path == target {
				found = true
			}
		}
		if !found {
			c.Files = append(c.Files, ManagedFile{Path: target, Hash: componentHash(b), Kind: "branding"})
		}
		if e = a.Save(); e != nil {
			return e
		}
	}
	return nil
}
