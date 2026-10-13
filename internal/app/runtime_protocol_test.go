package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func runtimeTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "rt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func runtimeFixture(t *testing.T, handler func(context.Context, *websocket.Conn)) string {
	t.Helper()
	socket := filepath.Join(runtimeTestDir(t), "test.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		handler(r.Context(), c)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close(); listener.Close() })
	return socket
}
func runtimeResponder(status string) func(context.Context, *websocket.Conn) {
	return func(ctx context.Context, c *websocket.Conn) {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var req struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(data, &req) != nil {
				return
			}
			if req.ID == 0 {
				continue
			}
			var result any = map[string]any{"userAgent": "fixture"}
			switch req.Method {
			case "thread/loaded/list":
				result = map[string]any{"data": []string{"t1"}, "nextCursor": nil}
			case "thread/read":
				result = map[string]any{"thread": map[string]any{"status": map[string]string{"type": status}}}
			}
			reply, _ := json.Marshal(map[string]any{"id": req.ID, "result": result})
			if c.Write(ctx, websocket.MessageText, reply) != nil {
				return
			}
		}
	}
}
func TestRuntimeHealthAndBusy(t *testing.T) {
	for _, test := range []struct {
		status     string
		busy, fail bool
	}{{"idle", false, false}, {"active", true, false}, {"inProgress", true, false}, {"unknown", false, true}} {
		t.Run(test.status, func(t *testing.T) {
			a := &App{P: Paths{Socket: runtimeFixture(t, runtimeResponder(test.status))}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := a.Health(ctx); err != nil {
				t.Fatal(err)
			}
			busy, err := a.Busy(ctx)
			if (err != nil) != test.fail || busy != test.busy {
				t.Fatalf("got busy=%v err=%v", busy, err)
			}
		})
	}
}
func TestRuntimeHandshakeDeadline(t *testing.T) {
	path := filepath.Join(runtimeTestDir(t), "stall.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, e := listener.Accept()
		if e == nil {
			accepted <- c
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = runtimeConnect(ctx, path)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("deadline was not enforced: %v", err)
	}
	select {
	case c := <-accepted:
		c.Close()
	default:
	}
}
func TestRuntimeInitializeDeadline(t *testing.T) {
	socket := runtimeFixture(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	a := &App{P: Paths{Socket: socket}}
	if a.Health(ctx) == nil {
		t.Fatal("missing initialize response accepted")
	}
}
func TestRuntimeProxyEOFAndProtocol(t *testing.T) {
	socket := runtimeFixture(t, runtimeResponder("idle"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := runtimeConnect(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	output := &runtimeTestWriter{notify: make(chan struct{}, 1)}
	result := make(chan error, 1)
	go func() { result <- runtimeProxyStreams(ctx, c, reader, output) }()
	if _, err = io.WriteString(writer, "{\"id\":1,\"method\":\"initialize\",\"params\":{}}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-output.notify:
	case <-ctx.Done():
		t.Fatal("reply missing")
	}
	writer.Close()
	select {
	case err = <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("EOF did not exit")
	}
	output.mu.Lock()
	defer output.mu.Unlock()
	var message map[string]any
	if json.Unmarshal(output.bytes, &message) != nil || message["id"] != float64(1) {
		t.Fatal(string(output.bytes))
	}
}

type runtimeTestWriter struct {
	mu     sync.Mutex
	bytes  []byte
	notify chan struct{}
	err    error
}

func (w *runtimeTestWriter) Write(b []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	w.mu.Lock()
	w.bytes = append(w.bytes, b...)
	w.mu.Unlock()
	select {
	case w.notify <- struct{}{}:
	default:
	}
	return len(b), nil
}
func TestRuntimeProxyOutputError(t *testing.T) {
	socket := runtimeFixture(t, func(ctx context.Context, c *websocket.Conn) {
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"event":"fixture"}`))
		_, _, _ = c.Read(ctx)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := runtimeConnect(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	r, w := io.Pipe()
	defer w.Close()
	broken := errors.New("broken pipe")
	if err = runtimeProxyStreams(ctx, c, r, &runtimeTestWriter{err: broken}); !errors.Is(err, broken) {
		t.Fatalf("want output error, got %v", err)
	}
}
func TestRuntimeProxyRoutes(t *testing.T) {
	for _, test := range []struct {
		args  []string
		proxy bool
	}{{[]string{"app-server"}, true}, {[]string{"app-server", "--stdio"}, true}, {[]string{"app-server", "daemon", "start"}, false}, {[]string{"--help", "app-server"}, false}, {[]string{"-c", "app-server", "exec"}, false}, {[]string{"exec", "app-server"}, false}, {[]string{"-c", "x=1", "app-server"}, true}} {
		if got := runtimeShouldProxy(test.args); got != test.proxy {
			t.Fatalf("%v: got %v", test.args, got)
		}
	}
}
func TestRuntimeBindingRecord(t *testing.T) {
	root := runtimeTestDir(t)
	a := &App{P: Paths{Runtime: root}, S: State{Bindings: []Binding{{ID: "a1"}}}}
	if a.runtimeRecordConnection("bad") == nil {
		t.Fatal("unknown binding accepted")
	}
	if err := a.runtimeRecordConnection("a1"); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	b, err := os.ReadFile(filepath.Join(root, "connections", "a1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(b, &v) != nil || v["binding_id"] != "a1" {
		t.Fatal(string(b))
	}
}
