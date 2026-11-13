"""Shared Unix-socket client and daemon helpers for the artifact smoke tests."""
import json
import signal
import socket
import subprocess
import time

MAX_FRAME = 512 * 1024


def request(path, op, expect_error=None, **fields):
    """Send one request; return the result, or assert the stable error code."""
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
        connection.settimeout(3)
        connection.connect(str(path))
        connection.sendall(json.dumps({"op": op, **fields}).encode() + b"\n")
        with connection.makefile("rb") as stream:
            frame = stream.readline(MAX_FRAME + 1)
    if not frame.endswith(b"\n") or len(frame) > MAX_FRAME:
        raise AssertionError("invalid response frame")
    response = json.loads(frame)
    if expect_error is not None:
        assert not response["ok"] and response["error"] == expect_error, response
        return None
    assert response["ok"], response
    return response["result"]


def wait_ready(process, control, timeout=5):
    """Wait until the daemon answers a status request."""
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        assert process.poll() is None, "daemon exited during startup"
        if control.exists():
            try:
                return request(control, "status")
            except OSError:
                pass
        time.sleep(0.02)
    raise AssertionError("control socket readiness timed out")


def wait_status(control, predicate, timeout=5):
    """Poll status until predicate(snapshot) is true; return that snapshot."""
    end = time.monotonic() + timeout
    snapshot = None
    while time.monotonic() < end:
        snapshot = request(control, "status")
        if predicate(snapshot):
            return snapshot
        time.sleep(0.02)
    raise AssertionError("status predicate timed out: %r" % (snapshot,))


def stop(process, timeout=8):
    """SIGTERM the daemon and require a clean exit."""
    if process.poll() is None:
        process.send_signal(signal.SIGTERM)
        try:
            process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=3)
            raise AssertionError("daemon did not stop cleanly after SIGTERM")
    assert process.returncode == 0, "daemon exited unsuccessfully: %r" % process.returncode
