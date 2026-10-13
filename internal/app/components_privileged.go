package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

func (a *App) brandingTargetAllowed(c *Component, target string) bool {
	if c.Name != "branding" || c.Options["media"] == "" || filepath.Dir(target) != filepath.Clean(c.Options["media"]) {
		return false
	}
	switch filepath.Base(target) {
	case "favicon.ico", "favicon-dark-support.svg", "pwa-icon-192.png", "pwa-icon-maskable-192.png", "pwa-icon-512.png", "pwa-icon-maskable-512.png":
	default:
		return false
	}
	for _, id := range c.Backups {
		b, err := a.ReadBackup(id)
		if err == nil && b.Original == target {
			return true
		}
	}
	return false
}

func (a *App) componentBrandingWrite(ctx context.Context, c *Component, target string, data []byte, mode os.FileMode) error {
	err := AtomicWrite(target, data, mode)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrPermission) {
		return err
	}
	// Root escalation is limited to the six explicitly backed-up image paths.
	// Symlinked parent chains are rejected before any privileged command runs.
	if !a.brandingTargetAllowed(c, target) {
		return fmt.Errorf("refusing privileged branding write outside selected backed-up paths: %s", target)
	}
	if err = checkParents(target); err != nil {
		return err
	}
	return a.privilegedBrandingWrite(ctx, c, target, data, mode)
}
func (a *App) privilegedBrandingWrite(ctx context.Context, c *Component, target string, data []byte, mode os.FileMode) (err error) {
	if !a.brandingTargetAllowed(c, target) {
		return errors.New("unregistered privileged branding target")
	}
	if err = checkParents(target); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Join(a.P.Data, "tmp"), "branding-source-")
	if err != nil {
		return err
	}
	source := temp.Name()
	defer os.Remove(source)
	if _, err = temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	adjacent := filepath.Join(filepath.Dir(target), ".codex-bg-branding-"+ID())
	// This adjacent temporary replacement is the only target-directory exception.
	// It is never a glob and never an input-selected command or mode.
	defer func() {
		_, cleanupErr := a.componentRun(context.WithoutCancel(ctx), nil, "sudo", "-n", "rm", "-f", "--", adjacent)
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("privileged replacement cleanup failed: %w", cleanupErr))
		}
	}()
	if _, err = a.componentRun(ctx, nil, "sudo", "-n", "install", "-m", strconv.FormatUint(uint64(mode.Perm()), 8), "--", source, adjacent); err != nil {
		return err
	}
	if _, err = a.componentRun(ctx, nil, "sudo", "-n", "mv", "-f", "--", adjacent, target); err != nil {
		return err
	}
	actual, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	if componentHash(actual) != componentHash(data) {
		return errors.New("privileged branding write hash mismatch")
	}
	return nil
}
func (a *App) restoreBrandingBackup(ctx context.Context, c *Component, id string) error {
	if err := a.VerifyBackup(id); err != nil {
		return err
	}
	backup, err := a.ReadBackup(id)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(backup.DataPath)
	if err != nil {
		return err
	}
	if err = a.componentBrandingWrite(ctx, c, backup.Original, data, os.FileMode(backup.Mode)); err != nil {
		return err
	}
	hash, err := Hash(backup.Original)
	if err != nil {
		return err
	}
	if hash != backup.Hash {
		return errors.New("branding restore hash mismatch; backup retained")
	}
	backup.Ref = ""
	return a.SaveBackup(backup)
}
