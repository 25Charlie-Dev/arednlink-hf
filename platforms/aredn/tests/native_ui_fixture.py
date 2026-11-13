#!/usr/bin/env python3
"""Exercise native template with exact firmware auth source and synthetic Unix IPC.

Run only in the isolated build container; never point this at a node rootfs.
"""
import argparse
import base64
import json
import os
import signal
from pathlib import Path
import socket
import subprocess
import tempfile
import threading
import time


AUTH_LINE = 'root:$1$fixture$synthetic-only:0:0:0:0:0:0:'
TOKEN = 'a' * 64
ENDPOINT = '/cgi-bin/apps/arednlink-hf/admin'
SOCKET = '/tmp/run/arednlink-hf/control.sock'


EXPECTED_RESTART_COMMAND = '/bin/ubus -t 5 call rc init \'{"name":"arednlink-hf","action":"restart"}\' 2>/dev/null'

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--ucode', required=True)
    parser.add_argument('--firmware-root', type=Path, required=True)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--binary', type=Path)
    args = parser.parse_args()
    firmware = args.firmware_root.read_text()
    auth = firmware[firmware.index('const auth = {'):firmware.index('const ubusMethods =')]
    page = args.source / 'platforms/aredn/app/main/arednlink-hf.ut'
    cookie = 'authV1=' + base64.b64encode(AUTH_LINE.encode()).decode()
    calls = []
    pending = False
    reply_mode = 'normal'
    draft = '{"max_peers":4}'
    stopped = threading.Event()
    Path(SOCKET).parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    if os.path.lexists(SOCKET):
        raise RuntimeError('fixture socket path must be absent')
    server = socket.socket(socket.AF_UNIX)
    server.bind(SOCKET)
    os.chmod(SOCKET, 0o600)
    server.listen(2)
    server.settimeout(.1)

    def serve():
        nonlocal pending, draft
        while not stopped.is_set():
            try:
                conn, _ = server.accept()
            except TimeoutError:
                continue
            with conn:
                conn.settimeout(3)
                wire = b''
                while not wire.endswith(b'\n'):
                    chunk = conn.recv(4096)
                    if not chunk:
                        raise RuntimeError('incomplete fixture request')
                    wire += chunk
                    if len(wire) > 524288:
                        raise RuntimeError('oversized fixture request')
                request = json.loads(wire)
                calls.append(request)
                op = request['op']
                if reply_mode == 'timeout':
                    stopped.wait(3.4)
                    continue
                if reply_mode in ('malformed', 'oversized', 'truncated'):
                    invalid = {'malformed': b'{broken\n', 'oversized': b'x' * 524289, 'truncated': b'{'}[reply_mode]
                    try:
                        conn.sendall(invalid)
                    except BrokenPipeError:
                        pass
                    continue
                result = None
                if op == 'status':
                    result = {'health': {'radio': '<script>fixture</script>', 'guard': 'normal', 'tx': {'enabled': False}},
                              'peers': [{'claim': '</textarea><script>fixture</script>', 'probe': 'ACK'}]}
                elif op == 'config.get':
                    result = {'running': '{"max_peers":2}', 'saved': draft, 'pending': pending, 'save_disabled': ''}
                elif op in ('config.validate', 'config.save'):
                    try:
                        json.loads(request['config'])
                    except ValueError:
                        conn.sendall(b'{"ok":false,"error":"invalid_config"}\n')
                        continue
                    if op == 'config.save':
                        pending, draft = True, request['config']
                    result = {'valid': True}
                response = json.dumps({'ok': True, 'result': result}).encode() + b'\n'
                # Force the client to assemble fragmented responses.
                conn.sendall(response[:8])
                conn.sendall(response[8:])

    worker = threading.Thread(target=serve)
    worker.start()
    with tempfile.TemporaryDirectory() as temp:
        harness = Path(temp) / 'case.uc'

        def run(method='GET', authenticated=True, origin='http://node.local.mesh', operation='status', body='', marker=True, forceauth=False, stale_admin=False, csrf_cookie=TOKEN, csrf_header=TOKEN, fetch_site='same-origin', debug=False, native_cookie=None, restart_mode='ok'):
            headers = {'origin': origin, 'x-arednlink-csrf': csrf_header,
                       'x-arednlink-operation': operation, 'sec-fetch-site': fetch_site}
            headers['cookie'] = (native_cookie if native_cookie is not None else cookie if authenticated else 'authV1=invalid') + '; arednlinkCSRF=' + csrf_cookie
            env = {'REQUEST_URI': ENDPOINT, 'REQUEST_METHOD': method, 'CONTENT_LENGTH': str(len(body.encode())),
                   'CONTENT_TYPE': 'text/plain;charset=UTF-8', 'HTTP_HOST': 'node.local.mesh',
                   'REMOTE_ADDR': '192.0.2.1', 'headers': headers, 'AREDNLINK_PREFLIGHT': marker}
            preamble = '''import * as realfs from 'fs';
const fs = {popen: function(command, mode) {
    if (command !== RESTART_COMMAND || mode !== 'r') die('unexpected restart command');
    if (RESTART_MODE === 'open_failure') return null;
    return {read: function(n) { if (n !== 4097) die('unbounded restart read'); return ''; }, close: function() { return RESTART_MODE === 'failed' ? 1 : 0; }};
}, open: function(path) {
    if (path === '/dev/urandom') return realfs.open(path);
    if (path !== '/etc/shadow') die('unexpected auth read');
    let done = false;
    return {read: function() { if (done) return ''; done = true; return AUTH_LINE + '\\n'; }, close: function() {}};
}};
const uciMeshMethods = { get: function() { return null; }};
const configuration = {};
const config = {authenable: true, forceauth: FORCEAUTH, debug: DEBUG};
const response = {statusCode: 200, headers: {}};
'''.replace('AUTH_LINE', json.dumps(AUTH_LINE)).replace('FORCEAUTH', str(forceauth).lower()).replace('DEBUG', str(debug).lower()).replace('RESTART_MODE', json.dumps(restart_mode)).replace('RESTART_COMMAND', json.dumps(EXPECTED_RESTART_COMMAND))
            harness.write_text(preamble + auth + '\n' + f'''
let body = {json.dumps(body)};
let output = '';
global.uhttpd = {{recv: function(n) {{const c = substr(body, 0, n); body = substr(body, n); return c;}}, send: function(s) {{ output += s; }} }};
auth.isAdmin = {str(stale_admin).lower()};
const env = {json.dumps(env)};
const page = loadfile({json.dumps(str(page))}, {{raw_mode: false}});
const rendered = render(call, page, null, {{request: {{env: env, headers: env.headers}}, response: response, auth: auth, config: config, fs: fs}});
print(sprintf('%J', {{status: response.statusCode, body: rendered, output: output, override: response.override ?? false}}));
''')
            result = subprocess.run([args.ucode, str(harness)], text=True, capture_output=True)
            if result.returncode:
                raise AssertionError('synthetic ucode harness failed: ' + result.stderr)
            return json.loads(result.stdout)

        def check(condition, description):
            if not condition:
                raise AssertionError(description)
            print(description + ': passed')

        try:
            wrapper = args.source / 'platforms/aredn/app/arednlink-hf-entry.ut'
            stub = Path(temp) / 'native-root.ut'
            stub.write_text('{% global.handle_request = function(env) { uhttpd.send(env.AREDNLINK_PREFLIGHT === true && env.PATH_INFO === "/arednlink-hf" ? "native-dispatched" : "bad-dispatch"); }; %}')
            wrapped = Path(temp) / 'entry.ut'
            wrapper_source = wrapper.read_text().replace("'./arednlink-hf/gate.uc'", json.dumps(str(args.source / 'platforms/aredn/app/arednlink-hf/gate.uc'))).replace("'/app/root.ut'", json.dumps(str(stub)))
            wrapped.write_text(wrapper_source)
            def entry(env):
                harness.write_text('let output = ""; global.uhttpd = {send: function(s) {output += s;}}; render(loadfile(' + json.dumps(str(wrapped)) + ', {raw_mode:false})); global.handle_request(' + json.dumps(env) + '); print(output);')
                return subprocess.run([args.ucode, str(harness)], text=True, capture_output=True, check=True).stdout
            base = {'REQUEST_URI': ENDPOINT, 'REQUEST_METHOD': 'GET', 'headers': {}}
            check(entry(base) == 'native-dispatched', 'actual entry wrapper dispatches preflight marker')
            check('404 Not Found' in entry(dict(base, REQUEST_URI=ENDPOINT+'/extra')), 'actual entry wrapper rejects suffix')
            check('415 Unsupported Media Type' in entry(dict(base, REQUEST_METHOD='POST', CONTENT_TYPE='application/x-www-form-urlencoded')), 'actual entry wrapper refuses native form parser')
            before = len(calls)
            check(run(authenticated=False)['status'] == 401, 'unauthenticated direct GET refused')
            check(run('POST', authenticated=False)['status'] == 401, 'unauthenticated direct POST refused')
            check(run(marker=False)['status'] == 404, 'native alternate route refused')
            check(run(forceauth=True)['status'] == 401, 'forced native auth refused')
            check(run(debug=True)['status'] == 401, 'native debug mode refused')
            check(run(native_cookie='')['status'] == 401, 'missing native auth cookie refused')
            check(run('POST', csrf_cookie='')['status'] == 403, 'missing CSRF cookie refused')
            check(run('POST', csrf_header='b' * 64)['status'] == 403, 'mismatched CSRF token refused')
            check(run('POST', fetch_site='cross-site')['status'] == 403, 'cross-site browser metadata refused')
            check(run('POST', operation='unknown')['status'] == 400, 'unknown browser operation refused')
            check(run('POST', body='unexpected')['status'] == 400, 'status body refused')
            check(run('POST', origin='http://foreign.invalid')['status'] == 403, 'cross-site POST refused')
            check(run('POST', authenticated=False, operation='service.restart')['status'] == 401, 'restart requires native authentication')
            check(run('POST', operation='service.restart', csrf_header='b' * 64)['status'] == 403, 'restart requires CSRF token')
            check(run('POST', operation='service.restart', body='x')['status'] == 400, 'restart refuses browser body')
            check(len(calls) == before, 'refusals never touched IPC')
            result = run('POST', operation='service.restart')
            check(result['status'] == 200 and json.loads(result['body'])['result']['state'] == 'requested', 'restart request independent of daemon IPC')
            result = run('POST', operation='service.restart', restart_mode='open_failure')
            check(result['status'] == 503 and json.loads(result['body'])['error'] == 'restart_failed', 'native restart spawn failure distinguished')
            result = run('POST', operation='service.restart', restart_mode='failed')
            check(result['status'] == 503 and json.loads(result['body'])['error'] == 'restart_failed', 'failed native restart distinguished')
            check(len(calls) == before, 'restart never touched daemon IPC')
            result = run()
            check(result['status'] == 200 and 'data-csrf=' in result['body'] and not result['override'], 'authenticated page rendered')
            for operation in ('status', 'config.get', 'config.validate', 'config.save'):
                result = run('POST', operation=operation, body=draft if operation.startswith('config.') and operation not in ('config.get',) else '')
                check(result['status'] == 200 and json.loads(result['body'])['ok'], operation + ' over real Unix socket')
            check(pending, 'save reached synthetic daemon')
            snapshot = json.loads(run('POST', operation='config.get')['body'])['result']
            check(snapshot['pending'] and snapshot['running'] != snapshot['saved'], 'running settings unchanged after save')
            result = run('POST', operation='config.validate', body='{broken')
            check(not json.loads(result['body'])['ok'], 'validation error preserved')
            check(run(authenticated=False, stale_admin=True)['status'] == 401, 'cached administrator privilege cleared')
            for mode in ('malformed', 'oversized', 'truncated', 'timeout'):
                reply_mode = mode
                started = time.monotonic()
                result = run('POST')
                elapsed = time.monotonic() - started
                check(result['status'] == 503 and json.loads(result['body'])['error'] == 'daemon_unavailable', mode + ' IPC response refused')
                if mode == 'timeout':
                    check(2.5 <= elapsed <= 4.5, 'IPC timeout bounded to three seconds')
            reply_mode = 'normal'
            if args.binary:
                stopped.set()
                worker.join(timeout=4)
                server.close()
                os.unlink(SOCKET)
                config_path = Path(temp) / 'config.json'
                config_path.write_text(json.dumps({'control_socket': SOCKET, 'max_peers': 2, 'radio': {'kind': 'none', 'beacon': {'enabled': False}}}))
                config_path.chmod(0o600)
                process = subprocess.Popen([str(args.binary), '-config', str(config_path)], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
                try:
                    deadline = time.monotonic() + 5
                    while not os.path.exists(SOCKET) and time.monotonic() < deadline:
                        if process.poll() is not None:
                            raise AssertionError('real backend exited before socket readiness')
                        time.sleep(.02)
                    check(os.path.exists(SOCKET), 'real backend socket ready')
                    for op in ('status', 'config.get', 'config.validate', 'config.save'):
                        data = '{"max_peers":4}' if op in ('config.validate', 'config.save') else ''
                        result = run('POST', operation=op, body=data)
                        check(result['status'] == 200 and json.loads(result['body'])['ok'], op + ' through native page to real backend')
                    result = json.loads(run('POST', operation='config.get')['body'])['result']
                    check(result['pending'] and json.loads(result['running'])['max_peers'] == 2 and json.loads(result['saved'])['max_peers'] == 4, 'real backend restart-only save preserved running settings')
                    check(json.loads(config_path.read_text())['max_peers'] == 4, 'real backend atomic save persisted')
                    result = run('POST', operation='config.save', body='{broken')
                    check(not json.loads(result['body'])['ok'] and json.loads(config_path.read_text())['max_peers'] == 4, 'real backend invalid save preserved file')
                finally:
                    process.send_signal(signal.SIGTERM)
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=3)
                        raise AssertionError('real backend SIGTERM deadline exceeded')
                    process.stderr.close()
                check(process.returncode == 0 and not os.path.exists(SOCKET), 'real backend clean SIGTERM removed socket')
        finally:
            stopped.set()
            worker.join(timeout=4)
            server.close()
            if os.path.exists(SOCKET):
                os.unlink(SOCKET)
    if worker.is_alive():
        raise RuntimeError('fixture socket thread did not stop')


if __name__ == '__main__':
    main()
