package app

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Keep only the selected release's bytes in Runtime. Old selection paths stay
// valid through small symlinks, so transaction rollback can use their exact old
// Candidate.Path. Data cache cleanup must preserve codex-rollback.
func (a *App) archiveOldCodexPackages(c *Component) error {
	root := filepath.Join(c.Path, "packages-home", "packages", "standalone")
	current, err := filepath.EvalSymlinks(filepath.Join(root, "current"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	releases := filepath.Join(root, "releases")
	entries, err := os.ReadDir(releases)
	if err != nil {
		return err
	}
	archive := filepath.Join(a.P.Data, "cache", "codex-rollback")
	if err = os.MkdirAll(archive, 0700); err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(releases, entry.Name())
		if path == current {
			continue
		}
		target := filepath.Join(archive, entry.Name()+"-"+ID())
		if err = os.Rename(path, target); err != nil {
			if !errors.Is(err, syscall.EXDEV) {
				return err
			}
			if err = copyComponentTree(path, target); err != nil {
				return err
			}
			if err = os.RemoveAll(path); err != nil {
				return err
			}
		}
		if err = os.Symlink(target, path); err != nil {
			// On failure leave the old path executable when same-filesystem restoration
			// is possible. The archive is retained even if restoration also fails.
			restore := os.Rename(target, path)
			return errors.Join(err, restore)
		}
	}
	return nil
}
func copyComponentTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if !info.Mode().IsRegular() {
			return errors.New("unexpected special file in Codex release")
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}
