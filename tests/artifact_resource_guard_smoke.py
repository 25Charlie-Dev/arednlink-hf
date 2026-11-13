#!/usr/bin/env python3
"""Check real proc measurements, guarded shutdown, and the control-socket status surface."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile

from control_client import request, stop, wait_ready


def exercise(binary):
    with tempfile.TemporaryDirectory(prefix="guard-smoke-") as temporary:
        directory = Path(temporary)
        os.chmod(directory, 0o700)
        control = directory / "control.sock"
        path = directory / "config.json"
        for enabled in (False, True):
            config = {"control_socket": str(control)}
            if enabled:
                config["resource_guard"] = {
                    "enabled": True, "max_rss_kib": 1048576,
                    "min_available_kib": 1024, "interval": "1s",
                }
            path.write_text(json.dumps(config), encoding="utf-8")
            path.chmod(0o600)
            process = subprocess.Popen([binary, "-config", str(path)],
                                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                snapshot = wait_ready(process, control)
                health = snapshot["health"]
                assert health["healthy"], "health never became ready"
                guard = health["resource_guard"]
                assert guard["state"] == ("healthy" if enabled else "disabled"), guard
                if enabled:
                    assert guard["rss_kib"] > 0 and guard["available_kib"] >= 1024, guard
                else:
                    assert guard["rss_kib"] is None and guard["available_kib"] is None, guard
                request(control, "config.get")
            finally:
                stop(process)
            assert not control.exists(), "socket retained after SIGTERM"

        path.write_text(json.dumps({
            "control_socket": str(control),
            "resource_guard": {"enabled": True, "max_rss_kib": 1024, "min_available_kib": 1024, "interval": "1s"},
        }), encoding="utf-8")
        with (directory / "guard.log").open("wb") as log:
            process = subprocess.Popen([binary, "-config", str(path)], stdout=log, stderr=log)
            try:
                process.wait(timeout=8)
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait(timeout=3)
        assert process.returncode != 0, "RSS threshold did not stop controller with failure"
        log_text = (directory / "guard.log").read_text(encoding="utf-8")
        assert "controller rss threshold exceeded" in log_text and "rss_kib" in log_text
        try:
            request(control, "status")
        except OSError:
            pass
        else:
            raise AssertionError("control socket survived guard shutdown")
    print("PASS: real proc measurements, disabled defaults, SIGTERM, RSS guard nonzero shutdown and socket teardown")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True)
    arguments = parser.parse_args()
    exercise(str(Path(arguments.binary).resolve(strict=True)))
