#!/usr/bin/env python3
"""Isolated real-uhttpd/browser rehearsal with synthetic native auth material.

Never use a node's /etc/shadow, cookie or config as an input. The firmware input
is tracked public source only. uhttpd is the actual target-version server.
"""
import argparse
import base64
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request


PREFIX = '/cgi-bin/apps/arednlink-hf/admin'
AUTH_LINE = 'root:$1$fixture$synthetic-only:0:0:0:0:0:0:'


def stop(process):
    if process.poll() is None:
        process.send_signal(signal.SIGTERM)
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=3)
            raise AssertionError('fixture process did not stop on SIGTERM')
    if process.returncode != 0:
        raise AssertionError('fixture process exited unsuccessfully')


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--firmware-root', type=Path, required=True)
    parser.add_argument('--uhttpd', type=Path, required=True)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--node', default='node')
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    firmware = args.firmware_root.read_text()
    auth = firmware[firmware.index('const auth = {'):firmware.index('const ubusMethods =')]
    with tempfile.TemporaryDirectory(prefix='native-browser-') as temporary:
        directory = Path(temporary)
        directory.chmod(0o700)
        docroot = directory / 'www'
        shutil.copytree(args.source / 'platforms/aredn/www', docroot)
        # Browsers may request a conventional favicon independently of this page.
        (docroot / 'favicon.ico').write_bytes(b'')
        shadow = directory / 'synthetic-shadow'
        shadow.write_text(AUTH_LINE + '\n')
        shadow.chmod(0o600)
        cookie_file = directory / 'synthetic-cookie'
        cookie_file.write_text(base64.b64encode(AUTH_LINE.encode()).decode())
        cookie_file.chmod(0o600)
        page = args.source / 'platforms/aredn/app/main/arednlink-hf.ut'
        native = directory / 'synthetic-native-root.ut'
        native.write_text('''{%
// Fixture dependency: exact public native auth object; synthetic shadow/config.
import * as realfs from 'fs';
const fs = {open: function(path) {return realfs.open(path === '/etc/shadow' ? SHADOW : path);} };
const config = {authenable: true, forceauth: false, debug: false};
const uciMeshMethods = {get: function() {return null;} };
const configuration = {};
let response;
'''.replace('SHADOW', json.dumps(str(shadow))) + auth + '''
global.handle_request = function(env) {
    response = {statusCode: 200, headers: {'Content-Type': 'text/html', 'Cache-Control': 'no-store'} };
    auth.runAuthentication(env, response);
    const rendered = render(call, loadfile(PAGE, {raw_mode:false}), null, {
        request: {env: env, headers: env.headers}, response: response, auth: auth, config: config, fs: realfs
    });
    if (!response.override) {
        uhttpd.send(`Status: ${response.statusCode} OK\\r\\n`);
        for (let name in response.headers)
            uhttpd.send(`${name}: ${response.headers[name]}\\r\\n`);
        uhttpd.send('\\r\\n', rendered);
    }
};
%}
'''.replace('PAGE', json.dumps(str(page))))
        entry = directory / 'entry.ut'
        entry.write_text((args.source / 'platforms/aredn/app/arednlink-hf-entry.ut').read_text()
                         .replace("'./arednlink-hf/gate.uc'", json.dumps(str(args.source / 'platforms/aredn/app/arednlink-hf/gate.uc')))
                         .replace("'/app/root.ut'", json.dumps(str(native))))
        control = Path('/tmp/run/arednlink-hf/control.sock')
        control.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        if os.path.lexists(control):
            raise AssertionError('isolated control path must be absent')
        config = directory / 'config.json'
        config.write_text(json.dumps({'control_socket': str(control), 'max_peers': 2,
                                      'radio': {'kind': 'none', 'beacon': {'enabled': False}}}))
        config.chmod(0o600)
        with socket.socket() as allocator:
            allocator.bind(('127.0.0.1', 0))
            port = allocator.getsockname()[1]
        base = f'http://127.0.0.1:{port}'
        logs = []
        marker = directory / 'restart-requested'
        http_env = dict(os.environ, AREDNLINK_FIXTURE_RESTART=str(marker))
        daemon = None
        server = None
        try:
            for name in ('daemon', 'uhttpd'):
                logs.append((args.output / (name + '.log')).open('w'))
            daemon = subprocess.Popen([str(args.binary), '-config', str(config)],
                                      stdout=logs[0], stderr=logs[0])
            server = subprocess.Popen([str(args.uhttpd), '-f', '-p', f'127.0.0.1:{port}',
                                      '-h', str(docroot), '-o', PREFIX, '-O', str(entry)],
                                      stdout=logs[1], stderr=logs[1], env=http_env)
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                if daemon.poll() is not None or server.poll() is not None:
                    raise AssertionError('fixture service exited before readiness')
                try:
                    with urllib.request.urlopen(base + PREFIX, timeout=1):
                        pass
                except urllib.error.HTTPError as error:
                    if error.code == 401 and control.exists():
                        break
                except OSError:
                    pass
                time.sleep(.05)
            else:
                raise AssertionError('fixture HTTP and control readiness timed out')
            env = dict(os.environ, SYNTHETIC_COOKIE_FILE=str(cookie_file))
            check = args.source / 'platforms/aredn/tests/browser/check.cjs'
            subprocess.run([args.node, str(check), base, str(args.output), 'save'], env=env, check=True)
            if json.loads(config.read_text())['max_peers'] != 4:
                raise AssertionError('browser save did not persist expected value')
            stop(daemon)
            if control.exists():
                raise AssertionError('SIGTERM left control socket behind')
            daemon = subprocess.Popen([str(args.binary), '-config', str(config)], stdout=logs[0], stderr=logs[0])
            deadline = time.monotonic() + 5
            while not control.exists() and time.monotonic() < deadline:
                if daemon.poll() is not None:
                    raise AssertionError('fixture daemon restart failed')
                time.sleep(.02)
            subprocess.run([args.node, str(check), base, str(args.output), 'restart'], env=env, check=True)
            daemon.kill()
            daemon.wait(timeout=5)
            if daemon.returncode != -signal.SIGKILL:
                raise AssertionError('recovery fixture did not kill the daemon')
            daemon = None
            if not control.exists():
                raise AssertionError('crash fixture must leave a stale control socket')
            recovered = []
            helper_errors = []
            cancel_helper = threading.Event()
            def launch_on_request():
                deadline = time.monotonic() + 40
                while not marker.exists() and not cancel_helper.is_set() and time.monotonic() < deadline:
                    time.sleep(.05)
                if cancel_helper.is_set():
                    return
                if not marker.exists():
                    helper_errors.append('browser did not request restart')
                    return
                if marker.read_text() != 'requested\n':
                    helper_errors.append('restart stub marker contents invalid')
                    return
                recovered.append(subprocess.Popen([str(args.binary), '-config', str(config)],
                                                  stdout=logs[0], stderr=logs[0]))
            helper = threading.Thread(target=launch_on_request)
            helper.start()
            try:
                subprocess.run([args.node, str(check), base, str(args.output), 'recovery'], env=env, check=True, timeout=50)
            finally:
                cancel_helper.set()
                helper.join(timeout=5)
                if recovered:
                    daemon = recovered[0]
            if helper.is_alive() or helper_errors or not recovered or daemon.poll() is not None:
                raise AssertionError('real daemon recovery failed: ' + ', '.join(helper_errors))
            (args.output / 'recovery-daemon-pid.txt').write_text(str(daemon.pid) + '\n')
        finally:
            if daemon is not None:
                stop(daemon)
            if server is not None and server.poll() is None:
                stop(server)
            for log in logs:
                log.close()
    print('real uhttpd/browser save, restart and outage recovery rehearsal: passed')


if __name__ == '__main__':
    main()
