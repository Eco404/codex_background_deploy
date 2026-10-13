package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var discoveryVersion = regexp.MustCompile(`\bv?(\d+)\.\d+(?:\.\d+)?(?:[-+][A-Za-z0-9.-]+)?`)

func readPrefix(file string, limit int64) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, limit))
	return string(b)
}
func executable(file string) bool {
	s, e := os.Stat(file)
	return e == nil && s.Mode().IsRegular() && s.Mode()&0111 != 0
}
func (a *App) probe(ctx context.Context, command string, args ...string) ([]byte, error) {
	child, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return a.Run(child, command, args...)
}
func discoveryRoots(home string, selected []string) []string {
	roots := []string{
		filepath.Join(home, ".vscode/extensions"), filepath.Join(home, ".vscode-insiders/extensions"),
		filepath.Join(home, ".vscode-server/extensions"), filepath.Join(home, ".vscode-server-insiders/extensions"),
		filepath.Join(home, ".local/share/code-server/extensions"), filepath.Join(home, ".vscode-oss/extensions"),
		filepath.Join(home, ".vscode-oss-dev/extensions"), filepath.Join(home, ".vscode-codium/extensions"),
		filepath.Join(home, ".var/app/com.visualstudio.code/data/vscode/extensions"),
		filepath.Join(home, ".var/app/com.vscodium.codium/data/codium/extensions"),
	}
	roots = append(roots, selected...)
	seen := map[string]bool{}
	out := []string{}
	for _, root := range roots {
		root = filepath.Clean(root)
		if !seen[root] {
			seen[root] = true
			out = append(out, root)
		}
	}
	return out
}
func legacyEntry(text string) bool {
	return strings.Contains(text, "codex-daemon-bridge") || strings.Contains(text, "vscode-codex-background-deploy extension hook") || strings.Contains(text, "codex-background/payload/bridge")
}

// Discover only inspects files and invokes bounded version/capability commands.
// It never sources a shell, starts a service, downloads, or creates install state.
func (a *App) Discover(ctx context.Context) (Inventory, error) {
	inv := Inventory{Candidates: map[string][]Candidate{}, Extensions: []Extension{}, Legacy: []string{}, Warnings: []string{}}
	if err := ctx.Err(); err != nil {
		return inv, err
	}
	inv.Host, _ = os.Hostname()
	if u, e := user.Current(); e == nil {
		inv.User = u.Username
	}
	home := a.P.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	_, dockerErr := os.Stat("/.dockerenv")
	inv.Container = dockerErr == nil || strings.Contains(readPrefix("/proc/1/cgroup", 16384), "docker") || strings.Contains(readPrefix("/proc/1/cgroup", 16384), "containerd")
	// These capabilities are independent: a container can also host systemd.
	if _, err := a.probe(ctx, "systemctl", "--user", "show-environment"); err == nil {
		inv.Systemd = true
	}
	if a.S.Installed {
		inv.ServiceState = "unknown"
		switch a.S.Backend {
		case "systemd":
			if output, err := a.probe(ctx, "systemctl", "--user", "show", "codex-bg.service", "--property=ActiveState", "--value"); err == nil {
				switch strings.TrimSpace(string(output)) {
				case "active":
					inv.ServiceState = "running"
				case "inactive", "failed":
					inv.ServiceState = "stopped"
				}
			}
		case "container":
			record, err := a.runtimeReadPID()
			if errors.Is(err, os.ErrNotExist) {
				inv.ServiceState = "stopped"
			} else if err == nil && runtimeValidPID(record) {
				inv.ServiceState = "running"
			}
		}
	}
	aptFound, dpkgFound := false, false
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if executable(filepath.Join(dir, "apt-get")) {
			aptFound = true
		}
		if executable(filepath.Join(dir, "dpkg-query")) {
			dpkgFound = true
		}
	}
	inv.Apt = aptFound && dpkgFound
	inv.Privileged = os.Geteuid() == 0
	if !inv.Privileged && inv.Container && inv.Apt {
		_, err := a.probe(ctx, "sudo", "-n", "true")
		inv.Privileged = err == nil
	}
	if inv.Container {
		for _, file := range []string{"/usr/bin/entrypoint.sh", "/usr/local/bin/entrypoint.sh"} {
			if strings.Contains(readPrefix(file, 65536), "ENTRYPOINTD") {
				inv.EntrypointDir = os.Getenv("ENTRYPOINTD")
				if inv.EntrypointDir == "" {
					inv.EntrypointDir = filepath.Join(home, "entrypoint.d")
				}
				break
			}
		}
	}
	for _, p := range []string{os.Getenv("CODE_SERVER_MEDIA_DIR"), "/usr/lib/code-server/src/browser/media", "/usr/local/lib/code-server/src/browser/media"} {
		if s, e := os.Stat(p); e == nil && s.IsDir() {
			inv.MediaDir = p
			break
		}
	}
	type located struct{ kind, path, source string }
	locations := []located{}
	add := func(kind, p, source string) {
		if p != "" {
			locations = append(locations, located{kind, p, source})
		}
	}
	for _, kind := range []string{"codex", "node", "conda", "codexapp", "code-server"} {
		names := []string{kind}
		if kind == "codexapp" {
			names = []string{"codexapp", "CodexApp"}
		}
		for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
			if dir == "" {
				continue
			}
			for _, name := range names {
				add(kind, filepath.Join(dir, name), "PATH")
			}
		}
	}
	for _, p := range []string{"/usr/bin/code-server", "/usr/local/bin/code-server", filepath.Join(home, ".local/bin/code-server")} {
		add("code-server", p, "existing code-server")
	}
	for _, p := range []string{a.S.Codex.Path, filepath.Join(home, ".codex/packages/standalone/current/codex"), filepath.Join(home, ".local/bin/codex"), filepath.Join(home, "apps/codex/bin/codex"), filepath.Join(a.P.Runtime, "codex/current/codex")} {
		add("codex", p, "existing installation")
	}
	for _, base := range []string{filepath.Join(home, ".nvm"), os.Getenv("NVM_DIR")} {
		if base == "" {
			continue
		}
		for _, name := range []string{"node", "codex"} {
			matches, _ := filepath.Glob(filepath.Join(base, "versions/node/*/bin", name))
			sort.Sort(sort.Reverse(sort.StringSlice(matches)))
			for _, p := range matches {
				add(name, p, "installed NVM version")
			}
		}
	}
	for _, base := range []string{filepath.Join(home, "miniconda3"), filepath.Join(home, "anaconda3"), filepath.Join(home, "miniforge3"), filepath.Join(home, "apps/miniconda3"), "/opt/conda", "/opt/miniconda3"} {
		add("conda", filepath.Join(base, "bin/conda"), "existing Conda")
		add("node", filepath.Join(base, "bin/node"), "Conda environment")
	}
	for _, p := range []string{filepath.Join(home, ".local/share/codex-background-addons/node/runtime/bin/node"), filepath.Join(home, "apps/node/bin/node")} {
		add("node", p, "legacy/private Node")
	}
	for _, base := range []string{filepath.Join(home, "apps/CodexApp"), filepath.Join(home, "apps/codexapp"), filepath.Join(home, ".local/share/codex-background-addons/codexapp"), filepath.Join(home, ".local/share/CodexApp")} {
		for _, name := range []string{"codexapp", "CodexApp", "bin/codexapp", "node_modules/.bin/codexapp"} {
			add("codexapp", filepath.Join(base, name), "existing CodexApp")
		}
	}
	for _, root := range discoveryRoots(home, a.S.SelectedRoots) {
		dirs, _ := filepath.Glob(filepath.Join(root, "openai.chatgpt-*"))
		sort.Strings(dirs)
		for _, dir := range dirs {
			entries, _ := filepath.Glob(filepath.Join(dir, "bin/*/codex"))
			for _, entry := range entries {
				if !executable(entry) {
					continue
				}
				head := readPrefix(entry, 8192)
				ext := Extension{Root: root, Directory: dir, Entry: entry, Version: strings.TrimPrefix(filepath.Base(dir), "openai.chatgpt-"), Product: filepath.Base(filepath.Dir(root)), Hooked: strings.Contains(head, Marker), Legacy: legacyEntry(head)}
				inv.Extensions = append(inv.Extensions, ext)
				if ext.Legacy {
					inv.Legacy = append(inv.Legacy, "extension hook: "+entry)
				}
				if !ext.Hooked && !ext.Legacy {
					add("codex", entry, "extension bundled CLI")
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, loc := range locations {
		if err := ctx.Err(); err != nil {
			return inv, err
		}
		if !executable(loc.path) {
			continue
		}
		resolved, e := filepath.EvalSymlinks(loc.path)
		if e != nil {
			continue
		}
		if !filepath.IsAbs(resolved) {
			resolved, _ = filepath.Abs(resolved)
		}
		key := loc.kind + "\x00" + resolved
		if seen[key] {
			continue
		}
		seen[key] = true
		candidate := Candidate{Kind: loc.kind, Path: resolved, Source: loc.source}
		head := readPrefix(resolved, 8192)
		if strings.Contains(head, Marker) || legacyEntry(head) || strings.Contains(head, "payload/bridge/wrapper.sh") {
			candidate.Reason = "managed wrapper excluded to prevent recursion"
			inv.Candidates[loc.kind] = append(inv.Candidates[loc.kind], candidate)
			continue
		}
		output, err := a.probe(ctx, resolved, "--version")
		candidate.Version = discoveryVersion.FindString(string(output))
		candidate.Compatible = err == nil && candidate.Version != ""
		if !candidate.Compatible {
			candidate.Reason = "version check failed or unsupported output"
		}
		if candidate.Compatible && loc.kind == "node" {
			var major int
			fmt.Sscanf(strings.TrimPrefix(candidate.Version, "v"), "%d", &major)
			if major < 18 {
				candidate.Compatible = false
				candidate.Reason = "Node >=18 required for optional components"
			}
		}
		if candidate.Compatible && loc.kind == "codex" {
			help, e := a.probe(ctx, resolved, "app-server", "--help")
			if e != nil || !strings.Contains(string(help), "unix://") {
				candidate.Compatible = false
				candidate.Reason = "app-server Unix socket capability unavailable"
			}
		}
		if loc.kind == "codex" && loc.source == "extension bundled CLI" {
			candidate.Compatible = false
			candidate.Reason = "此入口将被接管且可能随扩展升级删除；请选择独立CLI或安装受管版本"
		}
		inv.Candidates[loc.kind] = append(inv.Candidates[loc.kind], candidate)
	}
	for kind, candidates := range inv.Candidates {
		sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Compatible && !candidates[j].Compatible })
		inv.Candidates[kind] = candidates
	}
	for _, p := range []string{filepath.Join(home, ".config/systemd/user/codex-app-server.service"), filepath.Join(home, ".config/systemd/user/codex-extension-hook.timer"), filepath.Join(home, "entrypoint.d/05-start-codex-daemon.sh"), filepath.Join(home, "apps/codex-daemon-bridge"), filepath.Join(home, ".local/share/codex-daemon-bridge"), filepath.Join(home, ".local/share/codex-background"), filepath.Join(home, ".codex/app-server-control/app-server-control.sock")} {
		if _, e := os.Lstat(p); e == nil {
			inv.Legacy = append(inv.Legacy, p)
		}
	}
	for _, p := range []string{filepath.Join(home, ".vscode-server/data/Machine/settings.json"), filepath.Join(home, ".vscode-server-insiders/data/Machine/settings.json"), filepath.Join(home, ".local/share/code-server/User/settings.json"), filepath.Join(home, ".config/Code/User/settings.json"), filepath.Join(home, ".config/Code - OSS/User/settings.json"), filepath.Join(home, ".config/Code - Insiders/User/settings.json"), filepath.Join(home, ".config/VSCodium/User/settings.json")} {
		raw := readPrefix(p, 4<<20)
		if raw == "" {
			continue
		}
		if value, ok := discoverySetting(raw); ok {
			inv.Legacy = append(inv.Legacy, fmt.Sprintf("CLI setting: %s → %s", p, value))
		}
	}
	return inv, nil
}

// Decode just the top-level target; never include other settings or parse errors
// containing source text in the inventory. Comments and quoted URLs are safe.
func discoverySetting(text string) (string, bool) {
	var clean strings.Builder
	quoted, escaped := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if quoted {
			clean.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		if c == '"' {
			quoted = true
			clean.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(text) && text[i+1] == '/' {
			for i < len(text) && text[i] != '\n' {
				i++
			}
			clean.WriteByte('\n')
			continue
		}
		if c == '/' && i+1 < len(text) && text[i+1] == '*' {
			i += 2
			for i+1 < len(text) && !(text[i] == '*' && text[i+1] == '/') {
				i++
			}
			i++
			continue
		}
		clean.WriteByte(c)
	}
	raw := strings.TrimPrefix(clean.String(), "\ufeff")
	clean.Reset()
	quoted = false
	escaped = false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if quoted {
			clean.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		if c == '"' {
			quoted = true
		}
		if c == ',' {
			j := i + 1
			for j < len(raw) && strings.ContainsRune(" \r\n\t", rune(raw[j])) {
				j++
			}
			if j < len(raw) && (raw[j] == '}' || raw[j] == ']') {
				continue
			}
		}
		clean.WriteByte(c)
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(clean.String()), &obj) != nil {
		return "", false
	}
	var value string
	if json.Unmarshal(obj["chatgpt.cliExecutable"], &value) != nil || value == "" {
		return "", false
	}
	return value, true
}
