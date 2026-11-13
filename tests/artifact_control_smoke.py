#!/usr/bin/env python3
"""Exercise the native control daemon against synthetic local inputs only."""
import argparse
import json
import os
import pathlib
import signal
import socket
import subprocess
import tempfile
import time


def request(path, op, **fields):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
        connection.settimeout(3)
        connection.connect(str(path))
        connection.sendall(json.dumps({"op": op, **fields}).encode() + b"\n")
        with connection.makefile("rb") as stream:
            frame = stream.readline(512 * 1024 + 1)
        if not frame.endswith(b"\n") or len(frame) > 512 * 1024:
            raise AssertionError("invalid response frame")
        response = json.loads(frame)
        assert response["ok"], response
        return response["result"]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True)
    args = parser.parse_args()
    with tempfile.TemporaryDirectory() as temporary, socket.socket() as modem:
        directory = pathlib.Path(temporary)
        os.chmod(directory, 0o700)
        modem.bind(("127.0.0.1", 0))
        modem.listen(1)
        modem.settimeout(3)
        control = directory / "control.sock"
        config = directory / "config.json"
        settings = {
            "control_socket": str(control),
            "resource_guard": {"enabled": False},
            "radio": {"kind": "js8call", "endpoint": "%s:%d" % modem.getsockname(),
                      "callsign": "N0CALL", "destination": "@AREDN"},
        }
        config.write_text(json.dumps(settings))
        config.chmod(0o600)
        process = subprocess.Popen([args.binary, "-config", str(config)],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        try:
            with modem.accept()[0] as connection:
                connection.settimeout(3)
                end = time.monotonic() + 3
                while not control.exists() and time.monotonic() < end:
                    assert process.poll() is None, "daemon exited during startup"
                    time.sleep(0.01)
                text = "W1AW: N0CALL AL3 PFFBXVIDLIKO 00V \u2662"
                connection.sendall(json.dumps({
                    "type": "RX.DIRECTED", "value": text,
                    "params": {"FROM": "W1AW", "TO": "N0CALL", "CMD": " ",
                               "EXTRA": "", "TEXT": text},
                }).encode() + b"\n")
                end = time.monotonic() + 3
                while time.monotonic() < end:
                    snapshot = request(control, "status")
                    if snapshot["peers"]:
                        break
                    time.sleep(0.01)
                assert snapshot["health"]["state"] == "connected", snapshot
                assert snapshot["health"]["rx"]["recent_callsigns"][0]["callsign"] == "W1AW"
                assert "tx" not in snapshot["health"], snapshot
                assert snapshot["peers"][0]["probe"] == "NOT_CHECKED"
                assert snapshot["peers"][0]["services"] == 31, snapshot
                assert len(snapshot["peers"][0]["service_names"]) == 5, snapshot
                initial = request(control, "config.get")
                assert not initial["pending"] and not initial["save_disabled"]
                settings["max_peers"] = 12
                request(control, "config.validate", config=json.dumps(settings))
                request(control, "config.save", config=json.dumps(settings))
                saved = request(control, "config.get")
                assert saved["pending"] and json.loads(saved["running"])["max_peers"] == 64
                assert json.loads(saved["saved"])["max_peers"] == 12
                # Non-evidential: a 50 ms window cannot observe a beacon that would not
                # fire for at least 30 minutes even when enabled. The TX-disabled check
                # is the status assertion above ("tx" absent from health); the schedule
                # itself is covered by a Go unit test.
                memory = pathlib.Path("/proc/%d/status" % process.pid).read_text()
                print(next(line for line in memory.splitlines() if line.startswith("VmRSS:")))
                process.send_signal(signal.SIGTERM)
                assert process.wait(timeout=3) == 0
                assert not control.exists(), "socket retained after SIGTERM"
                print("native control artifact: synthetic AL3 RX, service claims, config save, SIGTERM; tx absent from status passed")
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=3)
            if process.stderr:
                diagnostic = process.stderr.read(4096).decode(errors="replace")
                process.stderr.close()
                if process.returncode:
                    raise AssertionError(diagnostic)


if __name__ == "__main__":
    main()
