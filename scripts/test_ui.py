#!/usr/bin/env python3
"""Run the Go UI fixture in a real PTY without installing anything.

Build the fixture with go test -c -o installer-data/tmp/ui-tests ./internal/app.
Uses only Python's standard library; Python is not a deployment dependency.
"""
import errno
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import signal
import struct
import tempfile
import termios
import time

ROOT = Path(__file__).resolve().parent.parent


def scenario(name, width, cancel=None):
    with tempfile.TemporaryDirectory(prefix="ui-smoke-", dir=ROOT / "installer-data/tmp") as folder:
        base = Path(folder)
        pid, fd = pty.fork()
        if pid == 0:
            os.environ["CODEX_BG_UI_TEST_ROOT"] = folder
            os.environ["TERM"] = "xterm-256color"
            os.execv(str(ROOT / "installer-data/tmp/ui-tests"), ["ui-tests", "-test.run=^TestUIProgressPTYHelper$"])
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 24, width, 0, 0))
        transcript = []

        def expect(text):
            output = b""
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                if select.select([fd], [], [], .05)[0]:
                    try:
                        chunk = os.read(fd, 65536)
                    except OSError as error:
                        if error.errno == errno.EIO:
                            break
                        raise
                    if not chunk:
                        break
                    output += chunk
                    if text.encode() in output:
                        transcript.append(output.decode(errors="replace"))
                        return
            raise AssertionError((name, text, output.decode(errors="replace")))

        def send(keys, text):
            os.write(fd, keys)
            expect(text)

        def wait_exit():
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                done, status = os.waitpid(pid, os.WNOHANG)
                if done:
                    assert status == 0, (name, status)
                    return
                if select.select([fd], [], [], .05)[0]:
                    try:
                        transcript.append(os.read(fd, 65536).decode(errors="replace"))
                    except OSError:
                        pass
            raise AssertionError((name, "did not exit after worker cleanup"))

        try:
            expect("r 刷新")
            send(b"\r", "安装后台")
            send(b"\r", "/fixture/extensions")
            send(b"\r", "[x]")
            send(b"\x1b[B\x1b[B\r", "Codex 运行时")
            send(b"\r", "systemd 用户服务")
            send(b"\r", "预览并确认")
            send(b"\x1b[B\x1b[B\x1b[B\x1b[B\x1b[B\r", "使用官方源")
            send(b"\r", "不使用代理")
            send(b"\r", "确认继续")
            send(b"y", "步骤 1/4")
            if cancel == "keyboard":
                send(b"\x03", "操作未完成")
                os.write(fd, b"q")
            elif cancel == "signal":
                os.kill(pid, signal.SIGTERM)
            else:
                expect("安装操作完成")
                os.write(fd, b"q")
            wait_exit()
            assert (base / "worker-finished").exists(), "UI exited before worker completed"
            log = (base / "installer-data/logs/installer.log").read_text()
            assert "步骤 1/4" in log
            assert ("取消清理已完成" in log) == (cancel is not None)
            assert "步骤 4/4" in log if cancel is None else "步骤 4/4" not in log
            assert not (base / "runtime/state.json").exists()
            return {"name": name, "width": width, "passed": True, "workerFinishedBeforeExit": True,
                    "capturedLog": True, "noDeployment": True}, "".join(transcript)
        finally:
            os.close(fd)
            try:
                os.kill(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            try:
                os.waitpid(pid, 0)
            except ChildProcessError:
                pass


def main():
    reports, transcripts = [], []
    for name, width, cancel in [("normal", 100, None), ("narrow", 48, None),
                                ("keyboard-cancel", 80, "keyboard"), ("signal-cancel", 80, "signal")]:
        report, transcript = scenario(name, width, cancel)
        reports.append(report)
        transcripts.append(f"\n{name}\n{transcript}")
        print(name + ": passed", flush=True)
    (ROOT / "installer-data/reports/options-ui-pty.json").write_text(json.dumps(reports, indent=2) + "\n")
    (ROOT / "installer-data/reports/options-ui-pty.txt").write_text("".join(transcripts))


if __name__ == "__main__":
    main()
