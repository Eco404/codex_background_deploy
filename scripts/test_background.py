#!/usr/bin/env python3
"""Isolated Go installer and active-turn test; no external provider or credentials.

Build ./codex-bg first. CB_TEST_CODEX selects an existing standalone Codex.
All test resources stay under installer-data/tmp; reports contain only a summary.
"""
import http.server
import json
import os
from pathlib import Path
import select
import signal
import subprocess
import tempfile
import threading
import time
import traceback

PROJECT = Path(__file__).resolve().parents[1]
BINARY = PROJECT / "codex-bg"
CODEX = Path(
    os.environ.get(
        "CB_TEST_CODEX", "/home/coder/.codex/packages/standalone/current/codex"
    )
)
TEMP = PROJECT / "installer-data/tmp"
TEMP.mkdir(parents=True, exist_ok=True)
ROOT = Path(tempfile.mkdtemp(prefix="it-", dir=TEMP))
HOME = ROOT / "h"
RUNTIME = ROOT / "r"
INSTALLER = ROOT / "i"
DATA = INSTALLER / "installer-data"
CODEX_HOME = HOME / ".codex"
BIN = ROOT / "bin"
EXTENSIONS = HOME / "extensions"
ENTRYPOINT = HOME / "entrypoint.d"
for directory in [HOME, INSTALLER, CODEX_HOME, BIN, EXTENSIONS]:
    directory.mkdir(parents=True, exist_ok=True)
(BIN / "git").write_text("#!/bin/sh\nexit 1\n")
(BIN / "git").chmod(0o700)
ENV = {
    "HOME": str(HOME),
    "CODEX_HOME": str(CODEX_HOME),
    "PATH": f"{BIN}:/usr/bin:/bin",
    "LANG": "C.UTF-8",
    "TMPDIR": str(ROOT),
    "USER": str(os.getuid()),
}
BASE = [
    str(BINARY),
    "--home",
    str(HOME),
    "--installer-dir",
    str(INSTALLER),
    "--runtime",
    str(RUNTIME),
]
RESULT = {"root": str(ROOT), "checks": [], "commands": [], "success": False}
PROVIDER_REQUEST = threading.Event()
RELEASE_RESPONSE = threading.Event()
PROVIDER_FINISHED = threading.Event()
CLIENTS = []
BUFFERS = {}
MESSAGES = {}


def check(name, condition):
    RESULT["checks"].append({"name": name, "pass": bool(condition)})
    if not condition:
        raise AssertionError(name)


def command(*args, allowed=(0,), timeout=50):
    completed = subprocess.run(
        [*BASE, *args],
        env=ENV,
        cwd=ROOT,
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    RESULT["commands"].append(
        {
            "args": list(args),
            "code": completed.returncode,
            "stdout": completed.stdout,
            "stderr": completed.stderr,
        }
    )
    if allowed is not None and completed.returncode not in allowed:
        raise AssertionError(
            f"{args}: rc={completed.returncode}\n{completed.stderr}\n{completed.stdout}"
        )
    return completed


def state():
    return json.loads((RUNTIME / "state.json").read_text())


def own_processes():
    found = {}
    for entry in Path("/proc").iterdir():
        if not entry.name.isdecimal() or int(entry.name) == os.getpid():
            continue
        try:
            env = (entry / "environ").read_bytes().split(b"\0")
            if (
                f"HOME={HOME}".encode() in env
                and f"CODEX_HOME={CODEX_HOME}".encode() in env
            ):
                found[int(entry.name)] = (
                    (entry / "cmdline")
                    .read_bytes()
                    .replace(b"\0", b" ")
                    .decode(errors="replace")
                )
        except (FileNotFoundError, ProcessLookupError, PermissionError):
            pass
    return found


def create_extension(version):
    entry = EXTENSIONS / f"openai.chatgpt-{version}" / "bin/linux-x64/codex"
    entry.parent.mkdir(parents=True)
    original = f'#!/bin/sh\nprintf "fixture original {version}\\n"\n'.encode()
    entry.write_bytes(original)
    entry.chmod(0o755)
    return entry, original


class LocalProvider(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        RESULT.setdefault("provider_requests", []).append(
            {
                "path": self.path,
                "body": json.loads(raw),
                "authorization_present": bool(self.headers.get("Authorization")),
            }
        )
        PROVIDER_REQUEST.set()
        RELEASE_RESPONSE.wait(20)
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
        payload = "".join(
            f'event: {event["type"]}\ndata: {json.dumps({**event, "sequence_number": i})}\n\n'
            for i, event in enumerate(events)
        ).encode()
        try:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            self.wfile.flush()
            RESULT["provider_response_written"] = True
        except Exception as error:
            RESULT["provider_write_error"] = repr(error)
        finally:
            PROVIDER_FINISHED.set()


HTTPD = http.server.ThreadingHTTPServer(("127.0.0.1", 0), LocalProvider)
threading.Thread(target=HTTPD.serve_forever, daemon=True).start()
(CODEX_HOME / "config.toml").write_text(
    f"""model = "local-test"
model_provider = "local"
approval_policy = "never"
sandbox_mode = "danger-full-access"
[analytics]
enabled = false
[model_providers.local]
name = "Isolated localhost fixture"
base_url = "http://127.0.0.1:{HTTPD.server_port}/v1"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = false
"""
)


def request(process, method, params=None, request_id=1, timeout=15):
    value = {"method": method, "params": params or {}}
    if request_id is not None:
        value["id"] = request_id
    process.stdin.write((json.dumps(value) + "\n").encode())
    process.stdin.flush()
    if request_id is None:
        return None
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        for message in MESSAGES[process.pid]:
            if message.get("id") == request_id:
                if "error" in message:
                    raise RuntimeError(json.dumps(message))
                return message["result"]
        readable, _, _ = select.select([process.stdout], [], [], 0.1)
        if not readable:
            continue
        chunk = os.read(process.stdout.fileno(), 65536)
        if not chunk:
            raise RuntimeError(f"proxy stdout ended: {process.poll()}")
        BUFFERS[process.pid] += chunk
        while b"\n" in BUFFERS[process.pid]:
            line, BUFFERS[process.pid] = BUFFERS[process.pid].split(b"\n", 1)
            if line:
                MESSAGES[process.pid].append(json.loads(line))
    raise TimeoutError(method)


def connect(entry, name):
    error_log = (ROOT / f"{name}.stderr.log").open("wb")
    process = subprocess.Popen(
        [str(entry), "app-server", "--stdio"],
        cwd=ROOT,
        env=ENV,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=error_log,
    )
    error_log.close()
    CLIENTS.append(process)
    BUFFERS[process.pid] = b""
    MESSAGES[process.pid] = []
    request(
        process,
        "initialize",
        {
            "clientInfo": {"name": "isolated-go-test", "version": "1.0"},
            "capabilities": {"experimentalApi": True},
        },
    )
    request(process, "initialized", request_id=None)
    return process


try:
    check("built Go runtime and real CLI exist", BINARY.is_file() and CODEX.is_file())
    first_entry, first_original = create_extension("1.0.0-linux-x64")
    command(
        "install",
        "--backend",
        "container",
        "--codex",
        str(CODEX),
        "--entrypoint-dir",
        str(ENTRYPOINT),
        "--extension-root",
        str(EXTENSIONS),
        "--yes",
    )
    check(
        "installed state records external CLI reuse",
        state()["installed"] and state()["codex"]["source"] == "reused",
    )
    check(
        "selected extension is hooked",
        b"codex-bg managed extension entry v4" in first_entry.read_bytes(),
    )
    check(
        "status performs healthy initialize",
        "initialize 正常" in command("status").stdout,
    )
    first = connect(first_entry, "first")
    thread = request(
        first,
        "thread/start",
        {
            "cwd": str(ROOT),
            "model": "local-test",
            "modelProvider": "local",
            "approvalPolicy": "never",
            "sandbox": "danger-full-access",
        },
        request_id=2,
    )
    thread_id = thread["thread"]["id"]
    request(
        first,
        "turn/start",
        {
            "threadId": thread_id,
            "input": [
                {
                    "type": "text",
                    "text": "Reply only LOCAL_BACKGROUND_COMPLETED",
                    "text_elements": [],
                }
            ],
        },
        request_id=3,
    )
    check("only local provider receives active turn", PROVIDER_REQUEST.wait(15))
    blocked = command("update", "--yes", allowed=None)
    check("busy update is refused without force", blocked.returncode != 0)
    first.stdin.close()
    first.wait(timeout=5)
    check(
        "unique UI client disconnects before provider responds",
        first.returncode == 0 and not PROVIDER_FINISHED.is_set(),
    )
    RELEASE_RESPONSE.set()
    check("local provider finishes while no UI connected", PROVIDER_FINISHED.wait(5))
    time.sleep(0.5)
    second = connect(first_entry, "second")
    final = request(
        second,
        "thread/read",
        {"threadId": thread_id, "includeTurns": True},
        request_id=4,
    )
    RESULT["final_thread"] = final
    check(
        "active turn completes after last UI disconnect",
        any(turn["status"] == "completed" for turn in final["thread"]["turns"]),
    )
    check(
        "reconnected UI reads final answer",
        "LOCAL_BACKGROUND_COMPLETED" in json.dumps(final["thread"]["turns"]),
    )
    check(
        "fixture uses exactly one local credential-free request",
        len(RESULT["provider_requests"]) == 1
        and not RESULT["provider_requests"][0]["authorization_present"],
    )
    second.stdin.close()
    second.wait(timeout=5)
    binding = state()["bindings"][0]["id"]
    check(
        "binding records observed connection",
        (RUNTIME / "connections" / f"{binding}.json").is_file(),
    )
    new_entry, new_original = create_extension("1.1.0-linux-x64")
    deadline = time.monotonic() + 6
    while time.monotonic() < deadline:
        if b"codex-bg managed extension entry v4" in new_entry.read_bytes():
            break
        time.sleep(0.1)
    check(
        "filesystem watcher automatically binds extension upgrade",
        b"codex-bg managed extension entry v4" in new_entry.read_bytes(),
    )
    command("repair", "--yes")
    check(
        "new extension version is reconciled",
        b"codex-bg managed extension entry v4" in new_entry.read_bytes(),
    )
    check(
        "both extension versions retain their own binding",
        len(state()["bindings"]) == 2,
    )
    command("update", "--yes")
    check("updated runtime healthy", "initialize 正常" in command("status").stdout)
    updates = [
        (path, json.loads(path.read_text()))
        for path in (DATA / "history").glob("*.json")
    ]
    update_id = next(
        path.stem
        for path, transaction in updates
        if transaction.get("action", transaction.get("Action")) == "update"
    )
    command("rollback", update_id, "--yes")
    check("rollback runtime healthy", "initialize 正常" in command("status").stdout)
    command("uninstall", "--yes")
    check(
        "first extension original restored byte-for-byte",
        first_entry.read_bytes() == first_original,
    )
    check(
        "new extension original restored byte-for-byte",
        new_entry.read_bytes() == new_original,
    )
    check(
        "external Codex and CodexHome preserved",
        CODEX.is_file() and (CODEX_HOME / "config.toml").is_file(),
    )
    check("container startup hook removed", not (ENTRYPOINT / "codex-bg.sh").exists())
    check(
        "runtime socket and executable removed",
        not (RUNTIME / "run/app-server.sock").exists()
        and not (RUNTIME / "codex-bg").exists(),
    )
    check(
        "auditable maintenance history retained",
        (DATA / "history").is_dir() and (DATA / "backups").is_dir(),
    )
    RESULT["remaining_before_cleanup"] = own_processes()
    check(
        "no isolated backend or child remains after uninstall",
        not RESULT["remaining_before_cleanup"],
    )
    RESULT["success"] = True
except Exception:
    RESULT["error"] = traceback.format_exc()
finally:
    RELEASE_RESPONSE.set()
    for process in CLIENTS:
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=3)
    leftovers = own_processes()
    for pid in leftovers:
        try:
            os.kill(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
    deadline = time.monotonic() + 5
    while own_processes() and time.monotonic() < deadline:
        time.sleep(0.05)
    for pid in own_processes():
        try:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    HTTPD.shutdown()
    HTTPD.server_close()
    RESULT["cleanup"] = {
        "terminated_owned_processes": leftovers,
        "remaining_owned_processes": own_processes(),
    }
    (ROOT / "messages.json").write_text(json.dumps(MESSAGES, indent=2))
    (ROOT / "result.json").write_text(json.dumps(RESULT, indent=2))
    summary = {
        "root": str(ROOT),
        "success": RESULT["success"],
        "checks": RESULT["checks"],
        "error": RESULT.get("error"),
        "cleanup": RESULT["cleanup"],
        "limits": "Real CLI + Go container lifecycle + localhost fake model only; no external provider, real IDE or systemd.",
    }
    reports = PROJECT / "installer-data/reports"
    reports.mkdir(parents=True, exist_ok=True)
    (reports / "background-test-latest.json").write_text(json.dumps(summary, indent=2))
    print(json.dumps(summary, indent=2))
    raise SystemExit(0 if RESULT["success"] else 1)
