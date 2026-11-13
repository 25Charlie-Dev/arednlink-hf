#!/usr/bin/env python3
"""Exercise restart-only configuration saving against a compiled local daemon."""
import argparse
import json
import os
import pathlib
import subprocess
import tempfile

from control_client import request, stop, wait_ready


def start(binary, config, control):
    process = subprocess.Popen([binary, "-config", str(config)], stdin=subprocess.DEVNULL,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        wait_ready(process, control)
    except BaseException:
        process.kill()
        process.wait(timeout=3)
        raise
    return process


def smoke(binary):
    with tempfile.TemporaryDirectory(prefix="config-smoke-") as directory:
        os.chmod(directory, 0o700)
        control = pathlib.Path(directory) / "control.sock"
        config = pathlib.Path(directory) / "config.json"
        original = {"control_socket": str(control)}
        config.write_text(json.dumps(original), encoding="utf-8")
        config.chmod(0o600)
        process = start(binary, config, control)
        try:
            initial = request(control, "config.get")
            assert json.loads(initial["running"])["max_peers"] == 64
            assert not initial["pending"] and not initial["save_disabled"], initial
            draft = dict(original, max_peers=12)
            # Invalid drafts (duplicate keys, unknown operation) are refused and leave the file intact.
            request(control, "config.validate", expect_error="invalid_config",
                    config='{"max_peers":2,"max_peers":3}')
            request(control, "config.save", expect_error="invalid_config",
                    config='{"max_peers":2,"max_peers":3}')
            request(control, "config.nope", expect_error="unknown_operation")
            assert json.loads(config.read_text()) == original
            request(control, "config.validate", config=json.dumps(draft))
            assert json.loads(config.read_text()) == original, "validate must not write"
            request(control, "config.save", config=json.dumps(draft))
            assert json.loads(config.read_text()) == draft
            saved = request(control, "config.get")
            assert saved["pending"], saved
            assert json.loads(saved["running"])["max_peers"] == 64
            assert json.loads(saved["saved"])["max_peers"] == 12
            assert config.stat().st_mode & 0o777 == 0o600
            request(control, "status")
        finally:
            stop(process)
        process = start(binary, config, control)
        try:
            reloaded = request(control, "config.get")
            assert json.loads(reloaded["running"])["max_peers"] == 12
            assert json.loads(reloaded["saved"])["max_peers"] == 12
            assert not reloaded["pending"], reloaded
        finally:
            stop(process)
    print("configuration artifact smoke passed: guarded save, pending restart, restart load, status, SIGTERM")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True)
    arguments = parser.parse_args()
    smoke(str(pathlib.Path(arguments.binary).resolve()))
