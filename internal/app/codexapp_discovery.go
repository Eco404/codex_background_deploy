package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const codexAppPrereleaseID = `(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)`

var codexAppPackageVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-` + codexAppPrereleaseID + `(\.` + codexAppPrereleaseID + `)*)?(\+([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?$`)
var codexAppVersionLabel = regexp.MustCompile(`(?i)\bcodex(?:app|ui)\b`)

type codexAppProbe struct {
	Version string
	Entry   string
	Node    string
}

// probeCodexApp does not depend on --version: the npm CLI does not implement
// that option in 0.1.90. Its package metadata and bounded help command together
// establish the version, entry identity, and options needed by our supervisor.
func (a *App) probeCodexApp(ctx context.Context, path string) (string, error) {
	result, err := a.probeCodexAppDetails(ctx, path)
	return result.Version, err
}

func (a *App) probeCodexAppDetails(ctx context.Context, path string) (codexAppProbe, error) {
	result := codexAppProbe{}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return result, err
	}
	entry, err := filepath.EvalSymlinks(path)
	if err != nil || !executable(entry) {
		return result, errors.New("CodexApp entry must resolve to an executable regular file")
	}
	result.Entry = entry
	version, npm, err := codexAppPackage(entry)
	if err != nil {
		return result, err
	}
	if !npm {
		// An unassociated JS file is not a native CLI. Never execute it with an
		// arbitrary interpreter or assume a nearby unrelated package is its own.
		ext := strings.ToLower(filepath.Ext(entry))
		head := readPrefix(entry, 256)
		if ext == ".js" || ext == ".mjs" || ext == ".cjs" || (strings.HasPrefix(head, "#!") && strings.Contains(strings.SplitN(head, "\n", 2)[0], "node")) {
			return result, errors.New("CodexApp JavaScript entry has no matching codexapp package.json bin entry")
		}
		output, err := a.probeCodexAppCommand(ctx, nil, entry, "--version")
		version = strings.TrimPrefix(discoveryVersion.FindString(string(output)), "v")
		if err != nil || !codexAppVersionLabel.Match(output) || !codexAppPackageVersion.MatchString(version) {
			return result, errors.New("CodexApp version check failed or unsupported output")
		}
		result.Version = version
		return result, nil
	}
	node, err := a.codexAppProbeNode(ctx, entry)
	if err != nil {
		return result, err
	}
	env := map[string]string{"PATH": filepath.Dir(node) + string(os.PathListSeparator) + os.Getenv("PATH")}
	help, err := a.probeCodexAppCommand(ctx, env, node, entry, "--help")
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return result, fmt.Errorf("CodexApp help check interrupted: %w", err)
		}
		return result, errors.New("CodexApp help check failed with the selected Node runtime")
	}
	// These are the exact controls used at managed startup. In particular the
	// no-login/no-tunnel/no-open switches avoid additional launch side effects.
	options := map[string]bool{}
	for _, word := range strings.Fields(string(help)) {
		options[strings.TrimSuffix(word, ",")] = true
	}
	for _, option := range []string{"--port", "--password", "--no-login", "--no-tunnel", "--no-open"} {
		if !options[option] {
			return result, fmt.Errorf("CodexApp lacks required option %s", option)
		}
	}
	result.Version, result.Node = version, node
	return result, nil
}

// A package is associated only when its declared codexapp executable resolves
// to this entry and remains inside the real package directory. An ancestor's
// name/version alone cannot turn an unrelated script into a reusable CodexApp.
func codexAppPackage(entry string) (version string, found bool, err error) {
	for dir := filepath.Dir(entry); ; dir = filepath.Dir(dir) {
		file := filepath.Join(dir, "package.json")
		if info, statErr := os.Stat(file); statErr == nil && info.Mode().IsRegular() {
			if info.Size() > 1<<20 {
				return "", false, errors.New("CodexApp package metadata is too large")
			}
			var pkg struct {
				Name    string          `json:"name"`
				Version string          `json:"version"`
				Bin     json.RawMessage `json:"bin"`
			}
			b, readErr := os.ReadFile(file)
			if readErr != nil || json.Unmarshal(b, &pkg) != nil {
				return "", false, errors.New("CodexApp package metadata cannot be read or parsed")
			}
			if pkg.Name == "codexapp" {
				if !codexAppPackageVersion.MatchString(pkg.Version) {
					return "", false, errors.New("CodexApp package has an invalid version")
				}
				var bin string
				if json.Unmarshal(pkg.Bin, &bin) != nil {
					var bins map[string]string
					if json.Unmarshal(pkg.Bin, &bins) == nil {
						bin = bins["codexapp"]
					}
				}
				if bin == "" || filepath.IsAbs(bin) || !Within(dir, filepath.Join(dir, bin)) {
					return "", false, errors.New("CodexApp package has an invalid bin.codexapp entry")
				}
				declared, resolveErr := filepath.EvalSymlinks(filepath.Join(dir, bin))
				if resolveErr != nil || declared != entry || !Within(dir, declared) {
					return "", false, errors.New("CodexApp package bin.codexapp does not match the selected entry")
				}
				return pkg.Version, true, nil
			}
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return "", false, fmt.Errorf("cannot inspect CodexApp package metadata: %w", statErr)
		}
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	return "", false, nil
}

func (a *App) probeCodexAppCommand(ctx context.Context, env map[string]string, name string, args ...string) ([]byte, error) {
	child, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// Reuse cancellation of whole subprocess groups, environment isolation and
	// test injection, but discovery must not produce installation progress.
	quiet := *a
	quiet.Out, quiet.Err = nil, nil
	output, err := quiet.componentRun(child, env, name, args...)
	if child.Err() != nil {
		return output, child.Err()
	}
	return output, err
}

func (a *App) codexAppProbeNode(ctx context.Context, entry string) (string, error) {
	check := func(path string) (string, error) {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !executable(resolved) {
			return "", errors.New("Node executable unavailable")
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return "", err
		}
		output, err := a.probeCodexAppCommand(ctx, nil, resolved, "--version")
		var major int
		if err != nil {
			return "", err
		}
		if _, err = fmt.Sscanf(strings.TrimSpace(string(output)), "v%d", &major); err != nil || major < 20 {
			return "", errors.New("CodexApp requires Node >=20")
		}
		return resolved, nil
	}
	// An explicitly registered runtime is authoritative; do not silently bypass
	// an incompatible selection with a different detected installation.
	if node := a.componentBinary("node"); node != "" {
		path, err := check(node)
		if err != nil {
			return "", fmt.Errorf("registered CodexApp Node is incompatible: %w", err)
		}
		return path, nil
	}
	candidates := []string{}
	if c := a.S.Components["codexapp"]; c != nil && c.Options["node"] != "" {
		candidates = append(candidates, c.Options["node"])
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir != "" {
			candidates = append(candidates, filepath.Join(dir, "node"))
		}
	}
	for _, base := range []string{filepath.Join(a.P.Home, ".nvm"), os.Getenv("NVM_DIR")} {
		if base == "" {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(base, "versions/node/*/bin/node"))
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		candidates = append(candidates, matches...)
	}
	for _, base := range []string{filepath.Join(a.P.Home, "miniconda3"), filepath.Join(a.P.Home, "anaconda3"), filepath.Join(a.P.Home, "miniforge3"), filepath.Join(a.P.Home, "apps/miniconda3"), "/opt/conda", "/opt/miniconda3", filepath.Join(a.P.Home, ".local/share/codex-background-addons/node/runtime"), filepath.Join(a.P.Home, "apps/node")} {
		candidates = append(candidates, filepath.Join(base, "bin/node"))
	}
	// npm global layouts often keep Node next to the prefix's bin directory;
	// check this without relying on NVM's potentially broken default alias.
	for dir := filepath.Dir(entry); ; dir = filepath.Dir(dir) {
		candidates = append(candidates, filepath.Join(dir, "bin/node"))
		if filepath.Dir(dir) == dir {
			break
		}
	}
	seen := map[string]bool{}
	for _, path := range candidates {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || seen[resolved] {
			continue
		}
		seen[resolved] = true
		if path, err = check(resolved); err == nil {
			return path, nil
		}
	}
	return "", errors.New("CodexApp needs an available Node >=20 runtime; select or install a compatible Node")
}
