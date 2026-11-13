// SPDX-License-Identifier: GPL-3.0-only
import * as socket from 'socket';

function milliseconds() {
    const t = clock(true);
    return t[0] * 1000 + int(t[1] / 1000000);
};

function ready(conn, flags, deadline) {
    const remaining = deadline - milliseconds();
    if (remaining <= 0)
        die('ipc_timeout');
    const events = socket.poll(remaining, [conn, flags]);
    if (!events || !length(events) || !(events[0][1] & flags))
        die('ipc_timeout');
};

export function query(request) {
    const conn = socket.connect({ path: '/tmp/run/arednlink-hf/control.sock' }, null, null, 3000);
    if (!conn)
        die('ipc_unavailable');
    let result;
    try {
        const wire = sprintf('%J\n', request);
        let offset = 0;
        let deadline = milliseconds() + 3000;
        while (offset < length(wire)) {
            ready(conn, socket.POLLOUT, deadline);
            const n = conn.send(substr(wire, offset), socket.MSG_DONTWAIT | socket.MSG_NOSIGNAL);
            if (n === null || n <= 0)
                die('ipc_write');
            offset += n;
        }
        let response = '';
        deadline = milliseconds() + 3000;
        for (;;) {
            ready(conn, socket.POLLIN, deadline);
            const chunk = conn.recv(4096, socket.MSG_DONTWAIT);
            if (!length(chunk))
                die('ipc_incomplete');
            response += chunk;
            if (length(response) > 524288)
                die('ipc_oversize');
            const newline = index(response, '\n');
            if (newline >= 0) {
                if (newline !== length(response) - 1)
                    die('ipc_framing');
                result = json(substr(response, 0, newline));
                if (type(result) !== 'object' || type(result.ok) !== 'bool')
                    die('ipc_response');
                break;
            }
        }
    }
    catch (e) {
        conn.close();
        die('ipc_failed');
    }
    conn.close();
    return result;
};
