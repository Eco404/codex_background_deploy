package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func marketplaceWriteFixture(t *testing.T) (*App, *Component, string) {
	t.Helper()
	a := engineApp(t)
	target := filepath.Join(a.P.Home, "code-server", "product.json")
	engineFile(t, target, `{"extensionsGallery":{"serviceUrl":"original"}}`)
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatal(err)
	}
	backup, err := a.BackupFile(target, "marketplace", "component:marketplace")
	if err != nil {
		t.Fatal(err)
	}
	c := &Component{Name: "marketplace", Backups: []string{backup.ID}, Options: map[string]string{"product": target}}
	return a, c, target
}

func marketplaceTestRecord(t *testing.T, a *App, c *Component, target string) (string, marketplaceReplacement) {
	t.Helper()
	hash, err := Hash(target)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	id := ID()
	r := marketplaceReplacement{Target: target, Temporary: filepath.Join(filepath.Dir(target), ".codex-bg-marketplace-"+id), Source: filepath.Join(a.P.Data, "tmp", "marketplace-source-"+id), BackupID: c.Backups[0], OriginalHash: hash, NewHash: HashBytes([]byte("new")), UID: st.Uid, GID: st.Gid, Mode: 0644}
	path := filepath.Join(a.P.Data, "transactions", "marketplace-replacements", id+".json")
	raw, _ := json.Marshal(r)
	if err := AtomicWrite(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path, r
}

// Simulates sudo using fixture-owned files only. The test temporarily grants
// itself directory access to emulate root; no privileged process is launched.
func marketplaceFakeSudo(t *testing.T, target string, visit func(context.Context, []string) error) func(context.Context, string, []string, []string) ([]byte, error) {
	t.Helper()
	return func(ctx context.Context, name string, args, _ []string) ([]byte, error) {
		if name != "sudo" || len(args) < 3 || args[0] != "-n" || args[1] != "--" {
			t.Fatalf("unexpected command: %s %q", name, args)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("privileged command has no deadline")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		parent := filepath.Dir(target)
		info, err := os.Stat(parent)
		if err != nil {
			return nil, err
		}
		if err = os.Chmod(parent, 0700); err != nil {
			return nil, err
		}
		defer os.Chmod(parent, info.Mode().Perm())
		switch args[2] {
		case "install":
			if len(args) != 12 || args[3] != "-m" || args[5] != "-o" || args[7] != "-g" || args[9] != "--" {
				t.Fatalf("unsafe install: %q", args)
			}
			data, e := os.ReadFile(args[10])
			if e != nil {
				return nil, e
			}
			mode, _ := strconv.ParseUint(args[4], 8, 32)
			uid, _ := strconv.Atoi(args[6])
			gid, _ := strconv.Atoi(args[8])
			if err = os.WriteFile(args[11], data, os.FileMode(mode)); err == nil {
				err = os.Chown(args[11], uid, gid)
			}
			if err == nil {
				err = os.Chmod(args[11], os.FileMode(mode))
			}
		case "mv":
			if len(args) != 8 || strings.Join(args[3:6], " ") != "-f -T --" || args[7] != target {
				t.Fatalf("unsafe mv: %q", args)
			}
			err = os.Rename(args[6], args[7])
		case "rm":
			if len(args) != 6 || args[3] != "-f" || args[4] != "--" || filepath.Dir(args[5]) != parent || !strings.HasPrefix(filepath.Base(args[5]), ".codex-bg-marketplace-") {
				t.Fatalf("unsafe rm: %q", args)
			}
			err = os.Remove(args[5])
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		default:
			t.Fatalf("unexpected sudo subcommand: %q", args)
		}
		if err == nil && visit != nil {
			err = visit(ctx, args)
		}
		return nil, err
	}
}

func marketplaceAssertNoResidue(t *testing.T, a *App, target string) {
	t.Helper()
	for _, dir := range []string{filepath.Dir(target), filepath.Join(a.P.Data, "tmp"), filepath.Join(a.P.Data, "transactions", "marketplace-replacements")} {
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Name() != "product.json" {
				t.Fatalf("unexpected replacement residue: %s", filepath.Join(dir, entry.Name()))
			}
		}
	}
}

func TestMarketplaceWriteRequiresSelectedVerifiedOriginal(t *testing.T) {
	for _, kind := range []string{"component", "target", "backup", "hash", "symlink", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			a, c, target := marketplaceWriteFixture(t)
			before := engineRead(t, target)
			hash := HashBytes([]byte(before))
			mode := os.FileMode(0644)
			switch kind {
			case "component":
				c.Name = "branding"
			case "target":
				c.Options["product"] = target + ".other"
			case "backup":
				b, _ := a.ReadBackup(c.Backups[0])
				engineFile(t, b.DataPath, "corrupted")
			case "hash":
				hash = HashBytes([]byte("different original"))
			case "symlink":
				real := filepath.Join(a.P.Home, "real.json")
				if err := os.Rename(target, real); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, target); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				mode = 0666
			}
			if err := a.writeMarketplaceProduct(context.Background(), c, target, []byte("new"), mode, hash); err == nil {
				t.Fatal("unsafe write accepted")
			}
			if engineRead(t, target) != before {
				t.Fatal("original changed despite rejected write")
			}
		})
	}
}

func TestMarketplaceWriteLocalPreservesPermissionsAndOwnership(t *testing.T) {
	a, c, target := marketplaceWriteFixture(t)
	oldHash, _ := Hash(target)
	info, _ := os.Stat(target)
	st := info.Sys().(*syscall.Stat_t)
	if err := a.writeMarketplaceProduct(context.Background(), c, target, []byte("new"), 0644, oldHash); err != nil {
		t.Fatal(err)
	}
	if err := verifyMarketplaceReplacement(target, marketplaceReplacement{NewHash: HashBytes([]byte("new")), UID: st.Uid, GID: st.Gid, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	marketplaceAssertNoResidue(t, a, target)
	other := *st
	other.Uid++
	if marketplaceLocalOwnership(target, &other) {
		t.Fatal("would replace file belonging to another user without retaining ownership")
	}
}

func TestMarketplaceWritePrivilegedAndInterruptedCleanup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission failures need an unprivileged test process")
	}
	for _, scenario := range []string{"success", "install-failure", "cancel", "external-update"} {
		t.Run(scenario, func(t *testing.T) {
			a, c, target := marketplaceWriteFixture(t)
			before := engineRead(t, target)
			// Some bind mounts grant ACL write access even after chmod(0500).
			// A different supplementary group deterministically exercises the
			// ownership-preserving privileged branch on those filesystems too.
			groups, _ := os.Getgroups()
			for _, gid := range groups {
				if gid != os.Getegid() {
					if err := os.Chown(target, -1, gid); err != nil {
						t.Fatal(err)
					}
					break
				}
			}
			if err := os.Chmod(target, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Dir(target), 0500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(filepath.Dir(target), 0700) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := []string{}
			a.RunCommand = marketplaceFakeSudo(t, target, func(_ context.Context, args []string) error {
				calls = append(calls, args[2])
				if args[2] == "install" {
					switch scenario {
					case "install-failure":
						return errors.New("simulated partial install")
					case "cancel":
						cancel()
					case "external-update":
						return os.WriteFile(target, []byte("external"), 0644)
					}
				}
				return nil
			})
			err := a.writeMarketplaceProduct(ctx, c, target, []byte("new"), 0644, HashBytes([]byte(before)))
			if scenario == "success" {
				if err != nil || engineRead(t, target) != "new" || strings.Join(calls, ",") != "install,mv" {
					t.Fatalf("privileged replacement failed: %v, calls %v", err, calls)
				}
			} else {
				if err == nil {
					t.Fatal("failed operation claims success")
				}
				want := before
				if scenario == "external-update" {
					want = "external"
				}
				if engineRead(t, target) != want || len(calls) == 0 || calls[0] != "install" || strings.Contains(strings.Join(calls, ","), "mv") {
					t.Fatalf("failed operation changed original or skipped cleanup: %v", calls)
				}
			}
			marketplaceAssertNoResidue(t, a, target)
		})
	}
}

func TestMarketplaceRecoveryOnlyRemovesRegisteredPaths(t *testing.T) {
	for _, invalid := range []string{"", "temporary", "source", "backup", "symlink"} {
		t.Run(invalid, func(t *testing.T) {
			a, c, target := marketplaceWriteFixture(t)
			record, r := marketplaceTestRecord(t, a, c, target)
			engineFile(t, r.Source, "partial source")
			engineFile(t, r.Temporary, "partial copy")
			victim := filepath.Join(a.P.Home, "keep-me")
			engineFile(t, victim, "unrelated")
			switch invalid {
			case "temporary":
				r.Temporary = victim
			case "source":
				r.Source = victim
			case "backup":
				r.BackupID = ID()
			case "symlink":
				if err := os.Remove(r.Temporary); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(victim, r.Temporary); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(r)
			if err := AtomicWrite(record, raw, 0600); err != nil {
				t.Fatal(err)
			}
			err := a.cleanupMarketplaceReplacements()
			if invalid == "" {
				if err != nil {
					t.Fatal(err)
				}
				marketplaceAssertNoResidue(t, a, target)
			} else if err == nil {
				t.Fatal("invalid recovery record accepted")
			}
			if engineRead(t, victim) != "unrelated" {
				t.Fatal("cleanup modified unrelated file")
			}
		})
	}
}
