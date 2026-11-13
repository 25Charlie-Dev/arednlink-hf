#!/usr/bin/env python3
"""Exercise a compiled JS8Call artifact using a synthetic loopback modem."""
import argparse
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile

from control_client import request, stop, wait_ready, wait_status


def send(modem, event, value, params):
    modem.sendall(json.dumps({"type": event, "value": value, "params": params}).encode() + b"\n")


def verify_reception(control, modem):
    initial = wait_status(control, lambda s: s["health"]["state"] == "connected")["health"]
    assert initial["rx"] == {"last_heard": None, "age_seconds": None, "recent_callsigns": []}
    for call in ["W1AW", "W2AW", "W3AW", "W4AW", "W5AW", "W2AW", "W6AW"]:
        if call == "W3AW":
            send(modem, "RX.DIRECTED", call + ": @HB HEARTBEAT", {
                "FROM": call, "TO": "@HB", "CMD": " HEARTBEAT", "TEXT": call + ": @HB HEARTBEAT"})
        else:
            send(modem, "RX.ACTIVITY", call + ": ordinary chat", {})
    expected = ["W6AW", "W2AW", "W5AW", "W4AW", "W3AW"]
    view = wait_status(control, lambda s: [
        station["callsign"] for station in s["health"]["rx"]["recent_callsigns"]] == expected)
    health = view["health"]
    assert health["healthy"] and health["rx"]["last_heard"].endswith("Z")
    assert all(station["last_heard"].endswith("Z") for station in health["rx"]["recent_callsigns"])
    assert view["peers"] == [], view
    wait_status(control, lambda s: s["health"]["rx"]["age_seconds"] >= 1)
    send(modem, "RX.ACTIVITY", "continuation mentions W9AW: hello", {})
    after = wait_status(control, lambda s: s["health"]["rx"]["last_heard"] != health["rx"]["last_heard"])
    assert after["health"]["rx"]["recent_callsigns"] == health["rx"]["recent_callsigns"]
    assert after["peers"] == [], after
    return after["health"]["rx"]


def verify_artifact(binary, directory, listener):
    os.chmod(directory, 0o700)
    control = directory / "control.sock"
    config = directory / "config.json"
    config.write_text(json.dumps({
        "control_socket": str(control),
        "radio": {"kind": "js8call", "endpoint": "127.0.0.1:%d" % listener.getsockname()[1],
                  "callsign": "N0CALL", "destination": "@AREDN"}}), encoding="utf-8")
    config.chmod(0o600)
    with (directory / "daemon.log").open("wb") as log:
        process = subprocess.Popen([str(binary), "-config", str(config)], stdout=log, stderr=log)
        try:
            modem, _ = listener.accept()
            with modem:
                modem.settimeout(2)
                wait_ready(process, control)
                history = verify_reception(control, modem)
            listener.close()
            disconnected = wait_status(control, lambda s: s["health"]["state"] == "backoff")
            assert not disconnected["health"]["healthy"]
            assert disconnected["health"]["rx"]["last_heard"] == history["last_heard"]
            assert disconnected["health"]["rx"]["recent_callsigns"] == history["recent_callsigns"]
            stop(process, timeout=5)
            assert not control.exists(), "socket retained after SIGTERM"
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="receive-smoke-") as temporary, socket.socket() as listener:
        listener.settimeout(5)
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        verify_artifact(args.binary.resolve(strict=True), Path(temporary), listener)
    print("PASS: passive receive history, unique five-station MRU, empty peers, honest disconnect and SIGTERM")


if __name__ == "__main__":
    main()
