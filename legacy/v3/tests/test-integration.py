#!/usr/bin/env python3
"""Real isolated control-plane test. Never calls a provider or systemd.

Runs actual Codex/Node and injects a temporary npm that only copies an existing
ws package. Override CB_TEST_CODEX, CB_TEST_NODE or CB_TEST_WS to use other local
installations. Artifacts stay in the printed /tmp directory for review.
"""
import json
import os
from pathlib import Path
import select
import shutil
import signal
import subprocess
import tempfile
import time
import traceback

SOURCE = Path(__file__).resolve().parents[1]
CODEX = Path(
    os.environ.get(
        "CB_TEST_CODEX", "/home/coder/.codex/packages/standalone/current/codex"
    )
)
NODE = Path(
    os.environ.get(
        "CB_TEST_NODE",
        shutil.which("node") or "/home/coder/.nvm/versions/node/v22.16.0/bin/node",
    )
)
WS = Path(os.environ.get("CB_TEST_WS", "/home/coder/apps/codexapp/node_modules/ws"))
ROOT = Path(tempfile.mkdtemp(prefix="codex-background-integration-"))
HOME = ROOT / "home"
HOME.mkdir()
BIN = ROOT / "bin"
BIN.mkdir()
(BIN / "node").symlink_to(NODE)
# Installer intentionally prefers npm next to its selected Node. Use a private
# symlink to the real executable, with our network-free npm beside it.
(BIN / "npm").write_text(
    """#!/bin/bash
set -euo pipefail
if [[ ${CB_TEST_FAIL_NPM:-0} = 1 ]]; then exit 73; fi
prefix=''
while [ "$#" -gt 0 ]; do
    if [ "$1" = --prefix ]; then prefix=$2; shift; fi
    shift
done
[[ "$prefix" = "$CB_TEST_ROOT"/* ]] || exit 91
mkdir -p "$prefix/node_modules"
rm -rf -- "$prefix/node_modules/ws"
cp -a "$CB_TEST_WS" "$prefix/node_modules/ws"
"""
)
(BIN / "npm").chmod(0o700)
RUNTIME = HOME / ".local/share/codex-background"
STATE = HOME / ".local/state/codex-background"
CODEX_HOME = HOME / ".codex"
CODEX_HOME.mkdir()
ENTRY = HOME / "entrypoint.d"
SETTINGS = HOME / "settings.json"
ORIGINAL_SETTINGS = b'{\n  // preserved comment\n  "editor.fontSize": 17,\n  "chatgpt.cliExecutable": "/old/custom/codex",\n}\n'
SETTINGS.write_bytes(ORIGINAL_SETTINGS)
ENV = {
    "HOME": str(HOME),
    "CODEX_HOME": str(CODEX_HOME),
    "PATH": f"{BIN}:/usr/bin:/bin",
    "LANG": "C.UTF-8",
    "CB_TEST_ROOT": str(ROOT),
    "CB_TEST_WS": str(WS),
}
RESULT = {
    "root": str(ROOT),
    "checks": [],
    "commands": [],
    "success": False,
    "limits": "Container backend only, isolated actual app-server control-plane, no provider/model turn or real IDE.",
}
CLIENTS = []


def check(name, value):
    RESULT["checks"].append({"name": name, "pass": bool(value)})
    if not value:
        raise AssertionError(name)


def command(args, *, allowed=(0,), timeout=45):
    process = subprocess.run(
        args, env=ENV, cwd=SOURCE, capture_output=True, text=True, timeout=timeout
    )
    RESULT["commands"].append(
        {
            "args": [str(a) for a in args],
            "code": process.returncode,
            "stdout": process.stdout,
            "stderr": process.stderr,
        }
    )
    if allowed is not None and process.returncode not in allowed:
        raise AssertionError(
            f"{args}: rc={process.returncode}\n{process.stderr}\n{process.stdout}"
        )
    return process


def deploy(*args):
    return command(["/bin/bash", str(SOURCE / "deploy.sh"), *args])


def options():
    return [
        "--backend",
        "container",
        "--entrypoint-dir",
        str(ENTRY),
        "--settings",
        str(SETTINGS),
        "--codex",
        str(CODEX),
        "--node",
        str(BIN / "node"),
        "--yes",
    ]


def own_processes():
    """Exact temp HOME and CODEX_HOME prove ownership before any termination."""
    found = []
    for item in Path("/proc").iterdir():
        if not item.name.isdecimal() or int(item.name) == os.getpid():
            continue
        try:
            env = (item / "environ").read_bytes().split(b"\0")
            if (
                f"HOME={HOME}".encode() in env
                and f"CODEX_HOME={CODEX_HOME}".encode() in env
            ):
                found.append(int(item.name))
        except (PermissionError, FileNotFoundError, ProcessLookupError):
            pass
    return found


def process_alive(pid):
    try:
        return Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[0] != "Z"
    except FileNotFoundError:
        return False


def read_supervisor():
    return int((STATE / "supervisor.pid").read_text().split()[0])


def protocol_roundtrip(label):
    error_file = (ROOT / f"{label}.stderr.log").open("wb")
    process = subprocess.Popen(
        [str(RUNTIME / "codex"), "app-server", "--stdio"],
        env=ENV,
        cwd=ROOT,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=error_file,
    )
    CLIENTS.append(process)
    requests = [
        {
            "id": 1,
            "method": "initialize",
            "params": {
                "clientInfo": {"name": "isolated-integration", "version": "1.0"}
            },
        },
        {"method": "initialized"},
        {"id": 2, "method": "thread/list", "params": {"limit": 10}},
    ]
    messages = []
    buffered = b""
    try:
        for request in requests:
            process.stdin.write((json.dumps(request) + "\n").encode())
            process.stdin.flush()
            if "id" not in request:
                continue
            deadline = time.monotonic() + 10
            while not any(message.get("id") == request["id"] for message in messages):
                if time.monotonic() >= deadline:
                    raise TimeoutError(f'{label}: {request["method"]}')
                readable, _, _ = select.select([process.stdout], [], [], 0.2)
                if readable:
                    chunk = os.read(process.stdout.fileno(), 65536)
                    if not chunk:
                        raise RuntimeError(f"{label}: bridge stdout closed")
                    buffered += chunk
                    while b"\n" in buffered:
                        line, buffered = buffered.split(b"\n", 1)
                        if line:
                            messages.append(json.loads(line))
            reply = next(
                message for message in messages if message.get("id") == request["id"]
            )
            check(
                f'{label}: {request["method"]} success',
                "result" in reply and "error" not in reply,
            )
        check(
            f"{label}: no real sessions visible",
            next(m for m in messages if m.get("id") == 2)["result"]["data"] == [],
        )
        process.stdin.close()
        process.wait(timeout=5)
        check(f"{label}: EOF exits bridge", process.returncode == 0)
    finally:
        if process.poll() is None:
            process.terminate()
            process.wait(timeout=5)
        error_file.close()
        (ROOT / f"{label}.messages.json").write_text(json.dumps(messages, indent=2))


try:
    check(
        "real Codex/Node/ws exist",
        CODEX.is_file() and NODE.is_file() and (WS / "package.json").is_file(),
    )
    package = json.loads((SOURCE / "payload/bridge/package.json").read_text())
    check(
        "test ws version matches pinned package",
        json.loads((WS / "package.json").read_text())["version"]
        == package["dependencies"]["ws"],
    )
    # Fail before selecting the runtime, then after app-server start while applying
    # malformed JSONC. Both failures must roll back and permit a clean retry.
    ENV["CB_TEST_FAIL_NPM"] = "1"
    failed = command(
        ["/bin/bash", str(SOURCE / "deploy.sh"), "install", *options()], allowed=None
    )
    ENV.pop("CB_TEST_FAIL_NPM")
    check("npm failure preserves nonzero exit status", failed.returncode == 73)
    check(
        "npm failure leaves no runtime or state",
        not RUNTIME.exists() and not STATE.exists(),
    )
    check(
        "npm failure preserves original settings",
        SETTINGS.read_bytes() == ORIGINAL_SETTINGS,
    )
    check(
        "npm failure cleans staging directory",
        not list(RUNTIME.parent.glob("codex-background.stage.*")),
    )
    invalid_settings = b'{ "editor.fontSize": invalid-json }\n'
    SETTINGS.write_bytes(invalid_settings)
    failed = command(
        ["/bin/bash", str(SOURCE / "deploy.sh"), "install", *options()], allowed=None
    )
    check("invalid settings installation fails", failed.returncode != 0)
    check(
        "settings failure restores clean runtime/state",
        not RUNTIME.exists() and not STATE.exists(),
    )
    check(
        "settings failure preserves original invalid bytes",
        SETTINGS.read_bytes() == invalid_settings,
    )
    check(
        "settings failure removes startup adapter",
        not (ENTRY / "20-codex-background.sh").exists(),
    )
    check("settings rollback leaves no backend process", not own_processes())
    SETTINGS.write_bytes(ORIGINAL_SETTINGS)
    deploy("install", *options())
    check("runtime installed", (RUNTIME / "managed-v3").is_file())
    check(
        "container entrypoint installed", (ENTRY / "20-codex-background.sh").is_file()
    )
    check("settings points to runtime", str(RUNTIME / "codex") in SETTINGS.read_text())
    deploy("status")
    first_pid = read_supervisor()
    check("supervisor alive", process_alive(first_pid))
    # A long-lived child inheriting the installer lock would break maintenance.
    command(["/usr/bin/flock", "-n", str(STATE / "lock"), "/bin/true"], timeout=3)
    check("installer lock released while backend is alive", True)
    command([str(RUNTIME / "payload/control.sh"), "start"])
    check("start is idempotent", first_pid == read_supervisor())
    protocol_roundtrip("first")
    check("supervisor survives first UI disconnect", process_alive(first_pid))
    protocol_roundtrip("reconnect")
    deploy("update", *options(), "--restart")
    check("old supervisor stopped on update restart", not process_alive(first_pid))
    deploy("status")
    command(["/usr/bin/flock", "-n", str(STATE / "lock"), "/bin/true"], timeout=3)
    protocol_roundtrip("updated")
    deploy("uninstall", "--yes")
    check(
        "original settings restored byte-for-byte",
        SETTINGS.read_bytes() == ORIGINAL_SETTINGS,
    )
    check(
        "runtime and managed state removed", not RUNTIME.exists() and not STATE.exists()
    )
    check(
        "startup and controller removed",
        not (ENTRY / "20-codex-background.sh").exists()
        and not (HOME / ".local/bin/codex-background").exists(),
    )
    check(
        "external Codex and Node preserved",
        CODEX.is_file() and NODE.is_file() and (BIN / "node").is_file(),
    )
    check("Codex user data preserved", CODEX_HOME.is_dir())
    RESULT["post_uninstall_processes"] = {
        str(pid): Path(f"/proc/{pid}/cmdline")
        .read_bytes()
        .replace(b"\0", b" ")
        .decode(errors="replace")
        for pid in own_processes()
    }
    check("no owned background process remains", not own_processes())
    RESULT["success"] = True
except Exception:
    RESULT["error"] = traceback.format_exc()
finally:
    for client in CLIENTS:
        if client.poll() is None:
            client.terminate()
            try:
                client.wait(timeout=3)
            except subprocess.TimeoutExpired:
                client.kill()
                client.wait(timeout=3)
    # Even a failed install must not leave a daemon. Only touch processes with
    # both exact isolated environment values, never production runtime PIDs.
    leftover = own_processes()
    for pid in leftover:
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
    RESULT["cleanup"] = {
        "terminated_owned_pids": leftover,
        "remaining_owned_pids": own_processes(),
    }
    (ROOT / "result.json").write_text(json.dumps(RESULT, indent=2))
    print(json.dumps({'success': RESULT['success'], 'checks': len(RESULT['checks']),
                      'result': str(ROOT / 'result.json'), 'cleanup': RESULT['cleanup']}, indent=2))
    raise SystemExit(0 if RESULT["success"] else 1)
