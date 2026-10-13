package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NetworkOptions applies only to this operation's downloads and installation
// subprocesses. It is deliberately never persisted in runtime configuration.
type NetworkOptions struct {
	UseMirrors                           bool
	NPMRegistry, NodeMirror, CondaMirror string
	Proxy, ProxyMode                     string
}

func DefaultNetworkOptions() NetworkOptions {
	return NetworkOptions{NPMRegistry: "https://repo.huaweicloud.com/repository/npm", NodeMirror: "https://repo.huaweicloud.com/nodejs", CondaMirror: "https://mirrors.ustc.edu.cn/anaconda/miniconda", ProxyMode: "inherit"}
}
func validNetworkURL(value string, proxy bool) error {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.Opaque != "" || u.Fragment != "" || strings.ContainsAny(value, "\r\n\t") {
		return errors.New("requires an absolute URL without a fragment or control characters")
	}
	if u.Scheme != "https" && u.Scheme != "http" && !(proxy && (u.Scheme == "socks5" || u.Scheme == "socks5h")) {
		return errors.New("unsupported URL scheme")
	}
	if !proxy && (u.User != nil || u.RawQuery != "") {
		return errors.New("mirror URLs must not contain credentials or a query")
	}
	if proxy && (u.Path != "" && u.Path != "/" || u.RawQuery != "") {
		return errors.New("proxy URL must contain only scheme, credentials and host:port")
	}
	return nil
}
func (n NetworkOptions) Validate() error {
	if n.UseMirrors {
		for _, item := range []struct{ name, value string }{{"npm registry", n.NPMRegistry}, {"Node mirror", n.NodeMirror}, {"Conda mirror", n.CondaMirror}} {
			if item.value == "" {
				continue
			}
			if err := validNetworkURL(item.value, false); err != nil {
				return fmt.Errorf("%s: %w", item.name, err)
			}
		}
	}
	switch n.ProxyMode {
	case "", "inherit", "none":
	case "custom":
		if err := validNetworkURL(n.Proxy, true); err != nil {
			return fmt.Errorf("proxy: %w", err)
		}
	default:
		return errors.New("proxy mode must be inherit, none or custom")
	}
	return nil
}
func (n NetworkOptions) normalized() NetworkOptions {
	d := DefaultNetworkOptions()
	if n.NPMRegistry == "" {
		n.NPMRegistry = d.NPMRegistry
	}
	if n.NodeMirror == "" {
		n.NodeMirror = d.NodeMirror
	}
	if n.CondaMirror == "" {
		n.CondaMirror = d.CondaMirror
	}
	if n.ProxyMode == "" {
		n.ProxyMode = "inherit"
	}
	return n
}
func (n NetworkOptions) Summary() string {
	n = n.normalized()
	mirror := "官方源"
	if n.UseMirrors {
		mirror = "镜像源：npm=" + n.NPMRegistry + "；Node=" + n.NodeMirror + "；Conda=" + n.CondaMirror
	}
	proxy := "继承当前代理环境"
	if n.ProxyMode == "none" {
		proxy = "直连（不使用代理）"
	}
	if n.ProxyMode == "custom" {
		proxy = "代理：" + redactNetworkURL(n.Proxy)
	}
	return mirror + "\n" + proxy
}
func redactNetworkURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[invalid URL]"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
func (n NetworkOptions) redact(text string) string {
	proxies := []string{n.Proxy}
	for _, key := range []string{"http_proxy", "https_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		proxies = append(proxies, os.Getenv(key))
	}
	for _, raw := range proxies {
		if raw != "" {
			text = strings.ReplaceAll(text, raw, redactNetworkURL(raw))
			if u, e := url.Parse(raw); e == nil && u.User != nil {
				if p, ok := u.User.Password(); ok && p != "" {
					text = strings.ReplaceAll(text, p, "***")
				}
			}
		}
	}
	return text
}
func (a *App) networkEnv(extra map[string]string) map[string]string {
	env := map[string]string{}
	n := a.Network.normalized()
	registry, node := "https://registry.npmjs.org", "https://nodejs.org/dist"
	if n.UseMirrors {
		registry, node = n.NPMRegistry, n.NodeMirror
	}
	// Both explicit official and mirror selections override inherited npm config
	// for this command without editing the user's files.
	env["npm_config_registry"], env["NPM_CONFIG_REGISTRY"] = registry, registry
	env["npm_config_disturl"], env["NPM_CONFIG_DISTURL"] = node, node
	env["npm_config_color"], env["FORCE_COLOR"] = "false", "0"
	if n.ProxyMode != "inherit" {
		value := ""
		if n.ProxyMode == "custom" {
			value = n.Proxy
		}
		for _, k := range []string{"http_proxy", "https_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
			env[k] = value
		}
		// An explicitly selected proxy must not be silently bypassed by stale NO_PROXY.
		env["no_proxy"] = ""
		env["NO_PROXY"] = ""
		if value == "" {
			env["npm_config_proxy"] = "false"
			env["npm_config_https_proxy"] = "false"
		} else {
			env["npm_config_proxy"] = value
			env["npm_config_https_proxy"] = value
		}
		env["NPM_CONFIG_PROXY"] = env["npm_config_proxy"]
		env["NPM_CONFIG_HTTPS_PROXY"] = env["npm_config_https_proxy"]
		env["npm_config_noproxy"], env["NPM_CONFIG_NOPROXY"] = "", ""
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}
func (a *App) installRun(ctx context.Context, env map[string]string, name string, args ...string) ([]byte, error) {
	if err := a.Network.Validate(); err != nil {
		return nil, err
	}
	return a.componentRunOutput(ctx, a.networkEnv(env), true, name, args...)
}
func (a *App) networkClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	n := a.Network.normalized()
	if n.ProxyMode == "none" {
		t.Proxy = nil
	}
	if n.ProxyMode == "custom" {
		u, _ := url.Parse(n.Proxy)
		t.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: t, Timeout: 10 * time.Minute}
}
func (a *App) progress(format string, args ...any) {
	if a.Out != nil {
		fmt.Fprintln(a.Out, a.Network.redact(fmt.Sprintf(format, args...)))
	}
}

type downloadProgress struct {
	a           *App
	name        string
	size, total int64
	last        time.Time
}

func (p *downloadProgress) Write(b []byte) (int, error) {
	p.size += int64(len(b))
	if time.Since(p.last) >= time.Second {
		p.last = time.Now()
		if p.total > 0 {
			p.a.progress("下载 %s：%.1f / %.1f MiB（%d%%）", p.name, float64(p.size)/(1<<20), float64(p.total)/(1<<20), p.size*100/p.total)
		} else {
			p.a.progress("下载 %s：%.1f MiB", p.name, float64(p.size)/(1<<20))
		}
	}
	return len(b), nil
}
func (a *App) download(ctx context.Context, raw, destination string) error {
	if err := a.Network.Validate(); err != nil {
		return err
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return errors.New("download requires an absolute HTTP(S) URL without credentials")
	}
	a.progress("下载 %s（%s）", filepath.Base(destination), redactNetworkURL(raw))
	// Keep command injection available for existing isolated installer tests.
	if a.RunCommand != nil {
		_, err := a.installRun(ctx, nil, "curl", "--fail", "--location", "--retry", "2", "--connect-timeout", "15", "--max-time", "600", raw, "--output", destination)
		return err
	}
	if err := a.EnsureData(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(a.P.Data, "tmp"), 0700); err != nil {
		return err
	}
	client := a.networkClient()
	defer client.CloseIdleConnections()
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		last = a.downloadOnce(ctx, client, raw, destination)
		if last == nil {
			return nil
		}
		if attempt < 2 {
			a.progress("下载未完成，重试 %d/2：%s", attempt+1, a.Network.redact(last.Error()))
		}
	}
	return errors.New(a.Network.redact(last.Error()))
}
func (a *App) downloadOnce(ctx context.Context, client *http.Client, raw, destination string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "codex-bg/"+Version)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download HTTP %d from %s", resp.StatusCode, redactNetworkURL(raw))
	}
	tmp, err := os.CreateTemp(filepath.Join(a.P.Data, "tmp"), "download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	p := &downloadProgress{a: a, name: filepath.Base(destination), total: resp.ContentLength, last: time.Now()}
	size, err := io.Copy(io.MultiWriter(tmp, p), resp.Body)
	if err != nil {
		return err
	}
	if resp.ContentLength >= 0 && size != resp.ContentLength {
		return io.ErrUnexpectedEOF
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	// All callers use installer-data cache/tmp paths, which share this filesystem.
	if err = os.Rename(tmp.Name(), destination); err != nil {
		return err
	}
	a.progress("下载完成 %s（%.1f MiB）", filepath.Base(destination), float64(size)/(1<<20))
	return nil
}

// Installation subprocess output is forwarded one complete line at a time so
// credentials split between pipe writes are still redacted before UI/log use.
type installOutput struct {
	a         *App
	line      []byte
	oversized bool
}

func (w *installOutput) Write(b []byte) (int, error) {
	for _, ch := range b {
		if ch == '\n' || ch == '\r' {
			w.flush()
			continue
		}
		if w.oversized {
			continue
		}
		if len(w.line) >= 32<<10 {
			w.line = nil
			w.oversized = true
			continue
		}
		w.line = append(w.line, ch)
	}
	return len(b), nil
}
func (w *installOutput) flush() {
	if w.oversized {
		w.a.progress("[过长的子进程输出已省略]")
	} else if len(w.line) > 0 {
		w.a.progress("%s", string(w.line))
	}
	w.line = nil
	w.oversized = false
}
