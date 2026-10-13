package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const runtimeTimeout = 5 * time.Second

func runtimeConnect(ctx context.Context, socket string) (*websocket.Conn, error) {
	dialer := &net.Dialer{}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", socket)
	}}
	client := &http.Client{Transport: transport}
	dialCtx, cancel := context.WithTimeout(ctx, runtimeTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, "ws://localhost/rpc", &websocket.DialOptions{HTTPClient: client, CompressionMode: websocket.CompressionDisabled})
	transport.CloseIdleConnections()
	if err != nil {
		return nil, fmt.Errorf("connect app-server: %w", err)
	}
	conn.SetReadLimit(64 << 20)
	return conn, nil
}

type runtimeRPC struct {
	c  *websocket.Conn
	id int
}

func (r *runtimeRPC) call(ctx context.Context, method string, params any, target any) error {
	r.id++
	id := r.id
	data, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	if err = r.c.Write(ctx, websocket.MessageText, data); err != nil {
		return err
	}
	for {
		kind, data, err := r.c.Read(ctx)
		if err != nil {
			return err
		}
		if kind != websocket.MessageText {
			return errors.New("unexpected binary app-server frame")
		}
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err = json.Unmarshal(data, &reply); err != nil {
			return errors.New("invalid app-server JSON")
		}
		if reply.ID != id {
			continue
		}
		if len(reply.Error) > 0 && string(reply.Error) != "null" {
			return fmt.Errorf("app-server %s failed", method)
		}
		if len(reply.Result) == 0 {
			return errors.New("missing app-server result")
		}
		if target != nil {
			return json.Unmarshal(reply.Result, target)
		}
		return nil
	}
}
func runtimeInitialize(ctx context.Context, c *websocket.Conn) (*runtimeRPC, error) {
	rpc := &runtimeRPC{c: c}
	if err := rpc.call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "codex-bg", "version": Version}, "capabilities": map[string]bool{"experimentalApi": true}}, nil); err != nil {
		return nil, err
	}
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"method":"initialized"}`)); err != nil {
		return nil, err
	}
	return rpc, nil
}
func (a *App) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, runtimeTimeout)
	defer cancel()
	c, err := runtimeConnect(ctx, a.P.Socket)
	if err != nil {
		return err
	}
	defer c.CloseNow()
	_, err = runtimeInitialize(ctx, c)
	return err
}

// Busy fails closed when a connected server cannot establish its active state.
// No model request is made: only initialize, loaded/list and thread/read.
func (a *App) Busy(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, runtimeTimeout)
	defer cancel()
	c, err := runtimeConnect(ctx, a.P.Socket)
	if err != nil {
		return false, err
	}
	defer c.CloseNow()
	rpc, err := runtimeInitialize(ctx, c)
	if err != nil {
		return false, err
	}
	cursor := ""
	seen := map[string]bool{}
	for pages := 0; pages < 100; pages++ {
		params := map[string]any{"limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var loaded struct {
			Data       *[]string `json:"data"`
			NextCursor *string   `json:"nextCursor"`
		}
		if err = rpc.call(ctx, "thread/loaded/list", params, &loaded); err != nil {
			return false, err
		}
		if loaded.Data == nil {
			return false, errors.New("loaded thread response is missing data")
		}
		for _, id := range *loaded.Data {
			var read struct {
				Thread struct {
					Status struct {
						Type string `json:"type"`
					} `json:"status"`
					Turns []struct {
						Status string `json:"status"`
					} `json:"turns"`
				} `json:"thread"`
			}
			if err = rpc.call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false}, &read); err != nil {
				return false, err
			}
			switch read.Thread.Status.Type {
			case "active", "inProgress":
				return true, nil
			case "idle", "notLoaded":
			default:
				return false, fmt.Errorf("thread %s has unknown status %q", id, read.Thread.Status.Type)
			}
		}
		if loaded.NextCursor == nil || *loaded.NextCursor == "" {
			return false, nil
		}
		cursor = *loaded.NextCursor
		if seen[cursor] {
			return false, errors.New("repeated thread cursor")
		}
		seen[cursor] = true
	}
	return false, errors.New("too many loaded thread pages")
}

func runtimeShouldProxy(args []string) bool {
	app := false
	skip := false
	for _, arg := range args {
		if skip {
			skip = false
			continue
		}
		switch arg {
		case "--help", "-h", "--version", "-V":
			return false
		case "-c", "--config", "--enable", "--disable", "--code-mode-host":
			skip = true
			continue
		}
		if !app {
			if strings.HasPrefix(arg, "-") {
				continue
			}
			if arg != "app-server" {
				return false
			}
			app = true
			continue
		}
		switch arg {
		case "daemon", "proxy", "generate-ts", "generate-json-schema", "generate-internal-json-schema", "help", "version":
			return false
		}
	}
	return app
}
func runtimeEnv(home, codexHome string) []string {
	env := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "CODEX_HOME=") && !strings.HasPrefix(v, "HOME=") {
			env = append(env, v)
		}
	}
	return append(env, "CODEX_HOME="+codexHome, "HOME="+home)
}
func (a *App) Proxy(ctx context.Context, bindingID string, args []string) error {
	output := a.Out
	if output == nil {
		output = os.Stdout
	}
	errout := a.Err
	if errout == nil {
		errout = os.Stderr
	}
	if !runtimeShouldProxy(args) {
		cmd := exec.CommandContext(ctx, a.S.Codex.Path, args...)
		cmd.Env = runtimeEnv(a.P.Home, a.S.CodexHome)
		cmd.Stdin = os.Stdin
		cmd.Stdout = output
		cmd.Stderr = errout
		return cmd.Run()
	}
	c, err := runtimeConnect(ctx, a.P.Socket)
	if err != nil {
		return err
	}
	defer c.CloseNow()
	if err = a.runtimeRecordConnection(bindingID); err != nil {
		return err
	}
	return runtimeProxyStreams(ctx, c, os.Stdin, output)
}
func runtimeProxyStreams(ctx context.Context, c *websocket.Conn, input io.ReadCloser, output io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.CloseNow()
	results := make(chan error, 2)
	go func() {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 64<<10), 64<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(strings.TrimSpace(string(line))) == 0 {
				continue
			}
			if err := c.Write(ctx, websocket.MessageText, line); err != nil {
				results <- err
				return
			}
		}
		results <- scanner.Err()
	}()
	go func() {
		for {
			kind, data, err := c.Read(ctx)
			if err != nil {
				results <- err
				return
			}
			if kind != websocket.MessageText {
				results <- errors.New("unexpected binary app-server frame")
				return
			}
			data = append(data, '\n')
			if _, err = output.Write(data); err != nil {
				results <- err
				return
			}
		}
	}()
	var err error
	select {
	case err = <-results:
	case <-ctx.Done():
		err = ctx.Err()
	}
	cancel()
	_ = input.Close()
	_ = c.CloseNow()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
func (a *App) runtimeRecordConnection(id string) error {
	if id == "" {
		return nil
	}
	valid := false
	if id == "codexapp" {
		c := a.S.Components["codexapp"]
		valid = c != nil && c.Status == "installed"
	}
	for _, binding := range a.S.Bindings {
		if binding.ID == id {
			valid = true
			break
		}
	}
	if !valid {
		return errors.New("unknown extension binding")
	}
	if strings.ContainsAny(id, "/\\") || id == "." || id == ".." {
		return errors.New("invalid extension binding ID")
	}
	dir := filepath.Join(a.P.Runtime, "connections")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]any{"binding_id": id, "last_connection": time.Now().UTC()})
	file, err := os.CreateTemp(dir, ".connection-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, id+".json"))
}
