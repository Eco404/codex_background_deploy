#!/usr/bin/env python3
"""Offline real-Codex turn survival fixture, with localhost-only fake Responses SSE.

Uses CB_TEST_CODEX, CB_TEST_NODE and CB_TEST_WS overrides if supplied. No API key
is copied. Fake git disables automatic external plugin registry access. Every
server, thread and filesystem artifact is confined to a fresh /tmp directory.
"""
import http.server, json, os, pathlib, select, signal, subprocess, threading, time, traceback, tempfile, shutil

SOURCE = pathlib.Path(__file__).resolve().parents[1]
R = pathlib.Path(tempfile.mkdtemp(prefix="codex-background-turn-"))
H = R / "home"
C = H / ".codex"
C.mkdir(parents=True)
B = R / "bin"
B.mkdir()
# Prevent the CLI's background plugin Git updater from making outbound requests.
(B / "git").write_text("#!/bin/bash\nexit 1\n")
(B / "git").chmod(0o700)
CLI = os.environ.get(
    "CB_TEST_CODEX", "/home/coder/.codex/packages/standalone/current/codex"
)
NODE = os.environ.get(
    "CB_TEST_NODE",
    shutil.which("node") or "/home/coder/.nvm/versions/node/v22.16.0/bin/node",
)
BRIDGE = str(SOURCE / "payload/bridge/bridge.mjs")
SOCK = R / "server.sock"
WS = pathlib.Path(
    os.environ.get("CB_TEST_WS", "/home/coder/apps/codexapp/node_modules/ws")
)
E = {
    "HOME": str(H),
    "CODEX_HOME": str(C),
    "PATH": f"{B}:/usr/bin:/bin",
    "NODE_PATH": str(WS.parent),
    "CB_SOCKET": str(SOCK),
    "LANG": "C.UTF-8",
}
result = {"root": str(R), "requests": [], "assertions": []}
processes = []
files = []
first_request = threading.Event()
allow_response = threading.Event()
response_sent = threading.Event()
buffers = {}
messages = {}


class Provider(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        result["requests"].append(
            {
                "path": self.path,
                "body": json.loads(body),
                "auth_present": bool(self.headers.get("Authorization")),
            }
        )
        first_request.set()
        allow_response.wait(15)
        message = {
            "id": "msg_local_1",
            "type": "message",
            "status": "completed",
            "role": "assistant",
            "content": [
                {
                    "type": "output_text",
                    "text": "LOCAL_BACKGROUND_COMPLETED",
                    "annotations": [],
                }
            ],
        }
        response = {
            "id": "resp_local_1",
            "object": "response",
            "created_at": int(time.time()),
            "status": "completed",
            "model": "local-test",
            "output": [message],
            "usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
        }
        events = [
            {
                "type": "response.created",
                "response": {**response, "status": "in_progress", "output": []},
            },
            {
                "type": "response.output_item.added",
                "output_index": 0,
                "item": {**message, "status": "in_progress", "content": []},
            },
            {
                "type": "response.content_part.added",
                "item_id": message["id"],
                "output_index": 0,
                "content_index": 0,
                "part": {"type": "output_text", "text": "", "annotations": []},
            },
            {
                "type": "response.output_text.delta",
                "item_id": message["id"],
                "output_index": 0,
                "content_index": 0,
                "delta": "LOCAL_BACKGROUND_COMPLETED",
            },
            {
                "type": "response.output_text.done",
                "item_id": message["id"],
                "output_index": 0,
                "content_index": 0,
                "text": "LOCAL_BACKGROUND_COMPLETED",
            },
            {
                "type": "response.content_part.done",
                "item_id": message["id"],
                "output_index": 0,
                "content_index": 0,
                "part": message["content"][0],
            },
            {"type": "response.output_item.done", "output_index": 0, "item": message},
            {"type": "response.completed", "response": response},
        ]
        data = "".join(
            f'event: {e["type"]}\ndata: {json.dumps({**e,"sequence_number":i})}\n\n'
            for i, e in enumerate(events)
        ).encode()
        try:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            self.wfile.flush()
            result["response_written"] = True
        except Exception as e:
            result["provider_write_error"] = repr(e)
        finally:
            response_sent.set()


httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Provider)
threading.Thread(target=httpd.serve_forever, daemon=True).start()
(C / "config.toml").write_text(
    f"""model = "local-test"
model_provider = "local"
approval_policy = "never"
sandbox_mode = "danger-full-access"
[analytics]
enabled = false
[model_providers.local]
name = "Isolated local fixture"
base_url = "http://127.0.0.1:{httpd.server_port}/v1"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = false
"""
)


def check(name, value):
    result["assertions"].append({"name": name, "pass": bool(value)})
    if not value:
        raise AssertionError(name)


def launch(command, name):
    f = (R / f"{name}.stderr.log").open("wb")
    files.append(f)
    p = subprocess.Popen(
        command,
        cwd=R,
        env=E,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=f,
        start_new_session=True,
    )
    processes.append(p)
    buffers[p.pid] = b""
    messages[p.pid] = []
    return p


def request(p, method, params=None, id=1, timeout=15):
    req = {"method": method, "params": params or {}}
    if id is not None:
        req["id"] = id
    p.stdin.write((json.dumps(req) + "\n").encode())
    p.stdin.flush()
    if id is None:
        return
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        for m in messages[p.pid]:
            if m.get("id") == id:
                if "error" in m:
                    raise RuntimeError(json.dumps(m))
                return m["result"]
        ready, _, _ = select.select([p.stdout], [], [], 0.1)
        if ready:
            chunk = os.read(p.stdout.fileno(), 65536)
            if not chunk:
                raise RuntimeError("stdout ended " + str(p.poll()))
            buffers[p.pid] += chunk
            while b"\n" in buffers[p.pid]:
                line, buffers[p.pid] = buffers[p.pid].split(b"\n", 1)
                if line:
                    messages[p.pid].append(json.loads(line))
    raise TimeoutError(method)


def connect(name):
    p = launch([NODE, BRIDGE], name)
    request(
        p,
        "initialize",
        {
            "clientInfo": {"name": "local-background-test", "version": "1.0"},
            "capabilities": {"experimentalApi": True},
        },
    )
    request(p, "initialized", id=None)
    return p


try:
    server = launch([CLI, "app-server", "--listen", "unix://" + str(SOCK)], "server")
    server.stdin.close()
    deadline = time.monotonic() + 10
    while not SOCK.exists() and time.monotonic() < deadline:
        time.sleep(0.05)
    check("isolated server listening", SOCK.exists())
    first = connect("first")
    thread = request(
        first,
        "thread/start",
        {
            "cwd": str(R),
            "approvalPolicy": "never",
            "sandbox": "danger-full-access",
            "model": "local-test",
            "modelProvider": "local",
        },
        id=2,
    )
    result["thread_start"] = thread
    tid = thread["thread"]["id"]
    turn = request(
        first,
        "turn/start",
        {
            "threadId": tid,
            "input": [
                {
                    "type": "text",
                    "text": "Reply only LOCAL_BACKGROUND_COMPLETED",
                    "text_elements": [],
                }
            ],
        },
        id=3,
    )
    result["turn_start"] = turn
    check("local fixture receives model request", first_request.wait(15))
    first.stdin.close()
    first.wait(timeout=5)
    check(
        "UI bridge exited before provider returns",
        first.returncode == 0 and not response_sent.is_set(),
    )
    check("background server alive during disconnected turn", server.poll() is None)
    # Intentionally complete the fixture only while no UI client is connected.
    allow_response.set()
    check("local fixture finished response", response_sent.wait(5))
    time.sleep(0.7)
    second = connect("second")
    final = request(
        second, "thread/read", {"threadId": tid, "includeTurns": True}, id=4
    )
    result["final_thread"] = final
    turns = final["thread"]["turns"]
    check(
        "turn completed after all clients disconnected",
        any(t.get("status") == "completed" for t in turns),
    )
    check(
        "reconnected thread contains fixture answer",
        "LOCAL_BACKGROUND_COMPLETED" in json.dumps(turns),
    )
    check(
        "only localhost provider requested, without credentials",
        len(result["requests"]) == 1 and not result["requests"][0]["auth_present"],
    )
    second.stdin.close()
    second.wait(timeout=5)
    result["success"] = True
except Exception:
    result["success"] = False
    result["error"] = traceback.format_exc()
finally:
    allow_response.set()
    for p in reversed(processes):
        if p.poll() is None:
            os.killpg(p.pid, signal.SIGTERM)
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(p.pid, signal.SIGKILL)
                p.wait(timeout=5)
    for f in files:
        f.close()
    httpd.shutdown()
    httpd.server_close()
    result["cleanup"] = {str(p.pid): p.returncode for p in processes}
    (R / "messages.json").write_text(json.dumps(messages, indent=2))
    (R / "result.json").write_text(json.dumps(result, indent=2))
    print(json.dumps({'success': result['success'], 'assertions': len(result['assertions']),
                      'result': str(R / 'result.json'), 'cleanup': result['cleanup']}, indent=2))
    raise SystemExit(0 if result["success"] else 1)
